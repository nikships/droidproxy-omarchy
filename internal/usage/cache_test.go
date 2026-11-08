package usage

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/grok"
	"github.com/nikships/droidproxy-omarchy/internal/meta"
)

type testClock struct {
	mu   sync.Mutex
	date time.Time
}

func newTestClock() *testClock { return &testClock{date: time.Unix(1_790_000_000, 0)} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.date
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.date = c.date.Add(d)
	c.mu.Unlock()
}

func noJitter() time.Duration { return 0 }

func cacheAccount(id string, provider auth.ServiceType) auth.Account {
	return auth.Account{ID: id, Email: "owner@example.com", Type: provider, FilePath: "/tmp/usage-cache-tests/" + id}
}

func cacheSample(account auth.Account, at time.Time) AccountUsage {
	used := 25.0
	reset := at.Add(time.Hour)
	usage := successAccount(account, []Window{newWindow("5-hour", &used, "", &reset)})
	usage.UpdatedAt = &at
	return usage
}

func rateLimitedSample(account auth.Account, retryAfter *time.Time) AccountUsage {
	usage := failedAccount(account, "Claude usage API returned 429")
	usage.RateLimited = true
	usage.RetryAfter = retryAfter
	return usage
}

func TestUsageCacheSharesOneClaudeBudgetAcrossConcurrentReaders(t *testing.T) {
	clock := newTestClock()
	cache := newUsageCache("", clock.now, noJitter)
	account := cacheAccount("claude.json", auth.Claude)
	var calls atomic.Int32
	release := make(chan struct{})
	fetch := func() AccountUsage {
		calls.Add(1)
		<-release
		return cacheSample(account, clock.now())
	}

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := cache.usage(account, fetch); len(got.Windows) != 1 {
				t.Errorf("windows = %+v", got.Windows)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	clock.advance(299 * time.Second)
	cache.usage(account, fetch)
	if calls.Load() != 1 {
		t.Fatalf("calls after 299s = %d, want 1", calls.Load())
	}
	clock.advance(time.Second)
	cache.usage(account, fetch)
	if calls.Load() != 2 {
		t.Fatalf("calls after 300s = %d, want 2", calls.Load())
	}
}

func TestUsageCacheBudgetsArePerAccountAndOtherProvidersUseFifteenSeconds(t *testing.T) {
	clock := newTestClock()
	cache := newUsageCache("", clock.now, noJitter)
	accounts := []auth.Account{
		cacheAccount("claude.json", auth.Claude),
		cacheAccount("another-seat.json", auth.Claude),
		cacheAccount("codex.json", auth.Codex),
	}
	var calls atomic.Int32
	readAll := func() {
		for _, account := range accounts {
			cache.usage(account, func() AccountUsage {
				calls.Add(1)
				return cacheSample(account, clock.now())
			})
		}
	}
	readAll()
	clock.advance(15 * time.Second)
	readAll()
	if calls.Load() != 4 {
		t.Fatalf("calls = %d, want 4", calls.Load())
	}
}

func TestUsageCacheRateLimitBacksOffKeepsLastSampleAndHonorsRetryAfter(t *testing.T) {
	clock := newTestClock()
	cache := newUsageCache("", clock.now, noJitter)
	account := cacheAccount("claude.json", auth.Claude)
	original := cacheSample(account, clock.now())
	mustNotFetch := func() AccountUsage {
		t.Fatal("fetched during cooldown")
		return original
	}
	cache.usage(account, func() AccountUsage { return original })
	clock.advance(300 * time.Second)

	for _, wait := range []time.Duration{300, 600, 1200, 2400, 3600, 3600} {
		wait *= time.Second
		result := cache.usage(account, func() AccountUsage { return rateLimitedSample(account, nil) })
		if len(result.Windows) != 1 || *result.Windows[0].UsedPercent != 25 {
			t.Fatalf("windows = %+v, want the last good sample", result.Windows)
		}
		if !result.UpdatedAt.Equal(*original.UpdatedAt) || result.Error != "Claude usage API returned 429" {
			t.Fatalf("result = %+v", result)
		}
		clock.advance(wait - time.Second)
		if cached := cache.usage(account, mustNotFetch); cached.Error != result.Error {
			t.Fatalf("cached error = %q", cached.Error)
		}
		clock.advance(time.Second)
	}

	until := clock.now().Add(7200 * time.Second)
	cache.usage(account, func() AccountUsage { return rateLimitedSample(account, &until) })
	clock.advance(7199 * time.Second)
	cache.usage(account, mustNotFetch)
	clock.advance(time.Second)
	if recovered := cache.usage(account, func() AccountUsage { return cacheSample(account, clock.now()) }); recovered.Error != "" {
		t.Fatalf("recovered error = %q", recovered.Error)
	}

	// Success reset the streak: the next 429 waits 300s again, and the old
	// Retry-After is already in the past.
	clock.advance(300 * time.Second)
	cache.usage(account, func() AccountUsage { return rateLimitedSample(account, &until) })
	clock.advance(300 * time.Second)
	if afterReset := cache.usage(account, func() AccountUsage { return original }); afterReset.Error != "" {
		t.Fatalf("after reset error = %q", afterReset.Error)
	}
}

func TestUsageCacheCooldownSurvivesRestartWithoutCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "droidproxy", "usage-cache.json")
	clock := newTestClock()
	account := cacheAccount("claude.json", auth.Claude)
	newUsageCache(path, clock.now, noJitter).usage(account, func() AccountUsage { return rateLimitedSample(account, nil) })

	restarted := newUsageCache(path, clock.now, noJitter)
	if result := restarted.usage(account, func() AccountUsage {
		t.Fatal("restart bypassed the persisted cooldown")
		return AccountUsage{}
	}); result.Error != "Claude usage API returned 429" || result.Email != "owner@example.com" {
		t.Fatalf("result = %+v", result)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	saved, _ := os.ReadFile(path)
	for _, secret := range []string{"access_token", "refresh_token", "owner@example.com"} {
		if strings.Contains(string(saved), secret) {
			t.Fatalf("cache file contains %q: %s", secret, saved)
		}
	}
}

func TestRetryAfterDateParsesSecondsAndHTTPDates(t *testing.T) {
	now := time.Unix(0, 0).UTC()
	for value, want := range map[string]time.Duration{
		"300":                           300 * time.Second,
		"0":                             0,
		"Thu, 01 Jan 1970 00:10:00 GMT": 600 * time.Second,
	} {
		got := retryAfterDate(value, now)
		if got == nil || !got.Equal(now.Add(want)) {
			t.Errorf("retryAfterDate(%q) = %v, want %v", value, got, now.Add(want))
		}
	}
	for _, value := range []string{"", "-1", "NaN", "Infinity", "later"} {
		if got := retryAfterDate(value, now); got != nil {
			t.Errorf("retryAfterDate(%q) = %v, want nil", value, got)
		}
	}
}

func TestRefreshHonorsClaudeRetryAfterOnManualRefresh(t *testing.T) {
	var calls atomic.Int32
	env := newTestEnv(t, nil, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(http.StatusTooManyRequests)
	}, nil)
	path := writeAccountFile(t, env.home, "claude-a@b.com.json", map[string]any{
		"type": "claude", "email": "a@b.com", "access_token": "claude-token",
	})
	account := auth.Account{ID: filepath.Base(path), Email: "a@b.com", Type: auth.Claude, FilePath: path}

	for range 2 {
		env.tracker.Refresh(nil, []auth.Account{account}, nil, nil)
		waitFor(t, func() bool { return !env.tracker.IsRefreshing() })
	}
	if calls.Load() != 1 {
		t.Fatalf("usage API calls = %d, want 1", calls.Load())
	}
	if accounts := env.tracker.Accounts(); len(accounts) != 1 || accounts[0].Error != "Claude usage API returned 429" {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestRefreshHonorsCodexRetryAfterOnManualRefresh(t *testing.T) {
	var calls atomic.Int32
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "7200")
		w.WriteHeader(http.StatusTooManyRequests)
	}, nil, nil)
	path := writeAccountFile(t, env.home, "codex-a@b.com.json", map[string]any{
		"type": "codex", "email": "a@b.com", "access_token": "codex-token",
	})
	account := auth.Account{ID: filepath.Base(path), Email: "a@b.com", Type: auth.Codex, FilePath: path}

	got := env.tracker.fetchCodexUsage(account)
	if calls.Load() != 1 {
		t.Fatalf("usage API calls = %d, want 1", calls.Load())
	}
	if !got.RateLimited || got.RetryAfter == nil {
		t.Fatalf("RateLimited=%v RetryAfter=%v, want rate-limited with Retry-After so the cache backs off", got.RateLimited, got.RetryAfter)
	}
}

func TestGrokUsage429IsRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	tracker := NewTracker()
	tracker.SetBaseURLsWithGrok("", "", "", srv.URL)
	tracker.SetGrokToken(func() (string, *grok.AuthError) { return "tok", nil })
	account := auth.Account{ID: "grok.json", Type: auth.Grok, FilePath: "/tmp/grok.json"}

	got := tracker.fetchGrokUsage(account)
	if !got.RateLimited || got.RetryAfter == nil {
		t.Fatalf("RateLimited=%v RetryAfter=%v, want rate-limited with Retry-After", got.RateLimited, got.RetryAfter)
	}
}

// A 429 from the Codex credits read must cool the whole sample down even
// when the usage read itself succeeded; otherwise the next poll 15s later
// re-hits the credits endpoint against the provider's Retry-After.
func TestCodexCredits429MarksSampleRateLimited(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wham/usage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000}}}`))
	})
	mux.HandleFunc("/wham/rate-limit-reset-credits", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	t.Setenv("HOME", t.TempDir())
	tracker := NewTracker()
	tracker.SetBaseURLs(srv.URL+"/wham/usage", "", "")
	path := writeAuthFile(t, map[string]any{"access_token": "tok", "account_id": "acct-1"})
	account := auth.Account{ID: "codex.json", Type: auth.Codex, FilePath: path}

	got := tracker.fetchCodexUsage(account)
	if got.Error != "" {
		t.Fatalf("usage read itself succeeded, got error %q", got.Error)
	}
	if !got.RateLimited || got.RetryAfter == nil {
		t.Fatalf("RateLimited=%v RetryAfter=%v, want credits 429 propagated", got.RateLimited, got.RetryAfter)
	}
	if until := time.Until(*got.RetryAfter); until < 59*time.Minute || until > 61*time.Minute {
		t.Fatalf("RetryAfter in %v, want ~1h", until)
	}
}

// A 429 from the Meta probe must mark the sample rate-limited so the
// cache honors the provider cooldown instead of the 60s interval.
func TestMetaProbe429MarksSampleRateLimited(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tracker := NewTracker()
	tracker.SetMetaSnapshots(func(string) *meta.UsageSnapshot {
		return &meta.UsageSnapshot{
			WindowUsedPercent: 10, WindowResetsAt: time.Now().Add(time.Hour), WindowDurationMins: 300,
			WeeklyUsedPercent: 20, WeeklyResetsAt: time.Now().Add(24 * time.Hour), ObservedAt: time.Now(),
		}
	})
	tracker.SetMetaProbe(func(string) meta.ProbeOutcome {
		return meta.ProbeOutcome{RateLimited: true, RetryAfter: "3600"}
	})
	account := auth.Account{ID: "m1", Email: "m@x.com", Type: auth.Meta}

	got := tracker.fetchMetaUsage(account)
	if got.Error != "" {
		t.Fatalf("stored snapshot should serve, got error %q", got.Error)
	}
	if !got.RateLimited || got.RetryAfter == nil {
		t.Fatalf("RateLimited=%v RetryAfter=%v, want probe 429 propagated", got.RateLimited, got.RetryAfter)
	}
	if until := time.Until(*got.RetryAfter); until < 59*time.Minute || until > 61*time.Minute {
		t.Fatalf("RetryAfter in %v, want ~1h", until)
	}
}

func TestMetaCacheBudgetIsPerAccountWithMinuteCooldown(t *testing.T) {
	clock := newTestClock()
	cache := newUsageCache("", clock.now, noJitter)
	shared := "/tmp/usage-cache-tests/meta-accounts.json"
	a := auth.Account{ID: "meta:a@example.com", Email: "a@example.com", Type: auth.Meta, FilePath: shared}
	b := auth.Account{ID: "meta:b@example.com", Email: "b@example.com", Type: auth.Meta, FilePath: shared}
	var calls atomic.Int32
	fetch := func() AccountUsage {
		calls.Add(1)
		return failedAccount(a, "probe failed")
	}

	cache.usage(a, fetch)
	clock.advance(30 * time.Second)
	cache.usage(a, fetch)
	if calls.Load() != 1 {
		t.Fatalf("calls = %d after 30s, want 1 (failed Meta probe cools down 60s)", calls.Load())
	}
	cache.usage(b, fetch)
	if calls.Load() != 2 {
		t.Fatalf("calls = %d for second Meta account, want 2 (separate budget per account)", calls.Load())
	}
	clock.advance(31 * time.Second)
	cache.usage(a, fetch)
	if calls.Load() != 3 {
		t.Fatalf("calls = %d after 61s, want 3 (cooldown elapsed)", calls.Load())
	}
}

// Cached readers convert the entry they hold after unlocking, so
// recordObserved must swap in a fresh entry (keeping the budget),
// never mutate the published one. Run with -race: concurrent cached
// reads plus observations must stay clean and coherent.
func TestRecordObservedSwapsEntryAndRacesClean(t *testing.T) {
	clock := newTestClock()
	cache := newUsageCache("", clock.now, noJitter)
	account := auth.Account{ID: "meta:a@example.com", Email: "a@example.com", Type: auth.Meta, FilePath: "/tmp/usage-cache-tests/meta-accounts.json"}
	used := 10.0
	sample := func() AccountUsage {
		now := clock.now()
		return AccountUsage{UpdatedAt: &now, Windows: []Window{{Title: "5-hour", UsedPercent: &used}}}
	}
	cache.usage(account, sample)

	now := clock.now()
	observed := AccountUsage{UpdatedAt: &now, Windows: []Window{{Title: "5-hour", UsedPercent: &used}, {Title: "Weekly", UsedPercent: &used}}}
	before := cache.entries[cache.keyFor(account)]
	cache.recordObserved(account, observed)
	after := cache.entries[cache.keyFor(account)]
	if before == after {
		t.Fatal("recordObserved mutated the published entry in place")
	}
	if !after.NextAttemptAt.Equal(before.NextAttemptAt) {
		t.Fatalf("budget changed: %v -> %v", before.NextAttemptAt, after.NextAttemptAt)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = cache.usage(account, sample)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				cache.recordObserved(account, observed)
			}
		}()
	}
	wg.Wait()
}

// Local credential failures cost no upstream quota, so they must not
// reserve the polling budget: after a token renewal the very next
// read refetches instead of serving the cached expiry error.
func TestLocalFailureDoesNotReserveBudget(t *testing.T) {
	clock := newTestClock()
	cache := newUsageCache("", clock.now, noJitter)
	account := auth.Account{ID: "codex:a@example.com", Email: "a@example.com", Type: auth.Codex, FilePath: "/tmp/usage-cache-tests/codex-a.json"}
	var calls atomic.Int32
	fetch := func() AccountUsage {
		if calls.Add(1) == 1 {
			return AccountUsage{Error: "Access token expired", LocalFailure: true}
		}
		now := clock.now()
		used := 5.0
		return AccountUsage{UpdatedAt: &now, Windows: []Window{{Title: "5-hour", UsedPercent: &used}}}
	}
	if got := cache.usage(account, fetch); got.Error == "" {
		t.Fatal("first read should surface the expiry error")
	}
	got := cache.usage(account, fetch)
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (immediate refetch after local failure)", calls.Load())
	}
	if got.Error != "" || len(got.Windows) != 1 {
		t.Fatalf("second read = %+v, want fresh sample", got)
	}
}
