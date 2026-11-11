// Package prefs is the UserDefaults replacement: a small JSON key/value store
// at ~/.config/droidproxy/settings.json. Reads are served from memory, writes
// are persisted atomically and announced on events.PrefsChanged.
package prefs

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

// Keys match the macOS UserDefaults keys so settings read the same everywhere.
const (
	KeyGPT6AstraFastMode         = "gpt6AstraFastMode"
	KeyGPT61SolFastMode          = "gpt61SolFastMode"
	KeyGPT61TerraFastMode        = "gpt61TerraFastMode"
	KeyGPT6LunaFastMode          = "gpt6LunaFastMode"
	KeyMetaContributorMode       = "metaContributorMode"
	KeyAllowRemote               = "allowRemote"
	KeySecretKey                 = "secretKey"
	KeyBindAddress               = "bindAddress"
	KeyBetaFlag                  = "BETA_FLAG"
	KeyVerboseLogging            = "verboseLogging"
	KeySequentialAccountFailover = "sequentialAccountFailover"
	KeyEnabledProviders          = "enabledProviders"
	KeyEnabledModels             = "enabledModels"
	KeyAutoCheckUpdates          = "autoCheckUpdates"
	KeyAutoInstallUpdates        = "autoInstallUpdates"
	KeyLastUpdateCheck           = "lastUpdateCheck"
	KeySkippedVersion            = "skippedVersion"
	KeyRequestJournal            = "requestJournal"
)

const DefaultBindAddress = "127.0.0.1"

// Store is a JSON-backed preferences store.
type Store struct {
	mu     sync.RWMutex
	path   string
	values map[string]json.RawMessage
	loaded bool
}

var (
	sharedMu sync.Mutex
	shared   *Store
)

// Shared returns the process-wide store for paths.PrefsPath(). If HOME or
// XDG_CONFIG_HOME change (tests), a fresh store is opened for the new path.
func Shared() *Store {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	p := paths.PrefsPath()
	if shared == nil || shared.path != p {
		shared = Open(p)
	}
	return shared
}

// Open loads (or lazily creates) a store at path.
func Open(path string) *Store {
	s := &Store{path: path, values: map[string]json.RawMessage{}}
	s.load()
	return s
}

func (s *Store) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded = true
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		logx.Logf("[Prefs] Ignoring unreadable %s: %v", s.path, err)
		return
	}
	s.values = m
}

// Reload re-reads the file from disk (used when another process edits it).
func (s *Store) Reload() { s.load() }

func (s *Store) persistLocked() error {
	if err := paths.EnsureDir(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.values, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Has reports whether key has an explicit value.
func (s *Store) Has(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.values[key]
	return ok
}

// Get decodes key into out and reports whether the key was present and valid.
func (s *Store) Get(key string, out any) bool {
	s.mu.RLock()
	raw, ok := s.values[key]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

// Set stores v under key, persists, and publishes events.PrefsChanged.
func (s *Store) Set(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.values[key] = raw
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		logx.Logf("[Prefs] Failed to persist %s: %v", key, err)
	}
	events.Publish(events.PrefsChanged)
	return err
}

// setMapBool atomically sets one entry of a map[string]bool preference.
// Reading the map, changing one entry, and persisting happen under one
// lock: concurrent callers each editing a different entry (control calls
// run per-connection) must not lose each other's updates.
func (s *Store) setMapBool(key, entry string, value bool) error {
	s.mu.Lock()
	m := map[string]bool{}
	if raw, ok := s.values[key]; ok {
		_ = json.Unmarshal(raw, &m)
	}
	m[entry] = value
	raw, err := json.Marshal(m)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.values[key] = raw
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		logx.Logf("[Prefs] Failed to persist %s: %v", key, err)
	}
	events.Publish(events.PrefsChanged)
	return err
}

// Delete removes key.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	delete(s.values, key)
	err := s.persistLocked()
	s.mu.Unlock()
	events.Publish(events.PrefsChanged)
	return err
}

// Bool returns key as a bool, or def when unset.
func (s *Store) Bool(key string, def bool) bool {
	var v bool
	if s.Get(key, &v) {
		return v
	}
	return def
}

// String returns key as a string, or def when unset.
func (s *Store) String(key string, def string) string {
	var v string
	if s.Get(key, &v) {
		return v
	}
	return def
}

// StringSlice returns key as a []string, or nil when unset.
func (s *Store) StringSlice(key string) []string {
	var v []string
	if s.Get(key, &v) {
		return v
	}
	return nil
}

// ---- AppPreferences equivalents -------------------------------------------

func GPT6AstraFastMode() bool   { return Shared().Bool(KeyGPT6AstraFastMode, false) }
func GPT61SolFastMode() bool    { return Shared().Bool(KeyGPT61SolFastMode, false) }
func GPT61TerraFastMode() bool  { return Shared().Bool(KeyGPT61TerraFastMode, false) }
func GPT6LunaFastMode() bool    { return Shared().Bool(KeyGPT6LunaFastMode, false) }
func MetaContributorMode() bool { return Shared().Bool(KeyMetaContributorMode, false) }
func AllowRemote() bool         { return Shared().Bool(KeyAllowRemote, false) }
func SecretKey() string         { return Shared().String(KeySecretKey, "") }
func BetaFlag() bool            { return Shared().Bool(KeyBetaFlag, false) }
func VerboseLogging() bool      { return Shared().Bool(KeyVerboseLogging, false) }
func SequentialAccountFailover() bool {
	return Shared().Bool(KeySequentialAccountFailover, false)
}
func AutoCheckUpdates() bool   { return Shared().Bool(KeyAutoCheckUpdates, true) }
func AutoInstallUpdates() bool { return Shared().Bool(KeyAutoInstallUpdates, false) }
func RequestJournal() bool     { return Shared().Bool(KeyRequestJournal, false) }

// BindAddress is only honored when the beta flag is on. Empty or multi-line
// values are rejected so they can't produce an invalid listener host or inject
// extra lines into the generated YAML config.
func BindAddress() string {
	if !BetaFlag() {
		return DefaultBindAddress
	}
	trimmed := strings.TrimSpace(Shared().String(KeyBindAddress, DefaultBindAddress))
	if trimmed == "" || strings.ContainsAny(trimmed, "\r\n") {
		return DefaultBindAddress
	}
	return trimmed
}

// EnabledProviders returns the per-provider enabled map (missing = enabled).
func EnabledProviders() map[string]bool {
	m := map[string]bool{}
	Shared().Get(KeyEnabledProviders, &m)
	return m
}

// IsProviderEnabled reports whether provider (a ServiceType raw value such as
// "claude" or "antigravity") is enabled. Providers default to enabled.
func IsProviderEnabled(provider string) bool {
	if v, ok := EnabledProviders()[provider]; ok {
		return v
	}
	return true
}

// SetProviderEnabled persists the enabled state for provider.
func SetProviderEnabled(provider string, enabled bool) error {
	return Shared().setMapBool(KeyEnabledProviders, provider, enabled)
}

// EnabledModels returns the per-model enabled map, keyed by catalog ID slug
// (missing = enabled).
func EnabledModels() map[string]bool {
	m := map[string]bool{}
	Shared().Get(KeyEnabledModels, &m)
	return m
}

// IsModelEnabled reports whether the catalog model with this ID slug is
// enabled. Models default to enabled.
func IsModelEnabled(slug string) bool {
	if v, ok := EnabledModels()[slug]; ok {
		return v
	}
	return true
}

// SetModelEnabled persists the enabled state for one catalog model.
func SetModelEnabled(slug string, enabled bool) error {
	return Shared().setMapBool(KeyEnabledModels, slug, enabled)
}

// DefaultWindowPrimerMinutes is 07:00, in minutes after local midnight.
const DefaultWindowPrimerMinutes = 7 * 60

// Window primer keys are per provider ("claude", "codex", "meta").
func WindowPrimerEnabledKey(provider string) string   { return "windowPrimerEnabled." + provider }
func WindowPrimerMinutesKey(provider string) string   { return "windowPrimerMinutes." + provider }
func windowPrimerLastFiredKey(provider string) string { return "windowPrimerLastFired." + provider }

// WindowPrimerEnabled reports whether "Auto-start daily" is on for provider.
func WindowPrimerEnabled(provider string) bool {
	return Shared().Bool(WindowPrimerEnabledKey(provider), false)
}

// WindowPrimerMinutes is the chosen time in minutes after local midnight.
func WindowPrimerMinutes(provider string) int {
	var v int
	if Shared().Get(WindowPrimerMinutesKey(provider), &v) {
		return v
	}
	return DefaultWindowPrimerMinutes
}

// WindowPrimerLastFired is when provider's window was last started, or nil.
// Stored as unix seconds, like lastUpdateCheck.
func WindowPrimerLastFired(provider string) *time.Time {
	var seconds int64
	if !Shared().Get(windowPrimerLastFiredKey(provider), &seconds) {
		return nil
	}
	t := time.Unix(seconds, 0)
	return &t
}

// SetWindowPrimerLastFired records when provider's window was started.
func SetWindowPrimerLastFired(provider string, t time.Time) error {
	return Shared().Set(windowPrimerLastFiredKey(provider), t.Unix())
}
