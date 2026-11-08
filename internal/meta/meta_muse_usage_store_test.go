package meta

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseUsageEvent(t *testing.T) {
	observed := time.Unix(1_800_000_000, 0).UTC()
	line := `data: {"subscription":{"tier":"pro","weekly":{"resets_at":1800086400,"used_percent":11.5},"window":{"resets_at":1800018000,"used_percent":42.25,"window_duration_mins":300}},"type":"response.subscription_usage"}`
	snapshot := ParseUsageEvent(line, observed)
	if snapshot == nil {
		t.Fatal("expected a snapshot")
	}
	if snapshot.WindowUsedPercent != 42.25 || snapshot.WeeklyUsedPercent != 11.5 {
		t.Fatalf("percents = %.2f/%.2f", snapshot.WindowUsedPercent, snapshot.WeeklyUsedPercent)
	}
	if snapshot.WindowDurationMins != 300 {
		t.Fatalf("duration = %v", snapshot.WindowDurationMins)
	}
	if !snapshot.WindowResetsAt.Equal(time.Unix(1_800_018_000, 0)) {
		t.Fatalf("window reset = %v", snapshot.WindowResetsAt)
	}
	if !snapshot.WeeklyResetsAt.Equal(time.Unix(1_800_086_400, 0)) {
		t.Fatalf("weekly reset = %v", snapshot.WeeklyResetsAt)
	}
	if snapshot.Tier != "pro" || !snapshot.ObservedAt.Equal(observed) {
		t.Fatalf("tier/observed = %q %v", snapshot.Tier, snapshot.ObservedAt)
	}
}

func TestParseUsageEventDefaultsDuration(t *testing.T) {
	line := `data: {"subscription":{"weekly":{"resets_at":1800086400,"used_percent":5},"window":{"resets_at":1800018000,"used_percent":5}},"type":"response.subscription_usage"}`
	snapshot := ParseUsageEvent(line, time.Now())
	if snapshot == nil || snapshot.WindowDurationMins != DefaultWindowDurationMins {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestParseUsageEventRejectsNonUsageLines(t *testing.T) {
	now := time.Now()
	for _, line := range []string{
		``,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		`data: {"type":"response.subscription_usage"}`,
		`data: not json`,
		`: keep-alive`,
	} {
		if snapshot := ParseUsageEvent(line, now); snapshot != nil {
			t.Fatalf("line %q parsed as %+v", line, snapshot)
		}
	}
}

func TestScanUsageChunkCarriesPartialLines(t *testing.T) {
	now := time.Now()
	event := `data: {"subscription":{"weekly":{"resets_at":1800086400,"used_percent":5},"window":{"resets_at":1800018000,"used_percent":6}},"type":"response.subscription_usage"}` + "\n"
	first := event[:len(event)/2]
	second := event[len(event)/2:]

	pending, snapshots := ScanUsageChunk([]byte("data: {\"type\":\"response.created\"}\n"+first), "", now)
	if len(snapshots) != 0 {
		t.Fatalf("snapshots = %v", snapshots)
	}
	pending, snapshots = ScanUsageChunk([]byte(second+"data: [DONE]\n"), pending, now)
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %v", snapshots)
	}
	if snapshots[0].WindowUsedPercent != 6 || snapshots[0].WeeklyUsedPercent != 5 {
		t.Fatalf("snapshot = %+v", snapshots[0])
	}
	if pending != "" {
		t.Fatalf("pending = %q", pending)
	}
}

func TestUsageStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewUsageStore(dir)
	snapshot := UsageSnapshot{
		WindowUsedPercent:  10,
		WindowResetsAt:     time.Unix(1_800_018_000, 0).UTC(),
		WindowDurationMins: 300,
		WeeklyUsedPercent:  20,
		WeeklyResetsAt:     time.Unix(1_800_086_400, 0).UTC(),
		Tier:               "pro",
		ObservedAt:         time.Unix(1_800_000_000, 0).UTC(),
	}
	store.Record("account-1", snapshot)

	got := store.Snapshot("account-1")
	if got == nil {
		t.Fatal("expected a snapshot")
	}
	if got.WindowUsedPercent != snapshot.WindowUsedPercent ||
		got.WeeklyUsedPercent != snapshot.WeeklyUsedPercent ||
		got.WindowDurationMins != snapshot.WindowDurationMins ||
		got.Tier != snapshot.Tier ||
		!got.WindowResetsAt.Equal(snapshot.WindowResetsAt) ||
		!got.WeeklyResetsAt.Equal(snapshot.WeeklyResetsAt) ||
		!got.ObservedAt.Equal(snapshot.ObservedAt) {
		t.Fatalf("got %+v, want %+v", got, snapshot)
	}
	if store.Snapshot("missing") != nil {
		t.Fatal("expected nil for unknown account")
	}
	info, err := os.Stat(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o", info.Mode().Perm())
	}

	store.Remove("account-1")
	if store.Snapshot("account-1") != nil {
		t.Fatal("expected snapshot to be removed")
	}
}
