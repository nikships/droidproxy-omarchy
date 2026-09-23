# Porting conventions (Swift → Go)

DroidProxy for Omarchy is a Linux port of the macOS app. The Swift reference
lives at `~/repos/droidproxy/src` (read-only). The port must keep **full
functional parity**: same behavior, same edge cases, same user-facing strings,
same on-disk formats and credential locations.

## Layout

| Swift | Go package |
|---|---|
| `AppPreferences.swift` | `internal/prefs` (done) |
| `AuthPaths.swift`, hardcoded paths | `internal/paths` (done) |
| `NotificationNames.swift` / `NotificationCenter` | `internal/events` (done) |
| `NSLog` | `internal/logx.Logf` (done); `/tmp/droidproxy-debug.log` → `logx.Debugf` |
| `AuthStatus.swift` types | `internal/auth/types.go` (done) |
| `AuthStatus.swift` manager, `AuthDirectoryMonitor.swift` | `internal/auth` |
| `ClaudeAnthropicBetaRewriter`, `ClaudeThinkingBlockSanitizer`, `ClaudeAuthSeatFiles` | `internal/claude` |
| `OAuthUsageTracker.swift` | `internal/usage` |
| `GrokRequestSanitizer`, `GrokNativeToolCallRewriter`, `GrokEndFeatureRunRepair` | `internal/grok` (transform files) |
| `GrokAuth.swift` | `internal/grok` (auth files) |
| `MetaMuseUpstream`, `MetaMuseCredentialStore`, `MetaMuseSupport` | `internal/meta` |
| `DroidProxyModelCatalog.swift` + Factory apply logic in `SettingsView.swift` | `internal/catalog` |
| `CopilotSupport.swift` | not ported (Copilot is out of scope on Linux) |
| `ServerManager.swift` | `internal/backend` |
| `ThinkingProxy.swift` | `internal/proxy` (done; Junie/Copilot/Cursor/Kimi paths omitted) |
| `AppDelegate.swift`, `SettingsView.swift` actions | `internal/daemon` |

## Rules

- Go 1.24, stdlib first. Allowed third-party modules are already in `go.mod`
  (`fsnotify`, `godbus/dbus/v5`). Do not edit `go.mod`/`go.sum`.
- Port every XCTest in `src/Tests/CLIProxyMenuBarTests` for your files to a Go
  `_test.go` with the same cases and assertions. Add tests for logic that had
  none when it is cheap to do.
- Keep doc comments that explain *why*; drop comments that narrate *what*.
- **Byte-exact JSON editing.** Where Swift edits raw JSON text to preserve key
  order (Anthropic prompt cache), do the same in Go. Never round-trip those
  bodies through `encoding/json`.
- Swift `JSONSerialization` with `.sortedKeys` → Go `encoding/json` on
  `map[string]any` (Go sorts map keys). Swift escapes `/` as `\/`; the Swift
  code undoes that where it matters—match the final bytes.
- `DispatchQueue`/`Timer` → goroutines, `time.Timer`/`time.Ticker`, `sync.Mutex`.
  `@Published` state → a mutex-guarded struct plus `events.Publish(topic)`.
- `NSWorkspace.shared.open(url)` → `xdg-open` via an injectable func var so
  tests don't launch a browser. `NSPasteboard` → `wl-copy`.
- `Process` → `os/exec`. `/usr/bin/pgrep` and `pkill` exist on Arch at
  `/usr/bin`.
- macOS-only paths become Linux equivalents (for example
  `~/Library/Application Support/fnm` → `~/.local/share/fnm`), and Linux-only
  locations are added where relevant (mise: `~/.local/share/mise/installs/node/*/bin`).
- Tests must not touch the real home directory: use `t.Setenv("HOME", t.TempDir())`;
  `paths` and `prefs.Shared()` follow `HOME`.
- Tests must not hit the network. Make base URLs and HTTP clients injectable.
- Run `gofmt -l`, `go vet`, and `go test -race` for your packages only
  (other packages may be mid-port).
