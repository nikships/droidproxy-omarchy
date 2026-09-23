package notify

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nikships/droidproxy-omarchy/internal/buildinfo"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

func withFallbacks(t *testing.T, lookErr error, startErr error) *[]string {
	t.Helper()
	t.Cleanup(func() {
		lookPath = origLook
		startDetached = origStart
	})
	var calls []string
	var mu sync.Mutex
	lookPath = func(name string) (string, error) {
		if lookErr != nil {
			return "", lookErr
		}
		return "/usr/bin/" + name, nil
	}
	startDetached = func(path string, args ...string) error {
		mu.Lock()
		calls = append(calls, path+" "+args[0])
		mu.Unlock()
		return startErr
	}
	return &calls
}

var (
	origLook  = lookPath
	origStart = startDetached
)

func TestNotifyWithoutBusFallsBackToSend(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	// Make SessionBus fail: an unusable address string does that.
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/droidproxy-test-bus")

	calls := withFallbacks(t, nil, nil)
	id, err := Notify("Title", "Body", Options{})
	if err != nil {
		t.Fatalf("Notify should not error when notify-send exists: %v", err)
	}
	if id != 0 {
		t.Errorf("fallback id = %d, want 0", id)
	}
	if len(*calls) != 1 || (*calls)[0] != "/usr/bin/notify-send Title" {
		t.Fatalf("expected notify-send call, got %v", *calls)
	}
}

func TestNotifyWithoutBusOrNotifySendIsNotFatal(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/droidproxy-test-bus")
	withFallbacks(t, errors.New("missing"), nil)
	_, err := Notify("Title", "Body", Options{})
	if err == nil {
		t.Fatal("expected an error when neither bus nor notify-send exist")
	}
}

func TestAppIconPrefersInstalledName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// No installed icons anywhere; expect the bare name as the last resort.
	icon := AppIcon()
	if icon != "droidproxy" {
		t.Errorf("AppIcon = %q, want %q", icon, "droidproxy")
	}

	// With an installed hicolor icon, the name is used.
	iconsDir := filepath.Join(paths.Home(), ".local/share/icons/hicolor/512x512/apps")
	if err := os.MkdirAll(iconsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iconsDir, "droidproxy.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if icon := AppIcon(); icon != "droidproxy" {
		t.Errorf("AppIcon = %q, want %q with installed icon", icon, "droidproxy")
	}
}

func TestAppName(t *testing.T) {
	if buildinfo.AppName != "DroidProxy" {
		t.Fatalf("app name = %q", buildinfo.AppName)
	}
}
