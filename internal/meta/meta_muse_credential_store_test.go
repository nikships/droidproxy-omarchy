package meta

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/events"
)

func testCredentials(t *testing.T, subject string, key string, revision int) Credentials {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"sub": subject, "iss": "test", "email": subject + "@example.test", "iat": revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	return Credentials{
		IdentityToken:   token,
		APIKey:          key,
		APIKeyExpiresAt: secondsFromTime(time.Unix(2_000_000_000, 0)),
	}
}

func newTestStore(t *testing.T) *CredentialStore {
	t.Helper()
	return NewCredentialStore(t.TempDir())
}

func TestLegacyMigrationAndLastRemovalDoNotResurrectAccount(t *testing.T) {
	store := newTestStore(t)
	if err := os.MkdirAll(store.Directory(), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := testCredentials(t, "alice", "test-key", 0)
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.legacyPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	accounts := store.Accounts()
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].Credentials != legacy {
		t.Errorf("credentials mismatch: %+v", accounts[0].Credentials)
	}
	if _, err := os.Stat(store.legacyPath()); !os.IsNotExist(err) {
		t.Error("legacy file must be removed after migration")
	}
	id := store.Accounts()[0].ID
	if !store.Remove(id) {
		t.Error("Remove should succeed")
	}
	if len(NewCredentialStore(store.Directory()).Accounts()) != 0 {
		t.Error("removal must persist")
	}
}

func TestMultipleAccountsReauthenticationAndPermissions(t *testing.T) {
	store := newTestStore(t)
	if !store.Save(testCredentials(t, "alice", "test-key", 0)) {
		t.Fatal("save alice failed")
	}
	if !store.Save(testCredentials(t, "bob", "test-key", 0)) {
		t.Fatal("save bob failed")
	}
	aliceID := store.Accounts()[0].ID
	if !store.ToggleDisabled(aliceID) {
		t.Fatal("toggle disabled failed")
	}
	if !store.Save(testCredentials(t, "alice", "new-key", 1)) {
		t.Fatal("re-save alice failed")
	}
	accounts := store.Accounts()
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	if accounts[0].ID != aliceID {
		t.Errorf("re-authentication must keep account position")
	}
	if accounts[0].Credentials.APIKey != "new-key" {
		t.Errorf("APIKey = %q, want new-key", accounts[0].Credentials.APIKey)
	}
	if !accounts[0].Disabled {
		t.Error("re-authentication must preserve disabled state")
	}
	if store.AuthAccounts()[0].DisplayName() != "alice@example.test" {
		t.Errorf("DisplayName = %q", store.AuthAccounts()[0].DisplayName())
	}
	info, err := os.Stat(store.AccountsPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("accounts.json perms = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("directory perms = %o, want 700", dirInfo.Mode().Perm())
	}
}

func TestLastEnabledGuardAndRemoval(t *testing.T) {
	store := newTestStore(t)
	if !store.Save(testCredentials(t, "alice", "test-key", 0)) {
		t.Fatal("save failed")
	}
	alice := store.Accounts()[0]
	if store.ToggleDisabled(alice.ID) {
		t.Error("the last enabled account cannot be disabled")
	}
	if !store.Save(testCredentials(t, "bob", "test-key", 0)) {
		t.Fatal("save failed")
	}
	bob := store.Accounts()[1]
	if !store.ToggleDisabled(alice.ID) {
		t.Error("disabling should work with two enabled accounts")
	}
	if store.ToggleDisabled(bob.ID) {
		t.Error("cannot disable the only remaining enabled account")
	}
	if !store.ToggleDisabled(alice.ID) {
		t.Error("re-enabling should work")
	}
	if !store.Remove(bob.ID) {
		t.Error("Remove should succeed")
	}
	accounts := store.Accounts()
	if len(accounts) != 1 || accounts[0].ID != alice.ID {
		t.Errorf("expected only alice, got %v", accounts)
	}
}

func TestRefreshCannotRestoreRemovedAccountOrOverwriteNewLogin(t *testing.T) {
	store := newTestStore(t)
	if !store.Save(testCredentials(t, "alice", "test-key", 0)) {
		t.Fatal("save failed")
	}
	old := store.Accounts()[0]
	if !store.Save(testCredentials(t, "alice", "new-login", 1)) {
		t.Fatal("re-save failed")
	}
	if store.UpdateKey(old, "stale", time.Now().Add(time.Hour)) {
		t.Error("stale snapshot must not overwrite a newer login")
	}
	if store.Accounts()[0].Credentials.APIKey != "new-login" {
		t.Errorf("APIKey = %q", store.Accounts()[0].Credentials.APIKey)
	}
	current := store.Accounts()[0]
	if !store.Remove(current.ID) {
		t.Fatal("Remove failed")
	}
	if store.UpdateKey(current, "resurrected", time.Now().Add(time.Hour)) {
		t.Error("stale snapshot must not resurrect a removed account")
	}
	if len(store.Accounts()) != 0 {
		t.Errorf("store should be empty, got %v", store.Accounts())
	}
}

func TestRefreshPreservesDisabledStateAndRejectsDuplicateResponse(t *testing.T) {
	store := newTestStore(t)
	if !store.Save(testCredentials(t, "alice", "test-key", 0)) {
		t.Fatal("save failed")
	}
	if !store.Save(testCredentials(t, "bob", "test-key", 0)) {
		t.Fatal("save failed")
	}
	snapshot := store.Accounts()[0]
	if !store.ToggleDisabled(snapshot.ID) {
		t.Fatal("toggle failed")
	}
	if !store.UpdateKey(snapshot, "refreshed", time.Now().Add(time.Hour)) {
		t.Fatal("update failed")
	}
	if !store.Accounts()[0].Disabled {
		t.Error("refresh must preserve disabled state")
	}
	if store.UpdateKey(snapshot, "stale", time.Now().Add(time.Hour)) {
		t.Error("a second refresh with the same snapshot must be rejected")
	}
}

func TestConfigurationIncludesOnlyEnabledUnexpiredKeysAndEscapesYAML(t *testing.T) {
	alice := AccountForCredentials(testCredentials(t, "alice", "quote\"\\\nkey", 0))
	bob := AccountForCredentials(testCredentials(t, "bob", "bob-key", 0))
	config := CompatibilityConfig([]Account{alice, bob}, true, time.Now())
	if strings.Count(config, "- api-key:") != 2 {
		t.Errorf("expected 2 api-key entries:\n%s", config)
	}
	if !strings.Contains(config, `quote\"\\\nkey`) {
		t.Errorf("YAML escaping missing:\n%s", config)
	}
	if !strings.Contains(config, "muse-spark-1.3-contributor") {
		t.Error("model list missing")
	}
	alice.Disabled = true
	bob.Credentials.APIKeyExpiresAt = secondsFromTime(time.Unix(1, 0))
	if got := CompatibilityConfig([]Account{alice, bob}, true, time.Now()); got != "" {
		t.Errorf("disabled/expired keys must produce empty config, got:\n%s", got)
	}
	if got := CompatibilityConfig([]Account{alice}, false, time.Now()); got != "" {
		t.Errorf("disabled integration must produce empty config, got:\n%s", got)
	}
}

func TestCorruptStoreIsNotOverwrittenByNewLogin(t *testing.T) {
	store := newTestStore(t)
	if !store.Save(testCredentials(t, "alice", "test-key", 0)) {
		t.Fatal("save failed")
	}
	corrupt := []byte("not-json")
	if err := os.WriteFile(store.AccountsPath(), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if store.Save(testCredentials(t, "bob", "test-key", 0)) {
		t.Error("save must fail while the store is unreadable")
	}
	data, err := os.ReadFile(store.AccountsPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(corrupt) {
		t.Error("corrupt store must not be overwritten")
	}
}

func TestCatalogEligibilityMatchesConfigurationKeys(t *testing.T) {
	store := newTestStore(t)
	if store.HasUsableAPIKey() {
		t.Error("empty store must have no usable key")
	}
	valid := testCredentials(t, "valid", "test-key", 0)
	valid.APIKeyExpiresAt = secondsFromTime(time.Now().Add(time.Hour))
	if !store.Save(valid) {
		t.Fatal("save failed")
	}
	if !store.HasUsableAPIKey() {
		t.Error("valid key should be usable")
	}

	expired := testCredentials(t, "expired", "test-key", 0)
	expired.APIKeyExpiresAt = secondsFromTime(time.Unix(1, 0))
	if !store.Save(expired) {
		t.Fatal("save failed")
	}
	if !store.Save(testCredentials(t, "empty", "", 0)) {
		t.Fatal("save failed")
	}
	// First account is "valid"; disabling it must clear usability.
	if !store.ToggleDisabled(store.Accounts()[0].ID) {
		t.Fatal("toggle failed")
	}
	if !store.HasCredentials() {
		t.Error("credentials must still exist")
	}
	if store.HasUsableAPIKey() {
		t.Error("no key should be usable")
	}
	if got := CompatibilityConfig(store.Accounts(), true, time.Now()); got != "" {
		t.Errorf("config should be empty, got:\n%s", got)
	}

	if !store.ToggleDisabled(store.Accounts()[0].ID) {
		t.Fatal("toggle failed")
	}
	if !store.HasUsableAPIKey() {
		t.Error("valid key should be usable again")
	}
	if got := UsableAPIKeys(store.Accounts(), time.Now()); len(got) != 1 || got[0] != valid.APIKey {
		t.Errorf("UsableAPIKeys = %v", got)
	}
	if CompatibilityConfig(store.Accounts(), true, time.Now()) == "" {
		t.Error("config should be non-empty")
	}
}

func TestKeyExpiringExactlyNowIsNotUsable(t *testing.T) {
	credentials := testCredentials(t, "boundary", "test-key", 0)
	account := AccountForCredentials(credentials)
	now := timeFromSeconds(credentials.APIKeyExpiresAt)
	if got := UsableAPIKeys([]Account{account}, now); len(got) != 0 {
		t.Errorf("key expiring exactly now must not be usable, got %v", got)
	}
	if got := CompatibilityConfig([]Account{account}, true, now); got != "" {
		t.Errorf("config must be empty, got:\n%s", got)
	}
}

func TestMutationsNotifySubscribers(t *testing.T) {
	store := newTestStore(t)
	fired := make(chan struct{}, 1)
	unsubscribe := events.Subscribe(events.MetaAccountsChanged, func() {
		fired <- struct{}{}
	})
	defer unsubscribe()
	if !store.Save(testCredentials(t, "alice", "test-key", 0)) {
		t.Fatal("save failed")
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("MetaAccountsChanged was not published")
	}
}

func TestAuthAccountsMapping(t *testing.T) {
	store := newTestStore(t)
	if !store.Save(testCredentials(t, "alice", "test-key", 0)) {
		t.Fatal("save failed")
	}
	accounts := store.AuthAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected 1 auth account, got %d", len(accounts))
	}
	a := accounts[0]
	if a.Type != auth.Meta {
		t.Errorf("Type = %v", a.Type)
	}
	if a.Email != "alice@example.test" || a.Login != "alice@example.test" {
		t.Errorf("email/login = %q/%q", a.Email, a.Login)
	}
	if a.Disabled {
		t.Error("fresh account must not be disabled")
	}
	if a.Expired == nil || !a.Expired.After(time.Now()) {
		t.Error("expiry must be set and in the future")
	}
	if a.FilePath != store.AccountsPath() {
		t.Errorf("FilePath = %q", a.FilePath)
	}
	// The store must satisfy the auth manager's MetaStore interface.
	var _ interface {
		AuthAccounts() []auth.Account
		ToggleDisabled(id string) bool
		Remove(id string) bool
	} = store
}

// MARK: - AuthManager (mock server, never real network)

func newMockMintServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		success := r.Header.Get("Authorization") == "Bearer good"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(map[bool]int{true: 200, false: 401}[success])
		if success {
			_, _ = w.Write([]byte(`{"api_key":"new-good"}`))
		} else {
			_, _ = w.Write([]byte(`{"error_description":"sensitive-body"}`))
		}
	}))
}

func TestRefreshContinuesAfterOneAccountFailsAndSkipsDisabledAccount(t *testing.T) {
	store := newTestStore(t)
	for _, token := range []string{"bad", "good", "disabled"} {
		credentials := Credentials{
			IdentityToken:   token,
			APIKey:          "old-" + token,
			APIKeyExpiresAt: secondsFromTime(time.Unix(1, 0)),
		}
		if !store.Save(credentials) {
			t.Fatalf("save %s failed", token)
		}
	}
	if !store.ToggleDisabled(store.Accounts()[2].ID) {
		t.Fatal("toggle failed")
	}
	server := newMockMintServer(t)
	defer server.Close()
	manager := NewAuthManager(store, server.Client())
	manager.MintURL = server.URL

	done := make(chan error, 1)
	manager.RefreshAPIKeyIfNeeded(false, func(err error) { done <- err })
	err := <-done
	if err == nil {
		t.Fatal("the failed account must surface an error")
	}
	var keys []string
	for _, a := range store.Accounts() {
		keys = append(keys, a.Credentials.APIKey)
	}
	want := []string{"old-bad", "new-good", "old-disabled"}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys = %v, want %v", keys, want)
		}
	}
	if manager.LastError() == "" {
		t.Error("lastError must be set")
	}
	if strings.Contains(manager.LastError(), "sensitive-body") {
		t.Error("upstream body must not leak into lastError")
	}
}

func TestCancelSignInPreservesExistingAccounts(t *testing.T) {
	store := newTestStore(t)
	if !store.Save(testCredentials(t, "alice", "test-key", 0)) {
		t.Fatal("save failed")
	}
	server := newMockMintServer(t)
	defer server.Close()
	manager := NewAuthManager(store, server.Client())
	manager.DeviceAuthorizationURL = server.URL
	manager.DeviceTokenURL = server.URL
	manager.MintURL = server.URL

	var callbacks atomic.Int32
	manager.StartAuthentication(
		func(code, verificationURL string) { callbacks.Add(1) },
		func(err error) { callbacks.Add(1) },
	)
	manager.CancelAuthentication()
	time.Sleep(200 * time.Millisecond)
	if got := callbacks.Load(); got != 0 {
		t.Fatalf("cancelled sign-in must have no callbacks, got %d", got)
	}
	if state := manager.State(); state.Kind != StateConnected {
		t.Errorf("state = %v, want connected", state)
	}
	if len(store.Accounts()) != 1 {
		t.Errorf("accounts = %d, want 1", len(store.Accounts()))
	}
}

func TestCancelWithoutAuthenticationIsNoOp(t *testing.T) {
	manager := NewAuthManager(newTestStore(t), http.DefaultClient)
	manager.CancelAuthentication()
	if state := manager.State(); state.Kind != StateIdle {
		t.Errorf("state = %v, want idle", state)
	}
}
