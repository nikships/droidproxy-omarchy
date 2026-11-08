package meta

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/events"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// UsageSnapshot is the last-observed subscription quota for one Meta Muse
// account (macOS MetaMuseUsageSnapshot).
//
// Meta exposes no usage endpoint: the 5-hour window and weekly percents
// arrive as a `response.subscription_usage` SSE event on the /v1/responses
// stream. ThinkingProxy sniffs that event while relaying Meta Responses
// traffic and records it here, so the UI shows the same numbers `muse /usage`
// would. Dates encode as unix seconds, matching the Swift encoder's
// .secondsSince1970 strategy so the file stays cross-platform compatible.
type UsageSnapshot struct {
	WindowUsedPercent  float64   `json:"windowUsedPercent"`
	WindowResetsAt     time.Time `json:"windowResetsAt"`
	WindowDurationMins float64   `json:"windowDurationMins"`
	WeeklyUsedPercent  float64   `json:"weeklyUsedPercent"`
	WeeklyResetsAt     time.Time `json:"weeklyResetsAt"`
	Tier               string    `json:"tier,omitempty"`
	ObservedAt         time.Time `json:"observedAt"`
}

// MarshalJSON encodes dates as unix seconds (float64).
func (s UsageSnapshot) MarshalJSON() ([]byte, error) {
	type alias UsageSnapshot
	return json.Marshal(struct {
		alias
		WindowResetsAt float64 `json:"windowResetsAt"`
		WeeklyResetsAt float64 `json:"weeklyResetsAt"`
		ObservedAt     float64 `json:"observedAt"`
	}{
		alias:          alias(s),
		WindowResetsAt: secondsFromTime(s.WindowResetsAt),
		WeeklyResetsAt: secondsFromTime(s.WeeklyResetsAt),
		ObservedAt:     secondsFromTime(s.ObservedAt),
	})
}

// UnmarshalJSON decodes dates from unix seconds (float64).
func (s *UsageSnapshot) UnmarshalJSON(data []byte) error {
	var raw struct {
		WindowUsedPercent  float64 `json:"windowUsedPercent"`
		WindowResetsAt     float64 `json:"windowResetsAt"`
		WindowDurationMins float64 `json:"windowDurationMins"`
		WeeklyUsedPercent  float64 `json:"weeklyUsedPercent"`
		WeeklyResetsAt     float64 `json:"weeklyResetsAt"`
		Tier               string  `json:"tier"`
		ObservedAt         float64 `json:"observedAt"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.WindowUsedPercent = raw.WindowUsedPercent
	s.WindowResetsAt = timeFromSeconds(raw.WindowResetsAt)
	s.WindowDurationMins = raw.WindowDurationMins
	s.WeeklyUsedPercent = raw.WeeklyUsedPercent
	s.WeeklyResetsAt = timeFromSeconds(raw.WeeklyResetsAt)
	s.Tier = raw.Tier
	s.ObservedAt = timeFromSeconds(raw.ObservedAt)
	return nil
}

// DefaultWindowDurationMins is the 5-hour window all subscription accounts
// report. Used when the event omits window_duration_mins.
const DefaultWindowDurationMins = 300.0

// usageEventPayload is the observed `response.subscription_usage` SSE shape:
//
//	data: {"subscription":{"tier":"...","weekly":{"resets_at":N,"used_percent":N},
//	"window":{"resets_at":N,"used_percent":N,"window_duration_mins":300}},
//	"type":"response.subscription_usage"}
//
// Non-streaming /v1/responses responses carry no `subscription` object, so
// only the stream is sniffed.
type usageEventPayload struct {
	Type         string `json:"type"`
	Subscription *struct {
		Tier   string `json:"tier"`
		Weekly struct {
			ResetsAt    float64 `json:"resets_at"`
			UsedPercent float64 `json:"used_percent"`
		} `json:"weekly"`
		Window struct {
			ResetsAt           float64  `json:"resets_at"`
			UsedPercent        float64  `json:"used_percent"`
			WindowDurationMins *float64 `json:"window_duration_mins"`
		} `json:"window"`
	} `json:"subscription"`
}

// ParseUsageEvent parses one `response.subscription_usage` SSE data line into
// a snapshot. It returns nil for anything else.
func ParseUsageEvent(dataLine string, observedAt time.Time) *UsageSnapshot {
	jsonText := dataLine
	if rest, ok := strings.CutPrefix(jsonText, "data:"); ok {
		jsonText = rest
	}
	var payload usageEventPayload
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonText)), &payload); err != nil {
		return nil
	}
	if payload.Type != "response.subscription_usage" || payload.Subscription == nil {
		return nil
	}
	sub := payload.Subscription
	durationMins := DefaultWindowDurationMins
	if sub.Window.WindowDurationMins != nil {
		durationMins = *sub.Window.WindowDurationMins
	}
	return &UsageSnapshot{
		WindowUsedPercent:  clampPercent(sub.Window.UsedPercent),
		WindowResetsAt:     timeFromSeconds(sub.Window.ResetsAt),
		WindowDurationMins: durationMins,
		WeeklyUsedPercent:  clampPercent(sub.Weekly.UsedPercent),
		WeeklyResetsAt:     timeFromSeconds(sub.Weekly.ResetsAt),
		Tier:               sub.Tier,
		ObservedAt:         observedAt,
	}
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// MaxUsagePendingCount caps the incomplete trailing SSE line carried across
// chunks, so a non-SSE body cannot grow the buffer without bound.
const MaxUsagePendingCount = 16 * 1024

// ScanUsageChunk splits relayed Meta response bytes into SSE lines and pulls
// out usage snapshots. Pure: callers record the returned snapshots. Relayed
// bytes are never modified — the proxy forwards them unchanged. It returns
// the incomplete trailing line for the next chunk.
func ScanUsageChunk(chunk []byte, pending string, observedAt time.Time) (string, []UsageSnapshot) {
	buffer := pending + string(chunk)
	var snapshots []UsageSnapshot
	for {
		idx := strings.IndexByte(buffer, '\n')
		if idx < 0 {
			break
		}
		rawLine := buffer[:idx]
		buffer = buffer[idx+1:]
		line := strings.TrimSuffix(rawLine, "\r")
		if !strings.HasPrefix(line, "data:") || !strings.Contains(line, "response.subscription_usage") {
			continue
		}
		if snapshot := ParseUsageEvent(line, observedAt); snapshot != nil {
			snapshots = append(snapshots, *snapshot)
		}
	}
	if len(buffer) > MaxUsagePendingCount {
		buffer = buffer[len(buffer)-MaxUsagePendingCount:]
	}
	return buffer, snapshots
}

// UsageStore persists last-observed Meta usage per account id next to the
// credential store (~/.droidproxy/meta/usage.json, 0600). Thread-safe:
// ThinkingProxy records from its relay goroutines while the usage tracker
// reads on its own.
type UsageStore struct {
	directory string
	mu        sync.Mutex
}

var (
	sharedUsageMu  sync.Mutex
	sharedUsage    *UsageStore
	sharedUsageDir string
)

// SharedUsageStore returns the process-wide usage store for
// paths.MetaDataDir(). If HOME changes (tests), a fresh store is opened for
// the new path, like Shared().
func SharedUsageStore() *UsageStore {
	sharedUsageMu.Lock()
	defer sharedUsageMu.Unlock()
	dir := paths.MetaDataDir()
	if sharedUsage == nil || sharedUsageDir != dir {
		sharedUsage = NewUsageStore(dir)
		sharedUsageDir = dir
	}
	return sharedUsage
}

// NewUsageStore opens a usage store rooted at directory.
func NewUsageStore(directory string) *UsageStore {
	return &UsageStore{directory: directory}
}

// UsagePath is the usage.json location.
func (s *UsageStore) UsagePath() string { return filepath.Join(s.directory, "usage.json") }

// Snapshot returns the last-observed usage for accountID, or nil when none
// was ever observed.
func (s *UsageStore) Snapshot(accountID string) *UsageSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshots, err := s.load()
	if err != nil {
		return nil
	}
	snapshot, ok := snapshots[accountID]
	if !ok {
		return nil
	}
	return &snapshot
}

// Record persists a snapshot for accountID and publishes
// events.MetaUsageChanged so the usage tracker refreshes just the Meta cards.
func (s *UsageStore) Record(accountID string, snapshot UsageSnapshot) {
	s.mu.Lock()
	snapshots, err := s.load()
	if err != nil {
		snapshots = map[string]UsageSnapshot{}
	}
	snapshots[accountID] = snapshot
	writeErr := s.write(snapshots)
	s.mu.Unlock()
	if writeErr != nil {
		logx.Logf("[Meta] Could not record usage snapshot: %v", writeErr)
		return
	}
	events.Publish(events.MetaUsageChanged)
}

// Remove deletes the snapshot for accountID, if any.
func (s *UsageStore) Remove(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshots, err := s.load()
	if err != nil {
		return
	}
	if _, ok := snapshots[accountID]; !ok {
		return
	}
	delete(snapshots, accountID)
	if err := s.write(snapshots); err != nil {
		logx.Logf("[Meta] Could not remove usage snapshot: %v", err)
	}
}

func (s *UsageStore) load() (map[string]UsageSnapshot, error) {
	data, err := os.ReadFile(s.UsagePath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]UsageSnapshot{}, nil
		}
		return nil, err
	}
	var snapshots map[string]UsageSnapshot
	if err := json.Unmarshal(data, &snapshots); err != nil {
		return nil, err
	}
	if snapshots == nil {
		snapshots = map[string]UsageSnapshot{}
	}
	return snapshots, nil
}

func (s *UsageStore) write(snapshots map[string]UsageSnapshot) error {
	if err := paths.EnsureDir(s.directory, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(snapshots)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.directory, ".usage.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.UsagePath()); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Chmod(s.UsagePath(), 0o600)
}
