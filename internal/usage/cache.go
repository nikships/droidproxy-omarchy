package usage

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
)

// usageCache holds one polling budget per credential file, shared by every
// usage consumer. Concurrent readers of the same account wait for the one
// request in flight, and the budget is persisted so a restart cannot retry
// early.
type usageCache struct {
	path   string
	now    func() time.Time
	jitter func() time.Duration

	mu       sync.Mutex
	entries  map[string]*cacheEntry
	inFlight map[string]*cacheFlight
}

// cacheEntry is the persisted form. It stores no credentials; account
// identity is re-stamped from the live account on every read.
type cacheEntry struct {
	Error             string        `json:"error,omitempty"`
	Windows           []cacheWindow `json:"windows,omitempty"`
	UpdatedAt         *time.Time    `json:"updatedAt,omitempty"`
	NextAttemptAt     time.Time     `json:"nextAttemptAt"`
	RateLimitFailures int           `json:"rateLimitFailures"`
}

type cacheWindow struct {
	Title       string     `json:"title"`
	UsedPercent *float64   `json:"usedPercent,omitempty"`
	ResetText   string     `json:"resetText,omitempty"`
	ResetDate   *time.Time `json:"resetDate,omitempty"`
}

type cacheFlight struct {
	done   chan struct{}
	result AccountUsage
}

const (
	claudeUsageInterval = 300 * time.Second
	otherUsageInterval  = 15 * time.Second
	maxRateLimitBackoff = time.Hour
	maxRateLimitStreak  = 5
)

func newUsageCache(path string, now func() time.Time, jitter func() time.Duration) *usageCache {
	c := &usageCache{
		path:     path,
		now:      now,
		jitter:   jitter,
		entries:  map[string]*cacheEntry{},
		inFlight: map[string]*cacheFlight{},
	}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &c.entries)
		}
	}
	return c
}

// usage returns the cached sample while the account's budget is spent, and
// otherwise runs fetch once for all concurrent callers.
func (c *usageCache) usage(account auth.Account, fetch func() AccountUsage) AccountUsage {
	key := string(account.Type) + ":" + filepath.Clean(account.FilePath)
	c.mu.Lock()
	if flight, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		<-flight.done
		return flight.result
	}
	now := c.now()
	if entry, ok := c.entries[key]; ok && now.Before(entry.NextAttemptAt) {
		c.mu.Unlock()
		return entry.toUsage(account)
	}

	interval := otherUsageInterval
	if account.Type == auth.Claude {
		interval = claudeUsageInterval
	}
	// Persist the reservation before fetching so a restart cannot retry it.
	entry, ok := c.entries[key]
	if !ok {
		entry = &cacheEntry{Error: "Usage refresh pending"}
	}
	entry.NextAttemptAt = now.Add(interval)
	c.entries[key] = entry
	flight := &cacheFlight{done: make(chan struct{})}
	c.inFlight[key] = flight
	c.persistLocked()
	c.mu.Unlock()

	result := fetch()

	c.mu.Lock()
	flight.result = c.finishLocked(key, account, result, interval)
	delete(c.inFlight, key)
	c.persistLocked()
	c.mu.Unlock()
	close(flight.done)
	return flight.result
}

func (c *usageCache) finishLocked(key string, account auth.Account, result AccountUsage, interval time.Duration) AccountUsage {
	usage := result
	previous := c.entries[key]
	if result.Error != "" && previous != nil && len(previous.Windows) > 0 {
		kept := previous.toUsage(account)
		usage.Windows = kept.Windows
		usage.UpdatedAt = kept.UpdatedAt
	}

	failures := 0
	if previous != nil {
		failures = previous.RateLimitFailures
	}
	backoff := interval
	switch {
	case result.RateLimited:
		failures = min(maxRateLimitStreak, failures+1)
		backoff = min(maxRateLimitBackoff, 300*time.Second<<(failures-1))
	case result.Error == "":
		failures = 0
	}
	if account.Type == auth.Claude {
		backoff += c.jitter()
	}
	next := c.now().Add(backoff)
	if result.RetryAfter != nil && result.RetryAfter.After(next) {
		next = *result.RetryAfter
	}

	entry := &cacheEntry{
		Error:             usage.Error,
		UpdatedAt:         usage.UpdatedAt,
		NextAttemptAt:     next,
		RateLimitFailures: failures,
	}
	for _, w := range usage.Windows {
		entry.Windows = append(entry.Windows, cacheWindow{Title: w.Title, UsedPercent: w.UsedPercent, ResetText: w.ResetText, ResetDate: w.ResetDate})
	}
	c.entries[key] = entry
	return usage
}

func (e *cacheEntry) toUsage(account auth.Account) AccountUsage {
	usage := AccountUsage{
		ID:           account.ID,
		Provider:     string(account.Type),
		ProviderName: account.Type.DisplayName(),
		Email:        account.DisplayName(),
		Error:        e.Error,
		UpdatedAt:    e.UpdatedAt,
	}
	for _, w := range e.Windows {
		usage.Windows = append(usage.Windows, newWindow(w.Title, w.UsedPercent, w.ResetText, w.ResetDate))
	}
	return usage
}

func (c *usageCache) persistLocked() {
	if c.path == "" {
		return
	}
	if err := c.writeLocked(); err != nil {
		logx.Logf("[Usage] Could not save the quota polling cache: %v", err)
	}
}

func (c *usageCache) writeLocked() error {
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(c.entries)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".usage-cache-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}

// retryAfterDate parses a Retry-After header: delay seconds or an HTTP date.
func retryAfterDate(value string, now time.Time) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
			return nil
		}
		date := now.Add(time.Duration(seconds * float64(time.Second)))
		return &date
	}
	if date, err := http.ParseTime(value); err == nil {
		return &date
	}
	return nil
}
