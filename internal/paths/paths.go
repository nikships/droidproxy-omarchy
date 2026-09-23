// Package paths is the single source of truth for every filesystem location
// DroidProxy reads or writes on Linux.
//
// Credential locations intentionally match the macOS app (~/.cli-proxy-api,
// ~/.droidproxy) so auth files can be copied between machines unchanged.
// Everything else follows the XDG base directory spec.
package paths

import (
	"os"
	"path/filepath"
	"strconv"
)

// Home returns the user's home directory. HOME is honored first so tests can
// redirect every derived path with t.Setenv("HOME", t.TempDir()).
func Home() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "/tmp"
	}
	return h
}

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(Home(), fallback)
}

// AuthDir is CLIProxyAPI's auth directory (~/.cli-proxy-api).
func AuthDir() string { return filepath.Join(Home(), ".cli-proxy-api") }

// MergedConfigPath is the generated CLIProxyAPI config (~/.cli-proxy-api/merged-config.yaml).
func MergedConfigPath() string { return filepath.Join(AuthDir(), "merged-config.yaml") }

// BackendLogsDir is where CLIProxyAPI writes request logs when verbose logging is on.
func BackendLogsDir() string { return filepath.Join(AuthDir(), "logs") }

// DataDir is DroidProxy's private data directory (~/.droidproxy).
func DataDir() string { return filepath.Join(Home(), ".droidproxy") }

// CopilotDataDir holds the Copilot API gateway's own state (~/.droidproxy/copilot-api).
func CopilotDataDir() string { return filepath.Join(DataDir(), "copilot-api") }

// MetaDataDir holds Meta Muse accounts (~/.droidproxy/meta).
func MetaDataDir() string { return filepath.Join(DataDir(), "meta") }

// ConfigHome is $XDG_CONFIG_HOME (default ~/.config).
func ConfigHome() string { return xdg("XDG_CONFIG_HOME", ".config") }

// DataHome is $XDG_DATA_HOME (default ~/.local/share).
func DataHome() string { return xdg("XDG_DATA_HOME", ".local/share") }

// StateHome is $XDG_STATE_HOME (default ~/.local/state).
func StateHome() string { return xdg("XDG_STATE_HOME", ".local/state") }

// ConfigDir is DroidProxy's preferences directory (~/.config/droidproxy).
func ConfigDir() string { return filepath.Join(ConfigHome(), "droidproxy") }

// PrefsPath is the JSON preferences file (the UserDefaults replacement).
func PrefsPath() string { return filepath.Join(ConfigDir(), "settings.json") }

// StateDir is DroidProxy's state directory (~/.local/state/droidproxy).
func StateDir() string { return filepath.Join(StateHome(), "droidproxy") }

// DebugLogPath is the ThinkingProxy per-request debug log.
func DebugLogPath() string { return filepath.Join(StateDir(), "droidproxy-debug.log") }

// InstallRoot is where released versions are unpacked (~/.local/share/droidproxy).
func InstallRoot() string { return filepath.Join(DataHome(), "droidproxy") }

// VersionsDir holds one directory per installed version.
func VersionsDir() string { return filepath.Join(InstallRoot(), "versions") }

// CurrentLink is the symlink that points at the active version directory.
func CurrentLink() string { return filepath.Join(InstallRoot(), "current") }

// UserBinDir is ~/.local/bin, where the droidproxy command is linked.
func UserBinDir() string { return filepath.Join(Home(), ".local", "bin") }

// SystemdUserDir is ~/.config/systemd/user.
func SystemdUserDir() string { return filepath.Join(ConfigHome(), "systemd", "user") }

// OmarchyPluginsDir is ~/.config/omarchy/plugins.
func OmarchyPluginsDir() string { return filepath.Join(ConfigHome(), "omarchy", "plugins") }

// ApplicationsDir is ~/.local/share/applications.
func ApplicationsDir() string { return filepath.Join(DataHome(), "applications") }

// IconsDir is ~/.local/share/icons.
func IconsDir() string { return filepath.Join(DataHome(), "icons") }

// FactorySettingsPath is Droid CLI's settings file (~/.factory/settings.json).
func FactorySettingsPath() string { return filepath.Join(Home(), ".factory", "settings.json") }

// RuntimeDir holds the control socket. $XDG_RUNTIME_DIR is per-user and 0700.
func RuntimeDir() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Join(v, "droidproxy")
	}
	return filepath.Join(os.TempDir(), "droidproxy-"+strconv.Itoa(os.Getuid()))
}

// ControlSocketPath is the daemon's local control API socket.
func ControlSocketPath() string {
	if v := os.Getenv("DROIDPROXY_SOCKET"); v != "" {
		return v
	}
	return filepath.Join(RuntimeDir(), "control.sock")
}

// ResourceRoot is the directory containing bin/, libexec/ and share/ for the
// running build. Release layout: <root>/bin/droidproxy. DROIDPROXY_ROOT
// overrides it for development builds.
func ResourceRoot() string {
	if v := os.Getenv("DROIDPROXY_ROOT"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(filepath.Dir(exe))
}

// BundledCLIProxyAPI is the bundled CLIProxyAPI binary.
func BundledCLIProxyAPI() string { return filepath.Join(ResourceRoot(), "libexec", "cli-proxy-api") }

// BundledConfigPath is the bundled CLIProxyAPI config template.
func BundledConfigPath() string { return filepath.Join(ResourceRoot(), "share", "config.yaml") }

// ShareDir holds bundled data (plugin, icons, templates).
func ShareDir() string { return filepath.Join(ResourceRoot(), "share") }

// EnsureDir creates dir (and parents) with the given permissions.
func EnsureDir(dir string, perm os.FileMode) error {
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	return os.Chmod(dir, perm)
}
