package usage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
)

func TestParseCodexResetCreditsFiltersAndSorts(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	data := []byte(`{"credits":[
		{"id":"late","status":"available","reset_type":"codex_rate_limits","expires_at":"2026-12-01T00:00:00Z"},
		{"id":"soon","status":"available","reset_type":"codex_rate_limits","expires_at":"2026-11-20T00:00:00Z"},
		{"id":"spent","status":"redeemed","reset_type":"codex_rate_limits","expires_at":"2026-12-01T00:00:00Z"},
		{"id":"other","status":"available","reset_type":"something_else","expires_at":"2026-12-01T00:00:00Z"},
		{"id":"stale","status":"available","reset_type":"codex_rate_limits","expires_at":"2026-01-01T00:00:00Z"}
	]}`)
	got := codexResetCredits(data, now)
	if got == nil || got.AvailableCount != 2 || got.NextCreditID != "soon" {
		t.Fatalf("credits = %+v, want 2 available with next=soon", got)
	}
}

func TestParseClaudeResetCreditsCountsLiveGrants(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	data := []byte(`{"cedar_ember":{"eligible":true,"next_grant_id":"g1","grants":[
		{"id":"g1","resets_left":2,"ends_at":"2026-12-01T00:00:00Z","paused":false,"usable_now":true},
		{"id":"g2","resets_left":1,"ends_at":"2026-12-15T00:00:00Z","paused":true,"usable_now":true},
		{"id":"g3","resets_left":1,"ends_at":"2025-01-01T00:00:00Z","usable_now":true}
	]}}`)
	got := parseClaudeResetCredits(data, now)
	if got == nil || got.AvailableCount != 2 || got.NextCreditID != "g1" {
		t.Fatalf("credits = %+v, want count 2 next g1", got)
	}
	if parseClaudeResetCredits([]byte(`{"cedar_ember":{"eligible":false}}`), now) != nil {
		t.Fatal("ineligible block must yield no credits")
	}
}

func writeAuthFile(t *testing.T, fields map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRedeemCodexResetConsumesSoonestCredit(t *testing.T) {
	var consumed map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/wham/rate-limit-reset-credits", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"credits":[{"id":"c1","status":"available","reset_type":"codex_rate_limits","expires_at":"2999-01-01T00:00:00Z"}]}`))
	})
	mux.HandleFunc("/wham/rate-limit-reset-credits/consume", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&consumed)
		_, _ = w.Write([]byte(`{"code":"reset"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tracker := NewTracker()
	tracker.SetBaseURLs(srv.URL+"/wham/usage", "", "")
	path := writeAuthFile(t, map[string]any{"access_token": "tok", "account_id": "acct-1"})
	account := auth.Account{ID: "codex.json", Type: auth.Codex, FilePath: path}

	message, ok := tracker.RedeemReset(account)
	if !ok {
		t.Fatalf("redeem failed: %s", message)
	}
	if consumed["credit_id"] != "c1" || consumed["redeem_request_id"] != redeemRequestID("acct-1:c1") {
		t.Fatalf("consume body = %+v, want credit c1 with deterministic request id", consumed)
	}
}

func TestRedeemClaudeResetPostsGrantClaim(t *testing.T) {
	var claim map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/oauth/usage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":10},"cedar_ember":{"eligible":true,"next_grant_id":"g1","grants":[{"id":"g1","resets_left":1,"ends_at":"2999-01-01T00:00:00Z","usable_now":true}]}}`))
	})
	mux.HandleFunc("/api/organizations/org-1/reset_rate_limits", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&claim)
		_, _ = w.Write([]byte(`{"result":"reset"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tracker := NewTracker()
	tracker.SetBaseURLs("", srv.URL+"/api/oauth/usage", "")
	path := writeAuthFile(t, map[string]any{"access_token": "tok", "organization_uuid": "org-1"})
	account := auth.Account{ID: "claude.json", Type: auth.Claude, FilePath: path}

	message, ok := tracker.RedeemReset(account)
	if !ok {
		t.Fatalf("redeem failed: %s", message)
	}
	if claim["program"] != "cedar_ember" || claim["grant_id"] != "g1" || claim["request_id"] == "" {
		t.Fatalf("claim body = %+v", claim)
	}
}

func TestRedeemClaudeResetUsesDistinctKeyPerRedemption(t *testing.T) {
	left := 2
	seen := map[string]bool{}
	var requestIDs []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/oauth/usage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":100},"cedar_ember":{"eligible":true,"next_grant_id":"g1","grants":[{"id":"g1","resets_left":` + strconv.Itoa(left) + `,"ends_at":"2999-01-01T00:00:00Z","usable_now":true}]}}`))
	})
	mux.HandleFunc("/api/organizations/org-1/reset_rate_limits", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id, _ := body["request_id"].(string)
		requestIDs = append(requestIDs, id)
		if seen[id] {
			_, _ = w.Write([]byte(`{"result":"already_used"}`))
			return
		}
		seen[id] = true
		left--
		_, _ = w.Write([]byte(`{"result":"reset"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	t.Setenv("HOME", t.TempDir())
	tracker := NewTracker()
	tracker.SetBaseURLs("", srv.URL+"/api/oauth/usage", "")
	path := writeAuthFile(t, map[string]any{"access_token": "tok", "organization_uuid": "org-1"})
	account := auth.Account{ID: "claude.json", Type: auth.Claude, FilePath: path}

	for i := 0; i < 2; i++ {
		if message, ok := tracker.RedeemReset(account); !ok {
			t.Fatalf("redeem %d failed: %s", i, message)
		}
	}
	if left != 0 {
		t.Fatalf("resets_left = %d, want both resets actually spent (request ids %v)", left, requestIDs)
	}
	if len(requestIDs) != 2 || requestIDs[0] == requestIDs[1] {
		t.Fatalf("request ids = %v, want two distinct keys", requestIDs)
	}
}

func TestRedeemResetInvalidatesCachedUsage(t *testing.T) {
	usageHits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/wham/usage", func(w http.ResponseWriter, r *http.Request) {
		usageHits++
		used := 90
		if usageHits > 1 {
			used = 10
		}
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":` + strconv.Itoa(used) + `,"limit_window_seconds":18000},"secondary_window":{"used_percent":50,"limit_window_seconds":604800}}}`))
	})
	mux.HandleFunc("/wham/rate-limit-reset-credits", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"credits":[{"id":"c1","status":"available","reset_type":"codex_rate_limits","expires_at":"2999-01-01T00:00:00Z"}]}`))
	})
	mux.HandleFunc("/wham/rate-limit-reset-credits/consume", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"reset"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	t.Setenv("HOME", t.TempDir())
	tracker := NewTracker()
	tracker.SetBaseURLs(srv.URL+"/wham/usage", "", "")
	path := writeAuthFile(t, map[string]any{"access_token": "tok", "account_id": "acct-1"})
	account := auth.Account{ID: "codex.json", Type: auth.Codex, FilePath: path}

	first := tracker.fetch([]auth.Account{account})
	if len(first) != 1 || first[0].Error != "" || usageHits != 1 {
		t.Fatalf("first fetch = %+v, hits = %d", first, usageHits)
	}
	if message, ok := tracker.RedeemReset(account); !ok {
		t.Fatalf("redeem failed: %s", message)
	}
	second := tracker.fetch([]auth.Account{account})
	if usageHits != 2 {
		t.Fatalf("usage hits = %d, want a refetch after redemption despite the polling budget", usageHits)
	}
	if len(second) != 1 || len(second[0].Windows) == 0 || second[0].Windows[0].UsedPercent == nil || *second[0].Windows[0].UsedPercent != 10 {
		t.Fatalf("second fetch = %+v, want post-reset sample", second)
	}
}
