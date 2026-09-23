// Package buildinfo holds identity and version values stamped in at build
// time with -ldflags "-X github.com/nikships/droidproxy-omarchy/internal/buildinfo.Version=1.2.3".
package buildinfo

var (
	Version   = "0.0.0-dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

const (
	AppName = "DroidProxy"
	// AppID is used for directory names, the systemd unit, and the desktop entry.
	AppID = "droidproxy"
	// PluginID is the Omarchy shell plugin id; the plugin directory under
	// ~/.config/omarchy/plugins/ must use exactly this name.
	PluginID = "nikships.droidproxy"
	// Repo is the GitHub owner/name that releases and update checks use.
	Repo      = "nikships/droidproxy-omarchy"
	RepoURL   = "https://github.com/" + Repo
	IssuesURL = RepoURL + "/issues"
)

// IsDev reports whether this binary was built without a release version.
func IsDev() bool {
	return Version == "0.0.0-dev"
}
