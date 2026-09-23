package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/meta"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

// bundledConfig reads the real packaging/config.yaml (two directories up) so
// the anchor tests exercise the shipped template rather than a copy that can
// drift, exactly like the Swift tests read Sources/Resources/config.yaml.
func bundledConfig(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "config.yaml"))
	if err != nil {
		t.Fatalf("reading bundled config: %v", err)
	}
	return string(data)
}

func occurrences(s, substr string) int {
	return strings.Count(s, substr)
}

func assertContainsOnce(t *testing.T, s, substr, context string) {
	t.Helper()
	if n := occurrences(s, substr); n != 1 {
		t.Errorf("%s: expected %q exactly once, found %d", context, substr, n)
	}
}

func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	prefs.Shared().Reload()
}

// Sequential failover is opt-in: it changes cooldown behavior for every
// provider, so an absent key must read as false and leave existing users on
// the historical round-robin routing. (Port of testDefaultIsOff.)
func TestSequentialFailoverDefaultsOff(t *testing.T) {
	isolateHome(t)
	if prefs.SequentialAccountFailover() {
		t.Fatal("sequential account failover must default to off")
	}
}

// (Port of testDisabledLeavesConfigUntouched.)
func TestApplyAccountFailoverDisabledLeavesConfigUntouched(t *testing.T) {
	config := bundledConfig(t)
	if got := ApplyAccountFailoverOverrides(config, false); got != config {
		t.Error("disabled failover must leave the config untouched")
	}
}

// (Port of testEnabledAppliesEveryOverride.)
func TestApplyAccountFailoverEnabledAppliesEveryOverride(t *testing.T) {
	updated := ApplyAccountFailoverOverrides(bundledConfig(t), true)

	for _, on := range []string{
		"max-retry-interval: 30",
		"disable-cooling: false",
		"transient-error-cooldown-seconds: -1",
		`strategy: "fill-first"`,
	} {
		if !strings.Contains(updated, on) {
			t.Errorf("missing ON value %q", on)
		}
	}
	// And none of the OFF values survive.
	for _, off := range []string{
		"max-retry-interval: 0",
		"disable-cooling: true",
		"transient-error-cooldown-seconds: 0",
		`strategy: "round-robin"`,
	} {
		if strings.Contains(updated, off) {
			t.Errorf("OFF value %q survived the override", off)
		}
	}
}

// The substitution table is only correct if each anchor appears exactly once
// in the bundled config. Zero occurrences means the override silently does
// nothing; more than one means an unrelated line gets rewritten.
// (Port of testEveryAnchorAppearsExactlyOnceInBundledConfig.)
func TestEveryAnchorAppearsExactlyOnceInBundledConfig(t *testing.T) {
	config := bundledConfig(t)
	for _, rule := range accountFailoverOverrides {
		assertContainsOnce(t, config, rule[0], "anchor check")
	}
}

// GenerateConfig also rewrites host, remote-management, and logging values by
// string match. The failover overrides must not disturb those.
// (Port of testFailoverOverridesPreserveOtherConfigAnchors.)
func TestFailoverOverridesPreserveOtherConfigAnchors(t *testing.T) {
	updated := ApplyAccountFailoverOverrides(bundledConfig(t), true)
	for _, anchor := range []string{
		"host: 127.0.0.1",
		"  allow-remote: false",
		`  secret-key: ""  # Leave empty to disable management API`,
		"debug: false",
		"logging-to-file: false",
	} {
		assertContainsOnce(t, updated, anchor, "surviving anchor")
	}
}

// Session affinity is required for stateful Codex Responses traffic
// (issue #58) and must stay on in both states.
// (Port of testSessionAffinityUnaffectedByOverrides.)
func TestSessionAffinityUnaffectedByOverrides(t *testing.T) {
	config := bundledConfig(t)
	updated := ApplyAccountFailoverOverrides(config, true)
	for _, s := range []string{"session-affinity: true", `session-affinity-ttl: "2h"`} {
		if !strings.Contains(config, s) || !strings.Contains(updated, s) {
			t.Errorf("session affinity anchor %q must survive in both states", s)
		}
	}
}

// Applying the overrides twice must be a no-op the second time, since
// GenerateConfig regenerates the merged config on every settings change.
// (Port of testOverridesAreIdempotent.)
func TestOverridesAreIdempotent(t *testing.T) {
	config := bundledConfig(t)
	once := ApplyAccountFailoverOverrides(config, true)
	twice := ApplyAccountFailoverOverrides(once, true)
	if once != twice {
		t.Error("re-applying the failover overrides must be a no-op")
	}
}

func TestRenderConfigAppliesUserSettings(t *testing.T) {
	template := "host: 127.0.0.1\nport: 8318\nhost: 127.0.0.1\n" +
		"remote-management:\n  allow-remote: false\n" +
		`  secret-key: ""  # Leave empty to disable management API` + "\n" +
		"debug: false\nlogging-to-file: false\n"
	got := renderConfig(template, configOptions{
		BindAddress:    "0.0.0.0",
		AllowRemote:    true,
		SecretKey:      "s3cret",
		VerboseLogging: true,
	})

	// Only the first host anchor is replaced.
	assertContainsOnce(t, got, "host: 0.0.0.0", "bind address")
	assertContainsOnce(t, got, "host: 127.0.0.1", "second host anchor")

	for _, want := range []string{
		"  allow-remote: true",
		`  secret-key: "s3cret"`,
		"debug: true",
		"logging-to-file: true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(got, "Leave empty to disable management API") {
		t.Error("secret-key substitution must drop the template comment")
	}
}

func TestRenderConfigProviderExclusions(t *testing.T) {
	got := renderConfig("debug: false\n", configOptions{DisabledProviders: []string{"claude", "grok"}})
	want := "debug: false\n\n# Provider exclusions (auto-added by DroidProxy)\noauth-excluded-models:\n  claude:\n    - \"*\"\n  grok:\n    - \"*\"\n"
	if got != want {
		t.Errorf("exclusions block mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestRenderConfigAppendsMetaBlockLast(t *testing.T) {
	got := renderConfig("debug: false\n", configOptions{
		DisabledProviders: []string{"kimi"},
		MetaBlock:         "\n# meta block\n",
	})
	want := "debug: false\n\n# Provider exclusions (auto-added by DroidProxy)\noauth-excluded-models:\n  kimi:\n    - \"*\"\n\n# meta block\n"
	if got != want {
		t.Errorf("meta block must be appended after the exclusions:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestGenerateConfigWritesMergedConfig(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	template := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(template, []byte(bundledConfig(t)), 0o644); err != nil {
		t.Fatal(err)
	}

	fixedNow := time.Unix(1760000000, 0)
	m := newManager(filepath.Join(dir, "cli-proxy-api"), template)
	m.metaAccounts = func() []meta.Account { return nil }
	m.metaEnabled = func() bool { return false }
	m.now = func() time.Time { return fixedNow }

	got := m.GenerateConfig()
	if want := paths.MergedConfigPath(); got != want {
		t.Fatalf("GenerateConfig() = %q, want %q", got, want)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	expected := renderConfig(bundledConfig(t), configOptions{
		BindAddress: prefs.DefaultBindAddress,
		MetaBlock:   meta.CompatibilityConfig(nil, false, fixedNow),
	})
	if string(data) != expected {
		t.Errorf("merged config mismatch:\ngot:  %q\nwant: %q", string(data), expected)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("merged config mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestGenerateConfigReflectsPrefsAndMeta(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	template := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(template, []byte(bundledConfig(t)), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := prefs.SetProviderEnabled("claude", false); err != nil {
		t.Fatal(err)
	}
	if err := prefs.SetProviderEnabled("grok", false); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Shared().Set(prefs.KeySequentialAccountFailover, true); err != nil {
		t.Fatal(err)
	}

	fixedNow := time.Unix(1760000000, 0)
	accounts := []meta.Account{{
		ID:    "acct-1",
		Email: "a@b.com",
		Credentials: meta.Credentials{
			IdentityToken:   "identity",
			APIKey:          "muse-key",
			APIKeyExpiresAt: float64(fixedNow.Add(time.Hour).Unix()),
		},
	}}
	m := newManager(filepath.Join(dir, "cli-proxy-api"), template)
	m.metaAccounts = func() []meta.Account { return accounts }
	m.metaEnabled = func() bool { return true }
	m.now = func() time.Time { return fixedNow }

	data, err := os.ReadFile(m.GenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)

	if !strings.Contains(config, `strategy: "fill-first"`) {
		t.Error("failover override not applied")
	}
	for _, provider := range []string{"claude", "grok"} {
		if !strings.Contains(config, "  "+provider+":\n    - \"*\"\n") {
			t.Errorf("missing exclusion for provider %q", provider)
		}
	}
	// Sorted provider order: claude before grok.
	if strings.Index(config, "  claude:") > strings.Index(config, "  grok:") {
		t.Error("excluded providers must be sorted")
	}
	if !strings.Contains(config, "api-key: \"muse-key\"") {
		t.Error("meta compatibility block missing")
	}
	if !strings.HasSuffix(config, meta.CompatibilityConfig(accounts, true, fixedNow)) {
		t.Error("meta compatibility block must be the last section")
	}
}

func TestGenerateConfigMissingTemplateReturnsTemplatePath(t *testing.T) {
	isolateHome(t)
	m := newManager("/nonexistent/cli-proxy-api", "/nonexistent/config.yaml")
	if got := m.GenerateConfig(); got != "/nonexistent/config.yaml" {
		t.Errorf("GenerateConfig() = %q, want the template path as fallback", got)
	}
}

func TestGenerateConfigWriteFailureFallsBackToTemplate(t *testing.T) {
	isolateHome(t)
	home := os.Getenv("HOME")
	// A file where ~/.cli-proxy-api should live makes the merged-config write
	// fail; the manager must fall back to the bundled template path.
	if err := os.WriteFile(filepath.Join(home, ".cli-proxy-api"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	template := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(template, []byte("port: 8318\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newManager(filepath.Join(dir, "cli-proxy-api"), template)
	if got := m.GenerateConfig(); got != template {
		t.Errorf("GenerateConfig() = %q, want template path %q on write failure", got, template)
	}
}
