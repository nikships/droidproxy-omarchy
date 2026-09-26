// Package desktop wraps the desktop integration calls the macOS app made
// through AppKit: opening URLs and folders, the clipboard, systemd user
// services, and omarchy-shell IPC. Every exec goes through a Runner so tests
// never launch real processes.
package desktop

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// Runner executes external commands. The default implementation uses
// os/exec; tests inject a fake.
type Runner interface {
	// Run executes name with args and blocks until it exits. It returns the
	// stdout output and the error (if any).
	Run(ctx context.Context, name string, args ...string) (string, error)
	// LookPath reports whether name can be found in PATH.
	LookPath(name string) (string, error)
	// StartDetached launches name with args without waiting for it, with its
	// output discarded, and lets it outlive the caller.
	StartDetached(name string, args ...string) error
}

// ExecRunner is the real Runner.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// LookPath implements Runner.
func (ExecRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

// StartDetached implements Runner. The child is re-parented to init so it
// survives the daemon restarting, and its stdio goes nowhere.
func (ExecRunner) StartDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = detachedAttr()
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	logx.Logf("[Desktop] Starting %s %s", name, strings.Join(args, " "))
	return cmd.Start()
}

// Default is the process-wide runner. Tests swap it and restore it.
var Default Runner = ExecRunner{}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return Default.Run(ctx, name, args...)
}

// OpenURL opens url in the default browser via xdg-open.
func OpenURL(url string) error { return OpenPath(url) }

// OpenPath opens path (a file, folder, or URL) with xdg-open, detached so a
// slow launcher cannot stall the daemon.
func OpenPath(path string) error {
	if path == "" {
		return errors.New("desktop: nothing to open")
	}
	return Default.StartDetached("xdg-open", path)
}

// CopyToClipboard copies text using wl-copy, falling back to xclip when
// wl-copy is missing (X11 sessions).
func CopyToClipboard(text string) error {
	if _, err := Default.LookPath("wl-copy"); err == nil {
		return copyWith("wl-copy", text)
	}
	if _, err := Default.LookPath("xclip"); err == nil {
		return copyWith("xclip", "-selection", "clipboard", text)
	}
	return errors.New("desktop: no clipboard tool found (install wl-copy or xclip)")
}

func copyWith(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Default.Run(ctx, name, args...)
	return err
}

// ---- systemd user service helpers ------------------------------------------

// ServiceUnit is the user unit DroidProxy installs and controls.
const ServiceUnit = "droidproxy.service"

// ErrNotUnderSystemd is returned when a systemd operation is attempted in a
// session that is not running under systemd user managers.
var ErrNotUnderSystemd = errors.New("desktop: not running under systemd")

// Systemctl runs systemctl --user with args and returns its output.
func Systemctl(ctx context.Context, args ...string) (string, error) {
	return run(ctx, 30*time.Second, "systemctl", append([]string{"--user"}, args...)...)
}

// SystemctlNoBlock runs systemctl --user --no-block (used for restart so the
// daemon is not killed while being the caller).
func SystemctlNoBlock(ctx context.Context, args ...string) (string, error) {
	return run(ctx, 30*time.Second, "systemctl", append([]string{"--user", "--no-block"}, args...)...)
}

// IsEnabled reports whether droidproxy.service is enabled.
func IsEnabled() bool {
	out, err := Systemctl(context.Background(), "is-enabled", ServiceUnit)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "enabled"
}

// IsRunning reports whether droidproxy.service is currently active.
func IsRunning() bool {
	out, err := Systemctl(context.Background(), "is-active", ServiceUnit)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "active"
}

// Enable enables the unit (launch at login).
func Enable() error {
	_, err := Systemctl(context.Background(), "enable", ServiceUnit)
	return err
}

// Disable disables the unit.
func Disable() error {
	_, err := Systemctl(context.Background(), "disable", ServiceUnit)
	return err
}

// Start starts the unit.
func Start() error {
	_, err := Systemctl(context.Background(), "start", ServiceUnit)
	return err
}

// Stop stops the unit.
func Stop() error {
	_, err := Systemctl(context.Background(), "stop", ServiceUnit)
	return err
}

// Restart restarts the unit without blocking; when running as the service
// itself, a blocking restart would kill the caller mid-command.
func Restart() error {
	_, err := SystemctlNoBlock(context.Background(), "restart", ServiceUnit)
	return err
}

// DaemonReload runs systemctl --user daemon-reload.
func DaemonReload() error {
	_, err := Systemctl(context.Background(), "daemon-reload")
	return err
}

// UnderSystemd reports whether this process runs under a systemd user session
// (the same check UIs use before offering journal links).
func UnderSystemd() bool {
	if os.Getenv("INVOCATION_ID") != "" || os.Getenv("JOURNAL_STREAM") != "" {
		return true
	}
	// Not every context exports those (manual `droidproxy serve` inside the
	// session); fall through to whether the user manager answers.
	out, err := Systemctl(context.Background(), "is-system-running")
	if err != nil && out == "" {
		// systemctl itself unreachable: no user manager.
		if _, lookErr := Default.LookPath("systemctl"); lookErr != nil {
			return false
		}
		return false
	}
	return true
}

// ---- omarchy-shell IPC ------------------------------------------------------

// ShellIPC sends an IPC call to the running omarchy shell:
// omarchy-shell <target> <method> [args...]. When the omarchy-shell binary is
// missing (or the shell is not running) the call is silently ignored,
// mirroring omarchy-shell -q.
func ShellIPC(target, method string, args ...string) error {
	if _, err := Default.LookPath("omarchy-shell"); err != nil {
		return nil
	}
	cmd := append([]string{target, method}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := run(ctx, 0, "omarchy-shell", cmd...)
	if err != nil {
		logx.Logf("[Desktop] omarchy-shell %s %s failed: %s", target, method, strings.TrimSpace(out))
	}
	return err
}

// DroidProxyIPC calls the DroidProxy plugin's own IPC target.
func DroidProxyIPC(method string, args ...string) error {
	return ShellIPC(buildinfoPluginTarget(), method, args...)
}

// The plugin registers target "droidproxy" with methods openWebUI (plus the
// legacy openSettings alias) and ping.
func buildinfoPluginTarget() string { return "droidproxy" }

// OpenSettings asks the plugin to open the settings web app. Prefer opening
// the web UI URL directly (cmdOpen does); this is the shell-IPC path.
func OpenSettings() error {
	return DroidProxyIPC("openWebUI")
}

// EnsureDirs creates the runtime directories the desktop helpers assume.
func EnsureDirs() error {
	return paths.EnsureDir(paths.RuntimeDir(), 0o700)
}
