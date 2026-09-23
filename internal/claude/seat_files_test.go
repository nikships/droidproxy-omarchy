package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const (
	testEmail        = "josef@kaikaku.ai"
	testPersonalUUID = "01a2b962-674f-47df-afdd-fc21609e4987"
	testTeamUUID     = "b305c0b8-71d7-4d4c-8cab-47e9f78eea3e"
)

func TestParsesTopLevelOrganizationFields(t *testing.T) {
	fields := OrganizationFieldsFrom(map[string]any{
		"organization_uuid": testPersonalUUID,
		"organization_name": "Josef Chen",
	})
	if fields.UUID != testPersonalUUID || fields.Name != "Josef Chen" {
		t.Fatalf("fields = %+v", fields)
	}
	if !fields.HasSeatIdentity() {
		t.Fatal("expected seat identity")
	}
}

func TestParsesNestedOrganizationObject(t *testing.T) {
	fields := OrganizationFieldsFrom(map[string]any{
		"organization": map[string]any{"uuid": testTeamUUID, "name": "KAIKAKU"},
	})
	if fields.UUID != testTeamUUID || fields.Name != "KAIKAKU" {
		t.Fatalf("fields = %+v", fields)
	}
}

func TestBlankOrganizationFieldsAreMissing(t *testing.T) {
	fields := OrganizationFieldsFrom(map[string]any{
		"organization_uuid": "  ",
		"organization_name": "",
	})
	if fields.UUID != "" || fields.Name != "" {
		t.Fatalf("fields = %+v", fields)
	}
	if fields.HasSeatIdentity() {
		t.Fatal("expected no seat identity")
	}
}

func TestPersonStyleNameIsPersonalMax(t *testing.T) {
	if got := SeatDisplayLabel(testEmail, "Josef Chen"); got != "Personal Max" {
		t.Fatalf("got %q", got)
	}
}

func TestAllCapsOrgIsTeam(t *testing.T) {
	if got := SeatDisplayLabel(testEmail, "KAIKAKU"); got != "KAIKAKU (Team)" {
		t.Fatalf("got %q", got)
	}
}

func TestCompanySuffixIsTeam(t *testing.T) {
	if got := SeatDisplayLabel(testEmail, "Acme Inc"); got != "Acme Inc (Team)" {
		t.Fatalf("got %q", got)
	}
}

func TestAmbiguousNameIsShownRaw(t *testing.T) {
	if got := SeatDisplayLabel(testEmail, "Pessoal"); got != "Pessoal" {
		t.Fatalf("got %q", got)
	}
	if got := SeatDisplayLabel(testEmail, "Acme"); got != "Acme" {
		t.Fatalf("got %q", got)
	}
}

func TestEmailPossessiveOrganizationIsPersonalMax(t *testing.T) {
	if got := SeatDisplayLabel(testEmail, testEmail+"'s Organization"); got != "Personal Max" {
		t.Fatalf("got %q", got)
	}
}

func TestExplicitOrganizationTypeWinsOverName(t *testing.T) {
	cases := []struct {
		jsonData map[string]any
		want     string
	}{
		{map[string]any{"organization_name": "Pessoal", "organization_type": "claude_max"}, "Personal Max"},
		{map[string]any{"organization_name": "Josef Chen", "organization_type": "claude_team"}, "Josef Chen (Team)"},
		{map[string]any{"organization_name": "KAIKAKU", "organization_type": "enterprise"}, "KAIKAKU (Enterprise)"},
		{map[string]any{"organization_type": "enterprise"}, "Enterprise"},
		{map[string]any{"organization_name": "Josef Chen", "organization_type": "claude_pro"}, "Personal Pro"},
	}
	for _, c := range cases {
		if got := SeatDisplayLabelFromJSON(testEmail, c.jsonData); got != c.want {
			t.Errorf("SeatDisplayLabelFromJSON = %q, want %q", got, c.want)
		}
	}
}

func TestMissingOrganizationNameYieldsNoSuffix(t *testing.T) {
	if got := SeatDisplayLabel(testEmail, ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

// Settings row composition lives on auth.Account.DisplayName in Go; the label
// suffix behavior mirrors the Swift AuthAccount tests here.
func TestSettingsRowShowsEmailAndClassifiedLabel(t *testing.T) {
	if got := SeatDisplayLabel(testEmail, "Josef Chen"); got != "Personal Max" {
		t.Fatalf("got %q", got)
	}
	if got := SeatDisplayLabel(testEmail, "KAIKAKU"); got != "KAIKAKU (Team)" {
		t.Fatalf("got %q", got)
	}
}

func TestUniqueFilenamePrefersUUID(t *testing.T) {
	name := UniqueFilename(testEmail, OrganizationFields{UUID: testPersonalUUID, Name: "Josef Chen"})
	if want := "claude-" + testEmail + "-" + testPersonalUUID + ".json"; name != want {
		t.Fatalf("got %q, want %q", name, want)
	}
}

func TestUniqueFilenameRejectsPathEscapingEmail(t *testing.T) {
	fields := OrganizationFields{UUID: testPersonalUUID, Name: "Josef Chen"}
	for _, email := range []string{"../evil@x.com", "foo/bar@x.com", `foo\bar@x.com`, ".", ".."} {
		if got := UniqueFilename(email, fields); got != "" {
			t.Errorf("UniqueFilename(%q) = %q, want empty", email, got)
		}
	}
}

func TestCanonicalFilenameMatchesCLIProxyAPI(t *testing.T) {
	if got := CanonicalFilename(testEmail); got != "claude-"+testEmail+".json" {
		t.Fatalf("got %q", got)
	}
	if !IsCanonicalClaudeEmailFile("claude-"+testEmail+".json", testEmail) {
		t.Fatal("canonical file not recognized")
	}
	if IsCanonicalClaudeEmailFile("claude-"+testEmail+"-kaikaku.json", testEmail) {
		t.Fatal("suffixed file recognized as canonical")
	}
}

// Scratch helpers.

func writeTestJSON(t *testing.T, dir, name string, object map[string]any) {
	t.Helper()
	data, err := json.Marshal(object) // Go marshals maps with sorted keys
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestClaudeFile(t *testing.T, dir, name, email, orgName, orgUUID, marker string) {
	t.Helper()
	writeTestJSON(t, dir, name, map[string]any{
		"type":              "claude",
		"email":             email,
		"organization_name": orgName,
		"organization_uuid": orgUUID,
		"access_token":      marker,
	})
}

func jsonNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	return names
}

func markerIn(t *testing.T, dir, filename string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil {
		return ""
	}
	var jsonData map[string]any
	if err := json.Unmarshal(data, &jsonData); err != nil {
		return ""
	}
	s, _ := jsonData["access_token"].(string)
	return s
}

func posixPermissions(t *testing.T, dir, filename string) os.FileMode {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, filename))
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestTwoOrgsSameEmailBecomeTwoUUIDFiles(t *testing.T) {
	dir := t.TempDir()

	writeTestClaudeFile(t, dir, "claude-"+testEmail+".json", testEmail, "Josef Chen", testPersonalUUID, "personal-token")
	if MigrateFile(filepath.Join(dir, "claude-"+testEmail+".json")) == "" {
		t.Fatal("expected migration")
	}
	writeTestClaudeFile(t, dir, "claude-"+testEmail+".json", testEmail, "KAIKAKU", testTeamUUID, "team-token")
	if MigrateFile(filepath.Join(dir, "claude-"+testEmail+".json")) == "" {
		t.Fatal("expected migration")
	}

	names := jsonNames(t, dir)
	for _, want := range []string{
		"claude-" + testEmail + "-" + testPersonalUUID + ".json",
		"claude-" + testEmail + "-" + testTeamUUID + ".json",
	} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s in %v", want, names)
		}
	}
	if len(names) != 2 {
		t.Fatalf("names = %v", names)
	}
	if got := markerIn(t, dir, "claude-"+testEmail+"-"+testPersonalUUID+".json"); got != "personal-token" {
		t.Fatalf("marker = %q", got)
	}
	if got := markerIn(t, dir, "claude-"+testEmail+"-"+testTeamUUID+".json"); got != "team-token" {
		t.Fatalf("marker = %q", got)
	}
}

func TestReauthSameOrgRefreshesUUIDFileOnly(t *testing.T) {
	dir := t.TempDir()

	writeTestClaudeFile(t, dir, "claude-"+testEmail+"-"+testPersonalUUID+".json", testEmail, "Josef Chen", testPersonalUUID, "old-personal")
	writeTestClaudeFile(t, dir, "claude-"+testEmail+"-"+testTeamUUID+".json", testEmail, "KAIKAKU", testTeamUUID, "team-token")
	writeTestClaudeFile(t, dir, "claude-"+testEmail+".json", testEmail, "Josef Chen", testPersonalUUID, "new-personal")

	MigrateCanonicalFiles(dir)

	names := jsonNames(t, dir)
	if len(names) != 2 {
		t.Fatalf("names = %v", names)
	}
	if got := markerIn(t, dir, "claude-"+testEmail+"-"+testPersonalUUID+".json"); got != "new-personal" {
		t.Fatalf("personal marker = %q", got)
	}
	if got := markerIn(t, dir, "claude-"+testEmail+"-"+testTeamUUID+".json"); got != "team-token" {
		t.Fatalf("team marker = %q", got)
	}
}

func TestMissingOrgFieldsLeaveCanonicalName(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, dir, "claude-"+testEmail+".json", map[string]any{
		"type": "claude", "email": testEmail, "access_token": "x",
	})

	if MigrateFile(filepath.Join(dir, "claude-"+testEmail+".json")) != "" {
		t.Fatal("expected no migration")
	}
	names := jsonNames(t, dir)
	if len(names) != 1 || names[0] != "claude-"+testEmail+".json" {
		t.Fatalf("names = %v", names)
	}
}

func TestNameOnlyCanonicalFileMigratesToSanitizedSuffix(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, dir, "claude-"+testEmail+".json", map[string]any{
		"type": "claude", "email": testEmail,
		"organization_name": "Acme Inc", "access_token": "name-only-token",
	})

	dest := MigrateFile(filepath.Join(dir, "claude-"+testEmail+".json"))
	want := "claude-" + testEmail + "-Acme-Inc.json"
	if filepath.Base(dest) != want {
		t.Fatalf("dest = %q, want %q", filepath.Base(dest), want)
	}
	names := jsonNames(t, dir)
	if len(names) != 1 || names[0] != want {
		t.Fatalf("names = %v", names)
	}
	if got := markerIn(t, dir, want); got != "name-only-token" {
		t.Fatalf("marker = %q", got)
	}
	if perm := posixPermissions(t, dir, want); perm != 0o600 {
		t.Fatalf("permissions = %o", perm)
	}
}

func TestMigratedDestinationIsOwnerReadableOnly(t *testing.T) {
	dir := t.TempDir()
	writeTestClaudeFile(t, dir, "claude-"+testEmail+".json", testEmail, "Josef Chen", testPersonalUUID, "personal-token")

	dest := MigrateFile(filepath.Join(dir, "claude-"+testEmail+".json"))
	want := "claude-" + testEmail + "-" + testPersonalUUID + ".json"
	if filepath.Base(dest) != want {
		t.Fatalf("dest = %q", filepath.Base(dest))
	}
	if perm := posixPermissions(t, dir, want); perm != 0o600 {
		t.Fatalf("permissions = %o", perm)
	}
}

func TestAlreadySuffixedNameMigratesToUUID(t *testing.T) {
	dir := t.TempDir()
	writeTestClaudeFile(t, dir, "claude-"+testEmail+"-kaikaku.json", testEmail, "KAIKAKU", testTeamUUID, "team-token")

	MigrateCanonicalFiles(dir)

	names := jsonNames(t, dir)
	if len(names) != 1 || names[0] != "claude-"+testEmail+"-"+testTeamUUID+".json" {
		t.Fatalf("names = %v", names)
	}
	if got := markerIn(t, dir, names[0]); got != "team-token" {
		t.Fatalf("marker = %q", got)
	}
}

func TestEmptyAndIncompleteJSONAreLeftAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-"+testEmail+".json")

	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if MigrateFile(path) != "" {
		t.Fatal("expected no migration for empty file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("empty file was removed")
	}

	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if MigrateFile(path) != "" {
		t.Fatal("expected no migration for malformed file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{" {
		t.Fatalf("file content changed: %q", data)
	}
}

func TestCodexAndOtherProvidersAreNotRenamed(t *testing.T) {
	dir := t.TempDir()
	writeTestJSON(t, dir, "codex-"+testEmail+".json", map[string]any{
		"type": "codex", "email": testEmail,
		"organization_uuid": testPersonalUUID, "organization_name": "Josef Chen",
	})
	writeTestJSON(t, dir, "gemini-"+testEmail+".json", map[string]any{
		"type": "gemini", "email": testEmail,
		"organization_uuid": testPersonalUUID,
	})

	MigrateCanonicalFiles(dir)

	names := jsonNames(t, dir)
	if len(names) != 2 || (names[0] != "codex-"+testEmail+".json" && names[1] != "codex-"+testEmail+".json") ||
		(names[0] != "gemini-"+testEmail+".json" && names[1] != "gemini-"+testEmail+".json") {
		t.Fatalf("names = %v", names)
	}
}
