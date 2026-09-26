package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/backend"
	"github.com/nikships/droidproxy-omarchy/internal/catalog"
	"github.com/nikships/droidproxy-omarchy/internal/control"
	"github.com/nikships/droidproxy-omarchy/internal/desktop"
	"github.com/nikships/droidproxy-omarchy/internal/grok"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
	"github.com/nikships/droidproxy-omarchy/internal/webui"
)

// authResultTitle is the dialog title the macOS app used for every
// authentication-flow alert, including the Factory-models apply result.
const authResultTitle = "Authentication Result"

// Call dispatches one control API action (docs/control-api.md is the
// contract).
func (d *Daemon) Call(ctx context.Context, method string, params json.RawMessage) control.CallResult {
	switch method {
	case "server.start":
		d.startServer(true)
		return control.OK("")

	case "server.stop":
		go func() {
			d.stopServer()
			d.notifyStateChanged()
		}()
		return control.OK("")

	case "server.toggle":
		if d.serverRunning() {
			return d.Call(ctx, "server.stop", params)
		}
		return d.Call(ctx, "server.start", params)

	case "server.restart":
		go func() {
			d.stopServer()
			time.Sleep(serverRestartDelay)
			d.startServer(true)
		}()
		return control.OK("")

	case "server.copyUrl":
		if !d.serverRunning() {
			return control.Fail("Server is not running.")
		}
		return d.copyServerURL()

	case "open.dashboard":
		if !d.serverRunning() {
			return control.Fail("Server is not running.")
		}
		bind := prefs.BindAddress()
		dashHost := bind
		if bind == "0.0.0.0" {
			dashHost = "127.0.0.1"
		}
		if err := desktop.OpenURL(fmt.Sprintf("http://%s:%d/management.html", dashHost, backend.BackendPort)); err != nil {
			return control.Fail("Could not open the dashboard: " + err.Error())
		}
		return control.OK("")

	case "open.authFolder":
		if err := desktop.OpenPath(paths.AuthDir()); err != nil {
			return control.Fail("Could not open the folder: " + err.Error())
		}
		return control.OK("")

	case "open.logsFolder":
		if err := ensureLogsDir(); err != nil {
			return control.Fail("Could not create the logs folder: " + err.Error())
		}
		if err := desktop.OpenPath(paths.BackendLogsDir()); err != nil {
			return control.Fail("Could not open the folder: " + err.Error())
		}
		return control.OK("")

	case "open.webui":
		if err := desktop.OpenURL(webui.URL()); err != nil {
			return control.Fail("Could not open the settings page: " + err.Error())
		}
		return control.OK("")

	case "open.url":
		var p struct {
			URL string `json:"url"`
		}
		if !parseParams(params, &p) || p.URL == "" {
			return control.Fail("Invalid parameters.")
		}
		if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
			return control.Fail("Only http(s) URLs can be opened.")
		}
		if err := desktop.OpenURL(p.URL); err != nil {
			return control.Fail("Could not open the URL: " + err.Error())
		}
		return control.OK("")

	case "clipboard.copy":
		var p struct {
			Text string `json:"text"`
		}
		if !parseParams(params, &p) || p.Text == "" {
			return control.Fail("Invalid parameters.")
		}
		if err := desktop.CopyToClipboard(p.Text); err != nil {
			return control.Fail("Could not copy: " + err.Error())
		}
		return control.OK("")

	case "provider.setEnabled":
		var p struct {
			Provider string `json:"provider"`
			Enabled  bool   `json:"enabled"`
		}
		if !parseParams(params, &p) || p.Provider == "" {
			return control.Fail("Invalid parameters.")
		}
		return d.setProviderEnabled(p.Provider, p.Enabled)

	case "provider.connect":
		var p struct {
			Provider string `json:"provider"`
		}
		if !parseParams(params, &p) || p.Provider == "" {
			return control.Fail("Invalid parameters.")
		}
		return d.connectProvider(ctx, p.Provider)

	case "provider.cancelAuth":
		var p struct {
			Provider string `json:"provider"`
		}
		if !parseParams(params, &p) || p.Provider == "" {
			return control.Fail("Invalid parameters.")
		}
		return d.cancelProviderAuth(p.Provider)

	case "account.toggleDisabled":
		var p struct {
			Provider  string `json:"provider"`
			AccountID string `json:"accountId"`
		}
		if !parseParams(params, &p) || p.Provider == "" || p.AccountID == "" {
			return control.Fail("Invalid parameters.")
		}
		return d.toggleAccountDisabled(p.Provider, p.AccountID)

	case "account.remove":
		var p struct {
			Provider  string `json:"provider"`
			AccountID string `json:"accountId"`
		}
		if !parseParams(params, &p) || p.Provider == "" || p.AccountID == "" {
			return control.Fail("Invalid parameters.")
		}
		return d.removeAccount(p.Provider, p.AccountID)

	case "junie.saveKey":
		var p struct {
			APIKey string `json:"apiKey"`
		}
		if !parseParams(params, &p) || strings.TrimSpace(p.APIKey) == "" {
			return control.Fail("Please enter your JetBrains Junie API key.").WithTitle(authResultTitle)
		}
		if err := auth.SaveJunieAPIKey(strings.TrimSpace(p.APIKey)); err != nil {
			return control.Fail("Failed to save Junie API Key: " + err.Error()).WithTitle(authResultTitle)
		}
		d.authMgr.CheckAuthStatus()
		d.notifyStateChanged()
		return control.OK("✓ Successfully saved Junie API Key.").WithTitle(authResultTitle)

	case "settings.set":
		var p struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if !parseParams(params, &p) || p.Key == "" {
			return control.Fail("Invalid parameters.")
		}
		return d.setSetting(p.Key, p.Value)

	case "factory.apply":
		message, err := catalog.ApplyFactoryCustomModels(d.isProviderEnabled)
		d.invalidateFactory()
		d.notifyStateChanged()
		if err != nil {
			return control.Fail(message).WithTitle(authResultTitle)
		}
		return control.OK(message).WithTitle(authResultTitle)

	case "usage.refresh":
		d.refreshUsage()
		return control.OK("")

	case "update.check":
		go func() {
			if err := d.updater.Check(d.ctx); err != nil {
				logx.Logf("[Daemon] Update check failed: %v", err)
			}
		}()
		return control.OK("")

	case "update.install":
		snap := d.updater.Snapshot()
		if snap.State != control.UpdateAvailable {
			return control.Fail("No update is available. Check for updates first.")
		}
		version := snap.LatestVersion
		go func() {
			if err := d.updater.Install(d.ctx); err != nil {
				logx.Logf("[Daemon] Update install failed: %v", err)
			}
		}()
		return control.OK("Installing DroidProxy " + version + "…")

	case "app.quit":
		go d.quit()
		return control.OK("")
	}
	return control.Fail("Unknown method: " + method)
}

func ensureLogsDir() error {
	return paths.EnsureDir(paths.BackendLogsDir(), 0o700)
}

func (d *Daemon) copyServerURL() control.CallResult {
	host := prefs.BindAddress()
	displayHost := host
	if host == "0.0.0.0" {
		displayHost = "localhost"
	}
	url := fmt.Sprintf("http://%s:%d", displayHost, ProxyPort)
	if err := desktop.CopyToClipboard(url); err != nil {
		return control.Fail("Could not copy: " + err.Error())
	}
	d.notifyUser("Copied", "Server URL copied to clipboard")
	return control.OK("")
}

// ---- provider actions ------------------------------------------------------

// serviceTypeFromParam resolves a provider id, rejecting the two that need
// special handling.
func serviceTypeFromParam(provider string) (auth.ServiceType, control.CallResult) {
	st, ok := auth.ServiceTypeFromAuthFileType(provider)
	if !ok {
		return "", control.Fail("Unknown provider: " + provider)
	}
	if st == auth.Copilot {
		return "", control.Fail("GitHub Copilot is not supported on Linux.")
	}
	return st, control.OK("")
}

func (d *Daemon) setProviderEnabled(provider string, enabled bool) control.CallResult {
	st, res := serviceTypeFromParam(provider)
	if !res.OK {
		return res
	}

	d.backend.SetProviderEnabled(st, enabled)
	switch st {
	case auth.Meta:
		if enabled {
			d.metaAuth.RefreshAPIKeyIfNeeded(false, nil)
		} else {
			d.cancelMetaAuth()
		}
	}
	d.invalidateFactory()
	d.notifyStateChanged()
	return control.OK("")
}

func (d *Daemon) connectProvider(ctx context.Context, provider string) control.CallResult {
	st, res := serviceTypeFromParam(provider)
	if !res.OK {
		return res
	}
	switch st {
	case auth.Junie:
		// The plugin opens the key dialog and calls junie.saveKey.
		return control.Fail("Junie uses an API key. Enter it in the Add Junie API Key dialog.")
	case auth.Grok:
		d.startGrokLogin()
		return control.OK("")
	case auth.Meta:
		d.startMetaLogin()
		return control.OK("")
	default:
		return d.runBundledLogin(ctx, st)
	}
}

func (d *Daemon) cancelProviderAuth(provider string) control.CallResult {
	st, res := serviceTypeFromParam(provider)
	if !res.OK {
		return res
	}
	switch st {
	case auth.Grok:
		d.mu.Lock()
		session := d.grokSession
		d.grokSession = nil
		d.grokUserCode = ""
		d.grokVerifyURL = ""
		delete(d.loginInFlight, auth.Grok)
		d.mu.Unlock()
		if session != nil {
			session.Cancel()
		}
	case auth.Meta:
		d.cancelMetaAuth()
	}
	d.notifyStateChanged()
	return control.OK("")
}

func (d *Daemon) cancelMetaAuth() {
	d.metaAuth.CancelAuthentication()
	d.mu.Lock()
	d.metaDeviceCode = ""
	d.metaVerifyURL = ""
	d.mu.Unlock()
	d.notifyStateChanged()
}

// bundledLoginCommands maps providers to the bundled binary's login flows.
func bundledLoginCommands() map[auth.ServiceType]backend.AuthCommand {
	return map[auth.ServiceType]backend.AuthCommand{
		auth.Claude:      backend.ClaudeLogin,
		auth.Codex:       backend.CodexLogin,
		auth.Antigravity: backend.AntigravityLogin,
		auth.Kimi:        backend.KimiLogin,
	}
}

// runBundledLogin starts a CLIProxyAPI login flow and returns its result
// (~1s, matching the macOS alert timing). If the caller disconnects first,
// the outcome is delivered as a message event instead.
func (d *Daemon) runBundledLogin(ctx context.Context, st auth.ServiceType) control.CallResult {
	command, ok := bundledLoginCommands()[st]
	if !ok {
		return control.Fail("Unknown provider: " + string(st))
	}

	d.mu.Lock()
	if d.loginInFlight[st] {
		d.mu.Unlock()
		return control.Fail("A " + st.DisplayName() + " login is already in progress.")
	}
	d.loginInFlight[st] = true
	d.mu.Unlock()
	d.notifyStateChanged()

	type outcome struct {
		ok     bool
		output string
	}
	ch := make(chan outcome, 1)
	d.backend.RunAuthCommandAsync(command, func(ok bool, output string) {
		ch <- outcome{ok: ok, output: output}
	})

	finish := func(res outcome) control.CallResult {
		d.mu.Lock()
		delete(d.loginInFlight, st)
		d.mu.Unlock()
		d.notifyStateChanged()
		if res.ok {
			return control.OK(successMessage(st)).WithTitle(authResultTitle)
		}
		details := res.output
		if details == "" {
			details = "No output from authentication process"
		}
		return control.Fail("Authentication failed. Please check if the browser opened and try again.\n\nDetails: " + details).WithTitle(authResultTitle)
	}

	select {
	case res := <-ch:
		return finish(res)
	case <-ctx.Done():
		go func() {
			result := finish(<-ch)
			level := control.LevelInfo
			if !result.OK {
				level = control.LevelError
			}
			d.postMessageEvent(authResultTitle, callResultBody(result), level)
		}()
		return control.OK("")
	}
}

// callResultBody extracts the user-facing text of a CallResult.
func callResultBody(r control.CallResult) string {
	if r.Message != "" {
		return r.Message
	}
	return r.Error
}

// successMessage returns the exact macOS alert text per provider. The
// Antigravity hint's terminal command is adapted to the Linux binary path.
func successMessage(st auth.ServiceType) string {
	switch st {
	case auth.Claude:
		return "🌐 Browser opened for Claude Code authentication.\n\nPlease complete the login in your browser.\n\nThe app will automatically detect your credentials."
	case auth.Codex:
		return "🌐 Browser opened for Codex authentication.\n\nPlease complete the login in your browser.\n\nThe app will automatically detect your credentials."
	case auth.Antigravity:
		return "🌐 Browser opened for Antigravity authentication.\n\nYou must have Google Antigravity installed before adding an Antigravity account.\n\nPlease complete the login in your browser.\n\nThe app will automatically detect your credentials.\n\nIf having issues, run in terminal:\n" +
			paths.BundledCLIProxyAPI() + " --config ~/.cli-proxy-api/merged-config.yaml -antigravity-login"
	case auth.Kimi:
		return "🌐 Browser opened for Kimi authentication.\n\nPlease complete the login in your browser.\n\nThe app will automatically detect your credentials."
	case auth.Junie:
		return "✓ Successfully saved Junie API Key."
	case auth.Grok:
		return "🌐 Browser opened for Grok (xAI) authentication.\n\nApprove access for SuperGrok / X Premium+, then DroidProxy will save credentials automatically."
	case auth.Meta:
		return "🌐 Meta Muse sign-in started."
	}
	return ""
}

// startGrokLogin is SettingsView.startGrokOAuthLogin: replace any running
// session, show the user code, and ignore stale completions.
func (d *Daemon) startGrokLogin() {
	d.mu.Lock()
	if d.grokSession != nil {
		d.grokSession.Cancel()
		d.grokSession = nil
	}
	d.grokUserCode = ""
	d.grokVerifyURL = ""
	d.loginInFlight[auth.Grok] = true
	d.mu.Unlock()
	d.notifyStateChanged()

	// Holder pattern: the closures must identity-check the session after
	// StartDeviceLogin returns, so the variable is declared up front.
	var session *grok.LoginSession
	session = grok.StartDeviceLogin(
		func(authz grok.DeviceAuthorization) {
			d.mu.Lock()
			d.grokUserCode = authz.UserCode
			d.grokVerifyURL = authz.VerificationURIComplete
			d.mu.Unlock()
			d.postMessageEvent(authResultTitle,
				"🌐 Browser opened for Grok login.\n\nIf prompted, enter code: "+authz.UserCode+"\n\nWaiting for approval…",
				control.LevelInfo)
			d.notifyStateChanged()
		},
		func(creds grok.Credentials, authErr *grok.AuthError) {
			d.mu.Lock()
			current := d.grokSession
			d.grokSession = nil
			d.grokUserCode = ""
			d.grokVerifyURL = ""
			delete(d.loginInFlight, auth.Grok)
			d.mu.Unlock()
			if current != session {
				return // stale completion from a cancelled/replaced session
			}
			switch {
			case authErr == nil:
				who := creds.Email
				if who == "" {
					who = "grok-user"
				}
				d.authMgr.CheckAuthStatus()
				d.postMessageEvent(authResultTitle,
					"✓ Grok OAuth connected as "+who+".\n\nSelect DroidProxy: Grok 4.7 or DroidProxy: Grok 4.7 Fast in Droid with `/model`.",
					control.LevelInfo)
			case authErr.Kind == grok.AuthErrCancelled:
				// The replacement session already cleared the state.
			case authErr.Kind == grok.AuthErrReauthRequired:
				d.postMessageEvent(authResultTitle, "Grok session expired. Reconnect Grok in Settings.", control.LevelError)
			default:
				d.postMessageEvent(authResultTitle, "Grok login failed: "+authErr.Error(), control.LevelError)
			}
			d.notifyStateChanged()
		},
	)
	d.mu.Lock()
	d.grokSession = session
	d.mu.Unlock()
}

// startMetaLogin is SettingsView.startMetaAuthentication.
func (d *Daemon) startMetaLogin() {
	d.mu.Lock()
	d.metaDeviceCode = ""
	d.metaVerifyURL = ""
	d.mu.Unlock()
	d.notifyStateChanged()
	d.metaAuth.StartAuthentication(
		func(code, verificationURL string) {
			d.mu.Lock()
			d.metaDeviceCode = code
			d.metaVerifyURL = verificationURL
			d.mu.Unlock()
			d.notifyStateChanged()
		},
		func(err error) {
			d.mu.Lock()
			d.metaDeviceCode = ""
			d.metaVerifyURL = ""
			d.mu.Unlock()
			if err == nil {
				d.postMessageEvent(authResultTitle,
					"Meta Muse connected.\n\nMuse Spark 1.3 and Muse Spark 1.3 Contributor are now available. Re-apply Factory custom models to pick them up.",
					control.LevelInfo)
			} else {
				d.postMessageEvent(authResultTitle, "Meta Muse authentication failed: "+err.Error(), control.LevelError)
			}
			d.invalidateFactory()
			d.notifyStateChanged()
		},
	)
}

// ---- account actions -------------------------------------------------------

func (d *Daemon) findAccount(provider, accountID string) (auth.Account, control.CallResult) {
	st, res := serviceTypeFromParam(provider)
	if !res.OK {
		return auth.Account{}, res
	}
	for _, a := range d.authMgr.Accounts(st) {
		if a.ID == accountID {
			return a, control.OK("")
		}
	}
	return auth.Account{}, control.Fail("Unknown account.")
}

func (d *Daemon) toggleAccountDisabled(provider, accountID string) control.CallResult {
	account, res := d.findAccount(provider, accountID)
	if !res.OK {
		return res.WithTitle(authResultTitle)
	}
	if !d.authMgr.ToggleAccountDisabled(account) {
		return control.Fail("Failed to update " + account.DisplayName() + ". Please try again.").WithTitle(authResultTitle)
	}
	// The account snapshot is pre-toggle: was disabled means now enabled.
	message := "✓ Disabled " + account.DisplayName()
	if account.Disabled {
		message = "✓ Enabled " + account.DisplayName()
		if account.Type == auth.Meta {
			d.metaAuth.RefreshAPIKeyIfNeeded(false, nil)
		}
	}
	d.invalidateFactory()
	d.notifyStateChanged()
	return control.OK(message).WithTitle(authResultTitle)
}

func (d *Daemon) removeAccount(provider, accountID string) control.CallResult {
	account, res := d.findAccount(provider, accountID)
	if !res.OK {
		return res.WithTitle(authResultTitle)
	}

	wasRunning := d.serverRunning()
	cleanup := func() control.CallResult {
		var result control.CallResult
		if d.authMgr.DeleteAccount(account) {
			result = control.OK("✓ Removed " + account.DisplayName() + " from " + account.Type.DisplayName())
		} else {
			result = control.Fail("Failed to remove account")
		}
		d.invalidateFactory()
		if wasRunning {
			go func() {
				time.Sleep(serverRestartDelay)
				d.startServer(false)
			}()
		}
		d.notifyStateChanged()
		return result.WithTitle(authResultTitle)
	}

	if wasRunning {
		// Stop the server, delete the file, restart (SettingsView parity:
		// the backend may hold the credential in memory).
		go func() {
			d.stopServer()
			_ = cleanup()
		}()
		return control.OK("")
	}
	return cleanup()
}

// ---- settings --------------------------------------------------------------

// setBoolPref decodes raw as a bool and persists it under key.
func setBoolPref(key string, raw json.RawMessage) error {
	var v bool
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return errors.New("invalid value")
	}
	return prefs.Shared().Set(key, v)
}

// setStringPref decodes raw as a string and persists it under key.
func setStringPref(key string, raw json.RawMessage) error {
	var v string
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return errors.New("invalid value")
	}
	return prefs.Shared().Set(key, v)
}

func (d *Daemon) setSetting(key string, value json.RawMessage) control.CallResult {
	fail := func(err error) control.CallResult {
		return control.Fail("Could not set " + key + ": " + err.Error())
	}

	switch key {
	case "launchAtLogin":
		var enabled bool
		if len(value) == 0 || json.Unmarshal(value, &enabled) != nil {
			return control.Fail("Invalid value.")
		}
		var err error
		if enabled {
			err = desktop.Enable()
		} else {
			err = desktop.Disable()
		}
		if err != nil {
			return fail(err)
		}
		d.invalidateLaunchAtLogin()

	case "allowRemote":
		if err := setBoolPref(prefs.KeyAllowRemote, value); err != nil {
			return fail(err)
		}
		d.backend.GenerateConfig()

	case "secretKey":
		if err := setStringPref(prefs.KeySecretKey, value); err != nil {
			return fail(err)
		}
		d.backend.GenerateConfig()

	case "bindAddress":
		if err := setStringPref(prefs.KeyBindAddress, value); err != nil {
			return fail(err)
		}
		d.backend.GenerateConfig()

	case "verboseLogging":
		if err := setBoolPref(prefs.KeyVerboseLogging, value); err != nil {
			return fail(err)
		}
		d.backend.GenerateConfig()

	case "sequentialAccountFailover":
		var enabled bool
		if len(value) == 0 || json.Unmarshal(value, &enabled) != nil {
			return control.Fail("Invalid value.")
		}
		d.backend.SetSequentialAccountFailover(enabled)

	case "beta":
		if err := setBoolPref(prefs.KeyBetaFlag, value); err != nil {
			return fail(err)
		}
		d.invalidateFactory()

	case "oledTheme":
		if err := setBoolPref(prefs.KeyOLEDTheme, value); err != nil {
			return fail(err)
		}

	case "backgroundOpacity":
		var opacity float64
		if len(value) == 0 || json.Unmarshal(value, &opacity) != nil {
			return control.Fail("Invalid value.")
		}
		if opacity < 0.10 {
			opacity = 0.10
		}
		if opacity > 1.0 {
			opacity = 1.0
		}
		if err := prefs.Shared().Set(prefs.KeyBackgroundOpacity, opacity); err != nil {
			return fail(err)
		}

	case "gpt6AstraFastMode":
		if err := setBoolPref(prefs.KeyGPT6AstraFastMode, value); err != nil {
			return fail(err)
		}
	case "gpt6SolFastMode":
		if err := setBoolPref(prefs.KeyGPT6SolFastMode, value); err != nil {
			return fail(err)
		}
	case "gpt6LunaFastMode":
		if err := setBoolPref(prefs.KeyGPT6LunaFastMode, value); err != nil {
			return fail(err)
		}

	case "metaContributorMode":
		if err := setBoolPref(prefs.KeyMetaContributorMode, value); err != nil {
			return fail(err)
		}
		d.invalidateFactory()

	case "autoCheckUpdates":
		if err := setBoolPref(prefs.KeyAutoCheckUpdates, value); err != nil {
			return fail(err)
		}
	case "autoInstallUpdates":
		if err := setBoolPref(prefs.KeyAutoInstallUpdates, value); err != nil {
			return fail(err)
		}

	default:
		return control.Fail("Unknown setting: " + key)
	}

	d.notifyStateChanged()
	return control.OK("")
}

// parseParams decodes a params object ({} when empty).
func parseParams(params json.RawMessage, v any) bool {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	return json.Unmarshal(params, v) == nil
}
