package auth

import (
	"strings"
	"time"
)

// ServiceType mirrors the macOS ServiceType enum. Raw values are persisted
// (enabledProviders) and must not change.
type ServiceType string

const (
	Claude      ServiceType = "claude"
	Codex       ServiceType = "codex"
	Antigravity ServiceType = "antigravity"
	Kimi        ServiceType = "kimi"
	Junie       ServiceType = "junie"
	Grok        ServiceType = "grok"
	Copilot     ServiceType = "copilot"
	Meta        ServiceType = "meta"
)

// AllServiceTypes is ServiceType.allCases in declaration order.
var AllServiceTypes = []ServiceType{Claude, Codex, Antigravity, Kimi, Junie, Grok, Copilot, Meta}

// ServiceTypeFromAuthFileType maps the "type" field of an auth JSON file (or a
// catalog provider key) to a ServiceType.
func ServiceTypeFromAuthFileType(s string) (ServiceType, bool) {
	switch strings.ToLower(s) {
	case "claude":
		return Claude, true
	case "codex":
		return Codex, true
	case "antigravity", "gemini", "gemini-cli":
		return Antigravity, true
	case "kimi":
		return Kimi, true
	case "junie":
		return Junie, true
	case "grok-cli", "grok":
		return Grok, true
	case "copilot", "github-copilot":
		return Copilot, true
	case "meta", "muse":
		return Meta, true
	}
	return "", false
}

// DisplayName is the user-facing provider name.
func (s ServiceType) DisplayName() string {
	switch s {
	case Claude:
		return "Claude Code"
	case Codex:
		return "Codex"
	case Antigravity:
		return "Antigravity"
	case Kimi:
		return "Kimi"
	case Junie:
		return "Junie"
	case Grok:
		return "Grok"
	case Copilot:
		return "GitHub Copilot"
	case Meta:
		return "Meta Muse"
	}
	return string(s)
}

// Account is one authenticated account (macOS AuthAccount).
type Account struct {
	ID    string // file name, or Meta account id
	Email string
	Login string // Copilot
	Type  ServiceType
	// Expired is when the file's access token stops working. With a refresh
	// token, CLIProxyAPI swaps in a new one on the account's next request, so
	// this passing is not the account expiring.
	Expired  *time.Time
	FilePath string
	Disabled bool
	// OrganizationName is the raw organization_name from a Claude auth JSON.
	OrganizationName string
	// ClaudeSeatLabel is the classified seat suffix (e.g. "Personal Max").
	ClaudeSeatLabel string
	// CanRefresh means the file has a refresh token, so an expired access
	// token is renewed on use.
	CanRefresh bool
}

// IsAccessExpired reports whether the access token's expiry is in the past.
func (a Account) IsAccessExpired() bool {
	return a.Expired != nil && a.Expired.Before(time.Now())
}

// IsExpired reports whether the account can't be used again until the owner
// signs in again.
func (a Account) IsExpired() bool {
	return a.IsAccessExpired() && !a.CanRefresh
}

// DisplayName matches AuthAccount.displayName.
func (a Account) DisplayName() string {
	var base string
	switch {
	case a.Email != "":
		base = a.Email
	case a.Login != "":
		base = a.Login
	default:
		return a.ID
	}
	if a.Type == Claude && a.ClaudeSeatLabel != "" {
		return base + " · " + a.ClaudeSeatLabel
	}
	return base
}
