package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/backend"
	"github.com/nikships/droidproxy-omarchy/internal/control"
	"github.com/nikships/droidproxy-omarchy/internal/desktop"
	"github.com/nikships/droidproxy-omarchy/internal/events"
	"github.com/nikships/droidproxy-omarchy/internal/notify"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// fakeProxy satisfies the proxyServer seam.
type fakeProxy struct {
	running atomic.Bool
}

func (p *fakeProxy) Start() error    { p.running.Store(true); return nil }
func (p *fakeProxy) Stop()           { p.running.Store(false) }
func (p *fakeProxy) IsRunning() bool { return p.running.Load() }

// fakeDesktop records every desktop interaction instead of touching the
// real session: systemctl state, clipboard writes, and xdg-open launches.
type fakeDesktop struct {
	mu        sync.Mutex
	enabled   bool
	runs      []string
	detached  []string
	clipboard []string
}

func (f *fakeDesktop) Run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, name+" "+strings.Join(args, " "))
	if name == "systemctl" {
		// desktop.Systemctl prepends --user (and --no-block for restarts).
		rest := args
		for len(rest) > 0 && (rest[0] == "--user" || rest[0] == "--no-block") {
			rest = rest[1:]
		}
		if len(rest) >= 2 && rest[0] == "is-enabled" {
			if f.enabled {
				return "enabled\n", nil
			}
			return "disabled\n", errors.New("disabled")
		}
		if len(rest) >= 1 {
			switch rest[0] {
			case "enable":
				f.enabled = true
			case "disable":
				f.enabled = false
			}
		}
		return "", nil
	}
	if name == "wl-copy" && len(args) >= 1 {
		f.clipboard = append(f.clipboard, args[0])
	}
	return "", nil
}

func (f *fakeDesktop) LookPath(name string) (string, error) {
	switch name {
	case "wl-copy", "xdg-open", "systemctl", "notify-send":
		return "/usr/bin/" + name, nil
	}
	return "", errors.New("not found: " + name)
}

func (f *fakeDesktop) StartDetached(name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detached = append(f.detached, name+" "+strings.Join(args, " "))
	return nil
}

func (f *fakeDesktop) detachedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.detached...)
}

func (f *fakeDesktop) clipboardTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.clipboard...)
}

func (f *fakeDesktop) runCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runs...)
}

// startTestDaemon boots a full daemon against temp HOME/XDG dirs, a fake
// proxy, a fake bundled backend, a fake desktop runner, and a local update
// feed. Nothing real is touched.
func startTestDaemon(t *testing.T) (*control.Client, *fakeDesktop) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	runtimeDir := filepath.Join(home, ".run")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	socket := filepath.Join(runtimeDir, "control.sock")
	t.Setenv("DROIDPROXY_SOCKET", socket)

	// Resource root with a fake bundled backend, config, and version stamp.
	root := t.TempDir()
	template, err := os.ReadFile(filepath.Join("..", "..", "packaging", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.EnsureDir(filepath.Join(root, "libexec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := paths.EnsureDir(filepath.Join(root, "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "libexec", "cli-proxy-api"), []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "share", "config.yaml"), template, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "share", "cliproxyapi-version"), []byte("7.3.13-test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROIDPROXY_ROOT", root)

	// The feed reports the test build's own version, so checks end "up to
	// date" without touching GitHub or offering an install.
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":    "0.0.0-dev",
			"tag":        "v0.0.0-dev",
			"notes":      "test feed",
			"releaseUrl": "https://example.com/release",
			"assets":     map[string]any{},
		})
	}))
	t.Cleanup(feed.Close)
	t.Setenv("DROIDPROXY_UPDATE_FEED", feed.URL)

	// Neutralize real desktop interactions.
	notify.Disabled.Store(true)
	t.Cleanup(func() { notify.Disabled.Store(false) })
	desk := &fakeDesktop{enabled: true}
	oldRunner := desktop.Default
	desktop.Default = desk
	t.Cleanup(func() { desktop.Default = oldRunner })
	oldPgrep, oldPkill := backend.PgrepPath, backend.PkillPath
	backend.PgrepPath, backend.PkillPath = "/bin/true", "/bin/true"
	t.Cleanup(func() { backend.PgrepPath, backend.PkillPath = oldPgrep, oldPkill })
	oldFactory := proxyFactory
	proxyFactory = func() proxyServer { return &fakeProxy{} }
	t.Cleanup(func() { proxyFactory = oldFactory })
	events.Reset()
	t.Cleanup(events.Reset)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop within 15s")
		}
	})

	client := control.NewClient(socket)
	waitFor(t, 15*time.Second, func() bool {
		_, err := client.State()
		return err == nil
	}, "daemon control API to come up")
	return client, desk
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func stateWhen(t *testing.T, client *control.Client, timeout time.Duration, cond func(control.State) bool, what string) control.State {
	t.Helper()
	var last control.State
	waitFor(t, timeout, func() bool {
		st, err := client.State()
		if err != nil {
			return false
		}
		last = st
		return cond(st)
	}, what)
	return last
}

func callOK(t *testing.T, client *control.Client, method string, params any) control.CallResult {
	t.Helper()
	res, err := client.Call(context.Background(), method, params)
	if err != nil {
		t.Fatalf("call %s: %v", method, err)
	}
	return res
}

func TestDaemonStateBasics(t *testing.T) {
	client, _ := startTestDaemon(t)

	st := stateWhen(t, client, 15*time.Second, func(s control.State) bool { return s.Server.Running },
		"servers to start")

	if st.App.Name != "DroidProxy" || st.App.Version == "" {
		t.Errorf("app info = %+v", st.App)
	}
	if st.App.CLIProxyAPIVersion != "7.3.13-test" {
		t.Errorf("CLIProxyAPIVersion = %q", st.App.CLIProxyAPIVersion)
	}
	if st.Server.ProxyPort != 8317 || st.Server.BackendPort != 8318 {
		t.Errorf("ports = %d/%d", st.Server.ProxyPort, st.Server.BackendPort)
	}
	if st.Server.URL != "http://127.0.0.1:8317" {
		t.Errorf("url = %q", st.Server.URL)
	}
	if st.Server.DashboardURL != "http://127.0.0.1:8318/management.html" {
		t.Errorf("dashboardUrl = %q", st.Server.DashboardURL)
	}
	if st.Server.LastError != "" {
		t.Errorf("lastError = %q", st.Server.LastError)
	}
	if !st.Settings.LaunchAtLogin {
		t.Error("launchAtLogin should reflect the enabled unit")
	}
	if st.Settings.BackgroundOpacity != 0.55 {
		t.Errorf("backgroundOpacity = %v", st.Settings.BackgroundOpacity)
	}
	if !st.Settings.AutoCheckUpdates {
		t.Error("autoCheckUpdates should default on")
	}
	wantProviders := []string{"claude", "codex", "meta", "antigravity", "kimi", "junie", "grok"}
	if len(st.Providers) != len(wantProviders) {
		t.Fatalf("providers = %d, want %d", len(st.Providers), len(wantProviders))
	}
	for i, want := range wantProviders {
		if st.Providers[i].ID != want {
			t.Errorf("provider %d = %q, want %q", i, st.Providers[i].ID, want)
		}
		if !st.Providers[i].Enabled {
			t.Errorf("provider %q should default enabled", want)
		}
	}
	if !st.Usage.Visible {
		t.Error("usage should be visible with codex/claude enabled")
	}
	if st.Paths.AuthDir != filepath.Join(os.Getenv("HOME"), ".cli-proxy-api") {
		t.Errorf("authDir = %q", st.Paths.AuthDir)
	}
}

func TestDaemonServerToggle(t *testing.T) {
	client, _ := startTestDaemon(t)
	stateWhen(t, client, 15*time.Second, func(s control.State) bool { return s.Server.Running },
		"servers to start")

	if res := callOK(t, client, "server.stop", nil); !res.OK {
		t.Fatalf("server.stop = %+v", res)
	}
	stateWhen(t, client, 10*time.Second, func(s control.State) bool { return !s.Server.Running },
		"servers to stop")

	if res := callOK(t, client, "server.start", nil); !res.OK {
		t.Fatalf("server.start = %+v", res)
	}
	stateWhen(t, client, 15*time.Second, func(s control.State) bool { return s.Server.Running },
		"servers to restart")
}

func TestDaemonSettingsAndDesktopActions(t *testing.T) {
	client, desk := startTestDaemon(t)

	if res := callOK(t, client, "settings.set", map[string]any{"key": "oledTheme", "value": true}); !res.OK {
		t.Fatalf("settings.set oledTheme = %+v", res)
	}
	if st, _ := client.State(); !st.Settings.OLEDTheme {
		t.Error("oledTheme did not persist")
	}

	if res := callOK(t, client, "settings.set", map[string]any{"key": "launchAtLogin", "value": false}); !res.OK {
		t.Fatalf("settings.set launchAtLogin = %+v", res)
	}
	found := false
	for _, call := range desk.runCalls() {
		if strings.Contains(call, "systemctl --user disable droidproxy.service") {
			found = true
		}
	}
	if !found {
		t.Errorf("systemctl disable not recorded; calls: %v", desk.runCalls())
	}
	if st, _ := client.State(); st.Settings.LaunchAtLogin {
		t.Error("launchAtLogin should now read disabled")
	}

	if res := callOK(t, client, "clipboard.copy", map[string]any{"text": "hello world"}); !res.OK {
		t.Fatalf("clipboard.copy = %+v", res)
	}
	if texts := desk.clipboardTexts(); len(texts) == 0 || texts[len(texts)-1] != "hello world" {
		t.Errorf("clipboard = %v", texts)
	}

	if res := callOK(t, client, "open.url", map[string]any{"url": "https://github.com/router-for-me/CLIProxyAPI"}); !res.OK {
		t.Fatalf("open.url = %+v", res)
	}
	opened := false
	for _, call := range desk.detachedCalls() {
		if strings.Contains(call, "xdg-open https://github.com/router-for-me/CLIProxyAPI") {
			opened = true
		}
	}
	if !opened {
		t.Errorf("xdg-open not recorded; calls: %v", desk.detachedCalls())
	}

	if res := callOK(t, client, "open.url", map[string]any{"url": "file:///etc/passwd"}); res.OK {
		t.Error("open.url must reject non-http(s) URLs")
	}
	if res := callOK(t, client, "settings.set", map[string]any{"key": "nonsense", "value": true}); res.OK {
		t.Error("unknown setting must fail")
	}
	if res := callOK(t, client, "no.such.method", nil); res.OK {
		t.Error("unknown method must fail")
	}
}

func TestDaemonJunieAndAccountActions(t *testing.T) {
	client, _ := startTestDaemon(t)

	res := callOK(t, client, "junie.saveKey", map[string]any{"apiKey": "junie-key-123"})
	if !res.OK || res.Message != "✓ Successfully saved Junie API Key." {
		t.Fatalf("junie.saveKey = %+v", res)
	}
	st := stateWhen(t, client, 10*time.Second, func(s control.State) bool {
		for _, p := range s.Providers {
			if p.ID == "junie" {
				return len(p.Accounts) == 1
			}
		}
		return false
	}, "junie account to appear")
	for _, p := range st.Providers {
		if p.ID == "junie" && (p.Accounts[0].DisplayName != "junie-user" || p.Accounts[0].Email != "junie-user") {
			t.Errorf("junie account = %+v", p.Accounts[0])
		}
	}

	// The last enabled account cannot be disabled.
	res = callOK(t, client, "account.toggleDisabled", map[string]any{"provider": "junie", "accountId": "junie.json"})
	if res.OK || res.Error != "Failed to update junie-user. Please try again." {
		t.Fatalf("toggleDisabled = %+v", res)
	}

	// Removing restarts the running server around the delete.
	if res := callOK(t, client, "account.remove", map[string]any{"provider": "junie", "accountId": "junie.json"}); !res.OK {
		t.Fatalf("account.remove = %+v", res)
	}
	stateWhen(t, client, 15*time.Second, func(s control.State) bool {
		junieEmpty := true
		for _, p := range s.Providers {
			if p.ID == "junie" && len(p.Accounts) > 0 {
				junieEmpty = false
			}
		}
		return junieEmpty && s.Server.Running
	}, "junie account removal and server restart")
}

func TestDaemonFactoryApply(t *testing.T) {
	client, _ := startTestDaemon(t)

	res := callOK(t, client, "factory.apply", nil)
	if !res.OK || !strings.Contains(res.Message, "DroidProxy models merged into Factory settings") {
		t.Fatalf("factory.apply = %+v", res)
	}
	st := stateWhen(t, client, 10*time.Second, func(s control.State) bool { return s.Factory.ModelsInstalled },
		"factory models to register as installed")
	if st.Factory.ModelsInstalled != true {
		t.Fatal("unreachable")
	}
	if _, err := os.Stat(paths.FactorySettingsPath()); err != nil {
		t.Errorf("factory settings file: %v", err)
	}
}

func TestDaemonBundledLoginFlow(t *testing.T) {
	client, _ := startTestDaemon(t)

	// The fake backend runs long enough to look alive at the 1s check, so
	// the login returns the macOS "browser opened" alert text.
	res := callOK(t, client, "provider.connect", map[string]any{"provider": "claude"})
	if !res.OK || !strings.Contains(res.Message, "🌐 Browser opened for Claude Code authentication.") {
		t.Fatalf("provider.connect claude = %+v", res)
	}

	if res := callOK(t, client, "provider.connect", map[string]any{"provider": "copilot"}); res.OK {
		t.Error("copilot must be rejected on Linux")
	}
	if res := callOK(t, client, "provider.connect", map[string]any{"provider": "junie"}); res.OK {
		t.Error("junie connect must point at junie.saveKey")
	}
	if res := callOK(t, client, "provider.connect", map[string]any{"provider": "nope"}); res.OK {
		t.Error("unknown provider must fail")
	}
}

func TestDaemonUpdateCheck(t *testing.T) {
	client, _ := startTestDaemon(t)

	if res := callOK(t, client, "update.check", nil); !res.OK {
		t.Fatalf("update.check = %+v", res)
	}
	stateWhen(t, client, 15*time.Second, func(s control.State) bool { return s.Update.State == control.UpdateUpToDate },
		"update check to finish")
}
