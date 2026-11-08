package usage

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

func TestParseEndpointFilter(t *testing.T) {
	filter, ok := ParseEndpointFilter("/droidproxy/usage?provider=Codex&account=a%2Bb%40x.com")
	if !ok || filter != (EndpointFilter{Provider: "codex", Account: "a+b@x.com"}) {
		t.Fatalf("filter = %+v, %v", filter, ok)
	}
	if filter, ok := ParseEndpointFilter("/droidproxy/usage"); !ok || filter != (EndpointFilter{}) {
		t.Fatalf("bare path = %+v, %v", filter, ok)
	}
	for _, path := range []string{"/v1/models", "/droidproxy/usage/extra"} {
		if _, ok := ParseEndpointFilter(path); ok {
			t.Errorf("%s matched", path)
		}
	}
}

func TestCurlCommandEncodesAccountAndTargetsProxyPort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	command := CurlCommand("codex", "codex-a+b@x.com.json")
	want := "curl -s 'http://127.0.0.1:8317/droidproxy/usage?provider=codex&account=codex-a%2Bb@x.com.json'"
	if command != want {
		t.Fatalf("command = %s\nwant      %s", command, want)
	}
	if strings.Contains(CurlCommand("meta", "it's"), "it's") {
		t.Fatal("single quote was not escaped")
	}
}

func TestCurlCommandBracketsIPv6BindAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := prefs.Shared().Set(prefs.KeyBetaFlag, true); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Shared().Set(prefs.KeyBindAddress, "::1"); err != nil {
		t.Fatal(err)
	}
	command := CurlCommand("codex", "codex-a.json")
	if !strings.Contains(command, "http://[::1]:8317/droidproxy/usage") {
		t.Fatalf("command = %s, want bracketed IPv6 authority", command)
	}
}

// A raw semicolon in the account value makes Go's query parser drop the
// pair, silently un-filtering the response; the copied curl must encode it.
func TestCurlCommandRoundTripsSemicolonAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	command := CurlCommand("claude", "claude-seat;work.json")
	if !strings.Contains(command, "claude-seat%3Bwork.json") {
		t.Fatalf("command = %s, want percent-encoded semicolon", command)
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(command, "curl -s '"), "'")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	filter, ok := ParseEndpointFilter(u.RequestURI())
	if !ok || filter.Provider != "claude" || filter.Account != "claude-seat;work.json" {
		t.Fatalf("round-trip filter = %+v, %v", filter, ok)
	}
}

func TestEndpointJSONReportsRemainingPercentAndSecondsUntilReset(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	reset := now.Add(7200 * time.Second)
	used := 97.46
	usage := AccountUsage{ID: "codex-a.json", Provider: "codex", Email: "a@x.com",
		Windows: []Window{newWindow("5-hour", &used, "", &reset)}}

	var root struct {
		Accounts []struct {
			Provider string           `json:"provider"`
			Error    *string          `json:"error"`
			Windows  []map[string]any `json:"windows"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(endpointJSON([]AccountUsage{usage}, now), &root); err != nil {
		t.Fatal(err)
	}
	account := root.Accounts[0]
	window := account.Windows[0]
	if account.Provider != "codex" || account.Error != nil || window["name"] != "5-hour" ||
		window["remaining_percent"] != 2.5 || window["resets_in_seconds"] != 7200.0 {
		t.Fatalf("account = %+v", account)
	}
}

func TestServeEndpointFiltersAccountsAndListsAvailableOnes(t *testing.T) {
	var calls atomic.Int32
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/rate-limit-reset-credits") {
			_, _ = w.Write([]byte(`{"credits":[]}`))
			return
		}
		calls.Add(1)
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10}}}`))
	}, nil, nil)
	path := writeAccountFile(t, env.home, "codex-a@b.com.json", map[string]any{
		"type": "codex", "email": "a@b.com", "access_token": "codex-token",
	})
	accounts := map[auth.ServiceType][]auth.Account{
		auth.Codex: {{ID: filepath.Base(path), Email: "a@b.com", Type: auth.Codex, FilePath: path}},
	}

	status, body := env.tracker.ServeEndpoint(EndpointFilter{Provider: "codex", Account: "A@B.com"}, accounts, time.Now())
	if status != 200 || !strings.Contains(string(body), `"remaining_percent": 90`) {
		t.Fatalf("status %d body %s", status, body)
	}
	env.tracker.ServeEndpoint(EndpointFilter{}, accounts, time.Now())
	if calls.Load() != 1 {
		t.Fatalf("usage API calls = %d, want 1 (the second read shares the 15s budget)", calls.Load())
	}

	status, body = env.tracker.ServeEndpoint(EndpointFilter{Provider: "claude"}, accounts, time.Now())
	if status != 404 || !strings.Contains(string(body), `"id": "codex-a@b.com.json"`) {
		t.Fatalf("status %d body %s", status, body)
	}
}
