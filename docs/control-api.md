# DroidProxy control API

The `droidproxy serve` daemon exposes a local control API. The Omarchy shell
plugin, the `droidproxy` CLI, and scripts all use it. It replaces the macOS
app's in-process calls between `AppDelegate`, `SettingsView`, and the managers.

## Transport

- HTTP/1.1 over a Unix socket at `$XDG_RUNTIME_DIR/droidproxy/control.sock`.
  The socket directory is `0700` and the socket is `0600`, so only the owning
  user can connect. `DROIDPROXY_SOCKET` overrides the path.
- `GET  /v1/state` returns the current [state snapshot](#state-snapshot).
- `GET  /v1/events` streams newline-delimited JSON (NDJSON) [events](#events).
- `POST /v1/call/<method>` runs an [action](#actions). The body is a JSON
  object of parameters (may be empty `{}`).

## CLI wrapper (what the QML plugin uses)

QML can't speak HTTP over a Unix socket, so the plugin runs the CLI through
Quickshell's `Process`:

| Command | Output |
|---|---|
| `droidproxy ctl state` | One JSON state snapshot. Exit 1 plus `{"type":"offline"}` if the daemon isn't reachable. |
| `droidproxy ctl watch` | NDJSON on stdout, one event per line, forever. When the daemon is unreachable it prints `{"type":"offline"}` once, retries every 2 s, and resumes with a fresh `state` event when the daemon comes back. It never exits on its own. |
| `droidproxy ctl call <method> ['<json params>']` | One JSON [action result](#action-result). Exit 0 when `ok` is true, otherwise 1. |

## Events

Every line of `/v1/events` (and `droidproxy ctl watch`) is one of:

```json
{"type":"state","state":{ ...state snapshot... }}
{"type":"message","id":"b3c1…","title":"Authentication Result","body":"✓ Grok OAuth connected as a@b.com.","level":"info"}
{"type":"offline"}
```

- `state` is sent immediately on connect and again after any change
  (coalesced to at most one every 100 ms). It is always a full snapshot.
- `message` carries the result of an asynchronous flow that finishes after
  the action call returned (device-code logins, update install). The plugin shows it the same way the macOS app showed its
  "Authentication Result" alert. `level` is `info` or `error`.
- `offline` is only produced by the CLI wrapper, never by the daemon.

## State snapshot

All keys are always present. Strings are never `null`; use `""`.

```jsonc
{
  "app": {
    "name": "DroidProxy",
    "version": "1.0.3",          // buildinfo.Version
    "commit": "abc1234",
    "repoUrl": "https://github.com/nikships/droidproxy-omarchy",
    "issuesUrl": "https://github.com/nikships/droidproxy-omarchy/issues",
    "cliProxyApiUrl": "https://github.com/router-for-me/CLIProxyAPI",
    "cliProxyApiVersion": "7.3.13"
  },
  "server": {
    "running": true,               // ThinkingProxy (:8317) and CLIProxyAPI (:8318) both up
    "starting": false,
    "proxyPort": 8317,
    "backendPort": 8318,
    "url": "http://localhost:8317",                       // what "Copy Server URL" copies
    "dashboardUrl": "http://127.0.0.1:8318/management.html",
    "lastError": ""                // e.g. "Could not start thinking proxy on port 8317 (timeout)"
  },
  "settings": {
    "launchAtLogin": true,         // systemctl --user is-enabled droidproxy.service
    "allowRemote": false,
    "secretKey": "",
    "bindAddress": "127.0.0.1",    // raw stored value (only applied when beta is on)
    "beta": false,
    "verboseLogging": false,
    "sequentialAccountFailover": false,
    "oledTheme": false,
    "backgroundOpacity": 0.55,
    "gpt6AstraFastMode": false,
    "gpt6SolFastMode": false,
    "gpt6LunaFastMode": false,
    "metaContributorMode": false,
    "autoCheckUpdates": true,
    "autoInstallUpdates": false
  },
  "paths": {
    "authDir": "/home/u/.cli-proxy-api",
    "logsDir": "/home/u/.cli-proxy-api/logs",
    "factorySettings": "/home/u/.factory/settings.json",
    "debugLog": "/home/u/.local/state/droidproxy/droidproxy-debug.log"
  },
  "factory": {
    "modelsInstalled": true        // checkFactoryModelsInstalled()
  },
  // Display order matches the macOS Settings "Services" section.
  "providers": [
    {
      "id": "claude",              // ServiceType raw value
      "name": "Claude Code",       // ServiceType.displayName
      "icon": "icon-claude.png",   // file under the plugin's assets/icons/
      "color": "#D97757",          // toggle tint from SettingsView
      "kind": "standard",          // standard | meta | junie | grok
      "enabled": true,             // provider-level toggle
      "authenticating": false,
      "help": "",                  // tooltip/help text ("" if none)
      "accounts": [
        {
          "id": "claude-a@b.com.json",   // file name (or Meta account id)
          "displayName": "a@b.com · Personal Max",
          "email": "a@b.com",
          "expired": false,
          "disabled": false
        }
      ]
    }
    // codex, meta, antigravity, kimi, junie, grok
  ],
  "meta": {
    "authenticating": false,
    "deviceCode": "",
    "verificationUrl": "",
    "lastError": ""
  },
  "grok": {
    "authenticating": false,
    "userCode": "",
    "verificationUrl": ""
  },
  "usage": {
    "visible": true,               // codex/claude enabled or has accounts
    "refreshing": false,
    "accounts": [
      {
        "provider": "codex",
        "providerName": "Codex",
        "email": "a@b.com",
        "loading": false,
        "error": "",
        "windows": [
          { "title": "5-hour", "remainingPercent": 72.5, "hasRemaining": true, "resetText": "Resets in 2h 10m" }
        ]
      }
    ]
  },
  "update": {
    "state": "idle",               // idle | checking | upToDate | available | downloading | installing | error
    "currentVersion": "1.0.3",
    "latestVersion": "",
    "notes": "",                   // release notes (markdown) for latestVersion
    "releaseUrl": "",
    "error": "",
    "lastChecked": "",             // RFC 3339 or ""
    "progress": 0                  // 0..1 while downloading
  }
}
```

## Actions

`POST /v1/call/<method>` with a JSON object body. Names are stable.

| Method | Params | Effect (macOS equivalent) |
|---|---|---|
| `server.start` | – | Start ThinkingProxy then CLIProxyAPI (`startServer`). |
| `server.stop` | – | Stop both (`stopServer`). |
| `server.toggle` | – | Start if stopped, stop if running. |
| `server.restart` | – | Stop then start. |
| `server.copyUrl` | – | Copy `server.url` with `wl-copy`, then notify "Copied". |
| `open.dashboard` | – | `xdg-open` the management dashboard. |
| `open.authFolder` | – | `xdg-open ~/.cli-proxy-api`. |
| `open.logsFolder` | – | Create and `xdg-open ~/.cli-proxy-api/logs`. |
| `open.url` | `{"url"}` | `xdg-open` an http(s) URL (verification links, footer links). |
| `clipboard.copy` | `{"text"}` | Copy arbitrary text (device codes). |
| `provider.setEnabled` | `{"provider","enabled"}` | Provider toggle. Meta cancels sign-in on disable and refreshes keys on enable. |
| `provider.connect` | `{"provider"}` | "Add Account"/"Connect". claude/codex/antigravity/kimi run the CLIProxyAPI login flow; grok and meta start their device-code flows. junie requires `junie.saveKey` instead and returns an error here. |
| `provider.cancelAuth` | `{"provider"}` | Cancel an in-progress grok/meta sign-in. |
| `account.toggleDisabled` | `{"provider","accountId"}` | Enable/disable one account (refuses to disable the last enabled one). |
| `account.remove` | `{"provider","accountId"}` | Remove an account (restarts the backend around the delete, like the macOS app). |
| `junie.saveKey` | `{"apiKey"}` | Write `~/.cli-proxy-api/junie.json` (0600). |
| `settings.set` | `{"key","value"}` | Set one key from `state.settings` (see below). |
| `factory.apply` | – | Apply/Re-apply Factory custom models (backup first). |
| `usage.refresh` | – | Refresh OAuth quota windows. |
| `update.check` | – | Check for updates now. |
| `update.install` | – | Download, verify, install the available update, then restart. |
| `app.quit` | – | Stop servers and exit the daemon (`systemctl --user stop`). |

`settings.set` keys and side effects:

| Key | Type | Side effect |
|---|---|---|
| `launchAtLogin` | bool | `systemctl --user enable/disable droidproxy.service` |
| `allowRemote`, `secretKey`, `bindAddress`, `verboseLogging` | bool/string | Regenerate merged config (CLIProxyAPI hot-reloads). |
| `sequentialAccountFailover` | bool | `setSequentialAccountFailover` (regenerate config). |
| `beta`, `oledTheme`, `backgroundOpacity` | bool/number | Stored; panel appearance and beta-gated rows. |
| `gpt6AstraFastMode`, `gpt6SolFastMode`, `gpt6LunaFastMode` | bool | ThinkingProxy fast mode. |
| `metaContributorMode` | bool | Stored; affects Factory model apply. |
| `autoCheckUpdates`, `autoInstallUpdates` | bool | Updater behavior. |

### Action result

```json
{"ok": true, "message": "✓ Disabled a@b.com", "title": "Authentication Result"}
{"ok": false, "error": "Failed to update a@b.com. Please try again.", "title": "Authentication Result"}
```

`message`/`error` hold the exact user-facing text the macOS app put in its
alert. `title` is optional (defaults to "DroidProxy"). A result with an empty
`message` needs no dialog.


## Not ported

GitHub Copilot (the macOS app's local `@jeffreycao/copilot-api` gateway on
:8319) is intentionally not part of the Linux port.
