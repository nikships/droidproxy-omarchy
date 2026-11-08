package installer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nikships/droidproxy-omarchy/internal/buildinfo"
)

// TestShippedPluginIsComplete guards the release tree: the plugin that
// stage-tree copies into share/plugin must be loadable as-is (manifest id
// matches the installer, entry points and referenced files exist, strings
// parse). CI has no Omarchy shell, so this is the completeness floor under
// `omarchy plugin validate` on a real desktop.
func TestShippedPluginIsComplete(t *testing.T) {
	root := filepath.Join("..", "..", "plugin")
	data, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		ID          string            `json:"id"`
		EntryPoints map[string]string `json:"entryPoints"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("manifest does not parse: %v", err)
	}
	if manifest.ID != buildinfo.PluginID {
		t.Fatalf("manifest id = %q, want %q", manifest.ID, buildinfo.PluginID)
	}
	for _, kind := range []string{"service", "barWidget"} {
		entry := manifest.EntryPoints[kind]
		if entry == "" {
			t.Fatalf("manifest entryPoints[%q] is empty", kind)
		}
		if _, err := os.Stat(filepath.Join(root, entry)); err != nil {
			t.Fatalf("entry point %s: %v", entry, err)
		}
	}

	i18n, err := os.ReadFile(filepath.Join(root, "i18n", "en.json"))
	if err != nil {
		t.Fatal(err)
	}
	var strings_ map[string]json.RawMessage
	if err := json.Unmarshal(i18n, &strings_); err != nil {
		t.Fatalf("i18n/en.json does not parse: %v", err)
	}

	panel, err := os.ReadFile(filepath.Join(root, manifest.EntryPoints["barWidget"]))
	if err != nil {
		t.Fatal(err)
	}
	for _, dead := range []string{"BarWidget", "login-procs", "openWebUI"} {
		if strings.Contains(string(panel), dead) {
			t.Errorf("panel still references %q", dead)
		}
	}
	for _, icon := range []string{"icon-active.png", "icon-inactive.png"} {
		if !strings.Contains(string(panel), icon) {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "assets", "icons", icon)); err != nil {
			t.Errorf("panel references missing icon %s: %v", icon, err)
		}
	}
}
