package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

// EndpointPath is answered by ThinkingProxy on :8317 itself, never forwarded.
// The web UI's gauges copy a curl for it, so an agent can poll its own quota
// and pause before it runs dry (Droid stops retrying failed requests after
// about 30 seconds).
const EndpointPath = "/droidproxy/usage"

const proxyPort = 8317

// EndpointFilter holds the optional query parameters: provider (codex,
// claude, grok or meta) and account (auth file name, email or display name).
type EndpointFilter struct {
	Provider string
	Account  string
}

// ParseEndpointFilter reports whether requestPath addresses the endpoint and
// returns its filter.
func ParseEndpointFilter(requestPath string) (EndpointFilter, bool) {
	u, err := url.Parse(requestPath)
	if err != nil || u.Path != EndpointPath {
		return EndpointFilter{}, false
	}
	query := u.Query()
	return EndpointFilter{
		Provider: strings.ToLower(query.Get("provider")),
		Account:  query.Get("account"),
	}, true
}

// ProxyHost is the host the copied curl targets: the configured bind address,
// since localhost would not reach a specific interface.
func ProxyHost() string {
	bind := prefs.BindAddress()
	if bind == "0.0.0.0" || bind == "" {
		return "127.0.0.1"
	}
	return bind
}

// CurlCommand is the command a gauge copies for one account.
func CurlCommand(provider, accountID string) string {
	return fmt.Sprintf("curl -s 'http://%s:%d%s?provider=%s&account=%s'",
		ProxyHost(), proxyPort, EndpointPath, provider, encodeQueryValue(accountID))
}

// encodeQueryValue keeps the characters the macOS app leaves readable (such
// as @ in auth file names) and also escapes ' so the command stays one
// shell-quoted argument.
func encodeQueryValue(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-._~!$()*,;:@/?", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// ServeEndpoint answers GET /droidproxy/usage from the given accounts, read
// through the same polling cache as the Settings gauges.
func (t *Tracker) ServeEndpoint(filter EndpointFilter, accounts map[auth.ServiceType][]auth.Account, now time.Time) (int, []byte) {
	candidates := func(st auth.ServiceType) []auth.Account {
		if filter.Provider != "" && filter.Provider != string(st) || !prefs.IsProviderEnabled(string(st)) {
			return nil
		}
		var out []auth.Account
		for _, a := range accounts[st] {
			if filter.Account == "" || accountMatches(a, filter.Account) {
				out = append(out, a)
			}
		}
		return out
	}
	eligible := eligibleAccounts(candidates(auth.Codex), candidates(auth.Claude), candidates(auth.Grok), candidates(auth.Meta))
	if len(eligible) == 0 {
		available := []map[string]string{}
		for _, st := range []auth.ServiceType{auth.Codex, auth.Claude, auth.Grok, auth.Meta} {
			for _, a := range accounts[st] {
				available = append(available, map[string]string{"provider": string(st), "account": a.DisplayName(), "id": a.ID})
			}
		}
		return 404, encodeEndpointJSON(map[string]any{"error": "No matching enabled OAuth account", "available": available})
	}
	return 200, endpointJSON(t.fetch(eligible), now)
}

func accountMatches(a auth.Account, wanted string) bool {
	needle := strings.ToLower(wanted)
	return strings.ToLower(a.ID) == needle || a.Email != "" && strings.ToLower(a.Email) == needle ||
		strings.ToLower(a.DisplayName()) == needle
}

// endpointJSON renders usages with each window's remaining percent and the
// seconds until it resets.
func endpointJSON(usages []AccountUsage, now time.Time) []byte {
	iso := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return t.UTC().Format(time.RFC3339)
	}
	rounded := func(v *float64) any {
		if v == nil {
			return nil
		}
		return math.Round(*v*10) / 10
	}
	accounts := make([]map[string]any, 0, len(usages))
	for _, u := range usages {
		windows := make([]map[string]any, 0, len(u.Windows))
		for _, w := range u.Windows {
			var resetsIn any
			if w.ResetDate != nil {
				resetsIn = max(0, int(w.ResetDate.Sub(now).Seconds()))
			}
			windows = append(windows, map[string]any{
				"name":              w.Title,
				"used_percent":      rounded(w.UsedPercent),
				"remaining_percent": rounded(w.RemainingPercent),
				"resets_at":         iso(w.ResetDate),
				"resets_in_seconds": resetsIn,
			})
		}
		var usageError any
		if u.Error != "" {
			usageError = u.Error
		}
		accounts = append(accounts, map[string]any{
			"provider": u.Provider,
			"account":  u.Email,
			"id":       u.ID,
			"windows":  windows,
			"error":    usageError,
			"as_of":    iso(u.UpdatedAt),
		})
	}
	return encodeEndpointJSON(map[string]any{"generated_at": now.UTC().Format(time.RFC3339), "accounts": accounts})
}

func encodeEndpointJSON(v any) []byte {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		return []byte(`{"error":"encoding failed"}`)
	}
	return buf.Bytes()
}
