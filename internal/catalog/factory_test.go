package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

func writeFactoryFile(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(home(t), ".factory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFactoryFile(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home(t), ".factory", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func modelIDsFromSettings(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var settings struct {
		CustomModels []map[string]any `json:"customModels"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("written settings are not valid JSON: %v\n%s", err, data)
	}
	return settings.CustomModels
}

func allEnabled(_ auth.ServiceType) bool { return true }

func home(t *testing.T) string {
	t.Helper()
	return os.Getenv("HOME")
}

// TestMain isolates the whole package from the developer's real HOME:
// SettingsModels reads prefs (per-model toggles), so without this the
// suite passes or fails with the state of the machine it runs on.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "droidproxy-catalog-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", "")
	os.Setenv("XDG_DATA_HOME", "")
	os.Setenv("XDG_STATE_HOME", "")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// isolateHome redirects paths and prefs to a temp dir for the test.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	prefs.Shared().Reload()
}

func TestApplyMergesModelsAndKeepsOtherSettings(t *testing.T) {
	isolateHome(t)
	writeFactoryFile(t, `{"theme":"dark","customModels":[{"id":"user-model","model":"m"},{"id":"custom:droidproxy:grok-4.5","model":"stale"}],"other":{"a":1}}`)

	msg, err := ApplyFactoryCustomModels(allEnabled)
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if !strings.HasPrefix(msg, "DroidProxy models merged into Factory settings.") {
		t.Errorf("unexpected message: %q", msg)
	}

	var settings map[string]any
	if err := json.Unmarshal(readFactoryFile(t), &settings); err != nil {
		t.Fatal(err)
	}
	if settings["theme"] != "dark" {
		t.Errorf("theme lost: %v", settings["theme"])
	}
	if other, ok := settings["other"].(map[string]any); !ok || other["a"] != float64(1) {
		t.Errorf("other lost: %v", settings["other"])
	}

	models := modelIDsFromSettings(t, readFactoryFile(t))
	var ids []string
	var userModel map[string]any
	for _, m := range models {
		id, _ := m["id"].(string)
		if id == "user-model" {
			userModel = m
		}
		ids = append(ids, id)
	}
	if userModel == nil {
		t.Error("user custom model was removed")
	} else if _, hasIndex := userModel["index"]; hasIndex {
		// Swift leaves kept (non-DroidProxy) models untouched, including any
		// index they already had; a fresh one has none.
		t.Errorf("user model index unexpectedly set: %v", userModel["index"])
	}
	if len(ids) != len(Definitions())+1 {
		t.Errorf("expected %d models, got %v", len(Definitions())+1, ids)
	}
	if modelIndexFor(t, models, "custom:droidproxy:fable-5-1") != 1 {
		t.Error("DroidProxy entries must start at index = kept count")
	}
}

func TestApplySkipsAndRemovesDisabledModelsOnly(t *testing.T) {
	isolateHome(t)
	victim := Definitions()[0]
	// A non-DroidProxy entry that merely carries the DroidProxy display name
	// must survive: ownership is the custom:droidproxy: id prefix.
	writeFactoryFile(t, `{"customModels":[{"id":"user-model","model":"m","displayName":"DroidProxy: mine"},{"id":"`+victim.SimpleID()+`","model":"stale"}]}`)
	if err := prefs.SetModelEnabled(victim.IDSlug, false); err != nil {
		t.Fatal(err)
	}

	if _, err := ApplyFactoryCustomModels(allEnabled); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range modelIDsFromSettings(t, readFactoryFile(t)) {
		if id, ok := m["id"].(string); ok {
			ids[id] = true
		}
	}
	if ids[victim.SimpleID()] {
		t.Errorf("disabled model %s was written to Factory", victim.SimpleID())
	}
	if !ids["user-model"] {
		t.Error("non-DroidProxy model was removed")
	}
	if len(ids) != len(Definitions()) {
		t.Errorf("expected %d entries (all but the disabled one, plus user model), got %d", len(Definitions()), len(ids))
	}
	if !CheckFactoryModelsInstalled(allEnabled) {
		t.Error("installed check should pass with the disabled model absent")
	}

	if err := prefs.SetModelEnabled(victim.IDSlug, true); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyFactoryCustomModels(allEnabled); err != nil {
		t.Fatalf("re-apply failed: %v", err)
	}
	found := false
	for _, m := range modelIDsFromSettings(t, readFactoryFile(t)) {
		if m["id"] == victim.SimpleID() {
			found = true
		}
	}
	if !found {
		t.Error("re-enabled model was not restored to Factory")
	}
}

func modelIndexFor(t *testing.T, models []map[string]any, id string) int {
	t.Helper()
	for _, m := range models {
		if m["id"] == id {
			f, ok := m["index"].(float64)
			if !ok {
				t.Fatalf("index missing for %s", id)
			}
			return int(f)
		}
	}
	t.Fatalf("model %s not found", id)
	return -1
}

func TestApplyRemovesLegacyAndOldCopilotEntries(t *testing.T) {
	isolateHome(t)
	writeFactoryFile(t, `{"customModels":[
		{"id":"custom:droidproxy:cursor-composer-2.5"},
		{"id":"custom:droidproxy:copilot-claude-opus-4-8"},
		{"id":"custom:CC:legacy-thing"},
		{"id":"keep-me"}
	]}`)

	if _, err := ApplyFactoryCustomModels(allEnabled); err != nil {
		t.Fatal(err)
	}
	written := string(readFactoryFile(t))
	for _, stale := range []string{
		"custom:droidproxy:cursor-composer-2.5",
		"custom:droidproxy:copilot-claude-opus-4-8",
		"custom:CC:legacy-thing",
	} {
		if strings.Contains(written, stale) {
			t.Errorf("stale id %s survived apply", stale)
		}
	}
	if !strings.Contains(written, "keep-me") {
		t.Error("non-DroidProxy entry was removed")
	}
}

func TestApplyWritesTimestampedBackup(t *testing.T) {
	isolateHome(t)
	writeFactoryFile(t, `{"customModels":[]}`)
	fake := time.Date(2026, 9, 24, 12, 34, 56, 0, time.Local)
	now = func() time.Time { return fake }
	t.Cleanup(func() { now = time.Now })

	if _, err := ApplyFactoryCustomModels(allEnabled); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(home(t), ".factory", "settings.json.droidproxy-20260924-123456.bak")
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if strings.TrimSpace(string(data)) != `{"customModels":[]}` {
		t.Errorf("backup content = %s", data)
	}

	// An unchanged re-apply writes no new backup.
	now = func() time.Time { return fake.Add(time.Minute) }
	if _, err := ApplyFactoryCustomModels(allEnabled); err != nil {
		t.Fatal(err)
	}
	if entries, _ := filepath.Glob(filepath.Join(home(t), ".factory", "*.bak")); len(entries) != 1 {
		t.Errorf("re-apply backups = %v, want only the first", entries)
	}

	// No settings file → no backup.
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home(t), ".factory", "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyFactoryCustomModels(allEnabled); err != nil {
		t.Fatal(err)
	}
	entries, _ := filepath.Glob(filepath.Join(home(t), ".factory", "*.bak"))
	if len(entries) != 0 {
		t.Errorf("unexpected backups: %v", entries)
	}
}

func TestCheckFactoryModelsInstalled(t *testing.T) {
	isolateHome(t)
	if CheckFactoryModelsInstalled(allEnabled) {
		t.Error("missing file must read as not installed")
	}

	if _, err := ApplyFactoryCustomModels(allEnabled); err != nil {
		t.Fatal(err)
	}
	if !CheckFactoryModelsInstalled(allEnabled) {
		t.Error("freshly applied settings must read as installed")
	}

	// Removing one DroidProxy entry breaks the exact-match check.
	var settings map[string]any
	if err := json.Unmarshal(readFactoryFile(t), &settings); err != nil {
		t.Fatal(err)
	}
	models := settings["customModels"].([]any)
	filtered := models[:len(models)-1]
	settings["customModels"] = filtered
	out, _ := json.Marshal(settings)
	writeFactoryFile(t, string(out))
	if CheckFactoryModelsInstalled(allEnabled) {
		t.Error("missing entry must read as not installed")
	}

	// A stale legacy entry alongside the current ones also breaks it.
	if _, err := ApplyFactoryCustomModels(allEnabled); err != nil {
		t.Fatal(err)
	}
	settings2 := map[string]any{}
	if err := json.Unmarshal(readFactoryFile(t), &settings2); err != nil {
		t.Fatal(err)
	}
	ms := settings2["customModels"].([]any)
	settings2["customModels"] = append(ms, map[string]any{"id": "custom:droidproxy:grok-4.5"})
	out2, _ := json.Marshal(settings2)
	writeFactoryFile(t, string(out2))
	if CheckFactoryModelsInstalled(allEnabled) {
		t.Error("stale legacy entry must read as not installed")
	}
}

func TestCheckFactoryModelsInstalledRespectsProviderFilter(t *testing.T) {
	isolateHome(t)
	writeFactoryFile(t, `{}`)
	if _, err := ApplyFactoryCustomModels(allEnabled); err != nil {
		t.Fatal(err)
	}
	enabled := func(s auth.ServiceType) bool { return s == auth.Claude }
	if CheckFactoryModelsInstalled(enabled) {
		t.Error("all-providers file must not match a claude-only filter")
	}
	if _, err := ApplyFactoryCustomModels(enabled); err != nil {
		t.Fatal(err)
	}
	if !CheckFactoryModelsInstalled(enabled) {
		t.Error("claude-only apply must read as installed for the same filter")
	}
	if CheckFactoryModelsInstalled(allEnabled) {
		t.Error("claude-only file must not match the all-providers filter")
	}
}

func TestUnknownProviderKeyIsEnabled(t *testing.T) {
	never := func(auth.ServiceType) bool { return false }
	// "meta" is a known key and gated off by the filter; a hypothetical unknown
	// provider key must not be filtered out. Every current key is known, so
	// verify via the filter adapter directly.
	filter := providerKeyFilter(never)
	if !filter("some-future-provider") {
		t.Error("unknown provider keys must default to enabled")
	}
	if filter("claude") {
		t.Error("known provider keys must respect the predicate")
	}
}

func TestApplyFailureMessage(t *testing.T) {
	isolateHome(t)
	// Make the settings path a directory so the write fails.
	dir := filepath.Join(home(t), ".factory")
	if err := os.MkdirAll(filepath.Join(dir, "settings.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	msg, err := ApplyFactoryCustomModels(allEnabled)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.HasPrefix(msg, "Failed to update Factory settings: ") {
		t.Errorf("unexpected failure message: %q", msg)
	}
}

// Overlapping applies (rapid panel toggles each fire one) must leave
// one coherent settings file: valid JSON, each DroidProxy model once.
// Run with -race.
func TestApplyFactoryCustomModelsConcurrent(t *testing.T) {
	isolateHome(t)
	writeFactoryFile(t, `{"customModels":[{"id":"user-model","model":"m"}]}`)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ApplyFactoryCustomModels(allEnabled); err != nil {
				t.Errorf("concurrent apply: %v", err)
			}
		}()
	}
	wg.Wait()

	seen := map[string]int{}
	for _, m := range modelIDsFromSettings(t, readFactoryFile(t)) {
		if id, ok := m["id"].(string); ok {
			seen[id]++
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("model %s written %d times", id, n)
		}
	}
	if seen["user-model"] != 1 {
		t.Error("user model lost")
	}
}
