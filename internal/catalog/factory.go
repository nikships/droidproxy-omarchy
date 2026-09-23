package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

// ApplySuccessMessage is shown after the Factory custom models are merged.
const ApplySuccessMessage = "DroidProxy models merged into Factory settings.\n\nYour other custom models were kept. Only previous DroidProxy entries were replaced. A timestamped backup was saved next to settings.json.\n\nIn Droid CLI use /model and search for “DroidProxy:”. Restart Factory or open a new session if the picker looks stale. Reasoning effort is controlled from Droid per session when the model exposes multiple levels."

// applyFailurePrefix precedes the error text when the merge fails.
const applyFailurePrefix = "Failed to update Factory settings: "

// legacyModelIDs were retired by prior releases. They are removed from
// customModels during Apply so users don't keep stale entries next to the
// current ones.
var legacyModelIDs = map[string]bool{
	"custom:droidproxy:grok-4.5":             true,
	"custom:droidproxy:grok-4.6":             true,
	"custom:droidproxy:cursor-composer-2.5":  true,
	"custom:droidproxy:cursor-grok-4.5":      true,
	"custom:droidproxy:cursor-grok-4.5-fast": true,
	"custom:droidproxy:cursor-grok-4.6":      true,
	"custom:droidproxy:cursor-grok-4.6-fast": true,
	"custom:droidproxy:cursor-small":         true,
}

// now is swapped in tests to make backup names deterministic.
var now = time.Now

// isDroidProxyID reports whether a customModels id belongs to DroidProxy. The
// "custom:droidproxy:" prefix also covers Copilot entries written by the macOS
// app ("custom:droidproxy:copilot-…"), so those are cleaned up on Apply.
func isDroidProxyID(id string, current map[string]bool) bool {
	return current[id] ||
		legacyModelIDs[id] ||
		strings.HasPrefix(id, "custom:droidproxy:") ||
		strings.HasPrefix(id, "custom:CC:")
}

// providerKeyFilter adapts a ServiceType predicate to catalog provider keys.
// Unknown keys are treated as enabled. A nil predicate reads the persisted
// enabledProviders preference (missing = enabled).
func providerKeyFilter(isProviderEnabled func(auth.ServiceType) bool) func(string) bool {
	if isProviderEnabled == nil {
		isProviderEnabled = func(s auth.ServiceType) bool { return prefs.IsProviderEnabled(string(s)) }
	}
	return func(providerKey string) bool {
		st, ok := auth.ServiceTypeFromAuthFileType(providerKey)
		if !ok {
			return true
		}
		return isProviderEnabled(st)
	}
}

func enabledFactorySettingsModels(isProviderEnabled func(auth.ServiceType) bool) []map[string]any {
	return SettingsModels(providerKeyFilter(isProviderEnabled))
}

// decodeJSON parses exactly one JSON value. Numbers are kept as json.Number so
// values in unrelated settings keys are written back with their original text.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

// objectArray mirrors Swift's `as? [[String: Any]]`: the cast fails unless
// every element is an object.
func objectArray(v any) ([]map[string]any, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

// CheckFactoryModelsInstalled reports whether ~/.factory/settings.json holds
// exactly the DroidProxy models that would be written for the currently
// enabled providers (no missing, stale, or legacy DroidProxy entries).
func CheckFactoryModelsInstalled(isProviderEnabled func(auth.ServiceType) bool) bool {
	data, err := os.ReadFile(paths.FactorySettingsPath())
	if err != nil {
		return false
	}
	v, err := decodeJSON(data)
	if err != nil {
		return false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	models, ok := objectArray(obj["customModels"])
	if !ok {
		return false
	}

	expected := map[string]bool{}
	for _, m := range enabledFactorySettingsModels(isProviderEnabled) {
		if id, ok := m["id"].(string); ok {
			expected[id] = true
		}
	}
	current := AllSettingsIDs()
	installed := map[string]bool{}
	for _, m := range models {
		if id, ok := m["id"].(string); ok && isDroidProxyID(id, current) {
			installed[id] = true
		}
	}
	if len(expected) == 0 || len(installed) != len(expected) {
		return false
	}
	for id := range expected {
		if !installed[id] {
			return false
		}
	}
	return true
}

// ApplyFactoryCustomModels merges the enabled DroidProxy models into
// ~/.factory/settings.json. Other settings keys and non-DroidProxy custom
// models are kept; every previous DroidProxy entry is replaced. A timestamped
// backup (settings.json.droidproxy-yyyyMMdd-HHmmss.bak) is written first.
//
// The returned message is the user-facing text for both outcomes; err is
// non-nil on failure.
func ApplyFactoryCustomModels(isProviderEnabled func(auth.ServiceType) bool) (string, error) {
	path := paths.FactorySettingsPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)

	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if v, err := decodeJSON(data); err == nil {
			if obj, ok := v.(map[string]any); ok {
				settings = obj
			}
		}
	}

	models, _ := objectArray(settings["customModels"])
	current := AllSettingsIDs()
	kept := make([]any, 0, len(models))
	for _, m := range models {
		if id, ok := m["id"].(string); ok && isDroidProxyID(id, current) {
			continue
		}
		kept = append(kept, m)
	}

	startIndex := len(kept)
	for offset, m := range enabledFactorySettingsModels(isProviderEnabled) {
		m["index"] = startIndex + offset
		kept = append(kept, m)
	}
	settings["customModels"] = kept

	if err := writeFactorySettings(path, settings); err != nil {
		logx.Logf("[SettingsView] Failed to apply Factory custom models: %v", err)
		return applyFailurePrefix + err.Error(), err
	}
	logx.Logf("[SettingsView] Factory custom models applied to %s", path)
	return ApplySuccessMessage, nil
}

func writeFactorySettings(path string, settings map[string]any) error {
	backupFactorySettingsIfPresent(path)

	// Go sorts map keys, matching JSONSerialization's .sortedKeys. HTML
	// escaping is off so URLs and "<"/">" stay literal, like the Swift output
	// after it undoes its "\/" escaping.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return err
	}
	data := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))

	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings.json.droidproxy-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func backupFactorySettingsIfPresent(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	backup := filepath.Join(filepath.Dir(path),
		"settings.json.droidproxy-"+now().Format("20060102-150405")+".bak")
	if err := copyFileExclusive(path, backup, info.Mode().Perm()); err != nil {
		logx.Logf("[SettingsView] Failed to back up Factory settings before applying custom models: %v", err)
		return
	}
	logx.Logf("[SettingsView] Backed up Factory settings to %s", backup)
}

// copyFileExclusive fails if dst exists, like FileManager.copyItem.
func copyFileExclusive(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(dst)
		return err
	}
	return f.Close()
}
