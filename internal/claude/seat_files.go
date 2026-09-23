// Package claude holds Claude-specific request rewriting and auth-file helpers.
package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/nikships/droidproxy-omarchy/internal/logx"
)

// Seat file management: keeps personal Max and org Claude seats on the same
// email from clobbering each other. CLIProxyAPI always writes
// `claude-{email}.json`; after the file is complete we rename it to
// `claude-{email}-{organization_uuid}.json` so the next `-claude-login` can
// create a fresh canonical file for the other org.

// OrganizationFields are the org identity fields of a Claude auth JSON.
type OrganizationFields struct {
	UUID string
	Name string
}

// HasSeatIdentity reports whether any org identity is present.
func (f OrganizationFields) HasSeatIdentity() bool {
	return f.UUID != "" || f.Name != ""
}

// OrganizationFieldsFrom extracts organization_uuid / organization_name from
// the top level, falling back to the nested "organization" object.
func OrganizationFieldsFrom(jsonData map[string]any) OrganizationFields {
	var fields OrganizationFields
	fields.UUID = nonemptyString(jsonData["organization_uuid"])
	fields.Name = nonemptyString(jsonData["organization_name"])
	if fields.UUID == "" || fields.Name == "" {
		if org, ok := jsonData["organization"].(map[string]any); ok {
			if fields.UUID == "" {
				fields.UUID = nonemptyString(org["uuid"])
				if fields.UUID == "" {
					fields.UUID = nonemptyString(org["organization_uuid"])
				}
			}
			if fields.Name == "" {
				fields.Name = nonemptyString(org["name"])
				if fields.Name == "" {
					fields.Name = nonemptyString(org["organization_name"])
				}
			}
		}
	}
	return fields
}

// SeatDisplayLabelFromJSON returns the settings/quota suffix after the email:
// "Personal Max", "{org} (Team)", or the raw organization name when we cannot
// classify the plan. Explicit organization_type / rate_limit_tier hints win
// over the name heuristic.
func SeatDisplayLabelFromJSON(email string, jsonData map[string]any) string {
	fields := OrganizationFieldsFrom(jsonData)
	if label := labelFromPlanHints(jsonData, fields.Name); label != "" {
		return label
	}
	return SeatDisplayLabel(email, fields.Name)
}

// SeatDisplayLabel classifies an organization name without plan hints.
func SeatDisplayLabel(email, organizationName string) string {
	if organizationName == "" {
		return ""
	}
	if LooksLikePersonalOrganizationName(organizationName, email) {
		return "Personal Max"
	}
	if LooksLikeTeamOrganizationName(organizationName) {
		return organizationName + " (Team)"
	}
	return organizationName
}

// CanonicalFilename is the name CLIProxyAPI writes on login.
func CanonicalFilename(email string) string {
	return "claude-" + email + ".json"
}

// IsCanonicalClaudeEmailFile reports whether filename is the canonical
// `claude-{email}.json` (not an already-suffixed variant).
func IsCanonicalClaudeEmailFile(filename, email string) bool {
	return filename == CanonicalFilename(email)
}

// UniqueFilename prefers the UUID (stable); otherwise a filesystem-safe
// organization name. Empty when there is no safe suffix or email.
func UniqueFilename(email string, fields OrganizationFields) string {
	email = filenameSafeEmail(email)
	if email == "" {
		return ""
	}
	suffix := filenameSafeSuffix(fields.UUID)
	if suffix == "" {
		suffix = filenameSafeSuffix(fields.Name)
	}
	if suffix == "" {
		return ""
	}
	return "claude-" + email + "-" + suffix + ".json"
}

// MigrateCanonicalFiles renames complete `claude-{email}.json` files that
// carry org identity. Incomplete / empty / non-Claude files are left untouched
// so a mid-write from CLIProxyAPI can be retried on the next watcher tick.
// It returns the destination paths of successful renames.
func MigrateCanonicalFiles(directory string) []string {
	entries, err := os.ReadDir(directory)
	if err != nil {
		logx.Logf("[ClaudeAuthSeatFiles] Failed to list %s: %v", directory, err)
		return nil
	}

	var destinations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(name), ".json") {
			continue
		}
		if dest := MigrateFile(filepath.Join(directory, name)); dest != "" {
			destinations = append(destinations, dest)
		}
	}
	return destinations
}

// MigrateFile renames one auth file when needed and returns the destination
// path, or "" when the file was left alone.
func MigrateFile(path string) string {
	filename := filepath.Base(path)
	if !strings.HasPrefix(filename, "claude-") || !strings.EqualFold(filepath.Ext(filename), ".json") {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if info.Size() == 0 {
		logx.Logf("[ClaudeAuthSeatFiles] Skipping empty or mid-write file %s", filename)
		return ""
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var jsonData map[string]any
	if err := json.Unmarshal(data, &jsonData); err != nil {
		return ""
	}
	if t, _ := jsonData["type"].(string); strings.ToLower(t) != "claude" {
		return ""
	}

	email := nonemptyString(jsonData["email"])
	if email == "" {
		return ""
	}

	fields := OrganizationFieldsFrom(jsonData)
	destName := UniqueFilename(email, fields)
	if destName == "" || destName == filename {
		return ""
	}

	// Canonical `claude-{email}.json` always moves once org identity exists.
	// Already-suffixed workaround names (`claude-{email}-kaikaku.json`) move
	// onto the stable UUID filename so a later re-auth refreshes that file
	// instead of leaving a sibling.
	isCanonical := IsCanonicalClaudeEmailFile(filename, email)
	if !isCanonical && fields.UUID == "" {
		return ""
	}

	destDir := filepath.Dir(path)
	dest := filepath.Join(destDir, destName)
	if err := writeRestrictedFile(data, dest); err != nil {
		logx.Logf("[ClaudeAuthSeatFiles] Failed to rename %s: %v", filename, err)
		return ""
	}
	if path != dest {
		if err := os.Remove(path); err != nil {
			logx.Logf("[ClaudeAuthSeatFiles] Failed to rename %s: %v", filename, err)
			return ""
		}
	}
	logx.Logf("[ClaudeAuthSeatFiles] Renamed %s -> %s", filename, destName)
	return dest
}

// Plan / name classification.

var companySuffixes = map[string]bool{
	"inc": true, "inc.": true, "llc": true, "ltd": true, "ltd.": true,
	"corp": true, "corporation": true, "gmbh": true, "ag": true, "plc": true,
	"company": true, "co": true, "co.": true, "team": true, "labs": true,
	"studio": true, "technologies": true, "tech": true, "limited": true,
}

var nameParticles = map[string]bool{
	"van": true, "von": true, "de": true, "da": true, "del": true,
	"della": true, "di": true, "la": true, "le": true, "du": true,
	"st": true, "st.": true, "der": true, "den": true, "bin": true, "al": true,
}

func labelFromPlanHints(jsonData map[string]any, organizationName string) string {
	hints := planHintStrings(jsonData)
	if len(hints) == 0 {
		return ""
	}
	for _, h := range hints {
		if isPersonalMaxHint(h) {
			return "Personal Max"
		}
	}
	for _, h := range hints {
		if isEnterpriseHint(h) {
			if organizationName != "" {
				return organizationName + " (Enterprise)"
			}
			return "Enterprise"
		}
	}
	for _, h := range hints {
		if isTeamHint(h) {
			if organizationName != "" {
				return organizationName + " (Team)"
			}
			return "Team"
		}
	}
	for _, h := range hints {
		if isPersonalProHint(h) {
			return "Personal Pro"
		}
	}
	return ""
}

func planHintStrings(jsonData map[string]any) []string {
	keys := []string{
		"organization_type", "rate_limit_tier", "billing_type",
		"plan", "subscription_type", "account_type",
	}
	var values []string
	for _, key := range keys {
		if v := nonemptyString(jsonData[key]); v != "" {
			values = append(values, v)
		}
	}
	if org, ok := jsonData["organization"].(map[string]any); ok {
		for _, key := range append(append([]string{}, keys...), "type") {
			if v := nonemptyString(org[key]); v != "" {
				values = append(values, v)
			}
		}
	}
	return values
}

func isPersonalMaxHint(raw string) bool {
	value := strings.ToLower(raw)
	return strings.Contains(value, "claude_max") || value == "max"
}

func isPersonalProHint(raw string) bool {
	value := strings.ToLower(raw)
	return strings.Contains(value, "claude_pro") || value == "pro"
}

func isTeamHint(raw string) bool {
	value := strings.ToLower(raw)
	return strings.Contains(value, "claude_team") || value == "team" || strings.Contains(value, "team_plan")
}

func isEnterpriseHint(raw string) bool {
	return strings.Contains(strings.ToLower(raw), "enterprise")
}

// LooksLikePersonalOrganizationName reports whether an org name looks like a
// personal Max seat rather than a company.
func LooksLikePersonalOrganizationName(name, email string) bool {
	if email != "" && LooksLikeEmailPersonalOrganization(name, email) {
		return true
	}
	return LooksLikePersonDisplayName(name)
}

// LooksLikeEmailPersonalOrganization matches Claude's `{email}'s Organization`.
func LooksLikeEmailPersonalOrganization(name, email string) bool {
	lowered := strings.ToLower(name)
	emailLower := strings.ToLower(email)
	if lowered == emailLower+"'s organization" || lowered == emailLower+"’s organization" {
		return true
	}
	local := emailLower
	if at := strings.Index(local, "@"); at >= 0 {
		local = local[:at]
	}
	if local != "" && (lowered == local+"'s organization" || lowered == local+"’s organization") {
		return true
	}
	return false
}

// LooksLikePersonDisplayName matches Anthropic's personal Max org naming, often
// the user's display name ("Josef Chen").
func LooksLikePersonDisplayName(name string) bool {
	parts := strings.Fields(name)
	if len(parts) < 2 || len(parts) > 4 {
		return false
	}
	contentWords := 0
	for _, part := range parts {
		lower := strings.ToLower(part)
		if companySuffixes[lower] {
			return false
		}
		if nameParticles[lower] {
			continue
		}
		if !looksLikeNameToken(part) {
			return false
		}
		contentWords++
	}
	return contentWords >= 2
}

// LooksLikeTeamOrganizationName reports whether an org name looks like a team
// (company suffix or an ALL-CAPS word of 3+ letters).
func LooksLikeTeamOrganizationName(name string) bool {
	if LooksLikePersonDisplayName(name) {
		return false
	}
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if companySuffixes[strings.ToLower(part)] {
			return true
		}
	}
	if len(parts) == 1 {
		token := parts[0]
		letters := 0
		hasLower := false
		allUpper := true
		for _, r := range token {
			if unicode.IsLetter(r) {
				letters++
				if unicode.IsLower(r) {
					hasLower = true
				}
			} else {
				allUpper = false
			}
		}
		if letters >= 3 && !hasLower && allUpper {
			return true
		}
	}
	return false
}

func looksLikeNameToken(token string) bool {
	runes := []rune(token)
	if len(runes) == 0 || !unicode.IsUpper(runes[0]) {
		return false
	}
	hasLower := false
	for _, r := range runes {
		if unicode.IsLower(r) {
			hasLower = true
		}
		if !unicode.IsLetter(r) && r != '-' && r != '\'' {
			return false
		}
	}
	return hasLower
}

// Helpers.

func nonemptyString(value any) string {
	s, _ := value.(string)
	trimmed := strings.TrimSpace(s)
	return trimmed
}

// filenameSafeEmail rejects values that would escape the auth directory as a
// path component. `@` stays so existing `claude-{email}-{uuid}.json` names
// keep matching.
func filenameSafeEmail(raw string) string {
	email := strings.TrimSpace(raw)
	if email == "" || strings.ContainsAny(email, "/\\") || email == "." || email == ".." {
		logx.Logf("[ClaudeAuthSeatFiles] Refusing unsafe email in filename: %s", email)
		return ""
	}
	return email
}

// writeRestrictedFile writes dest at 0o600 (via a temp file + rename) so OAuth
// tokens are never written world-readable, matching the macOS port.
func writeRestrictedFile(data []byte, dest string) error {
	temp := filepath.Join(filepath.Dir(dest), ".claude-seat-tmp")
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", filepath.Base(temp), err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(temp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, dest); err != nil {
		os.Remove(temp)
		return err
	}
	return os.Chmod(dest, 0o600)
}

// filenameSafeSuffix maps arbitrary org names/UUIDs onto [A-Za-z0-9._-] and
// collapses runs of dashes, like the Swift implementation.
func filenameSafeSuffix(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	result := b.String()
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	result = strings.Trim(result, "-.")
	if result == "" {
		return ""
	}
	return result
}
