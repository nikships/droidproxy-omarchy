package installer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nikships/droidproxy-omarchy/internal/buildinfo"
	"github.com/nikships/droidproxy-omarchy/internal/desktop"
)

// fakeRunner records executed commands; file operations are real.
type fakeRunner struct {
	runs []string
	outs map[string]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	return f.run(name, args...)
}

func (f *fakeRunner) run(name string, args ...string) (string, error) {
	f.runs = append(f.runs, name+" "+strings.Join(args, " "))
	key := name + " " + strings.Join(args, " ")
	if out, ok := f.outs[key]; ok {
		return out, nil
	}
	return "ok", nil
}

func (f *fakeRunner) LookPath(name string) (string, error) { return "/usr/bin/" + name, nil }

func (f *fakeRunner) StartDetached(name string, args ...string) error {
	_, err := f.run(name, args...)
	return err
}

// tempHome isolates every derived path from the real session environment
// (the session exports XDG_CONFIG_HOME etc., which otherwise win over HOME).
func tempHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
}

func withFakeRunner(t *testing.T) *fakeRunner {
	t.Helper()
	f := &fakeRunner{}
	old := desktop.Default
	desktop.Default = f
	t.Cleanup(func() { desktop.Default = old })
	return f
}

// buildReleaseTree creates a minimal release tree the way the tarball looks
// when extracted.
func buildReleaseTree(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel string, mode os.FileMode, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("bin/droidproxy", 0o755, "#!/bin/sh\nexit 0\n")
	write("libexec/cli-proxy-api", 0o755, "binary")
	write("share/config.yaml", 0o644, "config")
	write("share/plugin/manifest.json", 0o644, `{"id":"`+buildinfo.PluginID+`"}`)
	write("share/plugin/Service.qml", 0o644, "// qml")
	write("share/systemd/"+unitName, 0o644, "[Unit]\nDescription=test\n")
	write("share/applications/"+desktopFileName, 0o644, "[Desktop Entry]\n")
	write("share/icons/hicolor/512x512/apps/droidproxy.png", 0o644, "png")
	write("share/icons/hicolor/64x64/apps/droidproxy.png", 0o644, "png")
	write("VERSION", 0o644, version+"\n")
	return root
}

func TestInstallFresh(t *testing.T) {
	tempHome(t)
	f := withFakeRunner(t)
	src := buildReleaseTree(t, "1.2.3")

	if err := Install(Options{From: src}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Version dir with the tree copied in.
	dest := filepath.Join(os.Getenv("HOME"), ".local/share/droidproxy/versions/1.2.3")
	for _, rel := range []string{"bin/droidproxy", "libexec/cli-proxy-api", "share/config.yaml", "VERSION"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("missing %s in version dir: %v", rel, err)
		}
	}
	// current symlink flips to the new version.
	link, err := os.Readlink(filepath.Join(os.Getenv("HOME"), ".local/share/droidproxy/current"))
	if err != nil || link != "versions/1.2.3" {
		t.Errorf("current link = %q, err %v", link, err)
	}
	// ~/.local/bin link.
	binLink, err := os.Readlink(filepath.Join(os.Getenv("HOME"), ".local/bin/droidproxy"))
	if err != nil || !strings.HasSuffix(binLink, "/current/bin/droidproxy") {
		t.Errorf("bin link = %q, err %v", binLink, err)
	}
	// unit, desktop entry, icons.
	for _, p := range []string{
		filepath.Join(os.Getenv("HOME"), ".config/systemd/user/"+unitName),
		filepath.Join(os.Getenv("HOME"), ".local/share/applications/"+desktopFileName),
		filepath.Join(os.Getenv("HOME"), ".local/share/icons/hicolor/512x512/apps/droidproxy.png"),
		filepath.Join(os.Getenv("HOME"), ".local/share/icons/hicolor/64x64/apps/droidproxy.png"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	// plugin lands under its id, with no symlinks.
	plugin := filepath.Join(os.Getenv("HOME"), ".config/omarchy/plugins", buildinfo.PluginID)
	if _, err := os.Stat(filepath.Join(plugin, "manifest.json")); err != nil {
		t.Errorf("plugin not installed: %v", err)
	}
	// omarchy commands: enable on fresh install.
	joined := strings.Join(f.runs, "\n")
	if !strings.Contains(joined, "omarchy plugin enable "+buildinfo.PluginID) {
		t.Errorf("expected omarchy plugin enable, runs:\n%s", joined)
	}
	if !strings.Contains(joined, "systemctl --user enable droidproxy.service") ||
		!strings.Contains(joined, "systemctl --user start droidproxy.service") {
		t.Errorf("expected enable/start, runs:\n%s", joined)
	}
}

func TestInstallUpdateDoesNotEnable(t *testing.T) {
	tempHome(t)
	f := withFakeRunner(t)
	src := buildReleaseTree(t, "1.2.3")

	// Make the service look running so the update restarts it.
	f.outs = map[string]string{
		"systemctl --user is-active droidproxy.service": "active",
	}

	if err := Install(Options{From: src, Update: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	joined := strings.Join(f.runs, "\n")
	if strings.Contains(joined, "systemctl --user enable") || strings.Contains(joined, "systemctl --user start ") {
		t.Errorf("update install must not enable/start:\n%s", joined)
	}
	if !strings.Contains(joined, "restart") {
		t.Errorf("update install should restart a running service:\n%s", joined)
	}
}

func TestInstallIsIdempotentOverExistingVersion(t *testing.T) {
	tempHome(t)
	withFakeRunner(t)
	src := buildReleaseTree(t, "1.2.3")

	if err := Install(Options{From: src}); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if err := Install(Options{From: src}); err != nil {
		t.Fatalf("second Install: %v", err)
	}
}

func TestInstallRefusesTraversalVersion(t *testing.T) {
	tempHome(t)
	withFakeRunner(t)
	src := buildReleaseTree(t, "../../evil")
	if err := Install(Options{From: src}); err == nil {
		t.Fatal("expected traversal VERSION to be refused")
	}
}

func TestUninstallKeepsDataWithoutPurge(t *testing.T) {
	tempHome(t)
	f := withFakeRunner(t)

	src := buildReleaseTree(t, "1.2.3")
	if err := Install(Options{From: src}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Create the data dirs that must survive a non-purge uninstall.
	for _, dir := range []string{
		filepath.Join(os.Getenv("HOME"), ".config/droidproxy"),
		filepath.Join(os.Getenv("HOME"), ".droidproxy"),
		filepath.Join(os.Getenv("HOME"), ".cli-proxy-api"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := Uninstall(false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".local/share/droidproxy")); !os.IsNotExist(err) {
		t.Error("install root still exists")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".local/bin/droidproxy")); !os.IsNotExist(err) {
		t.Error("bin link still exists")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config/systemd/user/"+unitName)); !os.IsNotExist(err) {
		t.Error("unit file still exists")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config/omarchy/plugins", buildinfo.PluginID)); !os.IsNotExist(err) {
		t.Error("plugin still exists")
	}
	for _, dir := range []string{
		filepath.Join(os.Getenv("HOME"), ".config/droidproxy"),
		filepath.Join(os.Getenv("HOME"), ".droidproxy"),
		filepath.Join(os.Getenv("HOME"), ".cli-proxy-api"),
	} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("data dir %s should have been kept: %v", dir, err)
		}
	}
	joined := strings.Join(f.runs, "\n")
	if !strings.Contains(joined, "omarchy plugin disable "+buildinfo.PluginID) {
		t.Errorf("expected plugin disable, runs:\n%s", joined)
	}
}

func TestUninstallPurgeRemovesData(t *testing.T) {
	tempHome(t)
	withFakeRunner(t)

	for _, dir := range []string{
		filepath.Join(os.Getenv("HOME"), ".config/droidproxy"),
		filepath.Join(os.Getenv("HOME"), ".droidproxy"),
		filepath.Join(os.Getenv("HOME"), ".cli-proxy-api"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := Uninstall(true); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	for _, dir := range []string{
		filepath.Join(os.Getenv("HOME"), ".config/droidproxy"),
		filepath.Join(os.Getenv("HOME"), ".droidproxy"),
		filepath.Join(os.Getenv("HOME"), ".cli-proxy-api"),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s should have been purged", dir)
		}
	}
}

func TestRollback(t *testing.T) {
	tempHome(t)
	f := withFakeRunner(t)
	f.outs = map[string]string{
		"systemctl --user is-active droidproxy.service": "active",
	}

	// "Install" two versions by hand.
	versions := filepath.Join(os.Getenv("HOME"), ".local/share/droidproxy/versions")
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if err := os.MkdirAll(filepath.Join(versions, v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := flipCurrent("1.1.0"); err != nil {
		t.Fatal(err)
	}

	if err := Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	link, _ := os.Readlink(filepath.Join(os.Getenv("HOME"), ".local/share/droidproxy/current"))
	if link != "versions/1.0.0" {
		t.Errorf("current link = %q, want versions/1.0.0", link)
	}
	if !strings.Contains(strings.Join(f.runs, "\n"), "restart") {
		t.Error("rollback should restart the running service")
	}
}

func TestRollbackWithoutPrevious(t *testing.T) {
	tempHome(t)
	withFakeRunner(t)
	if err := flipCurrent("1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(); err == nil {
		t.Fatal("expected error with no previous version")
	}
}
