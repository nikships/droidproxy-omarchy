package usage

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/grok"
	"github.com/nikships/droidproxy-omarchy/internal/meta"
)

func TestParseClaudeWindowsTreatsUtilizationAsPercentForSonnetBucket(t *testing.T) {
	payload := `{
	  "five_hour": {"utilization": 7.0, "resets_at": "2026-06-05T13:00:00.885429+00:00"},
	  "seven_day": {"utilization": 11.0, "resets_at": "2026-06-10T15:59:59.885452+00:00"},
	  "seven_day_sonnet": {"utilization": 1.0, "resets_at": "2026-06-10T16:00:00.885459+00:00"}
	}`

	windows := ParseClaudeWindows([]byte(payload))

	used := func(title string) *float64 {
		for _, w := range windows {
			if w.Title == title {
				return w.UsedPercent
			}
		}
		return nil
	}
	if got := used("5-hour"); got == nil || *got != 7 {
		t.Fatalf("5-hour used = %v", got)
	}
	if got := used("Weekly"); got == nil || *got != 11 {
		t.Fatalf("Weekly used = %v", got)
	}

	var sonnet *Window
	for i := range windows {
		if windows[i].Title == "Weekly (Sonnet)" {
			sonnet = &windows[i]
		}
	}
	if sonnet == nil {
		t.Fatal("missing Weekly (Sonnet)")
	}
	if sonnet.UsedPercent == nil || *sonnet.UsedPercent != 1 {
		t.Fatalf("sonnet used = %v", sonnet.UsedPercent)
	}
	if sonnet.RemainingPercent == nil || *sonnet.RemainingPercent != 99 {
		t.Fatalf("sonnet remaining = %v", sonnet.RemainingPercent)
	}
}

func TestParseClaudeWindowsHandlesEdgeCases(t *testing.T) {
	payload := `{
	  "five_hour": {"utilization": 0.0, "resets_at": "2026-06-05T13:00:00.885429+00:00"},
	  "seven_day": {"utilization": 100.0, "resets_at": "2026-06-10T15:59:59.885452+00:00"},
	  "seven_day_sonnet": {"utilization": -5.0, "resets_at": "2026-06-10T16:00:00.885459+00:00"},
	  "seven_day_opus": {"utilization": 120.0, "resets_at": "2026-06-10T16:00:00.885459+00:00"}
	}`

	windows := ParseClaudeWindows([]byte(payload))

	find := func(title string) *Window {
		for i := range windows {
			if windows[i].Title == title {
				return &windows[i]
			}
		}
		return nil
	}

	fiveHour := find("5-hour")
	if fiveHour == nil || fiveHour.UsedPercent == nil || *fiveHour.UsedPercent != 0 {
		t.Fatalf("5-hour = %+v", fiveHour)
	}
	if *fiveHour.RemainingPercent != 100 || !fiveHour.HasRemaining {
		t.Fatalf("5-hour remaining = %+v", fiveHour)
	}

	weekly := find("Weekly")
	if weekly == nil || weekly.UsedPercent == nil || *weekly.UsedPercent != 100 {
		t.Fatalf("Weekly = %+v", weekly)
	}
	if *weekly.RemainingPercent != 0 || weekly.HasRemaining {
		t.Fatalf("Weekly remaining = %+v", weekly)
	}

	// Clamping edge cases.
	sonnet := find("Weekly (Sonnet)")
	if sonnet == nil || *sonnet.UsedPercent != 0 || *sonnet.RemainingPercent != 100 {
		t.Fatalf("Sonnet = %+v", sonnet)
	}
	opus := find("Weekly (Opus)")
	if opus == nil || *opus.UsedPercent != 100 || *opus.RemainingPercent != 0 {
		t.Fatalf("Opus = %+v", opus)
	}

	// Reset text uses the relative + absolute formatting.
	if fiveHour.ResetText == "" || !strings.Contains(fiveHour.ResetText, " (") {
		t.Fatalf("5-hour resetText = %q", fiveHour.ResetText)
	}
}

func TestParseClaudeWindowsHandlesMalformedJSON(t *testing.T) {
	if windows := ParseClaudeWindows([]byte("{ invalid json")); len(windows) != 0 {
		t.Fatalf("windows = %v", windows)
	}
}

func TestParseCodexWindowsFromRateLimitShape(t *testing.T) {
	payload := `{"rate_limit": {
	  "primary_window": {"used_percent": 27.5, "reset_after_seconds": 3600},
	  "secondary_window": {"used_percent": 10.0, "resets_at": "2030-01-01T00:00:00Z"}
	}}`

	var body any
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		t.Fatal(err)
	}
	windows := parseCodexWindows(body)

	if len(windows) != 2 {
		t.Fatalf("windows = %v", windows)
	}
	if windows[0].Title != "5-hour" || windows[0].UsedPercent == nil || *windows[0].UsedPercent != 27.5 {
		t.Fatalf("primary = %+v", windows[0])
	}
	if windows[1].Title != "Weekly" || windows[1].ResetDate == nil || windows[1].ResetText == "" {
		t.Fatalf("secondary = %+v", windows[1])
	}
}

func TestParseGenericWindowsPatternMatchesAndSorts(t *testing.T) {
	payload := `{"a": {"label": "weekly usage", "percentage": 0.2}, "b": {"label": "full plan", "paid_percent": 5}}`
	var body any
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		t.Fatal(err)
	}
	windows := parseGenericWindows(body)

	if len(windows) != 2 {
		t.Fatalf("windows = %v", windows)
	}
	if windows[0].Title != "Weekly" || windows[1].Title != "Full" {
		t.Fatalf("order = %s, %s", windows[0].Title, windows[1].Title)
	}
	if windows[0].UsedPercent == nil || *windows[0].UsedPercent != 20 {
		t.Fatalf("weekly percent = %v", windows[0].UsedPercent)
	}
}

func TestResetTextForMatchesSwiftFormat(t *testing.T) {
	date := time.Date(2026, 6, 5, 13, 0, 0, 0, time.UTC)
	now := time.Date(2026, 6, 5, 11, 0, 0, 0, time.UTC)
	got := relativeTimeString(date, now)
	if want := "in 2 hours"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	past := relativeTimeString(now.Add(-2*time.Hour), now)
	if want := "2 hours ago"; past != want {
		t.Fatalf("got %q, want %q", past, want)
	}
	full := ResetTextFor(date)
	if !strings.HasPrefix(full, relativeTimeString(date, time.Now())+" (") ||
		!strings.HasSuffix(full, "Jun 5, 2026 at 1:00 PM)") {
		t.Fatalf("full = %q", full)
	}
}

// Harness for network tests.

type testEnv struct {
	codex   *httptest.Server
	claude  *httptest.Server
	tokens  *httptest.Server
	tracker *Tracker
	home    string
}

func writeAccountFile(t *testing.T, dir, name string, content map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, name)
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestEnv(t *testing.T, codexHandler, claudeHandler, tokenHandler http.HandlerFunc) *testEnv {
	t.Helper()
	env := &testEnv{home: t.TempDir()}
	t.Setenv("HOME", env.home)

	if codexHandler != nil {
		env.codex = httptest.NewServer(codexHandler)
		t.Cleanup(env.codex.Close)
	}
	if claudeHandler != nil {
		env.claude = httptest.NewServer(claudeHandler)
		t.Cleanup(env.claude.Close)
	}
	if tokenHandler != nil {
		env.tokens = httptest.NewServer(tokenHandler)
		t.Cleanup(env.tokens.Close)
	}

	env.tracker = NewTracker()
	env.tracker.SetBaseURLs(
		serverURL(env.codex),
		serverURL(env.claude),
		serverURL(env.tokens),
	)
	return env
}

func serverURL(s *httptest.Server) string {
	if s == nil {
		// Point at a closed port: requests fail fast without network access.
		return "http://127.0.0.1:1/"
	}
	return s.URL
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

func TestRefreshFetchesCodexUsageAndSortsResults(t *testing.T) {
	requestHeaders := make(chan *http.Request, 1)
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		requestHeaders <- r
		if r.Header.Get("Authorization") != "Bearer codex-token" ||
			r.Header.Get("User-Agent") != "codex-cli" ||
			r.Header.Get("ChatGPT-Account-Id") != "acc-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":30}}}`))
	}, nil, nil)

	codexPath := writeAccountFile(t, env.home, "codex-a@b.com.json", map[string]any{
		"type": "codex", "email": "a@b.com", "access_token": "codex-token", "account_id": "acc-1",
	})

	changes := atomic.Int32{}
	env.tracker.SetOnChange(func() { changes.Add(1) })

	codex := auth.Account{ID: filepath.Base(codexPath), Email: "a@b.com", Type: auth.Codex, FilePath: codexPath}
	env.tracker.Refresh([]auth.Account{codex}, nil, nil, nil)

	if !env.tracker.IsRefreshing() {
		t.Fatal("expected refreshing state")
	}
	if accounts := env.tracker.Accounts(); len(accounts) != 1 || !accounts[0].Loading {
		t.Fatalf("placeholders = %+v", accounts)
	}

	waitFor(t, func() bool { return !env.tracker.IsRefreshing() })
	accounts := env.tracker.Accounts()
	if len(accounts) != 1 || accounts[0].Loading || accounts[0].Error != "" {
		t.Fatalf("accounts = %+v", accounts)
	}
	if accounts[0].Provider != "codex" || accounts[0].ProviderName != "Codex" || accounts[0].Email != "a@b.com" {
		t.Fatalf("account = %+v", accounts[0])
	}
	if len(accounts[0].Windows) != 2 || accounts[0].Windows[0].Title != "5-hour" {
		t.Fatalf("windows = %+v", accounts[0].Windows)
	}
	if changes.Load() < 2 {
		t.Fatalf("onChange called %d times, want >= 2", changes.Load())
	}
	select {
	case r := <-requestHeaders:
		_ = r
	default:
		t.Fatal("codex endpoint was not called")
	}
}

func TestRefreshClearsSnapshotWhenNoEligibleAccounts(t *testing.T) {
	env := newTestEnv(t, nil, nil, nil)

	disabled := auth.Account{ID: "x", Email: "a@b.com", Type: auth.Codex, Disabled: true}
	expiredAt := time.Now().Add(-time.Hour)
	expired := auth.Account{ID: "y", Email: "c@d.com", Type: auth.Codex, Expired: &expiredAt}

	env.tracker.Refresh([]auth.Account{disabled, expired}, nil, nil, nil)

	if env.tracker.IsRefreshing() {
		t.Fatal("unexpected refreshing state")
	}
	if accounts := env.tracker.Accounts(); len(accounts) != 0 {
		t.Fatalf("accounts = %v", accounts)
	}
}

func TestRefreshReportsMissingAccessToken(t *testing.T) {
	env := newTestEnv(t, nil, nil, nil)
	path := writeAccountFile(t, env.home, "codex-a@b.com.json", map[string]any{"type": "codex", "email": "a@b.com"})

	env.tracker.Refresh([]auth.Account{{ID: filepath.Base(path), Email: "a@b.com", Type: auth.Codex, FilePath: path}}, nil, nil, nil)
	waitFor(t, func() bool { return !env.tracker.IsRefreshing() })

	accounts := env.tracker.Accounts()
	if len(accounts) != 1 || accounts[0].Error != "Missing access token" {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestRefreshClaudeWithExpiredTokenRefreshesAndRetriesOn401(t *testing.T) {
	const originalRefresh = "rt+abc/123"
	var usageCalls atomic.Int32
	var tokenCalls atomic.Int32
	env := newTestEnv(t, nil,
		func(w http.ResponseWriter, r *http.Request) {
			usageCalls.Add(1)
			if r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if usageCalls.Load() == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"five_hour":{"utilization":25.0,"resets_at":"2030-01-01T00:00:00Z"}}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost ||
				r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			call := tokenCalls.Add(1)
			body := readAll(r)
			want := "client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&grant_type=refresh_token&refresh_token=" + url.QueryEscape(originalRefresh)
			if call == 1 && body != want {
				t.Errorf("first token request body = %q, want %q", body, want)
			}
			_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
		})

	claudePath := writeAccountFile(t, env.home, "claude-a@b.com.json", map[string]any{
		"type": "claude", "email": "a@b.com",
		"access_token": "old-access", "refresh_token": originalRefresh,
		"expired": time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
	})

	env.tracker.Refresh(nil, []auth.Account{{
		ID: filepath.Base(claudePath), Email: "a@b.com", Type: auth.Claude, FilePath: claudePath,
	}}, nil, nil)
	waitFor(t, func() bool { return !env.tracker.IsRefreshing() })

	accounts := env.tracker.Accounts()
	if len(accounts) != 1 || accounts[0].Error != "" {
		t.Fatalf("accounts = %+v", accounts)
	}
	if len(accounts[0].Windows) != 1 || accounts[0].Windows[0].Title != "5-hour" {
		t.Fatalf("windows = %+v", accounts[0].Windows)
	}

	// The auth file must be rewritten with the new tokens, pretty-printed.
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	var refreshed map[string]any
	if err := json.Unmarshal(data, &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed["access_token"] != "new-access" || refreshed["refresh_token"] != "new-refresh" {
		t.Fatalf("refreshed = %v", refreshed)
	}
	if refreshed["expired"] == "" || refreshed["last_refresh"] == "" {
		t.Fatalf("refreshed = %v", refreshed)
	}
	if !strings.HasPrefix(string(data), "{\n  \"") {
		t.Fatal("auth file not pretty-printed")
	}
	info, err := os.Stat(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o", info.Mode().Perm())
	}
}

func TestRefreshClaudeReportsUsageAPIStatus(t *testing.T) {
	env := newTestEnv(t, nil,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}, nil)

	path := writeAccountFile(t, env.home, "claude-a@b.com.json", map[string]any{
		"type": "claude", "email": "a@b.com", "access_token": "tok",
		"expired": time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
	})

	env.tracker.Refresh(nil, []auth.Account{{
		ID: filepath.Base(path), Email: "a@b.com", Type: auth.Claude, FilePath: path,
	}}, nil, nil)
	waitFor(t, func() bool { return !env.tracker.IsRefreshing() })

	accounts := env.tracker.Accounts()
	if len(accounts) != 1 || accounts[0].Error != "Claude usage API returned 500" {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func readAll(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	return string(data)
}

func TestParseGrokWindowsFromCreditPercent(t *testing.T) {
	payload := `{"config":{"creditUsagePercent":62.5,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2030-01-08T00:00:00Z"}}}`
	windows := ParseGrokWindows([]byte(payload))
	if len(windows) != 1 {
		t.Fatalf("windows = %+v", windows)
	}
	w := windows[0]
	if w.Title != "Weekly" {
		t.Fatalf("title = %q", w.Title)
	}
	if w.UsedPercent == nil || *w.UsedPercent != 62.5 {
		t.Fatalf("used = %v", w.UsedPercent)
	}
	if w.RemainingPercent == nil || *w.RemainingPercent != 37.5 || !w.HasRemaining {
		t.Fatalf("remaining = %+v", w)
	}
	if w.ResetText == "" {
		t.Fatal("empty reset text")
	}
}

func TestParseGrokWindowsFromOnDemandCap(t *testing.T) {
	payload := `{"config":{"onDemandCap":{"val":200},"onDemandUsed":{"val":50},"currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","end":"2030-02-01T00:00:00Z"}}}`
	windows := ParseGrokWindows([]byte(payload))
	if len(windows) != 1 || windows[0].Title != "Monthly" {
		t.Fatalf("windows = %+v", windows)
	}
	if windows[0].UsedPercent == nil || *windows[0].UsedPercent != 25 {
		t.Fatalf("used = %v", windows[0].UsedPercent)
	}
}

func TestParseGrokWindowsRejectsMissingConfig(t *testing.T) {
	for _, payload := range []string{`{}`, `{"config":{}}`, `{invalid`, `{"config":{"onDemandCap":{"val":0},"onDemandUsed":{"val":1}}}`} {
		if windows := ParseGrokWindows([]byte(payload)); len(windows) != 0 {
			t.Fatalf("payload %q gave %v", payload, windows)
		}
	}
}

func TestMetaWindowTitle(t *testing.T) {
	if got := MetaWindowTitle(300); got != "5-hour" {
		t.Fatalf("300 -> %q", got)
	}
	if got := MetaWindowTitle(90); got != "90-min" {
		t.Fatalf("90 -> %q", got)
	}
	if got := MetaWindowTitle(0); got != "Window" {
		t.Fatalf("0 -> %q", got)
	}
}

func TestRefreshMetaReadsLocalStore(t *testing.T) {
	env := newTestEnv(t, nil, nil, nil)
	observed := time.Now().Add(-time.Hour).Round(time.Second)
	resets := time.Now().Add(2 * time.Hour).Round(time.Second)
	weekly := time.Now().Add(24 * time.Hour).Round(time.Second)
	env.tracker.SetMetaSnapshots(func(accountID string) *meta.UsageSnapshot {
		if accountID != "meta-1" {
			return nil
		}
		return &meta.UsageSnapshot{
			WindowUsedPercent: 40, WindowResetsAt: resets, WindowDurationMins: 300,
			WeeklyUsedPercent: 12, WeeklyResetsAt: weekly, ObservedAt: observed,
		}
	})
	env.tracker.Refresh(nil, nil, nil, []auth.Account{
		{ID: "meta-1", Email: "m@x.com", Type: auth.Meta},
		{ID: "meta-2", Email: "n@x.com", Type: auth.Meta},
	})
	waitFor(t, func() bool { return !env.tracker.IsRefreshing() })

	accounts := env.tracker.Accounts()
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v", accounts)
	}
	if accounts[0].Error != "" || len(accounts[0].Windows) != 2 {
		t.Fatalf("meta-1 = %+v", accounts[0])
	}
	if accounts[0].Windows[0].Title != "5-hour" || accounts[0].Windows[1].Title != "Weekly" {
		t.Fatalf("windows = %+v", accounts[0].Windows)
	}
	if !strings.Contains(accounts[0].Windows[0].ResetText, "as of") {
		t.Fatalf("reset = %q", accounts[0].Windows[0].ResetText)
	}
	if accounts[1].Error == "" {
		t.Fatalf("meta-2 should report no observations: %+v", accounts[1])
	}
}

func TestRefreshGrokUsesBearerAndParsesWindow(t *testing.T) {
	var gotAuth, gotTokenAuth string
	grokServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTokenAuth = r.Header.Get("X-XAI-Token-Auth")
		_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":20,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2030-01-08T00:00:00Z"}}}`))
	}))
	t.Cleanup(grokServer.Close)

	env := newTestEnv(t, nil, nil, nil)
	env.tracker.SetBaseURLsWithGrok(serverURL(env.codex), serverURL(env.claude), serverURL(env.tokens), grokServer.URL)
	env.tracker.SetGrokToken(func() (string, *grok.AuthError) { return "grok-bearer", nil })
	// activeGrokAccounts only keeps the file LoadActiveCredentials serves;
	// HOME already points at env.home, so drop a matching credential file
	// into the auth dir scan location.
	authDir := filepath.Join(env.home, ".cli-proxy-api")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	grokPath := writeAccountFile(t, authDir, "grok-cli.json", map[string]any{
		"type": "grok-cli", "access": "a", "refresh": "r", "expires": float64(time.Now().Add(time.Hour).UnixMilli()),
	})

	env.tracker.Refresh(nil, nil, []auth.Account{
		{ID: "grok-cli.json", Email: "g@x.com", Type: auth.Grok, FilePath: grokPath},
	}, nil)
	waitFor(t, func() bool { return !env.tracker.IsRefreshing() })

	accounts := env.tracker.Accounts()
	if len(accounts) != 1 || accounts[0].Error != "" {
		t.Fatalf("accounts = %+v", accounts)
	}
	if len(accounts[0].Windows) != 1 || accounts[0].Windows[0].Title != "Weekly" {
		t.Fatalf("windows = %+v", accounts[0].Windows)
	}
	if gotAuth != "Bearer grok-bearer" || gotTokenAuth != "xai-grok-cli" {
		t.Fatalf("auth headers = %q %q", gotAuth, gotTokenAuth)
	}
}

func TestUpdateMetaAccountsOnlyTouchesMeta(t *testing.T) {
	env := newTestEnv(t, nil, nil, nil)
	env.tracker.SetMetaSnapshots(func(accountID string) *meta.UsageSnapshot { return nil })
	env.tracker.Refresh(nil, nil, nil, []auth.Account{{ID: "m1", Email: "m@x.com", Type: auth.Meta}})
	waitFor(t, func() bool { return !env.tracker.IsRefreshing() })

	observed := time.Now().Add(-time.Minute).Round(time.Second)
	env.tracker.SetMetaSnapshots(func(accountID string) *meta.UsageSnapshot {
		return &meta.UsageSnapshot{
			WindowUsedPercent: 1, WindowResetsAt: time.Now().Add(time.Hour), WindowDurationMins: 300,
			WeeklyUsedPercent: 2, WeeklyResetsAt: time.Now().Add(24 * time.Hour), ObservedAt: observed,
		}
	})
	env.tracker.UpdateMetaAccounts([]auth.Account{{ID: "m1", Email: "m@x.com", Type: auth.Meta}})
	accounts := env.tracker.Accounts()
	if len(accounts) != 1 || accounts[0].Error != "" || len(accounts[0].Windows) != 2 {
		t.Fatalf("accounts = %+v", accounts)
	}
}
