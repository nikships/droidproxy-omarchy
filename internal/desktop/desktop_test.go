package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunner records every call instead of executing it.
type fakeRunner struct {
	lookable map[string]bool
	runs     []runCall
	starts   []runCall
	failRun  map[string]error // keyed by "name arg1 arg2"
	outs     map[string]string
}

type runCall struct {
	name string
	args []string
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.runs = append(f.runs, runCall{name, args})
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if err, ok := f.failRun[key]; ok {
		return "", err
	}
	if out, ok := f.outs[key]; ok {
		return out, nil
	}
	return "ok", nil
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	if f.lookable[name] {
		return "/usr/bin/" + name, nil
	}
	return "", errors.New("not found")
}

func (f *fakeRunner) StartDetached(name string, args ...string) error {
	f.starts = append(f.starts, runCall{name, args})
	return nil
}

func withRunner(t *testing.T, r Runner) *fakeRunner {
	t.Helper()
	old := Default
	Default = r
	t.Cleanup(func() { Default = old })
	return r.(*fakeRunner)
}

func TestOpenPathUsesXDGOpen(t *testing.T) {
	f := withRunner(t, &fakeRunner{})
	if err := OpenURL("https://example.com"); err != nil {
		t.Fatalf("OpenURL: %v", err)
	}
	if len(f.starts) != 1 || f.starts[0].name != "xdg-open" || f.starts[0].args[0] != "https://example.com" {
		t.Fatalf("unexpected starts: %+v", f.starts)
	}
	if err := OpenPath(""); err == nil {
		t.Error("empty path should error")
	}
}

func TestCopyToClipboard(t *testing.T) {
	f := withRunner(t, &fakeRunner{lookable: map[string]bool{"wl-copy": true}})
	if err := CopyToClipboard("secret"); err != nil {
		t.Fatalf("CopyToClipboard: %v", err)
	}
	if f.runs[0].name != "wl-copy" || f.runs[0].args[0] != "secret" {
		t.Fatalf("unexpected run: %+v", f.runs[0])
	}

	// Falls back to xclip when wl-copy is missing.
	f2 := withRunner(t, &fakeRunner{lookable: map[string]bool{"xclip": true}})
	if err := CopyToClipboard("secret"); err != nil {
		t.Fatalf("CopyToClipboard: %v", err)
	}
	if f2.runs[0].name != "xclip" {
		t.Fatalf("expected xclip fallback, got %+v", f2.runs[0])
	}

	// No tool at all.
	f3 := withRunner(t, &fakeRunner{})
	if err := CopyToClipboard("secret"); err == nil {
		t.Error("expected error without clipboard tools")
	}
	_ = f3
}

func TestSystemdHelpers(t *testing.T) {
	f := withRunner(t, &fakeRunner{outs: map[string]string{
		"systemctl --user is-enabled droidproxy.service": "enabled",
		"systemctl --user is-active droidproxy.service":  "active",
	}})

	if !IsEnabled() {
		t.Error("expected enabled by default fake output")
	}
	if !IsRunning() {
		t.Error("expected active by default fake output")
	}
	if err := Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if err := DaemonReload(); err != nil {
		t.Fatalf("DaemonReload: %v", err)
	}

	var sawNoBlock bool
	for _, r := range f.runs {
		if r.name == "systemctl" && strings.Join(r.args, " ") == "--user --no-block restart droidproxy.service" {
			sawNoBlock = true
		}
	}
	if !sawNoBlock {
		t.Errorf("restart did not use --no-block: %+v", f.runs)
	}

	// Non-enabled output flips IsEnabled.
	f.failRun = map[string]error{}
	withRunner(t, &fakeRunner{
		failRun: map[string]error{"systemctl --user is-enabled droidproxy.service": errors.New("disabled")},
	})
	if IsEnabled() {
		t.Error("IsEnabled should be false when is-enabled fails")
	}
}

func TestUnderSystemd(t *testing.T) {
	f := withRunner(t, &fakeRunner{failRun: map[string]error{"systemctl --user is-system-running": errors.New("no manager")}})
	if UnderSystemd() {
		t.Error("UnderSystemd should be false when the user manager is unreachable")
	}
	_ = f

	t.Setenv("INVOCATION_ID", "abc")
	withRunner(t, &fakeRunner{failRun: map[string]error{"systemctl --user is-system-running": errors.New("no manager")}})
	if !UnderSystemd() {
		t.Error("INVOCATION_ID should make UnderSystemd true")
	}
}

func TestShellIPCIgnoresMissingBinary(t *testing.T) {
	withRunner(t, &fakeRunner{}) // nothing lookable
	if err := ShellIPC("droidproxy", "openSettings"); err != nil {
		t.Fatalf("ShellIPC with missing binary should be ignored, got %v", err)
	}
}

func TestShellIPCSendsCommand(t *testing.T) {
	f := withRunner(t, &fakeRunner{lookable: map[string]bool{"omarchy-shell": true}})
	if err := DroidProxyIPC("toggleMenu"); err != nil {
		t.Fatalf("DroidProxyIPC: %v", err)
	}
	if len(f.runs) != 1 || f.runs[0].name != "omarchy-shell" {
		t.Fatalf("unexpected runs: %+v", f.runs)
	}
	got := f.runs[0].args
	if got[0] != "droidproxy" || got[1] != "toggleMenu" {
		t.Fatalf("unexpected args: %v", got)
	}
}

func TestDetachedChild(t *testing.T) {
	// StartDetached for real, into a temp dir, with a binary that exits
	// immediately: proves the Setsid attribute is accepted.
	dir := t.TempDir()
	script := filepath.Join(dir, "noop")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (ExecRunner{}).StartDetached(script); err != nil {
		t.Fatalf("StartDetached: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
}
