// Package backend manages the bundled CLIProxyAPI child process (the macOS
// app's ServerManager): process lifecycle, streaming logs, merged-config
// generation, provider enable/disable, and the login flows the bundled binary
// provides.
package backend

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/claude"
	"github.com/nikships/droidproxy-omarchy/internal/events"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/meta"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

const (
	readinessCheckDelay        = 1 * time.Second
	gracefulTerminationTimeout = 2 * time.Second
	maxLogLines                = 1000

	authLivenessCheckDelay = 1 * time.Second
	authMigrationDelay     = 500 * time.Millisecond
)

// codexNudgeDelay matches the macOS app: after ~12s, send a newline so the
// Codex login's manual-callback prompt doesn't block forever. Var so tests
// can shorten it.
var codexNudgeDelay = 12 * time.Second

// orphanProcessNames are swept before every start. `cli-proxy-api` is the
// current bundled backend; `cli-proxy-api-plus` is the legacy name, swept so
// an orphan left over from a pre-migration build can't keep holding port 8318.
var orphanProcessNames = []string{"cli-proxy-api", "cli-proxy-api-plus"}

// PgrepPath and PkillPath locate the orphan-sweep tools. Vars so the daemon
// test harness can neutralize the sweep against real processes.
var (
	PgrepPath = "/usr/bin/pgrep"
	PkillPath = "/usr/bin/pkill"
)

// browserOpenedMessage is returned while a login process is still running and
// the browser is expected to have opened.
const browserOpenedMessage = "A browser window opened for authentication.\n\nComplete the login in your browser. DroidProxy will detect when you are authenticated."

// AuthCommand is a login flow run by the bundled binary.
type AuthCommand string

const (
	ClaudeLogin      AuthCommand = "claude"
	CodexLogin       AuthCommand = "codex"
	AntigravityLogin AuthCommand = "antigravity"
	KimiLogin        AuthCommand = "kimi"
)

// LoginFlag is the CLI flag passed to the bundled binary for this flow.
func (c AuthCommand) LoginFlag() string {
	switch c {
	case ClaudeLogin:
		return "-claude-login"
	case CodexLogin:
		return "-codex-login"
	case AntigravityLogin:
		return "-antigravity-login"
	case KimiLogin:
		return "-kimi-login"
	}
	return ""
}

// Manager owns the CLIProxyAPI child process.
type Manager struct {
	mu sync.Mutex

	binaryPath     string // bundled cli-proxy-api
	configTemplate string // bundled (unmerged) config.yaml

	// Injectable seams for tests.
	metaAccounts func() []meta.Account
	metaEnabled  func() bool
	now          func() time.Time
	pgrepPath    string
	pkillPath    string

	cmd     *exec.Cmd
	done    chan struct{} // closed once the backend process has fully exited
	running bool

	logMu sync.Mutex
	logs  ringBuffer
}

// New returns a Manager using the bundled binary and config from the running
// install's resource root.
func New() *Manager {
	return newManager(paths.BundledCLIProxyAPI(), paths.BundledConfigPath())
}

func newManager(binaryPath, configTemplate string) *Manager {
	return &Manager{
		binaryPath:     binaryPath,
		configTemplate: configTemplate,
		metaAccounts:   func() []meta.Account { return meta.Shared().Accounts() },
		metaEnabled:    func() bool { return prefs.IsProviderEnabled(string(auth.Meta)) },
		now:            time.Now,
		pgrepPath:      PgrepPath,
		pkillPath:      PkillPath,
		logs:           *newRingBuffer(maxLogLines),
	}
}

// IsRunning reports whether the backend process is up.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Logs returns a copy of the retained backend log lines (oldest first).
func (m *Manager) Logs() []string {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	return m.logs.elements()
}

// GenerateConfig regenerates ~/.cli-proxy-api/merged-config.yaml from the
// bundled template plus user settings and provider exclusions, and returns
// the path to use. CLIProxyAPI hot-reloads the file, so callers don't need to
// restart the backend. On a read or write failure the bundled template path is
// returned (matching the macOS app) so the server can still start.
func (m *Manager) GenerateConfig() string {
	template, err := os.ReadFile(m.configTemplate)
	if err != nil {
		return m.configTemplate
	}

	disabled := make([]string, 0, len(oauthProviderKeys))
	for serviceType, key := range oauthProviderKeys {
		if !prefs.IsProviderEnabled(string(serviceType)) {
			disabled = append(disabled, key)
		}
	}
	sort.Strings(disabled)

	metaBlock := meta.CompatibilityConfig(m.metaAccounts(), m.metaEnabled(), m.now())

	rendered := renderConfig(string(template), configOptions{
		BindAddress:        prefs.BindAddress(),
		AllowRemote:        prefs.AllowRemote(),
		SecretKey:          prefs.SecretKey(),
		VerboseLogging:     prefs.VerboseLogging(),
		SequentialFailover: prefs.SequentialAccountFailover(),
		DisabledProviders:  disabled,
		MetaBlock:          metaBlock,
	})

	merged := paths.MergedConfigPath()
	if err := writeMergedConfig(merged, rendered); err != nil {
		logx.Logf("[ServerManager] Failed to write merged config: %v", err)
		return m.configTemplate
	}
	return merged
}

func writeMergedConfig(path, content string) error {
	if err := paths.EnsureDir(paths.AuthDir(), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RegenerateConfig rewrites the merged config without extra log lines (used
// when Meta accounts change; the backend hot-reloads it).
func (m *Manager) RegenerateConfig() { m.GenerateConfig() }

// SetProviderEnabled persists the provider toggle and regenerates the merged
// config (hot reload — no restart needed).
func (m *Manager) SetProviderEnabled(serviceType auth.ServiceType, enabled bool) {
	if err := prefs.SetProviderEnabled(string(serviceType), enabled); err != nil {
		logx.Logf("[ServerManager] Failed to persist provider state: %v", err)
	}
	if enabled {
		m.addLog("✓ Enabled provider: " + serviceType.DisplayName())
	} else {
		m.addLog("⚠️ Disabled provider: " + serviceType.DisplayName())
	}
	m.GenerateConfig()
	m.addLog("Config updated (hot reload)")
}

// SetSequentialAccountFailover persists the routing toggle and regenerates
// the config. CLIProxyAPI hot-reloads routing strategy, cooldown scheduling
// and retry settings, so no restart is required.
func (m *Manager) SetSequentialAccountFailover(enabled bool) {
	if err := prefs.Shared().Set(prefs.KeySequentialAccountFailover, enabled); err != nil {
		logx.Logf("[ServerManager] Failed to persist failover setting: %v", err)
	}
	if enabled {
		m.addLog("✓ Sequential account failover enabled (fill-first, quota cooldowns on)")
	} else {
		m.addLog("⚠️ Sequential account failover disabled (round-robin, cooldowns off)")
	}
	m.GenerateConfig()
	m.addLog("Config updated (hot reload)")
}

// Start launches the backend with the merged config and reports whether it
// became ready. It blocks for the readiness window (~1s), so call it from a
// goroutine when that matters.
func (m *Manager) Start() bool {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return true
	}
	m.mu.Unlock()

	// Clean up any orphaned processes from previous crashes.
	m.killOrphanedProcesses()

	if !fileExists(m.binaryPath) {
		m.addLog("❌ Error: cli-proxy-api binary not found in app bundle")
		return false
	}
	configPath := m.GenerateConfig()
	if configPath == "" || !fileExists(configPath) {
		m.addLog("❌ Error: config.yaml not found")
		return false
	}

	// The macOS app passed "-config" here and "--config" in the auth flows;
	// the bundled binary accepts both spellings and the difference is kept.
	cmd := exec.Command(m.binaryPath, "-config", configPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.addLog("❌ Failed to start server: " + err.Error())
		return false
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.addLog("❌ Failed to start server: " + err.Error())
		return false
	}
	if err := cmd.Start(); err != nil {
		m.addLog("❌ Failed to start server: " + err.Error())
		return false
	}

	done := make(chan struct{})
	m.mu.Lock()
	m.cmd = cmd
	m.done = done
	m.running = true
	m.mu.Unlock()

	// The macOS app logged the user-facing proxy port (8317) here even though
	// this process binds 8318; the port this manager owns is 8318.
	m.addLog(fmt.Sprintf("✓ Server started on port %d", BackendPort))
	events.Publish(events.ServerStatusChanged)

	go m.streamLogs(stdout, "")
	go m.streamLogs(stderr, "⚠️ ")
	go m.waitBackend(cmd, done)

	// Give the backend a moment to actually bind before reporting success.
	select {
	case <-done:
		m.addLog("⚠️ Server exited before becoming ready")
		return false
	case <-time.After(readinessCheckDelay):
		return true
	}
}

// waitBackend reaps the backend process and publishes the state change.
func (m *Manager) waitBackend(cmd *exec.Cmd, done chan struct{}) {
	err := cmd.Wait()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}
	m.mu.Lock()
	if m.cmd == cmd {
		m.cmd = nil
	}
	m.running = false
	m.mu.Unlock()
	m.addLog(fmt.Sprintf("Server stopped with code: %d", code))
	close(done)
	events.Publish(events.ServerStatusChanged)
}

func (m *Manager) streamLogs(r io.Reader, prefix string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		m.addLog(prefix + scanner.Text())
	}
}

// Stop terminates the backend: SIGTERM first, then SIGKILL after the graceful
// window. Safe to call when not running.
func (m *Manager) Stop() {
	m.mu.Lock()
	cmd := m.cmd
	done := m.done
	if cmd == nil || done == nil {
		m.running = false
		m.mu.Unlock()
		events.Publish(events.ServerStatusChanged)
		return
	}
	m.mu.Unlock()

	m.addLog(fmt.Sprintf("Stopping server (PID: %d)...", cmd.Process.Pid))
	_ = cmd.Process.Signal(syscall.SIGTERM)

	select {
	case <-done:
	case <-time.After(gracefulTerminationTimeout):
		m.addLog("⚠️ Server didn't stop gracefully, force killing...")
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	m.addLog("✓ Server stopped")
	events.Publish(events.ServerStatusChanged)
}

// killOrphanedProcesses sweeps for orphaned backend processes that might be
// holding port 8318 from a previous crash.
func (m *Manager) killOrphanedProcesses() {
	for _, name := range orphanProcessNames {
		m.killOrphanedProcessesNamed(name)
	}
}

func (m *Manager) killOrphanedProcessesNamed(processName string) {
	// Match the exact process name (not -f against the full command line):
	// the backend's config path is ~/.cli-proxy-api/merged-config.yaml, so a
	// `-f cli-proxy-api` pattern would also match unrelated processes that
	// merely reference that directory.
	checkTask := exec.Command(m.pgrepPath, "-x", processName)
	output, err := checkTask.Output() // stderr suppressed; not critical
	if err != nil {
		// Exit code 1 means no processes found — this is fine.
		return
	}
	pids := nonEmptyLines(string(output))
	if len(pids) == 0 {
		return
	}

	m.addLog(fmt.Sprintf("⚠️ Found orphaned server process(es) [%s]: %s", processName, strings.Join(pids, ", ")))

	if err := exec.Command(m.pkillPath, "-9", "-x", processName).Run(); err != nil {
		return // silently fail - this is not critical
	}
	time.Sleep(500 * time.Millisecond)
	m.addLog("✓ Cleaned up orphaned processes")
}

// RunAuthCommand runs a login flow and blocks until the outcome is known
// (~1s), returning the user-facing result text. The process itself may keep
// running afterwards.
func (m *Manager) RunAuthCommand(command AuthCommand) (bool, string) {
	type result struct {
		ok      bool
		message string
	}
	ch := make(chan result, 1)
	m.RunAuthCommandAsync(command, func(ok bool, message string) {
		ch <- result{ok: ok, message: message}
	})
	r := <-ch
	return r.ok, r.message
}

// RunAuthCommandAsync is the callback form of RunAuthCommand; completion fires
// exactly once, from a goroutine.
func (m *Manager) RunAuthCommandAsync(command AuthCommand, completion func(bool, string)) {
	var once sync.Once
	finish := func(ok bool, message string) {
		once.Do(func() { completion(ok, message) })
	}

	if !fileExists(m.binaryPath) {
		finish(false, "cli-proxy-api binary not found in app bundle")
		return
	}

	// Auth flows use the bundled (unmerged) config; user-specific overrides
	// aren't needed for OAuth login itself.
	cmd := exec.Command(m.binaryPath, "--config", m.configTemplate, command.LoginFlag())
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		finish(false, "Failed to start auth process: "+err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		finish(false, "Failed to start auth process: "+err.Error())
		return
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		finish(false, "Failed to start auth process: "+err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		logx.Logf("[Auth] Failed to start: %v", err)
		finish(false, "Failed to start auth process: "+err.Error())
		return
	}
	logx.Logf("[Auth] Starting process: %s with args: %s", m.binaryPath, strings.Join(cmd.Args, " "))
	m.addLog(fmt.Sprintf("Authentication process started (PID: %d) - browser should open shortly", cmd.Process.Pid))
	logx.Logf("[Auth] Process started with PID: %d", cmd.Process.Pid)

	var out, errOut syncBuffer
	var copyWG sync.WaitGroup
	copyWG.Add(2)
	go func() { _, _ = io.Copy(&out, stdout); copyWG.Done() }()
	go func() { _, _ = io.Copy(&errOut, stderr); copyWG.Done() }()

	var exited atomic.Bool
	var exitCode atomic.Int32
	done := make(chan struct{})
	go func() {
		err := cmd.Wait()
		code := int32(0)
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = int32(exitErr.ExitCode())
			} else {
				code = -1
			}
		}
		exitCode.Store(code)
		exited.Store(true)
		close(done)
		logx.Logf("[Auth] Process terminated with exit code: %d", code)
		// Notify watchers when auth completes successfully so the UI picks up
		// the freshly written credential file.
		if code == 0 {
			go func() {
				time.Sleep(authMigrationDelay)
				claude.MigrateCanonicalFiles(paths.AuthDir())
				events.Publish(events.AuthDirectoryChanged)
			}()
		}
	}()

	// For Codex login, avoid blocking on the manual callback prompt after ~12s.
	if command == CodexLogin {
		go func() {
			select {
			case <-time.After(codexNudgeDelay):
			case <-done:
				return
			}
			if !exited.Load() {
				_, _ = stdin.Write([]byte("\n"))
				logx.Logf("[Auth] Sent newline to keep Codex login waiting for callback")
			}
			_ = stdin.Close()
		}()
	}

	// Wait briefly to check if the process crashes immediately.
	go func() {
		select {
		case <-done:
			// The copy goroutines finish draining the pipes after Wait
			// returns; only read once they're done so no output is missed.
			copyWG.Wait()
			combinedOut := out.String()
			combinedErr := errOut.String()

			logx.Logf("[Auth] Process died quickly - output: %s", abbreviate(combinedOut, 200))
			if strings.Contains(combinedOut, "Opening browser") || strings.Contains(combinedOut, "Attempting to open URL") {
				// Browser opened but process finished — treat as success.
				logx.Logf("[Auth] Browser opened, process completed")
				finish(true, browserOpenedMessage)
				return
			}
			logx.Logf("[Auth] Process failed")
			var message string
			switch {
			case combinedErr != "":
				message = combinedErr
			case combinedOut != "":
				message = combinedOut
			default:
				message = "Authentication process failed unexpectedly"
			}
			finish(false, message)
		case <-time.After(authLivenessCheckDelay):
			logx.Logf("[Auth] Process running after wait, returning success")
			finish(true, browserOpenedMessage)
		}
	}()
}

// syncBuffer is a bytes.Buffer guarded for one writer plus concurrent readers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (m *Manager) addLog(message string) {
	// The macOS app used a localized medium time style; this matches its shape.
	timestamp := time.Now().Format("3:04:05 PM")
	m.logMu.Lock()
	m.logs.append("[" + timestamp + "] " + message)
	m.logMu.Unlock()
}

// ringBuffer is a fixed-capacity FIFO of the retained backend log lines.
type ringBuffer struct {
	storage []string
	head    int
	tail    int
	count   int
}

func newRingBuffer(capacity int) *ringBuffer {
	if capacity < 1 {
		capacity = 1
	}
	return &ringBuffer{storage: make([]string, capacity)}
}

func (r *ringBuffer) append(s string) {
	capacity := len(r.storage)
	if capacity == 0 {
		return // an uninitialized ring drops instead of panicking
	}
	r.storage[r.tail] = s
	if r.count == capacity {
		r.head = (r.head + 1) % capacity
	} else {
		r.count++
	}
	r.tail = (r.tail + 1) % capacity
}

func (r *ringBuffer) elements() []string {
	if r.count == 0 {
		return nil
	}
	out := make([]string, 0, r.count)
	for i := 0; i < r.count; i++ {
		out = append(out, r.storage[(r.head+i)%len(r.storage)])
	}
	return out
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func nonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// abbreviate cuts s to at most limit bytes without splitting a rune.
func abbreviate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	for len(cut) > 0 && !utf8.RuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	if len(cut) == 0 {
		return ""
	}
	return cut + "…"
}
