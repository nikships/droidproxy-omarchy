package usage

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/claude"
)

// Banked usage resets (Codex reset credits, Claude cedar_ember grants) are
// one-time credits saved on the provider account. Reading them rides along
// with the normal usage reads; redeeming spends exactly one credit and is
// only ever user-initiated. The endpoint shapes follow the providers' own
// clients (Codex wham API, Anthropic OAuth usage), as also implemented by
// T3 Code's hub reader.

// ResetCredits is the banked-reset state for one account. NextCreditID is
// the opaque provider id of the soonest-expiring credit; it stays inside
// the daemon (the control API exposes only count and expiry).
type ResetCredits struct {
	AvailableCount int
	NextCreditID   string
	NextExpiresAt  *time.Time
	// NextResetsLeft is how many resets the next Claude grant itself
	// holds; part of the redemption idempotency key so each logical
	// redemption differs while retries of one attempt stay identical.
	NextResetsLeft int
}

// codexCreditsURL derives the reset-credits URL from the usage URL
// (.../wham/usage -> .../wham/rate-limit-reset-credits).
func codexCreditsURL(usageEndpoint string) string {
	return strings.TrimSuffix(usageEndpoint, "/usage") + "/rate-limit-reset-credits"
}

type codexCredit struct {
	ID        string
	ExpiresAt time.Time
}

func parseCodexCredits(data []byte, now time.Time) []codexCredit {
	var body struct {
		Credits []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			ResetType string `json:"reset_type"`
			ExpiresAt string `json:"expires_at"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil
	}
	var out []codexCredit
	for _, c := range body.Credits {
		if c.ResetType != "codex_rate_limits" || c.Status != "available" || c.ID == "" {
			continue
		}
		expires := parseISO8601Date(c.ExpiresAt)
		if expires == nil || !expires.After(now) {
			continue
		}
		out = append(out, codexCredit{ID: c.ID, ExpiresAt: *expires})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	return out
}

func codexResetCredits(data []byte, now time.Time) *ResetCredits {
	credits := parseCodexCredits(data, now)
	if len(credits) == 0 {
		return nil
	}
	next := credits[0].ExpiresAt
	return &ResetCredits{AvailableCount: len(credits), NextCreditID: credits[0].ID, NextExpiresAt: &next}
}

func codexRequest(client *http.Client, method, endpoint, token, chatGPTAccountID string, body []byte) ([]byte, int, http.Header, error) {
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		return nil, 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "codex-cli")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if chatGPTAccountID != "" {
		request.Header.Set("ChatGPT-Account-Id", chatGPTAccountID)
	}
	return doJSON(client, request)
}

// fetchCodexResetCredits is best-effort: a failed credits read never fails
// the usage read it rides along with. A 429 still reports its cooldown so
// the sample's polling budget honors it.
func (t *Tracker) fetchCodexResetCredits(account auth.Account) (credits *ResetCredits, rateLimited bool, retryAfter *time.Time) {
	authData, ok := authValues(account.FilePath)
	if !ok {
		return nil, false, nil
	}
	token, ok := authData["access_token"]
	if !ok || account.IsAccessExpired() {
		return nil, false, nil
	}
	client, endpoint := t.clientAndURL("codex")
	data, status, header, err := codexRequest(client, http.MethodGet, codexCreditsURL(endpoint), token, authData["account_id"], nil)
	if err != nil || status < 200 || status >= 300 {
		if status == http.StatusTooManyRequests {
			return nil, true, retryAfterDate(header.Get("Retry-After"), time.Now())
		}
		return nil, false, nil
	}
	return codexResetCredits(data, time.Now()), false, nil
}

// redeemNamespace matches T3 Code's UUIDv5 namespace, so a retry from
// either tool addresses the same provider-side redemption.
var redeemNamespace = []byte{0x6f, 0x1c, 0x2a, 0x9e, 0x2d, 0x4b, 0x4c, 0x1e, 0x9a, 0x7f, 0x3b, 0x8d, 0x5e, 0x0c, 0x1a, 0x42}

func redeemRequestID(name string) string {
	sum := sha1.Sum(append(append([]byte{}, redeemNamespace...), name...))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// parseClaudeResetCredits reads the cedar_ember block of a Claude OAuth
// usage payload. The redeemable count is the sum of resets_left over live
// grants, and only counts when the payload names a next grant.
func parseClaudeResetCredits(data []byte, now time.Time) *ResetCredits {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	block, ok := root["cedar_ember"].(map[string]any)
	if !ok {
		return nil
	}
	if eligible, _ := block["eligible"].(bool); !eligible {
		return nil
	}
	nextID, _ := block["next_grant_id"].(string)
	grants, _ := block["grants"].([]any)
	count := 0
	var next *ResetCredits
	for _, raw := range grants {
		grant, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := grant["id"].(string)
		if id == "" {
			continue
		}
		if paused, _ := grant["paused"].(bool); paused {
			continue
		}
		if usable, _ := grant["usable_now"].(bool); !usable {
			continue
		}
		var endsAt *time.Time
		if s, ok := grant["ends_at"].(string); ok && s != "" {
			endsAt = parseISO8601Date(s)
			if endsAt == nil || !endsAt.After(now) {
				continue
			}
		}
		left, _ := numberValue(grant["resets_left"])
		count += int(left)
		if id == nextID {
			next = &ResetCredits{NextCreditID: id, NextExpiresAt: endsAt, NextResetsLeft: int(left)}
		}
	}
	if next == nil || count == 0 {
		return nil
	}
	next.AvailableCount = count
	return next
}

// RedeemReset spends one banked reset for the account and returns a
// user-facing message. It never runs on its own; callers gate it behind an
// explicit user confirmation.
func (t *Tracker) RedeemReset(account auth.Account) (string, bool) {
	var message string
	var ok bool
	switch account.Type {
	case auth.Codex:
		message, ok = t.redeemCodexReset(account)
	case auth.Claude:
		message, ok = t.redeemClaudeReset(account)
	default:
		return "Banked resets are only available for Codex and Claude accounts.", false
	}
	if ok {
		// Quota and credit counts just changed provider-side; the next
		// read must not serve the pre-reset sample.
		t.cache.invalidate(account)
	}
	return message, ok
}

func (t *Tracker) redeemCodexReset(account auth.Account) (string, bool) {
	authData, ok := authValues(account.FilePath)
	if !ok {
		return "Missing access token.", false
	}
	token, ok := authData["access_token"]
	if !ok {
		return "Missing access token.", false
	}
	if account.IsAccessExpired() {
		return "Access token expired; it renews on this account's next request.", false
	}
	client, endpoint := t.clientAndURL("codex")
	creditsURL := codexCreditsURL(endpoint)
	data, status, _, err := codexRequest(client, http.MethodGet, creditsURL, token, authData["account_id"], nil)
	if err != nil || status < 200 || status >= 300 {
		return "Could not read banked resets for this account.", false
	}
	credits := parseCodexCredits(data, time.Now())
	if len(credits) == 0 {
		return "No banked reset is available for this account.", false
	}
	credit := credits[0]
	accountKey := authData["account_id"]
	if accountKey == "" {
		accountKey = account.ID
	}
	body, _ := json.Marshal(map[string]string{
		"redeem_request_id": redeemRequestID(accountKey + ":" + credit.ID),
		"credit_id":         credit.ID,
	})
	data, status, _, err = codexRequest(client, http.MethodPost, creditsURL+"/consume", token, authData["account_id"], body)
	if err != nil || status < 200 || status >= 300 {
		return "Codex could not redeem the reset.", false
	}
	var outcome struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &outcome); err != nil {
		return "Codex returned an unexpected reset response.", false
	}
	switch outcome.Code {
	case "reset":
		return "Banked reset applied. Quota windows are refreshed.", true
	case "already_redeemed":
		return "That reset was already redeemed. Quota is up to date.", true
	case "nothing_to_reset":
		return "Nothing to reset right now; the credit was not spent.", false
	case "no_credit":
		return "No banked reset is available for this account.", false
	default:
		return "Codex returned an unexpected reset response.", false
	}
}

func (t *Tracker) claudeTokenFor(account auth.Account) (string, bool) {
	authData, ok := authValues(account.FilePath)
	if !ok {
		return "", false
	}
	token, ok := authData["access_token"]
	if !ok {
		return "", false
	}
	if exp, ok := authData["expired"]; ok {
		if date := parseISO8601Date(exp); date != nil && !date.After(time.Now()) {
			newToken, err := t.refreshClaudeTokens(account.FilePath)
			if err != nil {
				return "", false
			}
			token = newToken
		}
	}
	return token, true
}

func claudeOrganizationUUID(account auth.Account) string {
	data, err := os.ReadFile(account.FilePath)
	if err != nil {
		return ""
	}
	var jsonData map[string]any
	if err := json.Unmarshal(data, &jsonData); err != nil {
		return ""
	}
	return claude.OrganizationFieldsFrom(jsonData).UUID
}

func (t *Tracker) redeemClaudeReset(account auth.Account) (string, bool) {
	token, ok := t.claudeTokenFor(account)
	if !ok {
		return "Sign in to Claude again to redeem resets.", false
	}
	organization := claudeOrganizationUUID(account)
	if organization == "" {
		return "Sign in to Claude again to redeem resets.", false
	}
	client, endpoint := t.clientAndURL("claude")

	data, status, _, err := doJSON(client, makeClaudeUsageRequest(endpoint, token))
	if err != nil || status < 200 || status >= 300 {
		return "Could not read banked resets for this account.", false
	}
	credits := parseClaudeResetCredits(data, time.Now())
	if credits == nil {
		return "No banked reset is available for this account.", false
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "Claude could not redeem the reset.", false
	}
	parsed.Path = "/api/organizations/" + url.PathEscape(organization) + "/reset_rate_limits"
	parsed.RawQuery = ""
	body, _ := json.Marshal(map[string]string{
		"program":  "cedar_ember",
		"grant_id": credits.NextCreditID,
		// resets_left distinguishes logical redemptions of one
		// multi-reset grant; a retry before the spend lands still
		// observes the same count and reuses the key.
		"request_id": redeemRequestID(fmt.Sprintf("claude:%s:%s:%d", organization, credits.NextCreditID, credits.NextResetsLeft)),
	})
	request, err := http.NewRequest(http.MethodPost, parsed.String(), bytes.NewReader(body))
	if err != nil {
		return "Claude could not redeem the reset.", false
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	request.Header.Set("Content-Type", "application/json")

	data, status, _, err = doJSON(client, request)
	if err != nil {
		return "Claude could not redeem the reset.", false
	}
	switch status {
	case 429:
		return "Claude is rate limiting resets. Try again soon.", false
	case 401, 403:
		return "Sign in to Claude again to redeem resets.", false
	}
	if status < 200 || status >= 300 {
		return "Claude could not redeem the reset.", false
	}
	var outcome struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(data, &outcome); err != nil {
		return "Claude returned an unexpected reset response.", false
	}
	switch outcome.Result {
	case "reset":
		return "Banked reset applied. Quota windows are refreshed.", true
	case "already_used":
		return "That reset was already used. Quota is up to date.", true
	case "not_limited":
		return "Nothing to reset right now; the credit was not spent.", false
	case "ineligible":
		return "No banked reset is available for this account.", false
	case "cooldown":
		return "Claude resets are cooling down. Try again later.", false
	default:
		return "Claude could not confirm the reset. If you are still limited in a moment, try again.", false
	}
}
