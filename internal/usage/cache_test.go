package usage

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
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
