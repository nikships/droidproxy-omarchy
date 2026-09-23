package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/events"
)

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// newTestManager builds a manager whose binary is a script the test writes
// afterwards, whose config template exists, and whose orphan sweep uses fake
// pgrep/pkill so tests never kill real processes.
func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	dir := t.TempDir()
	bin := filepath.Join(dir, "cli-proxy-api")
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("port: 8318\nhost: 127.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newManager(bin, cfg)
	m.pgrepPath = filepath.Join(dir, "pgrep")
	m.pkillPath = filepath.Join(dir, "pkill")
	writeScript(t, m.pgrepPath, "exit 1")
	writeScript(t, m.pkillPath, "exit 0")
	return m, dir
}

func waitForLogLine(t *testing.T, m *Manager, substr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, line := range m.Logs() {
			if strings.Contains(line, substr) {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestRingBufferWraps(t *testing.T) {
	r := newRingBuffer(3)
	for i := 1; i <= 5; i++ {
		r.append(fmt.Sprint(i))
	}
	if got := strings.Join(r.elements(), ","); got != "3,4,5" {
		t.Errorf("elements = %q, want 3,4,5", got)
	}
	r.append("6")
	if got := strings.Join(r.elements(), ","); got != "4,5,6" {
		t.Errorf("elements = %q, want 4,5,6", got)
	}
	if got := newRingBuffer(2).elements(); got != nil {
		t.Errorf("empty ring = %v, want nil", got)
	}
}

func TestStartAndStop(t *testing.T) {
	m, dir := newTestManager(t)
	writeScript(t, filepath.Join(dir, "cli-proxy-api"), "echo backend-ready\nexec sleep 30\n")

	if !m.Start() {
		t.Fatal("Start should report the backend ready")
	}
	if !m.IsRunning() {
		t.Fatal("backend should be running")
	}
	if !waitForLogLine(t, m, "✓ Server started on port 8318", 2*time.Second) {
		t.Fatal("missing start log; got:", m.Logs())
	}
	if !waitForLogLine(t, m, "backend-ready", 2*time.Second) {
		t.Fatal("backend stdout was not captured; got:", m.Logs())
	}

	// A second Start while running returns immediately without a new process.
	started := make(chan bool, 1)
	go func() { started <- m.Start() }()
	select {
	case ok := <-started:
		if !ok {
			t.Fatal("second Start while running should succeed immediately")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Start blocked")
	}

	m.Stop()
	if m.IsRunning() {
		t.Fatal("backend should be stopped")
	}
	if !waitForLogLine(t, m, "✓ Server stopped", 2*time.Second) {
		t.Fatal("missing stop log; got:", m.Logs())
	}

	// Stopping again is safe.
	m.Stop()
}

func TestStartFailsWhenBinaryMissing(t *testing.T) {
	m, _ := newTestManager(t) // the binary script is never written
	if m.Start() {
		t.Fatal("Start should fail without a bundled binary")
	}
	if !waitForLogLine(t, m, "❌ Error: cli-proxy-api binary not found in app bundle", time.Second) {
		t.Fatal("missing binary log; got:", m.Logs())
	}
}

func TestStartFailsWhenConfigMissing(t *testing.T) {
	m, dir := newTestManager(t)
	writeScript(t, filepath.Join(dir, "cli-proxy-api"), "exec sleep 30\n")
	if err := os.Remove(filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	if m.Start() {
		t.Fatal("Start should fail without a config")
	}
	if !waitForLogLine(t, m, "❌ Error: config.yaml not found", time.Second) {
		t.Fatal("missing config log; got:", m.Logs())
	}
}

func TestStartFailsWhenBackendExitsImmediately(t *testing.T) {
	m, dir := newTestManager(t)
	writeScript(t, filepath.Join(dir, "cli-proxy-api"), "exit 3\n")

	if m.Start() {
		t.Fatal("Start should report failure when the backend dies instantly")
	}
	if m.IsRunning() {
		t.Fatal("backend must not be marked running")
	}
	if !waitForLogLine(t, m, "Server stopped with code: 3", time.Second) {
		t.Fatal("missing exit-code log; got:", m.Logs())
	}
	if !waitForLogLine(t, m, "⚠️ Server exited before becoming ready", time.Second) {
		t.Fatal("missing readiness log; got:", m.Logs())
	}
}

func TestRunAuthCommandRunningProcess(t *testing.T) {
	m, dir := newTestManager(t)
	writeScript(t, filepath.Join(dir, "cli-proxy-api"), "exec sleep 2\n")

	ok, msg := m.RunAuthCommand(ClaudeLogin)
	if !ok || msg != browserOpenedMessage {
		t.Fatalf("RunAuthCommand = (%v, %q), want (true, browser message)", ok, msg)
	}
}

func TestRunAuthCommandBrowserOpenedOutput(t *testing.T) {
	m, dir := newTestManager(t)
	writeScript(t, filepath.Join(dir, "cli-proxy-api"), "echo \"Opening browser\"\nexit 0\n")

	ok, msg := m.RunAuthCommand(ClaudeLogin)
	if !ok || msg != browserOpenedMessage {
		t.Fatalf("RunAuthCommand = (%v, %q), want (true, browser message)", ok, msg)
	}
}

func TestRunAuthCommandReturnsStderrOnFailure(t *testing.T) {
	m, dir := newTestManager(t)
	writeScript(t, filepath.Join(dir, "cli-proxy-api"), "echo boom >&2\nexit 1\n")

	ok, msg := m.RunAuthCommand(ClaudeLogin)
	if ok {
		t.Fatal("failed login should report failure")
	}
	if msg != "boom\n" {
		t.Fatalf("message = %q, want stderr output", msg)
	}
}

func TestRunAuthCommandSilentFailure(t *testing.T) {
	m, dir := newTestManager(t)
	writeScript(t, filepath.Join(dir, "cli-proxy-api"), "exit 1\n")

	ok, msg := m.RunAuthCommand(ClaudeLogin)
	if ok || msg != "Authentication process failed unexpectedly" {
		t.Fatalf("RunAuthCommand = (%v, %q), want (false, generic failure)", ok, msg)
	}
}

func TestRunAuthCommandSuccessNotifiesWatchers(t *testing.T) {
	events.Reset()
	received := make(chan struct{}, 4)
	unsubscribe := events.Subscribe(events.AuthDirectoryChanged, func() { received <- struct{}{} })
	defer unsubscribe()

	m, dir := newTestManager(t)
	writeScript(t, filepath.Join(dir, "cli-proxy-api"), "exit 0\n")

	// A login process that exits instantly with no browser output reports
	// failure to the caller (matching the macOS app), but the exit-0 path
	// still migrates seat files and notifies watchers.
	ok, _ := m.RunAuthCommand(ClaudeLogin)
	if ok {
		t.Fatal("instant exit without browser output should report failure")
	}
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("expected AuthDirectoryChanged after successful auth exit")
	}
}

func TestCodexLoginNudgeSendsNewline(t *testing.T) {
	old := codexNudgeDelay
	codexNudgeDelay = 200 * time.Millisecond
	t.Cleanup(func() { codexNudgeDelay = old })

	m, dir := newTestManager(t)
	stdinLog := filepath.Join(dir, "stdin.log")
	writeScript(t, filepath.Join(dir, "cli-proxy-api"),
		fmt.Sprintf("read line\necho \"$line\" > %s\nexec sleep 3\n", stdinLog))

	ok, msg := m.RunAuthCommand(CodexLogin)
	if !ok || msg != browserOpenedMessage {
		t.Fatalf("RunAuthCommand = (%v, %q), want (true, browser message)", ok, msg)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(stdinLog); err == nil && string(data) == "\n" {
			return // newline received and recorded
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("codex login nudge did not write a newline to stdin")
}

func TestKillOrphanedProcesses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	pkillLog := filepath.Join(dir, "pkill.log")
	t.Setenv("PKILL_LOG", pkillLog)

	m := newManager("/nonexistent/cli-proxy-api", "/nonexistent/config.yaml")
	m.pgrepPath = filepath.Join(dir, "pgrep")
	m.pkillPath = filepath.Join(dir, "pkill")
	writeScript(t, m.pgrepPath, "printf '123\\n456\\n'\nexit 0\n")
	writeScript(t, m.pkillPath, "echo \"$@\" >> \"$PKILL_LOG\"\nexit 0\n")

	m.killOrphanedProcesses()

	if !waitForLogLine(t, m, "⚠️ Found orphaned server process(es) [cli-proxy-api]: 123, 456", 2*time.Second) {
		t.Fatal("missing orphan log; got:", m.Logs())
	}
	if !waitForLogLine(t, m, "✓ Cleaned up orphaned processes", 2*time.Second) {
		t.Fatal("missing cleanup log; got:", m.Logs())
	}
	data, err := os.ReadFile(pkillLog)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"-9 -x cli-proxy-api", "-9 -x cli-proxy-api-plus"}
	if len(calls) != len(want) {
		t.Fatalf("pkill calls = %v, want %v", calls, want)
	}
	for i, call := range want {
		if calls[i] != call {
			t.Errorf("pkill call %d = %q, want %q", i, calls[i], call)
		}
	}
}

func TestKillOrphanedProcessesNoneFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	pkillLog := filepath.Join(dir, "pkill.log")
	t.Setenv("PKILL_LOG", pkillLog)

	m := newManager("/nonexistent/cli-proxy-api", "/nonexistent/config.yaml")
	m.pgrepPath = filepath.Join(dir, "pgrep")
	m.pkillPath = filepath.Join(dir, "pkill")
	writeScript(t, m.pgrepPath, "exit 1\n") // no processes
	writeScript(t, m.pkillPath, "echo \"$@\" >> \"$PKILL_LOG\"\nexit 0\n")

	m.killOrphanedProcesses()

	if _, err := os.Stat(pkillLog); err == nil {
		t.Error("pkill must not run when pgrep finds nothing")
	}
	for _, line := range m.Logs() {
		if strings.Contains(line, "orphaned") {
			t.Errorf("unexpected orphan log: %q", line)
		}
	}
}
