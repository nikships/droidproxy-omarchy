package meta

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageFreshness(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if isUsageFresh(nil, now) {
		t.Error("nil snapshot must be stale")
	}
	if !isUsageFresh(&UsageSnapshot{ObservedAt: now.Add(-59 * time.Second)}, now) {
		t.Error("59s-old snapshot must be fresh")
	}
	if isUsageFresh(&UsageSnapshot{ObservedAt: now.Add(-60 * time.Second)}, now) {
		t.Error("60s-old snapshot must be stale")
	}
}

const probeUsageLine = `data: {"subscription":{"tier":"pro","weekly":{"resets_at":1800086400,"used_percent":11.5},"window":{"resets_at":1800018000,"used_percent":42.25,"window_duration_mins":300}},"type":"response.subscription_usage"}`

type probeEnv struct {
	prober   *UsageProber
	requests atomic.Int32
	status   int
	header   http.Header
}

func newProbeEnv(t *testing.T, accounts []Account) *probeEnv {
	t.Helper()
	dir := t.TempDir()
	env := &probeEnv{status: http.StatusOK}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" ||
			r.Header.Get("Authorization") != "Bearer key-a" ||
			r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Accept") != "text/event-stream" ||
			r.Header.Get("User-Agent") != "muse-build/1.3.0" ||
			string(body) != usageProbeBody {
			t.Errorf("probe request = %s %s %v %s", r.Method, r.URL.Path, r.Header, body)
		}
		for k, vs := range env.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(env.status)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"+probeUsageLine+"\n\n")
	}))
	t.Cleanup(server.Close)
	store := NewCredentialStore(dir)
	if err := store.write(accounts); err != nil {
		t.Fatal(err)
	}
	env.prober = &UsageProber{Credentials: store, Usage: NewUsageStore(dir), BaseURL: server.URL, Client: server.Client()}
	return env
}

func probeAccount(id, key string, expiresAt time.Time, disabled bool) Account {
	return Account{ID: id, Credentials: Credentials{APIKey: key, APIKeyExpiresAt: secondsFromTime(expiresAt)}, Disabled: disabled}
}

func TestUsageProbeRecordsStreamedUsage(t *testing.T) {
	env := newProbeEnv(t, []Account{probeAccount("a", "key-a", time.Now().Add(time.Hour), false)})
	if !env.prober.Refresh("a", false) {
		t.Fatal("probe should record a snapshot")
	}
	snapshot := env.prober.Usage.Snapshot("a")
	if snapshot == nil || snapshot.WindowUsedPercent != 42.25 || snapshot.WeeklyUsedPercent != 11.5 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	// The new snapshot is fresh, so a second refresh sends nothing unless forced.
	if env.prober.Refresh("a", false) || env.requests.Load() != 1 {
		t.Fatalf("fresh snapshot was probed again (%d requests)", env.requests.Load())
	}
	if !env.prober.Refresh("a", true) || env.requests.Load() != 2 {
		t.Fatalf("forced probe did not run (%d requests)", env.requests.Load())
	}
}

func TestUsageProbeSkipsUnusableAccounts(t *testing.T) {
	env := newProbeEnv(t, []Account{
		probeAccount("off", "key-a", time.Now().Add(time.Hour), true),
		probeAccount("old", "key-a", time.Now().Add(-time.Minute), false),
		probeAccount("nokey", "", time.Now().Add(time.Hour), false),
	})
	for _, id := range []string{"off", "old", "nokey", "missing"} {
		if env.prober.Refresh(id, true) {
			t.Errorf("%s should not be probed", id)
		}
	}
	if env.requests.Load() != 0 {
		t.Fatalf("requests = %d", env.requests.Load())
	}
}

func TestUsageProbeFailureKeepsStoredSnapshot(t *testing.T) {
	env := newProbeEnv(t, []Account{probeAccount("a", "key-a", time.Now().Add(time.Hour), false)})
	stored := UsageSnapshot{WindowUsedPercent: 5, ObservedAt: time.Now().Add(-time.Hour).Round(time.Second)}
	env.prober.Usage.Record("a", stored)
	env.status = http.StatusTooManyRequests
	if env.prober.Refresh("a", false) {
		t.Fatal("a failed probe must not report success")
	}
	if got := env.prober.Usage.Snapshot("a"); got == nil || got.WindowUsedPercent != 5 {
		t.Fatalf("stored snapshot = %+v", got)
	}
}

func TestUsageProbeReportsRateLimit(t *testing.T) {
	env := newProbeEnv(t, []Account{probeAccount("a", "key-a", time.Now().Add(time.Hour), false)})
	env.status = http.StatusTooManyRequests
	env.header = http.Header{"Retry-After": {"3600"}}
	outcome := env.prober.Probe("a", false)
	if outcome.Recorded || !outcome.RateLimited || outcome.RetryAfter != "3600" {
		t.Fatalf("outcome = %+v, want rate-limited with Retry-After 3600", outcome)
	}
}
