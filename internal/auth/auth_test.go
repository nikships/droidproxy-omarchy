package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/claude"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

const (
	testEmail        = "josef@kaikaku.ai"
	testPersonalUUID = "01a2b962-674f-47df-afdd-fc21609e4987"
	testTeamUUID     = "b305c0b8-71d7-4d4c-8cab-47e9f78eea3e"
)

func setAuthDirHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeAuthFile(t *testing.T, path string, jsonData map[string]any) {
	t.Helper()
	data, err := json.Marshal(jsonData)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDisplayNameIncludesClaudeSeatLabel(t *testing.T) {
	personal := Account{
		ID:              "claude-" + testEmail + "-" + testPersonalUUID + ".json",
		Email:           testEmail,
		Type:            Claude,
		ClaudeSeatLabel: claude.SeatDisplayLabel(testEmail, "Josef Chen"),
	}
	team := Account{
		ID:              "claude-" + testEmail + "-" + testTeamUUID + ".json",
		Email:           testEmail,
		Type:            Claude,
		ClaudeSeatLabel: claude.SeatDisplayLabel(testEmail, "KAIKAKU"),
	}
	if got := personal.DisplayName(); got != testEmail+" · Personal Max" {
		t.Fatalf("got %q", got)
	}
	if got := team.DisplayName(); got != testEmail+" · KAIKAKU (Team)" {
		t.Fatalf("got %q", got)
	}
}

func TestDisplayNameWithoutLabelShowsEmailOnly(t *testing.T) {
	account := Account{ID: "claude-" + testEmail + ".json", Email: testEmail, Type: Claude}
	if got := account.DisplayName(); got != testEmail {
		t.Fatalf("got %q", got)
	}
}

func TestCheckAuthStatusScansAndSkipsMetaFiles(t *testing.T) {
	setAuthDirHome(t)
	dir := paths.AuthDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	writeAuthFile(t, filepath.Join(dir, "claude-a@b.com.json"), map[string]any{
		"type": "claude", "email": "a@b.com", "expired": "2030-01-01T00:00:00Z",
	})
	writeAuthFile(t, filepath.Join(dir, "codex-a@b.com.json"), map[string]any{
		"type": "codex", "email": "a@b.com",
	})
	writeAuthFile(t, filepath.Join(dir, "meta-a@b.com.json"), map[string]any{
		"type": "meta", "email": "a@b.com",
	})
	writeAuthFile(t, filepath.Join(dir, "junk.json"), map[string]any{
		"noType": true,
	})

	m := NewManager()
	m.CheckAuthStatus()

	if got := len(m.Accounts(Claude)); got != 1 {
		t.Fatalf("claude accounts = %d", got)
	}
	if got := len(m.Accounts(Codex)); got != 1 {
		t.Fatalf("codex accounts = %d", got)
	}
	if got := len(m.Accounts(Meta)); got != 0 {
		t.Fatalf("meta accounts from auth dir = %d, want 0 (type meta files are skipped)", got)
	}
	if !m.HasAccounts(Claude) || !m.HasAccounts(Codex) || m.HasAccounts(Meta) {
		t.Fatal("HasAccounts mismatch")
	}
}

func TestParseExpiryAcceptsFractionalAndPlainRFC3339(t *testing.T) {
	setAuthDirHome(t)
	dir := paths.AuthDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeAuthFile(t, filepath.Join(dir, "a.json"), map[string]any{
		"type": "codex", "email": "a@b.com", "expired": "2030-01-01T00:00:00.123456+00:00",
	})
	writeAuthFile(t, filepath.Join(dir, "b.json"), map[string]any{
		"type": "codex", "email": "c@d.com", "expired": "2030-01-01T00:00:00Z",
	})
	writeAuthFile(t, filepath.Join(dir, "c.json"), map[string]any{
		"type": "codex", "email": "e@f.com", "expired": "not-a-date",
	})

	m := NewManager()
	m.CheckAuthStatus()

	accounts := m.Accounts(Codex)
	if len(accounts) != 3 {
		t.Fatalf("accounts = %d", len(accounts))
	}
	for _, a := range accounts {
		switch a.Email {
		case "a@b.com", "c@d.com":
			if a.Expired == nil || a.Expired.Unix() != time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Unix() {
				t.Fatalf("%s expiry = %v", a.Email, a.Expired)
			}
			if a.IsExpired() {
				t.Fatalf("%s unexpectedly expired", a.Email)
			}
		case "e@f.com":
			if a.Expired != nil {
				t.Fatalf("%s expiry = %v", a.Email, a.Expired)
			}
		}
	}
}

func TestIsExpiredUsesPastExpiry(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	expired := Account{Expired: &past}
	if !expired.IsExpired() {
		t.Fatal("expected expired")
	}
	var noExpiry Account
	if noExpiry.IsExpired() {
		t.Fatal("nil expiry must not be expired")
	}
}

type fakeMetaStore struct {
	accounts []Account
	toggled  string
	removed  string
}

func (f *fakeMetaStore) AuthAccounts() []Account { return f.accounts }
func (f *fakeMetaStore) ToggleDisabled(id string) bool {
	f.toggled = id
	return true
}
func (f *fakeMetaStore) Remove(id string) bool {
	f.removed = id
	return true
}

func TestMetaAccountsMergedFromMetaStore(t *testing.T) {
	setAuthDirHome(t)
	if err := os.MkdirAll(paths.AuthDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	store := &fakeMetaStore{accounts: []Account{{ID: "meta-1", Email: "m@x.com", Type: Meta}}}
	m := NewManager()
	m.SetMetaStore(store)
	m.CheckAuthStatus()

	if got := m.Accounts(Meta); len(got) != 1 || got[0].ID != "meta-1" {
		t.Fatalf("meta accounts = %v", got)
	}
	if m.HasAccounts(Meta) != true {
		t.Fatal("HasAccounts(Meta) = false")
	}
}

func TestToggleAccountDisabledRewritesSortedJSONAndRefusesLastEnabled(t *testing.T) {
	setAuthDirHome(t)
	dir := paths.AuthDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	writeAuthFile(t, filepath.Join(dir, "codex-a@b.com.json"), map[string]any{
		"type": "codex", "email": "a@b.com",
	})
	writeAuthFile(t, filepath.Join(dir, "codex-c@d.com.json"), map[string]any{
		"type": "codex", "email": "c@d.com",
	})
	soloPath := filepath.Join(dir, "grok-e@f.com.json")
	writeAuthFile(t, soloPath, map[string]any{"type": "grok", "email": "e@f.com"})

	m := NewManager()
	m.CheckAuthStatus()

	// Last enabled account of a type cannot be disabled.
	if m.ToggleAccountDisabled(Account{ID: "grok-e@f.com.json", Type: Grok, FilePath: soloPath}) {
		t.Fatal("disable of last enabled account should be refused")
	}

	first := m.Accounts(Codex)[0]
	if !m.ToggleAccountDisabled(first) {
		t.Fatal("toggle failed")
	}

	data, err := os.ReadFile(first.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	// Go marshals maps with sorted keys: "disabled" < "email" < "type".
	if want := `{"disabled":true,"email":"a@b.com","type":"codex"}`; string(data) != want {
		t.Fatalf("file = %s, want %s", data, want)
	}

	accounts := m.Accounts(Codex)
	for _, a := range accounts {
		if a.Email == "a@b.com" && !a.Disabled {
			t.Fatal("state not refreshed after toggle")
		}
	}
}

func TestToggleAccountDisabledCanReEnable(t *testing.T) {
	setAuthDirHome(t)
	dir := paths.AuthDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "codex-a@b.com.json")
	writeAuthFile(t, path, map[string]any{"type": "codex", "email": "a@b.com", "disabled": true})
	writeAuthFile(t, filepath.Join(dir, "codex-c@d.com.json"), map[string]any{"type": "codex", "email": "c@d.com"})

	m := NewManager()
	m.CheckAuthStatus()

	if !m.ToggleAccountDisabled(Account{ID: "codex-a@b.com.json", Type: Codex, FilePath: path}) {
		t.Fatal("toggle failed")
	}
	var jsonData map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &jsonData); err != nil {
		t.Fatal(err)
	}
	if disabled, _ := jsonData["disabled"].(bool); disabled {
		t.Fatal("expected re-enabled file")
	}
}

func TestDeleteAccountRemovesFileAndTriggersOnChange(t *testing.T) {
	setAuthDirHome(t)
	dir := paths.AuthDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "codex-a@b.com.json")
	writeAuthFile(t, path, map[string]any{"type": "codex", "email": "a@b.com"})

	m := NewManager()
	changes := 0
	m.SetOnChange(func() { changes++ })
	m.CheckAuthStatus()
	changes = 0 // ignore the initial scan

	if !m.DeleteAccount(Account{ID: "codex-a@b.com.json", Type: Codex, FilePath: path}) {
		t.Fatal("delete failed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file still exists")
	}
	if changes != 1 {
		t.Fatalf("onChange called %d times after delete", changes)
	}
	if m.HasAccounts(Codex) {
		t.Fatal("accounts not refreshed")
	}
}

func TestSetOnChangeFiresAfterScanAndToggle(t *testing.T) {
	setAuthDirHome(t)
	dir := paths.AuthDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "codex-a@b.com.json")
	writeAuthFile(t, path, map[string]any{"type": "codex", "email": "a@b.com"})
	writeAuthFile(t, filepath.Join(dir, "codex-c@d.com.json"), map[string]any{"type": "codex", "email": "c@d.com"})

	m := NewManager()
	calls := 0
	m.SetOnChange(func() { calls++ })
	m.CheckAuthStatus()
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
	if !m.ToggleAccountDisabled(Account{ID: "codex-a@b.com.json", Type: Codex, FilePath: path}) {
		t.Fatal("toggle failed")
	}
	if calls != 2 {
		t.Fatalf("calls after toggle = %d", calls)
	}
}

func TestSaveJunieAPIKeyWritesPretty0600(t *testing.T) {
	setAuthDirHome(t)

	if err := SaveJunieAPIKey(""); err != nil {
		t.Fatalf("empty key should be a no-op, got %v", err)
	}
	if err := SaveJunieAPIKey("sk-junie-123"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(paths.AuthDir(), "junie.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pretty map[string]any
	if err := json.Unmarshal(data, &pretty); err != nil {
		t.Fatal(err)
	}
	if pretty["type"] != "junie" || pretty["email"] != "junie-user" || pretty["apiKey"] != "sk-junie-123" || pretty["disabled"] != false {
		t.Fatalf("payload = %v", pretty)
	}
	if !containsPrettyIndent(data) {
		t.Fatal("not pretty-printed")
	}
	info, err := os.Stat(filepath.Join(paths.AuthDir(), "junie.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o", info.Mode().Perm())
	}
}

func TestDirectoryMonitorDebouncesAndSurvivesRecreation(t *testing.T) {
	home := setAuthDirHome(t)
	dir := paths.AuthDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	changes := make(chan struct{}, 16)
	monitor := NewDirectoryMonitor(50*time.Millisecond, "[Test]", func() { changes <- struct{}{} })
	if err := monitor.Start(); err != nil {
		t.Fatal(err)
	}
	defer monitor.Stop()

	writeAuthFile(t, filepath.Join(dir, "a.json"), map[string]any{"type": "codex", "email": "a@b.com"})
	writeAuthFile(t, filepath.Join(dir, "b.json"), map[string]any{"type": "codex", "email": "c@d.com"})

	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange not called")
	}
	// Debounce: the two rapid writes should coalesce into one call so far.
	select {
	case <-changes:
		t.Fatal("onChange fired more than once for coalesced writes")
	case <-time.After(120 * time.Millisecond):
	}

	// Recreating the directory must still be observed: the monitor re-adds
	// the watch when the directory reappears.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Give the monitor's goroutine time to process the Create event and
	// re-establish the watch before writing again.
	time.Sleep(100 * time.Millisecond)
	writeAuthFile(t, filepath.Join(dir, "c.json"), map[string]any{"type": "codex", "email": "e@f.com"})
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange not called after directory recreation")
	}
	_ = home
}

// containsPrettyIndent checks that data was written with two-space indentation.
func containsPrettyIndent(data []byte) bool {
	return len(data) > 0 && data[len(data)-1] == '\n' && string(data[:2]) == "{\n"
}
