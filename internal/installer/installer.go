// Package installer lays down (and removes) a DroidProxy release tree:
// the version directory under ~/.local/share/droidproxy, the `current`
// symlink, the ~/.local/bin link, the systemd user unit, the desktop entry,
// the hicolor icons, and the Omarchy shell plugin. It is driven both by the
// CLI (`droidproxy install`) and by the new version itself during an update
// (`.../versions/V/bin/droidproxy install --from <dir> --update`).
package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/buildinfo"
	"github.com/nikships/droidproxy-omarchy/internal/desktop"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// Options configures Install.
type Options struct {
	// From is the extracted release directory (the tree with bin/, libexec/,
	// share/, VERSION). Empty means paths.ResourceRoot().
	From string
	// Update marks an update install: the service is not enabled/started
	// (the updater restarts it); a running service is restarted because the
	// unit file or layout may have changed.
	Update bool
}

// unitName is the user unit the install manages.
const unitName = desktop.ServiceUnit

// desktopFileName is the desktop entry the install manages.
const desktopFileName = "droidproxy.desktop"

// Install performs a fresh install or an update from the release tree at
// opts.From. It never touches /usr/share/omarchy; the plugin is installed
// under the user's ~/.config/omarchy/plugins and enabled through Omarchy's
// own commands.
func Install(opts Options) error {
	from := opts.From
	if from == "" {
		from = paths.ResourceRoot()
	}
	from, err := filepath.Abs(from)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(filepath.Join(from, "bin", "droidproxy")); err != nil || fi.IsDir() {
		return fmt.Errorf("installer: %s does not look like a DroidProxy release (missing bin/droidproxy)", from)
	}

	version, err := releaseVersion(from)
	if err != nil {
		return err
	}
	if !validVersionDir(version) {
		return fmt.Errorf("installer: refusing invalid VERSION %q", version)
	}

	dest := filepath.Join(paths.VersionsDir(), version)
	if !sameRealDir(from, dest) {
		// Copy into place first; the current symlink only flips once the new
		// tree is complete.
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
		if err := copyTree(from, dest); err != nil {
			return fmt.Errorf("installer: copy release tree: %w", err)
		}
	}

	if err := flipCurrent(version); err != nil {
		return fmt.Errorf("installer: flip current symlink: %w", err)
	}

	if err := os.MkdirAll(paths.UserBinDir(), 0o755); err != nil {
		return err
	}
	binLink := filepath.Join(paths.UserBinDir(), "droidproxy")
	_ = os.Remove(binLink)
	if err := os.Symlink(filepath.Join(paths.CurrentLink(), "bin", "droidproxy"), binLink); err != nil {
		return fmt.Errorf("installer: link %s: %w", binLink, err)
	}

	if err := copyFile(
		filepath.Join(from, "share", "systemd", unitName),
		filepath.Join(paths.SystemdUserDir(), unitName), 0o644); err != nil {
		return fmt.Errorf("installer: systemd unit: %w", err)
	}
	if err := copyFile(
		filepath.Join(from, "share", "applications", desktopFileName),
		filepath.Join(paths.ApplicationsDir(), desktopFileName), 0o644); err != nil {
		return fmt.Errorf("installer: desktop entry: %w", err)
	}
	if err := copyIcons(filepath.Join(from, "share", "icons")); err != nil {
		return fmt.Errorf("installer: icons: %w", err)
	}
	if err := installPlugin(from); err != nil {
		return fmt.Errorf("installer: plugin: %w", err)
	}
	enablePlugin()

	if err := desktop.DaemonReload(); err != nil {
		logx.Logf("[Install] daemon-reload failed: %v", err)
	}

	if opts.Update {
		// The updater restarts right after Install returns; restart here
		// only when the service is already running (manual update).
		if desktop.IsRunning() {
			if err := desktop.Restart(); err != nil {
				return fmt.Errorf("installer: restart service: %w", err)
			}
		}
		return nil
	}

	if err := desktop.Enable(); err != nil {
		return fmt.Errorf("installer: enable service: %w", err)
	}
	if err := desktop.Start(); err != nil {
		return fmt.Errorf("installer: start service: %w", err)
	}
	return nil
}

// Uninstall removes everything Install laid down. With purge it also deletes
// the user's DroidProxy data and CLIProxyAPI credentials; otherwise those
// are kept and reported.
func Uninstall(purge bool) error {
	disablePlugin() // best effort: the shell may not be running
	if err := os.RemoveAll(filepath.Join(paths.OmarchyPluginsDir(), buildinfo.PluginID)); err != nil {
		logx.Logf("[Uninstall] plugin dir: %v", err)
	}

	_ = desktop.Disable()
	_ = desktop.Stop()
	if err := os.Remove(filepath.Join(paths.SystemdUserDir(), unitName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logx.Logf("[Uninstall] unit file: %v", err)
	}
	_ = desktop.DaemonReload()

	if err := os.Remove(filepath.Join(paths.ApplicationsDir(), desktopFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logx.Logf("[Uninstall] desktop entry: %v", err)
	}
	removeIcons()

	_ = os.Remove(filepath.Join(paths.UserBinDir(), "droidproxy"))
	if err := os.RemoveAll(paths.InstallRoot()); err != nil {
		logx.Logf("[Uninstall] install root: %v", err)
	}

	// User data and credentials are only removed on purge; tell the user
	// what survived so nothing feels lost.
	kept := []string{
		paths.ConfigDir(), // preferences (~/.config/droidproxy)
		paths.DataDir(),   // private state (~/.droidproxy)
		paths.AuthDir(),   // CLIProxyAPI credentials (~/.cli-proxy-api)
	}
	if purge {
		for _, dir := range kept {
			if err := os.RemoveAll(dir); err != nil {
				logx.Logf("[Uninstall] purge %s: %v", dir, err)
			}
		}
		fmt.Println("Purged preferences, state, and CLIProxyAPI credentials.")
		return nil
	}
	fmt.Println("Kept (remove manually or run `droidproxy uninstall --purge` to delete):")
	for _, dir := range kept {
		if _, err := os.Stat(dir); err == nil {
			fmt.Println("  " + dir)
		}
	}
	return nil
}

// Rollback flips the current symlink back to the most recent previous
// version and restarts the service. The binaries themselves are untouched.
func Rollback() error {
	cur, err := os.Readlink(paths.CurrentLink())
	if err != nil {
		return fmt.Errorf("installer: no current installation to roll back from: %w", err)
	}
	curVersion := filepath.Base(cur)

	entries, err := os.ReadDir(paths.VersionsDir())
	if err != nil {
		return err
	}
	prev := ""
	for _, e := range entries {
		if !e.IsDir() || e.Name() == curVersion {
			continue
		}
		if prev == "" || versionNewer(e.Name(), prev) {
			prev = e.Name()
		}
	}
	if prev == "" {
		return fmt.Errorf("installer: no previous version to roll back to (current: %s)", curVersion)
	}

	if err := flipCurrent(prev); err != nil {
		return err
	}
	if desktop.IsRunning() {
		return desktop.Restart()
	}
	fmt.Printf("Rolled back to %s. Start it with: systemctl --user start %s\n", prev, unitName)
	return nil
}

// ---- plugin -----------------------------------------------------------------

// installPlugin copies share/plugin into
// ~/.config/omarchy/plugins/<PluginID>. The temp staging dir lives OUTSIDE
// the plugins dir (the shell watches and hot-reloads that tree), so the
// swap-in is a single rename.
func installPlugin(from string) error {
	src := filepath.Join(from, "share", "plugin")
	if fi, err := os.Stat(filepath.Join(src, "manifest.json")); err != nil || fi.IsDir() {
		return fmt.Errorf("release has no share/plugin/manifest.json")
	}

	id := buildinfo.PluginID
	dest := filepath.Join(paths.OmarchyPluginsDir(), id)
	staging := filepath.Join(paths.InstallRoot(), ".staging-plugin")

	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := copyTree(src, staging); err != nil {
		return fmt.Errorf("stage plugin: %w", err)
	}
	if err := os.MkdirAll(paths.OmarchyPluginsDir(), 0o755); err != nil {
		return err
	}

	backup := filepath.Join(paths.InstallRoot(), ".old-plugin")
	if _, err := os.Lstat(dest); err == nil {
		if err := os.RemoveAll(backup); err != nil {
			return err
		}
		if err := os.Rename(dest, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, dest); err != nil {
		if _, statErr := os.Lstat(backup); statErr == nil {
			_ = os.Rename(backup, dest) // best-effort restore
		}
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
}

// enablePlugin turns the plugin on through Omarchy's own non-interactive
// command (`omarchy plugin enable <id>`, which forwards to
// `omarchy-shell shell enablePlugin <id> '{}'`). Best effort: the shell may
// not be running (ssh install, first boot before the session).
func enablePlugin() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := desktop.Default.Run(ctx, "omarchy", "plugin", "enable", buildinfo.PluginID); err != nil {
		logx.Logf("[Install] omarchy plugin enable failed (%v); falling back to omarchy-shell IPC", err)
		_ = desktop.ShellIPC("shell", "rescanPlugins")
		_ = desktop.ShellIPC("shell", "enablePlugin", buildinfo.PluginID, "{}")
	}
}

// disablePlugin is the uninstall counterpart (`omarchy plugin disable <id>`
// → `omarchy-shell shell setPluginEnabled <id> false`). Best effort.
func disablePlugin() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := desktop.Default.Run(ctx, "omarchy", "plugin", "disable", buildinfo.PluginID); err != nil {
		_ = desktop.ShellIPC("shell", "setPluginEnabled", buildinfo.PluginID, "false")
	}
}

// ---- file helpers -----------------------------------------------------------

// releaseVersion reads the VERSION file from a release tree, falling back
// to the running build's version.
func releaseVersion(from string) (string, error) {
	data, err := os.ReadFile(filepath.Join(from, "VERSION"))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		v := buildinfo.Version
		if v == "" {
			return "", errors.New("installer: no VERSION file and no build version")
		}
		return v, nil
	}
	return strings.TrimSpace(string(data)), nil
}

// validVersionDir refuses VERSION values that could escape the versions dir.
func validVersionDir(v string) bool {
	if v == "" || v == "." || v == ".." {
		return false
	}
	if strings.ContainsAny(v, "/\\") || strings.Contains(v, "..") {
		return false
	}
	return v == filepath.Base(v)
}

// flipCurrent points paths.CurrentLink() at versions/<version> atomically
// (symlink to a temp name, then rename over the link).
func flipCurrent(version string) error {
	root := paths.InstallRoot()
	if err := paths.EnsureDir(root, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(root, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Join("versions", version), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, paths.CurrentLink())
}

// copyTree copies a directory tree, preserving relative paths and file
// modes. Symlinks are refused: a release tree must be plain files, and the
// Omarchy plugins dir rejects symlinks anyway.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return paths.EnsureDir(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symlink; release trees must contain plain files", path)
		case d.Type().IsRegular():
			return copyFile(path, target, 0)
		default:
			return fmt.Errorf("unsupported file type at %s", path)
		}
	})
}

// copyFile copies one file, creating parent dirs. mode 0 preserves the
// source mode.
func copyFile(src, dst string, mode os.FileMode) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if mode == 0 {
		mode = fi.Mode().Perm()
	}
	if err := paths.EnsureDir(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyIcons copies share/icons/hicolor/<size>/apps/* into the user's
// hicolor icon dir.
func copyIcons(srcIcons string) error {
	root := filepath.Join(srcIcons, "hicolor")
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil // a release without icons is not fatal
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(srcIcons, path)
		if err != nil {
			return err
		}
		return copyFile(path, filepath.Join(paths.IconsDir(), rel), 0o644)
	})
}

// removeIcons deletes only the DroidProxy hicolor entries.
func removeIcons() {
	hicolor := filepath.Join(paths.IconsDir(), "hicolor")
	entries, err := os.ReadDir(hicolor)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(hicolor, e.Name(), "apps", "droidproxy.png"))
	}
}

// sameRealDir reports whether two paths resolve to the same directory.
func sameRealDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// versionNewer is a minimal dotted-numeric comparison for rollback
// candidates (good enough for our own version dirs).
func versionNewer(a, b string) bool {
	pa := splitVersion(a)
	pb := splitVersion(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return len(pa) > len(pb)
}

func splitVersion(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return nums
			}
			n = n*10 + int(c-'0')
		}
		nums = append(nums, n)
	}
	return nums
}
