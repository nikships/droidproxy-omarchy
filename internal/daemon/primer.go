package daemon

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/meta"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
	"github.com/nikships/droidproxy-omarchy/internal/usage"
)

// windowPrimerProviders run their subscription quota in rolling 5-hour
// windows that start at the first request. Sending one tiny request before
// the workday starts the clock early, so the window resets mid-session
// instead of 5 hours after the first real prompt. Higher plans on these
// providers (Claude Max/Ultra class, GPT Pro) have no 5-hour window at
// all, so each account's own usage payload decides (primerAppliesFor).
var windowPrimerProviders = []auth.ServiceType{auth.Claude, auth.Codex, auth.Meta}

// primerAppliesFor reports whether Auto-start daily can do anything for a
// provider's accounts: false only when every account's payload positively
// states it has no short window. Unknown usage keeps the primer available.
func primerAppliesFor(accounts []usage.AccountUsage) bool {
	for _, a := range accounts {
		if has, known := a.HasShortWindow(); has || !known {
			return true
		}
	}
	return len(accounts) == 0
}

// primerApplies is primerAppliesFor over the provider's current usage rows.
func (d *Daemon) primerApplies(st auth.ServiceType) bool {
	var accounts []usage.AccountUsage
	for _, a := range d.usage.Accounts() {
		if a.Provider == string(st) {
			accounts = append(accounts, a)
		}
	}
	return primerAppliesFor(accounts)
}

const (
	// Missing the exact minute (daemon starting late, laptop asleep) still
	// fires, but not hours later when starting a window would no longer help.
	windowPrimerCatchUp = 2 * time.Hour
	// A failed attempt is retried after this long, until the catch-up ends.
	windowPrimerRetryDelay = 5 * time.Minute
	windowPrimerTick       = 30 * time.Second
)

// shouldPrime fires once per day, at or after the scheduled wall-clock time
// (minutes after local midnight), within the catch-up window, and only if it
// has not fired since then. Yesterday's occurrence counts too, so a late
// evening schedule still catches up just after midnight.
func shouldPrime(now time.Time, scheduledMinutes int, lastFired *time.Time) bool {
	hour, minute := scheduledMinutes/60, scheduledMinutes%60
	for _, dayOffset := range []int{0, -1} {
		day := now.AddDate(0, 0, dayOffset)
		scheduled := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, now.Location())
		if now.Before(scheduled) || now.Sub(scheduled) >= windowPrimerCatchUp {
			continue
		}
		if lastFired == nil || lastFired.Before(scheduled) {
			return true
		}
	}
	return false
}

// primerProxyRequest is the request sent through ThinkingProxy for Claude and
// Codex. Meta has none: it goes straight to api.meta.ai via the usage probe.
func primerProxyRequest(st auth.ServiceType) (path, body string, ok bool) {
	switch st {
	case auth.Claude:
		return "/v1/messages", `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, true
	case auth.Codex:
		return "/v1/chat/completions", `{"model":"gpt-6-luna","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, true
	}
	return "", "", false
}

// windowPrimer is the opt-in "Auto-start daily" scheduler (macOS
// WindowPrimer). Nothing runs unless a provider's toggle is on.
type windowPrimer struct {
	// send primes one provider and returns how many requests succeeded.
	send func(auth.ServiceType) int
	// applies reports whether the provider's accounts have a short window
	// worth starting (see primerAppliesFor).
	applies func(auth.ServiceType) bool

	mu          sync.Mutex
	lastAttempt map[auth.ServiceType]time.Time
	inFlight    map[auth.ServiceType]bool
}

func newWindowPrimer(send func(auth.ServiceType) int, applies func(auth.ServiceType) bool) *windowPrimer {
	return &windowPrimer{
		send:        send,
		applies:     applies,
		lastAttempt: map[auth.ServiceType]time.Time{},
		inFlight:    map[auth.ServiceType]bool{},
	}
}

func (p *windowPrimer) run(ctx context.Context) {
	ticker := time.NewTicker(windowPrimerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			p.tick(now)
		}
	}
}

func (p *windowPrimer) tick(now time.Time) {
	for _, st := range windowPrimerProviders {
		provider := string(st)
		if !prefs.WindowPrimerEnabled(provider) || !p.applies(st) ||
			!shouldPrime(now, prefs.WindowPrimerMinutes(provider), prefs.WindowPrimerLastFired(provider)) {
			continue
		}
		p.mu.Lock()
		last, attempted := p.lastAttempt[st]
		if p.inFlight[st] || (attempted && now.Sub(last) < windowPrimerRetryDelay) {
			p.mu.Unlock()
			continue
		}
		p.lastAttempt[st] = now
		p.inFlight[st] = true
		p.mu.Unlock()
		go p.fire(st)
	}
}

func (p *windowPrimer) fire(st auth.ServiceType) {
	sent := p.send(st)
	// Record the fire before clearing inFlight so the next tick cannot send
	// a second round in between.
	if sent > 0 {
		if err := prefs.SetWindowPrimerLastFired(string(st), time.Now()); err != nil {
			logx.Logf("[WindowPrimer] %s: could not record the fire: %v", st, err)
		}
	}
	p.mu.Lock()
	delete(p.inFlight, st)
	p.mu.Unlock()
	logx.Logf("[WindowPrimer] %s: %d request(s) started a window", st, sent)
}

var primerClient = &http.Client{Timeout: 60 * time.Second}

// sendWindowPrimers sends one tiny request per enabled account. Meta uses
// each account's own key (a forced usage probe). Claude and Codex go through
// the local proxy exactly like real traffic, so they reuse its client
// identity handling. The proxy picks the account, so with several accounts
// the request is repeated once per enabled account: round-robin spreads
// them out, and in sequential mode only the account serving traffic matters.
func (d *Daemon) sendWindowPrimers(st auth.ServiceType) int {
	if !prefs.IsProviderEnabled(string(st)) {
		return 0
	}
	sent := 0
	if st == auth.Meta {
		prober := meta.NewUsageProber()
		for _, a := range d.metaStore.Accounts() {
			if prober.Refresh(a.ID, true) {
				sent++
			}
		}
		return sent
	}
	for _, a := range d.authMgr.Accounts(st) {
		if !a.Disabled && !a.IsExpired() && sendPrimerThroughProxy(st) {
			sent++
		}
	}
	return sent
}

func sendPrimerThroughProxy(st auth.ServiceType) bool {
	path, body, ok := primerProxyRequest(st)
	if !ok {
		return false
	}
	url := usage.ProxyBaseURL() + path
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		logx.Logf("[WindowPrimer] %s request failed: %v", st, err)
		return false
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-version", "2023-06-01")
	response, err := primerClient.Do(request)
	if err != nil {
		logx.Logf("[WindowPrimer] %s request failed: %v", st, err)
		return false
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		logx.Logf("[WindowPrimer] %s request returned HTTP %d", st, response.StatusCode)
		return false
	}
	return true
}
