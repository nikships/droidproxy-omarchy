package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
	"github.com/nikships/droidproxy-omarchy/internal/usage"
)

func primerDate(hour, minute, day int) time.Time {
	return time.Date(2026, time.October, day, hour, minute, 0, 0, time.UTC)
}

func TestShouldPrimeSchedule(t *testing.T) {
	const seven = 7 * 60
	fired := func(hour, minute, day int) *time.Time {
		v := primerDate(hour, minute, day)
		return &v
	}
	cases := []struct {
		name      string
		now       time.Time
		lastFired *time.Time
		want      bool
	}{
		{"before the scheduled time", primerDate(6, 59, 2), nil, false},
		{"at the scheduled time", primerDate(7, 0, 2), nil, true},
		{"late in the catch-up window", primerDate(8, 59, 2), nil, true},
		{"after the catch-up window", primerDate(9, 0, 2), nil, false},
		{"already fired today", primerDate(7, 5, 2), fired(7, 0, 2), false},
		{"fired yesterday", primerDate(7, 0, 3), fired(7, 1, 2), true},
	}
	for _, c := range cases {
		if got := shouldPrime(c.now, seven, c.lastFired); got != c.want {
			t.Errorf("%s: shouldPrime = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestShouldPrimeUsesWallClockAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	// 2026-03-08 springs forward: elapsed-minute math would put a 07:00
	// schedule at 08:00 local, so 07:30 must already be inside the window.
	now := time.Date(2026, time.March, 8, 7, 30, 0, 0, loc)
	if !shouldPrime(now, 7*60, nil) {
		t.Fatal("07:30 on spring-forward day should prime a 07:00 schedule")
	}
	before := time.Date(2026, time.March, 8, 6, 30, 0, 0, loc)
	if shouldPrime(before, 7*60, nil) {
		t.Fatal("06:30 on spring-forward day must not prime a 07:00 schedule")
	}
}

func TestShouldPrimeCatchesUpAcrossMidnight(t *testing.T) {
	now := primerDate(0, 15, 3)
	if !shouldPrime(now, 23*60+30, nil) {
		t.Fatal("00:15 should catch up a 23:30 schedule from the day before")
	}
	fired := primerDate(23, 35, 2)
	if shouldPrime(now, 23*60+30, &fired) {
		t.Fatal("23:30 already fired last night; must not fire again")
	}
	late := primerDate(1, 45, 3)
	if shouldPrime(late, 23*60+30, nil) {
		t.Fatal("01:45 is past the 2-hour catch-up for 23:30")
	}
}

func TestPrimerRequestsExistOnlyForProxyRoutedProviders(t *testing.T) {
	path, body, ok := primerProxyRequest(auth.Claude)
	if !ok || path != "/v1/messages" || !strings.Contains(body, "claude-haiku-4-5-20251001") {
		t.Fatalf("claude = %q %q %v", path, body, ok)
	}
	if path, _, ok := primerProxyRequest(auth.Codex); !ok || path != "/v1/chat/completions" {
		t.Fatalf("codex = %q %v", path, ok)
	}
	if _, _, ok := primerProxyRequest(auth.Meta); ok {
		t.Fatal("meta must not go through the proxy")
	}
}

func TestWindowPrimerRetriesUntilARequestLands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := prefs.Shared().Set(prefs.WindowPrimerEnabledKey("codex"), true); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Shared().Set(prefs.WindowPrimerMinutesKey("codex"), 7*60); err != nil {
		t.Fatal(err)
	}

	results := []int{0, 1}
	calls := make(chan auth.ServiceType, 4)
	p := newWindowPrimer(func(st auth.ServiceType) int {
		sent := results[0]
		results = results[1:]
		calls <- st
		return sent
	}, func(auth.ServiceType) bool { return true })
	// A past day, so the real clock fire() stamps is after its schedule.
	at := func(hour, minute int) time.Time {
		return time.Date(2026, time.October, 2, hour, minute, 0, 0, time.Local)
	}
	settle := func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			p.mu.Lock()
			busy := len(p.inFlight) > 0
			p.mu.Unlock()
			if !busy {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("primer request never finished")
	}

	p.tick(at(7, 0))
	if st := <-calls; st != auth.Codex {
		t.Fatalf("primed %s, want codex", st)
	}
	settle()
	if prefs.WindowPrimerLastFired("codex") != nil {
		t.Fatal("a round with no successful request must not count as fired")
	}

	p.tick(at(7, 3))
	p.tick(at(7, 5))
	<-calls
	settle()
	if prefs.WindowPrimerLastFired("codex") == nil {
		t.Fatal("a successful round must be recorded")
	}

	p.tick(at(7, 20))
	select {
	case st := <-calls:
		t.Fatalf("primed %s twice in one day", st)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPrimerAppliesFor(t *testing.T) {
	weeklyOnly := usage.AccountUsage{Windows: []usage.Window{{Title: "Weekly", LengthSeconds: 604800}}}
	withShort := usage.AccountUsage{Windows: []usage.Window{{Title: "5-hour", LengthSeconds: 18000}, {Title: "Weekly", LengthSeconds: 604800}}}
	unknown := usage.AccountUsage{Error: "boom"}

	cases := []struct {
		name     string
		accounts []usage.AccountUsage
		want     bool
	}{
		{"no accounts yet", nil, true},
		{"usage unknown", []usage.AccountUsage{unknown}, true},
		{"all weekly-only", []usage.AccountUsage{weeklyOnly, weeklyOnly}, false},
		{"one account has a 5-hour window", []usage.AccountUsage{weeklyOnly, withShort}, true},
		{"weekly-only plus unknown stays on", []usage.AccountUsage{weeklyOnly, unknown}, true},
	}
	for _, c := range cases {
		if got := primerAppliesFor(c.accounts); got != c.want {
			t.Errorf("%s: primerAppliesFor = %v, want %v", c.name, got, c.want)
		}
	}
}
