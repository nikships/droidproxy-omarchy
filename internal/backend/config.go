package backend

import (
	"fmt"
	"strings"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
)

// BackendPort is the CLIProxyAPI listener; ThinkingProxy forwards from 8317.
const BackendPort = 8318

// oauthProviderKeys maps providers to their `oauth-excluded-models` key in the
// merged config. Copilot is absent because it is not ported; Meta is absent
// because its exclusion is expressed through the compatibility config block.
var oauthProviderKeys = map[auth.ServiceType]string{
	auth.Claude:      "claude",
	auth.Codex:       "codex",
	auth.Antigravity: "antigravity",
	auth.Kimi:        "kimi",
	auth.Junie:       "junie",
	auth.Grok:        "grok",
}

// accountFailoverOverrides are the substitutions applied to the bundled
// config.yaml when "Sequential account failover" is enabled. Each pair is
// (bundled OFF value, ON value). The OFF strings must match
// packaging/config.yaml exactly; the anchor tests fail loudly if they drift.
//
// Together these switch the backend from "spread requests evenly and never
// park anything" to "burn one account at a time, then park it once its quota
// is gone":
//
//   - fill-first consumes one account fully before moving to the next,
//     keeping the remaining accounts untouched as reserve capacity.
//   - disable-cooling: false lets a quota-exhausted account be parked and
//     skipped. Without it, fill-first would re-probe the depleted account at
//     the front of the queue on every request.
//   - transient-error-cooldown-seconds: -1 keeps issue #57 fixed: transient
//     5xx failures still never park an auth, so the pool cannot black out.
//   - max-retry-interval: 30 waits briefly for the soonest account to recover
//     instead of failing a request while everything is cooling down.
var accountFailoverOverrides = [][2]string{
	{"max-retry-interval: 0", "max-retry-interval: 30"},
	{"disable-cooling: true", "disable-cooling: false"},
	{"transient-error-cooldown-seconds: 0", "transient-error-cooldown-seconds: -1"},
	{`  strategy: "round-robin"`, `  strategy: "fill-first"`},
}

// ApplyAccountFailoverOverrides applies the sequential-account-failover
// substitutions to raw config YAML. Pure string transformation, kept out of
// GenerateConfig so it can be unit tested without a bundle.
func ApplyAccountFailoverOverrides(config string, enabled bool) string {
	if !enabled {
		return config
	}
	updated := config
	for _, rule := range accountFailoverOverrides {
		if !strings.Contains(updated, rule[0]) {
			logx.Logf("[ServerManager] Warning: failover anchor '%s' missing from bundled config; override not applied", rule[0])
			continue
		}
		updated = strings.ReplaceAll(updated, rule[0], rule[1])
	}
	return updated
}

// configOptions carries every value renderConfig substitutes into the bundled
// template. Split out so the pure rendering can be tested without prefs.
type configOptions struct {
	BindAddress        string
	AllowRemote        bool
	SecretKey          string
	VerboseLogging     bool
	SequentialFailover bool
	// DisabledProviders holds the sorted oauth-excluded-models keys of every
	// provider toggled off.
	DisabledProviders []string
	// MetaBlock is the Meta Muse compatibility config block, "" when none.
	MetaBlock string
}

// renderConfig merges user settings and provider exclusions into the bundled
// config template, mirroring ServerManager.getConfigPath's substitutions.
func renderConfig(template string, opts configOptions) string {
	config := template

	// bindAddress is user-controlled (already validated in prefs). Replace
	// only the first `host:` anchor rather than every occurrence, and warn if
	// the expected anchor is missing so silent config drift is visible.
	if strings.Contains(config, "host: 127.0.0.1") {
		config = strings.Replace(config, "host: 127.0.0.1", "host: "+opts.BindAddress, 1)
	} else {
		logx.Logf("[ServerManager] Warning: 'host: 127.0.0.1' anchor not found in bundled config; bind address not applied")
	}

	config = strings.ReplaceAll(config, "  allow-remote: false", fmt.Sprintf("  allow-remote: %v", opts.AllowRemote))
	config = strings.ReplaceAll(config, `  secret-key: ""  # Leave empty to disable management API`, "  secret-key: \""+opts.SecretKey+"\"")
	config = strings.ReplaceAll(config, "debug: false", fmt.Sprintf("debug: %v", opts.VerboseLogging))
	config = strings.ReplaceAll(config, "logging-to-file: false", fmt.Sprintf("logging-to-file: %v", opts.VerboseLogging))
	config = ApplyAccountFailoverOverrides(config, opts.SequentialFailover)

	if len(opts.DisabledProviders) > 0 {
		var b strings.Builder
		b.WriteString("\n# Provider exclusions (auto-added by DroidProxy)\noauth-excluded-models:\n")
		for _, provider := range opts.DisabledProviders {
			fmt.Fprintf(&b, "  %s:\n    - \"*\"\n", provider)
		}
		config += b.String()
	}

	// Completions still go through the compatibility block (and its account
	// failover). Responses are TLS-forwarded by ThinkingProxy — CLIProxyAPI
	// would otherwise rewrite them into /chat/completions.
	return config + opts.MetaBlock
}
