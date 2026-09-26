// Package daemon runs the DroidProxy service: ThinkingProxy, the CLIProxyAPI
// backend process, the provider auth flows, the usage tracker, the
// self-updater, and the local control API the Omarchy shell plugin and the
// CLI talk to. It is the port of the macOS app's AppDelegate plus every
// action SettingsView performed.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/backend"
	"github.com/nikships/droidproxy-omarchy/internal/buildinfo"
	"github.com/nikships/droidproxy-omarchy/internal/catalog"
	"github.com/nikships/droidproxy-omarchy/internal/claude"
	"github.com/nikships/droidproxy-omarchy/internal/control"
	"github.com/nikships/droidproxy-omarchy/internal/desktop"
	"github.com/nikships/droidproxy-omarchy/internal/events"
	"github.com/nikships/droidproxy-omarchy/internal/grok"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/meta"
	"github.com/nikships/droidproxy-omarchy/internal/notify"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
	"github.com/nikships/droidproxy-omarchy/internal/proxy"
	"github.com/nikships/droidproxy-omarchy/internal/updater"
	"github.com/nikships/droidproxy-omarchy/internal/usage"
	"github.com/nikships/droidproxy-omarchy/internal/webui"
)

// ProxyPort is the user-facing proxy port (ThinkingProxy).
const ProxyPort = 8317

// proxyServer is the seam the ThinkingProxy port plugs into; see proxyFactory.
type proxyServer interface {
	Start() error
	Stop()
	IsRunning() bool
}

// proxyFactory builds the ThinkingProxy. A variable so tests can stub it.
var proxyFactory func() proxyServer = func() proxyServer { return proxy.New() }

// Timing constants ported from AppDelegate/SettingsView.
const (
	proxyReadinessAttempts = 60
	proxyReadinessInterval = 50 * time.Millisecond
	serverRestartDelay     = 300 * time.Millisecond
	authRefreshDebounce    = 500 * time.Millisecond
	// Re-mint well inside the ~24h Model API key lifetime
	// (MetaMuseAuthManager's own margin re-mints starting 6h before expiry).
	metaKeyRefreshInterval = time.Hour
	launchAtLoginCacheTTL  = 5 * time.Second
)

// Daemon owns every long-lived component of the service.
type Daemon struct {
	ctx    context.Context
	cancel context.CancelFunc

	backend   *backend.Manager
	proxy     proxyServer
	authMgr   *auth.Manager
	usage     *usage.Tracker
	metaStore *meta.CredentialStore
	metaAuth  *meta.AuthManager
	updater   *updater.Updater
	server    *control.Server
	web       *webui.Server

	monitor *auth.DirectoryMonitor

	mu sync.Mutex
	// Server lifecycle.
	starting  bool
	lastError string
	// Login flows.
	loginInFlight  map[auth.ServiceType]bool
	grokSession    *grok.LoginSession
	grokUserCode   string
	grokVerifyURL  string
	metaDeviceCode string
	metaVerifyURL  string
	// usageSignature mirrors SettingsView.codexUsageAccountSignature so the
	// tracker only refreshes when the eligible account set actually changes.
	usageSig string
	// Caches.
	factoryDirty     bool
	factoryInstalled bool
	launchEnabled    bool
	launchCheckedAt  time.Time
	cliproxyVersion  string
}

// Run starts the daemon and blocks until ctx is canceled (SIGTERM under
// systemd) or the control API fails fatally. It is the entry point of
// `droidproxy serve`.
func Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	d := &Daemon{
		ctx:           ctx,
		cancel:        cancel,
		loginInFlight: map[auth.ServiceType]bool{},
		factoryDirty:  true,
	}
	return d.run()
}

func (d *Daemon) run() error {
	logx.Logf("[Daemon] DroidProxy %s (%s) starting", buildinfo.Version, buildinfo.Commit)

	d.metaStore = meta.Shared()
	d.authMgr = auth.NewManager()
	d.authMgr.SetMetaStore(d.metaStore)
	d.metaAuth = meta.NewAuthManager(d.metaStore, &http.Client{Timeout: 30 * time.Second})
	catalog.SetMetaKeyChecker(func() bool { return d.metaStore.HasUsableAPIKey() })

	d.backend = backend.New()
	d.proxy = proxyFactory()

	d.usage = usage.NewTracker()
	d.usage.SetOnChange(d.notifyStateChanged)

	d.updater = updater.New(updater.Options{
		OnStateChanged:    d.notifyStateChanged,
		PostMessage:       d.postMessageEvent,
		OnUpdateAvailable: d.onUpdateAvailable,
	})

	d.server = control.NewServer(d)
	d.web = webui.NewServer(d)
	d.wireEvents()

	d.authMgr.CheckAuthStatus()
	d.startAuthMonitor()
	d.refreshUsage()

	// Start the servers automatically (AppDelegate parity). The automatic
	// start stays quiet: systemd starts us at every login, and a login toast
	// every session would be noise rather than signal.
	d.startServer(false)

	if d.metaAuth.HasCredentials() {
		d.metaAuth.RefreshAPIKeyIfNeeded(false, nil)
	}
	go d.metaKeyRefreshLoop()
	d.updater.StartScheduled(d.ctx)

	// The control API comes up last: once it answers, clients expect every
	// action to work. The settings web UI serves alongside it; a UI bind
	// failure is logged but not fatal (the proxy itself still works).
	apiErr := make(chan error, 1)
	go func() { apiErr <- d.server.ListenAndServe(d.ctx, "") }()
	go func() {
		if err := d.web.ListenAndServe(d.ctx); err != nil {
			logx.Logf("[Daemon] Settings web UI failed to start: %v", err)
		}
	}()

	select {
	case <-d.ctx.Done():
		d.shutdown()
		return nil
	case err := <-apiErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		d.shutdown()
		return nil
	}
}

func (d *Daemon) shutdown() {
	if d.monitor != nil {
		d.monitor.Stop()
	}
	// Stop the thinking proxy first to stop accepting new requests, then the
	// backend.
	d.proxy.Stop()
	d.backend.Stop()
	_ = d.server.Close()
	_ = d.web.Close()
}

// wireEvents bridges the in-process event bus (the NotificationCenter
// replacement) to control API state notifications.
func (d *Daemon) wireEvents() {
	events.Subscribe(events.ServerStatusChanged, d.notifyStateChanged)
	events.Subscribe(events.AuthDirectoryChanged, d.onAuthDirectoryChanged)
	events.Subscribe(events.MetaAccountsChanged, d.onMetaAccountsChanged)
	events.Subscribe(events.MetaUsageChanged, d.onMetaUsageChanged)
	events.Subscribe(events.PrefsChanged, d.notifyStateChanged)
	d.authMgr.SetOnChange(d.onAuthStateChanged)
	d.metaAuth.SetOnChange(func() {
		d.invalidateFactory()
		d.notifyStateChanged()
	})
}

func (d *Daemon) startAuthMonitor() {
	// AppDelegate's watcher: migrate legacy Claude seat files, then let the
	// AuthDirectoryChanged handlers rescan.
	d.monitor = auth.NewDirectoryMonitor(authRefreshDebounce, "[Daemon]", func() {
		claude.MigrateCanonicalFiles(paths.AuthDir())
		events.Publish(events.AuthDirectoryChanged)
	})
	if err := d.monitor.Start(); err != nil {
		logx.Logf("[Daemon] Auth directory monitor failed to start: %v", err)
	}
}

func (d *Daemon) onAuthDirectoryChanged() {
	d.authMgr.CheckAuthStatus()
	// SetOnChange fires for the rescan; notify explicitly as well in case the
	// scan found nothing new.
	d.notifyStateChanged()
}

func (d *Daemon) onMetaAccountsChanged() {
	// Regenerate the merged config so Completions keys/failover follow the
	// account change; the backend hot-reloads it.
	d.backend.RegenerateConfig()
	d.authMgr.CheckAuthStatus()
	d.invalidateFactory()
	d.notifyStateChanged()
}

// onMetaUsageChanged refreshes just the Meta usage cards from the local
// last-observed store. ThinkingProxy publishes this whenever it sniffs a new
// `response.subscription_usage` event, so Meta quota updates live without
// refetching the other providers.
func (d *Daemon) onMetaUsageChanged() {
	d.usage.UpdateMetaAccounts(d.authMgr.Accounts(auth.Meta))
	d.notifyStateChanged()
}

func (d *Daemon) onAuthStateChanged() {
	d.invalidateFactory()
	if sig := d.usageSignature(); sig != d.usageSig {
		d.mu.Lock()
		d.usageSig = sig
		d.mu.Unlock()
		d.refreshUsage()
	}
	d.notifyStateChanged()
}

// usageSignature mirrors SettingsView.codexUsageAccountSignature, extended to
// every usage-tracked provider: enabled, unexpired Codex, Claude, and Grok
// account ids plus enabled Meta ids (Meta ignores key expiry). Only a change
// here refreshes the quota tracker.
func (d *Daemon) usageSignature() string {
	var parts []string
	for _, st := range []auth.ServiceType{auth.Codex, auth.Claude, auth.Grok, auth.Meta} {
		var ids []string
		for _, a := range d.authMgr.Accounts(st) {
			if a.Disabled {
				continue
			}
			if st != auth.Meta && a.IsExpired() {
				continue
			}
			ids = append(ids, a.ID)
		}
		sort.Strings(ids)
		parts = append(parts, strings.Join(ids, "|"))
	}
	return strings.Join(parts, "||")
}

func (d *Daemon) refreshUsage() {
	codex := d.authMgr.Accounts(auth.Codex)
	claudeAccounts := d.authMgr.Accounts(auth.Claude)
	grokAccounts := d.authMgr.Accounts(auth.Grok)
	metaAccounts := d.authMgr.Accounts(auth.Meta)
	go d.usage.Refresh(codex, claudeAccounts, grokAccounts, metaAccounts)
}

func (d *Daemon) metaKeyRefreshLoop() {
	ticker := time.NewTicker(metaKeyRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.metaAuth.RefreshAPIKeyIfNeeded(false, nil)
		}
	}
}

// ---- server lifecycle (AppDelegate.startServer/stopServer parity) --------

// startServer starts ThinkingProxy then the backend. notifyUser controls the
// desktop notifications: user-initiated starts announce themselves, the
// automatic start at daemon boot does not.
func (d *Daemon) startServer(notifyUser bool) {
	d.mu.Lock()
	if d.starting || (d.proxy.IsRunning() && d.backend.IsRunning()) {
		d.mu.Unlock()
		return
	}
	d.starting = true
	d.mu.Unlock()
	d.notifyStateChanged()

	go func() {
		defer func() {
			d.mu.Lock()
			d.starting = false
			d.mu.Unlock()
			d.notifyStateChanged()
		}()

		if err := d.proxy.Start(); err != nil {
			d.setLastError(fmt.Sprintf("Could not start thinking proxy on port %d: %v", ProxyPort, err))
			if notifyUser {
				d.notifyUser("Server Failed", fmt.Sprintf("Could not start thinking proxy on port %d", ProxyPort))
			}
			return
		}

		// Poll for proxy readiness with timeout (AppDelegate parity).
		ready := false
		for i := 0; i < proxyReadinessAttempts; i++ {
			if d.proxy.IsRunning() {
				ready = true
				break
			}
			time.Sleep(proxyReadinessInterval)
		}
		if !ready {
			d.proxy.Stop()
			d.setLastError(fmt.Sprintf("Could not start thinking proxy on port %d (timeout)", ProxyPort))
			if notifyUser {
				d.notifyUser("Server Failed", fmt.Sprintf("Could not start thinking proxy on port %d (timeout)", ProxyPort))
			}
			return
		}

		if !d.backend.Start() {
			// Backend failed - stop the proxy to keep state consistent.
			d.proxy.Stop()
			d.setLastError(fmt.Sprintf("Could not start backend server on port %d", backend.BackendPort))
			if notifyUser {
				d.notifyUser("Server Failed", fmt.Sprintf("Could not start backend server on port %d", backend.BackendPort))
			}
			return
		}

		d.setLastError("")
		if notifyUser {
			d.notifyUser("Server Started", "DroidProxy is now running")
		}
	}()
}

// stopServer stops the thinking proxy first (no new requests), then the
// backend. Blocks for the graceful window.
func (d *Daemon) stopServer() {
	d.proxy.Stop()
	d.backend.Stop()
}

// serverRunning reports whether both local servers are up.
func (d *Daemon) serverRunning() bool {
	return d.proxy.IsRunning() && d.backend.IsRunning()
}

func (d *Daemon) setLastError(msg string) {
	d.mu.Lock()
	d.lastError = msg
	d.mu.Unlock()
	d.notifyStateChanged()
}

// ---- notifications and messages -------------------------------------------

func (d *Daemon) notifyStateChanged() {
	if d.server != nil {
		d.server.NotifyStateChanged()
	}
	if d.web != nil {
		d.web.NotifyStateChanged()
	}
}

// postMessageEvent broadcasts an async result to control API clients (the
// macOS app's "Authentication Result" alerts) and web UI browsers.
func (d *Daemon) postMessageEvent(title, body, level string) {
	if d.server != nil {
		d.server.PostMessage(title, body, level)
	}
	if d.web != nil {
		d.web.PostMessage(title, body, level)
	}
}

func (d *Daemon) notifyUser(title, body string) {
	if _, err := notify.Notify(title, body, notify.Options{}); err != nil {
		logx.Logf("[Daemon] Notification failed: %v", err)
	}
}

// onUpdateAvailable shows the Sparkle-style update notification with an
// Install button.
func (d *Daemon) onUpdateAvailable() {
	snap := d.updater.Snapshot()
	_, _ = notify.Notify("DroidProxy Update", "DroidProxy "+snap.LatestVersion+" is available.", notify.Options{
		Actions: []notify.Action{
			{Key: "install", Label: "Install Update"},
			{Key: "later", Label: "Later"},
		},
		OnAction: func(key string) {
			if key != "install" {
				return
			}
			go func() {
				if err := d.updater.Install(d.ctx); err != nil {
					logx.Logf("[Daemon] Update install failed: %v", err)
				}
			}()
		},
	})
}

// quit stops the servers and exits: under systemd by stopping our own unit
// (SIGTERM then does the graceful path), otherwise by cancelling the daemon
// context.
func (d *Daemon) quit() {
	d.stopServer()
	time.Sleep(300 * time.Millisecond)
	if desktop.UnderSystemd() {
		_ = desktop.Stop()
		return
	}
	d.cancel()
}
