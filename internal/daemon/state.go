package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/backend"
	"github.com/nikships/droidproxy-omarchy/internal/buildinfo"
	"github.com/nikships/droidproxy-omarchy/internal/catalog"
	"github.com/nikships/droidproxy-omarchy/internal/control"
	"github.com/nikships/droidproxy-omarchy/internal/desktop"
	"github.com/nikships/droidproxy-omarchy/internal/meta"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

// providerInfo describes one provider row. The order matches the macOS
// Settings "Services" section (Copilot was dropped from the Linux port):
// claude, codex, meta, antigravity, kimi, junie, grok.
type providerInfo struct {
	serviceType auth.ServiceType
	icon        string
	color       string
	kind        string
	help        string
}

var providerOrder = []providerInfo{
	{auth.Claude, "icon-claude.png", "#D97757", control.KindStandard, ""},
	{auth.Codex, "icon-codex.png", "#74AA9C", control.KindStandard, ""},
	{auth.Meta, "icon-meta.svg", "#0866FF", control.KindMeta, "Add Meta Muse accounts for automatic account failover."},
	{auth.Antigravity, "icon-gemini.png", "#4285F4", control.KindStandard, "Uses your Antigravity subscription for the Antigravity-backed Gemini, Claude, and GPT-OSS models."},
	{auth.Kimi, "icon-kimi.svg", "#00BF91", control.KindStandard, ""},
	{auth.Junie, "icon-junie.svg", "#48E054", control.KindJunie, "Enter your JetBrains Junie API key to use your JetBrains AI subscription for Junie Sonnet 5, Opus 5.5, and Fable 5.1."},
	{auth.Grok, "icon-grok.svg", "#1D9BF0", control.KindGrok, "Log in with SuperGrok / X Premium+ to use Grok 4.7 and Grok 4.7 Fast (no xAI API key)."},
}

// State assembles the full control API snapshot. It runs on HTTP handler
// goroutines, so every slow fact it needs is cached.
func (d *Daemon) State() control.State {
	bind := prefs.BindAddress()
	displayHost := bind
	if bind == "0.0.0.0" {
		displayHost = "localhost"
	}
	dashHost := bind
	if bind == "0.0.0.0" {
		dashHost = "127.0.0.1"
	}

	d.mu.Lock()
	starting := d.starting
	lastError := d.lastError
	grokSession := d.grokSession
	grokUserCode := d.grokUserCode
	grokVerifyURL := d.grokVerifyURL
	metaDeviceCode := d.metaDeviceCode
	metaVerifyURL := d.metaVerifyURL
	loginInFlight := make(map[auth.ServiceType]bool, len(d.loginInFlight))
	for k, v := range d.loginInFlight {
		loginInFlight[k] = v
	}
	d.mu.Unlock()

	metaState := d.metaAuth.State()

	providers := make([]control.ProviderState, 0, len(providerOrder))
	for _, p := range providerOrder {
		st := control.ProviderState{
			ID:      string(p.serviceType),
			Name:    p.serviceType.DisplayName(),
			Icon:    p.icon,
			Color:   p.color,
			Kind:    p.kind,
			Enabled: prefs.IsProviderEnabled(string(p.serviceType)),
			Help:    p.help,
		}
		switch {
		case loginInFlight[p.serviceType]:
			st.Authenticating = true
		case p.serviceType == auth.Grok && grokSession != nil:
			st.Authenticating = true
		case p.serviceType == auth.Meta && metaState.Kind == meta.StateAuthenticating:
			st.Authenticating = true
		}
		for _, a := range d.authMgr.Accounts(p.serviceType) {
			st.Accounts = append(st.Accounts, control.AccountState{
				ID:          a.ID,
				DisplayName: a.DisplayName(),
				Email:       a.Email,
				Expired:     a.IsExpired(),
				Disabled:    a.Disabled,
			})
		}
		providers = append(providers, st)
	}

	usageAccounts := d.usage.Accounts()
	usageRows := make([]control.UsageAccount, 0, len(usageAccounts))
	for _, a := range usageAccounts {
		windows := make([]control.UsageWindow, 0, len(a.Windows))
		for _, w := range a.Windows {
			cw := control.UsageWindow{
				Title:        w.Title,
				HasRemaining: w.HasRemaining,
				ResetText:    w.ResetText,
			}
			if w.RemainingPercent != nil {
				cw.RemainingPercent = *w.RemainingPercent
			}
			windows = append(windows, cw)
		}
		updatedAt := ""
		if a.UpdatedAt != nil {
			updatedAt = a.UpdatedAt.Format(time.RFC3339)
		}
		usageRows = append(usageRows, control.UsageAccount{
			Provider:     a.Provider,
			ProviderName: a.ProviderName,
			Email:        a.Email,
			Loading:      a.Loading,
			Error:        a.Error,
			Windows:      windows,
			UpdatedAt:    updatedAt,
		})
	}

	return control.State{
		App: control.AppInfo{
			Name:               buildinfo.AppName,
			Version:            buildinfo.Version,
			Commit:             buildinfo.Commit,
			RepoURL:            buildinfo.RepoURL,
			IssuesURL:          buildinfo.IssuesURL,
			CLIProxyAPIURL:     "https://github.com/router-for-me/CLIProxyAPI",
			CLIProxyAPIVersion: d.cliProxyVersion(),
		},
		Server: control.ServerState{
			Running:      d.serverRunning(),
			Starting:     starting,
			ProxyPort:    ProxyPort,
			BackendPort:  backend.BackendPort,
			URL:          fmt.Sprintf("http://%s:%d", displayHost, ProxyPort),
			DashboardURL: fmt.Sprintf("http://%s:%d/management.html", dashHost, backend.BackendPort),
			LastError:    lastError,
		},
		Settings: control.Settings{
			LaunchAtLogin:             d.launchAtLoginCached(),
			AllowRemote:               prefs.AllowRemote(),
			SecretKey:                 prefs.SecretKey(),
			BindAddress:               prefs.BindAddress(),
			Beta:                      prefs.BetaFlag(),
			VerboseLogging:            prefs.VerboseLogging(),
			SequentialAccountFailover: prefs.SequentialAccountFailover(),
			OLEDTheme:                 prefs.OLEDTheme(),
			BackgroundOpacity:         prefs.BackgroundOpacity(),
			GPT6AstraFastMode:         prefs.GPT6AstraFastMode(),
			GPT6SolFastMode:           prefs.GPT6SolFastMode(),
			GPT6LunaFastMode:          prefs.GPT6LunaFastMode(),
			MetaContributorMode:       prefs.MetaContributorMode(),
			AutoCheckUpdates:          prefs.AutoCheckUpdates(),
			AutoInstallUpdates:        prefs.AutoInstallUpdates(),
		},
		Paths: control.PathsInfo{
			AuthDir:         paths.AuthDir(),
			LogsDir:         paths.BackendLogsDir(),
			FactorySettings: paths.FactorySettingsPath(),
			DebugLog:        paths.DebugLogPath(),
		},
		Factory: control.FactoryState{
			ModelsInstalled: d.factoryModelsInstalled(),
		},
		Providers: providers,
		Meta: control.MetaState{
			Authenticating:  metaState.Kind == meta.StateAuthenticating,
			DeviceCode:      metaDeviceCode,
			VerificationURL: metaVerifyURL,
			LastError:       d.metaAuth.LastError(),
		},
		Grok: control.GrokState{
			Authenticating:  grokSession != nil,
			UserCode:        grokUserCode,
			VerificationURL: grokVerifyURL,
		},
		Usage: control.UsageState{
			Visible:    d.usageVisible(),
			Refreshing: d.usage.IsRefreshing(),
			Accounts:   usageRows,
		},
		Update: d.updater.Snapshot(),
	}
}

// isProviderEnabled adapts prefs to the catalog's predicate.
func (d *Daemon) isProviderEnabled(st auth.ServiceType) bool {
	return prefs.IsProviderEnabled(string(st))
}

// usageVisible mirrors SettingsView: the quota section shows when any
// usage-tracked provider is enabled or has any connected account.
func (d *Daemon) usageVisible() bool {
	for _, st := range []auth.ServiceType{auth.Codex, auth.Claude, auth.Grok, auth.Meta} {
		if prefs.IsProviderEnabled(string(st)) || d.authMgr.HasAccounts(st) {
			return true
		}
	}
	return false
}

// factoryModelsInstalled caches catalog.CheckFactoryModelsInstalled; the
// check parses ~/.factory/settings.json, which is too heavy for every
// snapshot.
func (d *Daemon) factoryModelsInstalled() bool {
	d.mu.Lock()
	dirty, installed := d.factoryDirty, d.factoryInstalled
	d.mu.Unlock()
	if !dirty {
		return installed
	}
	v := catalog.CheckFactoryModelsInstalled(d.isProviderEnabled)
	d.mu.Lock()
	d.factoryInstalled, d.factoryDirty = v, false
	d.mu.Unlock()
	return v
}

func (d *Daemon) invalidateFactory() {
	d.mu.Lock()
	d.factoryDirty = true
	d.mu.Unlock()
}

// launchAtLoginCached wraps desktop.IsEnabled (a systemctl call) with a short
// TTL so state snapshots stay cheap.
func (d *Daemon) launchAtLoginCached() bool {
	d.mu.Lock()
	enabled, checkedAt := d.launchEnabled, d.launchCheckedAt
	d.mu.Unlock()
	if !checkedAt.IsZero() && time.Since(checkedAt) < launchAtLoginCacheTTL {
		return enabled
	}
	v := desktop.IsEnabled()
	d.mu.Lock()
	d.launchEnabled, d.launchCheckedAt = v, time.Now()
	d.mu.Unlock()
	return v
}

func (d *Daemon) invalidateLaunchAtLogin() {
	d.mu.Lock()
	d.launchCheckedAt = time.Time{}
	d.mu.Unlock()
}

// cliProxyVersion reads the bundled CLIProxyAPI version once
// (share/cliproxyapi-version, written by the release build).
func (d *Daemon) cliProxyVersion() string {
	d.mu.Lock()
	v := d.cliproxyVersion
	d.mu.Unlock()
	if v != "" {
		return v
	}
	data, err := os.ReadFile(filepath.Join(paths.ShareDir(), "cliproxyapi-version"))
	if err == nil {
		v = strings.TrimSpace(string(data))
	}
	if v == "" {
		v = "unknown"
	}
	d.mu.Lock()
	d.cliproxyVersion = v
	d.mu.Unlock()
	return v
}
