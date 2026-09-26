package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/grok"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/meta"
)

const (
	requestTimeout = 15 * time.Second

	codexUsageEndpoint  = "https://chatgpt.com/backend-api/wham/usage"
	claudeUsageEndpoint = "https://api.anthropic.com/api/oauth/usage"
	claudeTokenEndpoint = "https://platform.claude.com/v1/oauth/token"
	grokUsageEndpoint   = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"

	// Public OAuth client id of the Claude CLI, same as the macOS app.
	claudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
)

// Tracker fetches OAuth usage for Codex and Claude accounts. The HTTP client
// and endpoints are injectable so tests never touch the network.
type Tracker struct {
	mu         sync.Mutex
	accounts   []AccountUsage
	refreshin  bool
	generation int
	onChange   func()

	client         *http.Client
	codexUsageURL  string
	claudeUsageURL string
	claudeTokenURL string
	grokUsageURL   string

	// grokToken resolves the SuperGrok bearer. Defaults to grok.AccessToken;
	// tests inject a stub so no refresh is attempted.
	grokToken func() (string, *grok.AuthError)
	// metaSnapshots reads last-observed Meta usage. Defaults to the shared
	// store; tests inject an in-memory stub.
	metaSnapshots func(accountID string) *meta.UsageSnapshot
}

// NewTracker creates a tracker with the default HTTP client and endpoints.
func NewTracker() *Tracker {
	return &Tracker{
		client:         &http.Client{Timeout: requestTimeout},
		codexUsageURL:  codexUsageEndpoint,
		claudeUsageURL: claudeUsageEndpoint,
		claudeTokenURL: claudeTokenEndpoint,
		grokUsageURL:   grokUsageEndpoint,
		grokToken:      grok.AccessToken,
		metaSnapshots:  func(accountID string) *meta.UsageSnapshot { return meta.SharedUsageStore().Snapshot(accountID) },
	}
}

// SetClient injects the HTTP client used for all usage and token requests.
func (t *Tracker) SetClient(client *http.Client) {
	t.mu.Lock()
	t.client = client
	t.mu.Unlock()
}

// SetBaseURLs overrides the endpoints (tests point these at httptest servers).
func (t *Tracker) SetBaseURLs(codexUsage, claudeUsage, claudeToken string) {
	t.SetBaseURLsWithGrok(codexUsage, claudeUsage, claudeToken, grokUsageEndpoint)
}

// SetBaseURLsWithGrok overrides all endpoints including the Grok billing URL.
func (t *Tracker) SetBaseURLsWithGrok(codexUsage, claudeUsage, claudeToken, grokUsage string) {
	t.mu.Lock()
	t.codexUsageURL, t.claudeUsageURL, t.claudeTokenURL, t.grokUsageURL = codexUsage, claudeUsage, claudeToken, grokUsage
	t.mu.Unlock()
}

// SetGrokToken injects the SuperGrok bearer resolver (tests only).
func (t *Tracker) SetGrokToken(fn func() (string, *grok.AuthError)) {
	t.mu.Lock()
	t.grokToken = fn
	t.mu.Unlock()
}

// SetMetaSnapshots injects the Meta usage reader (tests only).
func (t *Tracker) SetMetaSnapshots(fn func(accountID string) *meta.UsageSnapshot) {
	t.mu.Lock()
	t.metaSnapshots = fn
	t.mu.Unlock()
}

// SetOnChange registers a callback invoked whenever the snapshot changes
// (refresh started or completed).
func (t *Tracker) SetOnChange(fn func()) {
	t.mu.Lock()
	t.onChange = fn
	t.mu.Unlock()
}

// Accounts returns a copy of the current snapshot.
func (t *Tracker) Accounts() []AccountUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]AccountUsage(nil), t.accounts...)
}

// IsRefreshing reports whether a refresh is in flight.
func (t *Tracker) IsRefreshing() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.refreshin
}

// Refresh fetches usage for every eligible Codex, Claude, Grok, and Meta
// account. Accounts are queried concurrently; the snapshot starts as loading
// placeholders and is replaced by the sorted results. Disabled and expired
// accounts are skipped (Meta ignores key expiry: a stale card still shows its
// "as of" time), and an empty selection clears the snapshot.
func (t *Tracker) Refresh(codexAccounts, claudeAccounts, grokAccounts, metaAccounts []auth.Account) {
	var enabled []auth.Account
	for _, a := range codexAccounts {
		if !a.Disabled && !a.IsExpired() {
			enabled = append(enabled, a)
		}
	}
	for _, a := range claudeAccounts {
		if !a.Disabled && !a.IsExpired() {
			enabled = append(enabled, a)
		}
	}
	enabled = append(enabled, activeGrokAccounts(grokAccounts)...)
	for _, a := range metaAccounts {
		if !a.Disabled {
			enabled = append(enabled, a)
		}
	}

	t.mu.Lock()
	t.generation++
	generation := t.generation
	if len(enabled) == 0 {
		t.accounts = nil
		t.refreshin = false
		notify := t.onChange
		t.mu.Unlock()
		if notify != nil {
			notify()
		}
		return
	}

	placeholders := make([]AccountUsage, len(enabled))
	for i, a := range enabled {
		placeholders[i] = loadingPlaceholder(a, a.Type)
	}
	t.accounts = placeholders
	t.refreshin = true
	notify := t.onChange
	t.mu.Unlock()
	if notify != nil {
		notify()
	}

	results := make([]AccountUsage, len(enabled))
	var wg sync.WaitGroup
	for i, account := range enabled {
		wg.Add(1)
		go func(i int, account auth.Account) {
			defer wg.Done()
			switch account.Type {
			case auth.Claude:
				results[i] = t.fetchClaudeUsage(account, generation)
			case auth.Grok:
				results[i] = t.fetchGrokUsage(account)
			case auth.Meta:
				results[i] = t.fetchMetaUsage(account)
			default:
				results[i] = t.fetchCodexUsage(account, generation)
			}
		}(i, account)
	}

	go func() {
		wg.Wait()
		t.mu.Lock()
		if generation != t.generation {
			t.mu.Unlock()
			return
		}
		t.accounts = sortAccountUsages(results)
		t.refreshin = false
		notify := t.onChange
		t.mu.Unlock()
		if notify != nil {
			notify()
		}
	}()
}

// UpdateMetaAccounts replaces only the Meta cards from the local
// last-observed store. Used for live updates when ThinkingProxy sniffs a new
// snapshot, so a Meta response does not refetch Codex/Claude/Grok usage over
// the network.
func (t *Tracker) UpdateMetaAccounts(metaAccounts []auth.Account) {
	var fresh []AccountUsage
	for _, a := range metaAccounts {
		if !a.Disabled {
			fresh = append(fresh, t.fetchMetaUsage(a))
		}
	}
	t.mu.Lock()
	hasMeta := false
	for _, a := range t.accounts {
		if a.Provider == string(auth.Meta) {
			hasMeta = true
			break
		}
	}
	if !hasMeta && len(fresh) == 0 {
		t.mu.Unlock()
		return
	}
	kept := make([]AccountUsage, 0, len(t.accounts)+len(fresh))
	for _, a := range t.accounts {
		if a.Provider != string(auth.Meta) {
			kept = append(kept, a)
		}
	}
	t.accounts = sortAccountUsages(append(kept, fresh...))
	notify := t.onChange
	t.mu.Unlock()
	if notify != nil {
		notify()
	}
}

// sortAccountUsages returns a sorted copy: by provider id, then by email
// (case-insensitive).
func sortAccountUsages(values []AccountUsage) []AccountUsage {
	sorted := append([]AccountUsage(nil), values...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Provider == sorted[j].Provider {
			return strings.ToLower(sorted[i].Email) < strings.ToLower(sorted[j].Email)
		}
		return sorted[i].Provider < sorted[j].Provider
	})
	return sorted
}

// activeGrokAccounts mirrors the macOS tracker: the Grok bearer resolver only
// serves the newest enabled credential file, so other Grok auth files have no
// token source (and are not used for requests either).
func activeGrokAccounts(accounts []auth.Account) []auth.Account {
	_, activePath, ok := grok.LoadActiveCredentials()
	if !ok {
		return nil
	}
	activeFile := filepath.Base(activePath)
	var out []auth.Account
	for _, a := range accounts {
		if !a.Disabled && !a.IsExpired() && a.ID == activeFile {
			out = append(out, a)
		}
	}
	return out
}

// Request helper.

func (t *Tracker) clientAndURL(kind string) (*http.Client, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch kind {
	case "codex":
		return t.client, t.codexUsageURL
	case "claude":
		return t.client, t.claudeUsageURL
	case "grok":
		return t.client, t.grokUsageURL
	default:
		return t.client, t.claudeTokenURL
	}
}

// errText maps transport errors to the user-facing strings URLSession
// produced on macOS where the wording matters (timeouts).
func errText(err error) string {
	if errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "context deadline exceeded") ||
		strings.Contains(err.Error(), "Client.Timeout") {
		return "The request timed out."
	}
	return err.Error()
}

func doJSON(client *http.Client, request *http.Request) ([]byte, int, error) {
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, response.StatusCode, err
	}
	return data, response.StatusCode, nil
}

// Codex.

func (t *Tracker) fetchCodexUsage(account auth.Account, generation int) AccountUsage {
	authData, ok := authValues(account.FilePath)
	if !ok {
		return failedAccount(account, "Missing access token")
	}
	token, ok := authData["access_token"]
	if !ok {
		return failedAccount(account, "Missing access token")
	}
	client, endpoint := t.clientAndURL("codex")
	if _, err := url.Parse(endpoint); err != nil {
		return failedAccount(account, "Invalid usage endpoint")
	}

	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return failedAccount(account, "Invalid usage endpoint")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "codex-cli")
	if accountID, ok := authData["account_id"]; ok {
		request.Header.Set("ChatGPT-Account-Id", accountID)
	}

	return t.fetchJSONUsage(account, client, request, parseCodexWindows)
}

func (t *Tracker) fetchJSONUsage(account auth.Account, client *http.Client, request *http.Request, parse func(any) []Window) AccountUsage {
	data, status, err := doJSON(client, request)
	if err != nil {
		return failedAccount(account, errText(err))
	}
	if status < 200 || status >= 300 {
		return failedAccount(account, fmt.Sprintf("Usage API returned %d", status))
	}
	var body any
	if err := json.Unmarshal(data, &body); err != nil {
		return failedAccount(account, "Usage response did not include quota windows")
	}
	windows := parse(body)
	if len(windows) == 0 {
		return failedAccount(account, "Usage response did not include quota windows")
	}
	return successAccount(account, windows)
}

// Grok.

func (t *Tracker) fetchGrokUsage(account auth.Account) AccountUsage {
	t.mu.Lock()
	resolveToken := t.grokToken
	t.mu.Unlock()
	token, authErr := resolveToken()
	if authErr != nil {
		return failedAccount(account, authErr.Error())
	}

	client, endpoint := t.clientAndURL("grok")
	if _, err := url.Parse(endpoint); err != nil {
		return failedAccount(account, "Invalid usage endpoint")
	}
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return failedAccount(account, "Invalid usage endpoint")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	request.Header.Set("Accept", "application/json")

	data, status, err := doJSON(client, request)
	if err != nil {
		return failedAccount(account, errText(err))
	}
	// No retry on 401: the bearer resolver refreshes only on local expiry,
	// so a retry would resend the same rejected token.
	if status == 401 || status == 403 {
		return failedAccount(account, fmt.Sprintf("Grok rejected the stored credentials (HTTP %d). Reconnect Grok in Settings.", status))
	}
	if status < 200 || status >= 300 {
		return failedAccount(account, fmt.Sprintf("Grok usage API returned %d", status))
	}
	windows := ParseGrokWindows(data)
	if len(windows) == 0 {
		return failedAccount(account, "Usage response did not include quota windows")
	}
	return successAccount(account, windows)
}

// Meta.

// fetchMetaUsage reads the local last-observed store only. Meta has no usage
// endpoint: the 5-hour window and weekly percents arrive as
// `response.subscription_usage` SSE events that ThinkingProxy sniffs into the
// store, so unlike the other providers this never touches the network and
// only the account serving Responses traffic ever gets observations.
func (t *Tracker) fetchMetaUsage(account auth.Account) AccountUsage {
	t.mu.Lock()
	readSnapshot := t.metaSnapshots
	t.mu.Unlock()
	snapshot := readSnapshot(account.ID)
	if snapshot == nil {
		return failedAccount(account, "No usage observed yet — send a Meta request through DroidProxy")
	}
	windows := ParseMetaWindows(*snapshot, time.Now())
	if len(windows) == 0 {
		return failedAccount(account, "Stored Meta usage was incomplete")
	}
	usage := successAccount(account, windows)
	observed := snapshot.ObservedAt
	usage.UpdatedAt = &observed
	return usage
}

// Claude.

func (t *Tracker) fetchClaudeUsage(account auth.Account, generation int) AccountUsage {
	authData, ok := authValues(account.FilePath)
	if !ok {
		return failedAccount(account, "Missing access token")
	}
	token, ok := authData["access_token"]
	if !ok {
		return failedAccount(account, "Missing access token")
	}

	client, endpoint := t.clientAndURL("claude")
	expired := false
	if exp, ok := authData["expired"]; ok {
		if date := parseISO8601Date(exp); date != nil && !date.After(time.Now()) {
			expired = true
		}
	}

	if expired {
		newToken, err := t.refreshClaudeTokens(account.FilePath)
		if err != nil {
			return failedAccount(account, "Token refresh failed: "+err.Error())
		}
		token = newToken
	}
	return t.executeClaudeUsageRequest(account, client, endpoint, token, generation)
}

func (t *Tracker) executeClaudeUsageRequest(account auth.Account, client *http.Client, endpoint, token string, generation int) AccountUsage {
	data, status, err := doJSON(client, makeClaudeUsageRequest(endpoint, token))
	if err != nil {
		return failedAccount(account, errText(err))
	}
	if status == 401 || status == 403 {
		return t.retryClaudeUsageAfterRefresh(account, client, endpoint, generation)
	}
	if status < 200 || status >= 300 {
		return failedAccount(account, fmt.Sprintf("Claude usage API returned %d", status))
	}
	windows := ParseClaudeWindows(data)
	if len(windows) == 0 {
		return failedAccount(account, "Usage response did not include quota windows")
	}
	return successAccount(account, windows)
}

func (t *Tracker) retryClaudeUsageAfterRefresh(account auth.Account, client *http.Client, endpoint string, generation int) AccountUsage {
	if _, ok := authValues(account.FilePath); !ok {
		return failedAccount(account, "Failed to read credentials for retry")
	}
	newToken, err := t.refreshClaudeTokens(account.FilePath)
	if err != nil {
		return failedAccount(account, "Retry token refresh failed: "+err.Error())
	}
	data, status, err := doJSON(client, makeClaudeUsageRequest(endpoint, newToken))
	if err != nil || data == nil {
		return failedAccount(account, "No HTTP response on retry")
	}
	if status != 200 {
		return failedAccount(account, fmt.Sprintf("Claude usage returned %d after refresh retry", status))
	}
	windows := ParseClaudeWindows(data)
	if len(windows) == 0 {
		return failedAccount(account, "Usage response did not include quota windows")
	}
	return successAccount(account, windows)
}

func makeClaudeUsageRequest(endpoint, token string) *http.Request {
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	return request
}

// refreshClaudeTokens refreshes the OAuth tokens for an auth file, rewrites
// the file pretty-printed (access_token, refresh_token, expired, last_refresh)
// and returns the new access token.
func (t *Tracker) refreshClaudeTokens(filePath string) (string, error) {
	authData, ok := authValues(filePath)
	if !ok {
		return "", errors.New("No refresh token available")
	}
	refreshToken, ok := authData["refresh_token"]
	if !ok {
		return "", errors.New("No refresh token available")
	}

	client, endpoint := t.clientAndURL("token")
	body := "client_id=" + claudeClientID + "&grant_type=refresh_token&refresh_token=" + formURLEncode(refreshToken)
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
	if err != nil {
		return "", errors.New("Token refresh failed")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	data, status, err := doJSON(client, request)
	if err != nil || status != 200 {
		return "", errors.New("Token refresh failed")
	}

	var tokens struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &tokens); err != nil ||
		tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return "", errors.New("Invalid token response format")
	}

	expiresIn := tokens.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3600
	}
	now := time.Now()
	newExpired := now.Add(time.Duration(expiresIn) * time.Second).UTC().Format("2006-01-02T15:04:05Z")

	var existing map[string]any
	if raw, err := os.ReadFile(filePath); err == nil {
		_ = json.Unmarshal(raw, &existing)
	}
	if existing == nil {
		existing = map[string]any{}
	}
	existing["access_token"] = tokens.AccessToken
	existing["refresh_token"] = tokens.RefreshToken
	existing["expired"] = newExpired
	existing["last_refresh"] = now.UTC().Format("2006-01-02T15:04:05Z")

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(existing); err != nil {
		return "", err
	}
	if err := os.WriteFile(filePath, buf.Bytes(), 0o600); err != nil {
		return "", err
	}

	logx.Logf("[OAuthUsageTracker] Refreshed Claude tokens for %s", filePath)
	return tokens.AccessToken, nil
}

// formURLEncode percent-encodes with the unreserved set (RFC 3986), matching
// Swift's addingPercentEncoding(withAllowedCharacters:) in the tracker.
func formURLEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}
