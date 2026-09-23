package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/claude"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// MetaStore is the injection point for the Meta Muse credential store: meta
// accounts live outside the auth directory, so the manager merges them in
// after every scan. Implemented by internal/meta.
type MetaStore interface {
	AuthAccounts() []Account
	ToggleDisabled(id string) bool
	Remove(id string) bool
}

// Manager tracks all authenticated accounts per service type (macOS
// AuthManager). It is safe for concurrent use.
type Manager struct {
	mu              sync.Mutex
	serviceAccounts map[ServiceType][]Account
	metaStore       MetaStore
	onChange        func()
}

// NewManager creates an empty manager.
func NewManager() *Manager {
	return &Manager{serviceAccounts: emptyAccounts()}
}

func emptyAccounts() map[ServiceType][]Account {
	m := make(map[ServiceType][]Account, len(AllServiceTypes))
	for _, t := range AllServiceTypes {
		m[t] = nil
	}
	return m
}

// SetMetaStore injects the Meta Muse credential store.
func (m *Manager) SetMetaStore(store MetaStore) {
	m.mu.Lock()
	m.metaStore = store
	m.mu.Unlock()
}

// SetOnChange registers a callback invoked after the manager's state has been
// updated by a scan (CheckAuthStatus, ToggleAccountDisabled, DeleteAccount).
// It replaces the macOS app's authDirectoryChanged notification without
// feeding the directory watcher back into itself: the caller decides what to
// do (refresh UI, notify the backend, ...). fn runs on the caller's goroutine.
func (m *Manager) SetOnChange(fn func()) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

// Accounts returns a copy of the accounts for a service type.
func (m *Manager) Accounts(serviceType ServiceType) []Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Account(nil), m.serviceAccounts[serviceType]...)
}

// HasAccounts reports whether any account exists for a service type.
func (m *Manager) HasAccounts(serviceType ServiceType) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.serviceAccounts[serviceType]) > 0
}

// CheckAuthStatus rescans the auth directory and merges meta accounts.
func (m *Manager) CheckAuthStatus() {
	authDir := paths.AuthDir()
	claude.MigrateCanonicalFiles(authDir)

	files, err := os.ReadDir(authDir)
	if err != nil {
		logx.Logf("[AuthStatus] Error checking auth status: %v", err)
		files = nil
	}

	logx.Logf("[AuthStatus] Scanning %d files in auth directory", len(files))
	newAccounts := emptyAccounts()
	for _, entry := range files {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		logx.Logf("[AuthStatus] Checking file: %s", name)
		account, ok := m.parseAccount(filepath.Join(authDir, name), name)
		if !ok {
			continue
		}
		if account.Type == Meta {
			continue
		}
		newAccounts[account.Type] = append(newAccounts[account.Type], account)
		logx.Logf("[AuthStatus] Found %s auth: %s", account.Type.DisplayName(), account.DisplayName())
	}

	var notify func()
	m.mu.Lock()
	m.serviceAccounts = newAccounts
	if m.metaStore != nil {
		m.serviceAccounts[Meta] = append([]Account(nil), m.metaStore.AuthAccounts()...)
	}
	notify = m.onChange
	m.mu.Unlock()

	if notify != nil {
		notify()
	}
}

// ToggleAccountDisabled flips the "disabled" flag of an account's auth file.
// It refuses to disable the last enabled account of a service type. Returns
// true when the file (or meta store entry) was updated.
func (m *Manager) ToggleAccountDisabled(account Account) bool {
	if account.Type == Meta {
		m.mu.Lock()
		store := m.metaStore
		m.mu.Unlock()
		if store == nil {
			return false
		}
		updated := store.ToggleDisabled(account.ID)
		if updated {
			m.CheckAuthStatus()
		}
		return updated
	}

	data, err := os.ReadFile(account.FilePath)
	if err != nil {
		logx.Logf("[AuthStatus] Failed to toggle disabled state: %v", err)
		return false
	}
	var jsonData map[string]any
	if err := json.Unmarshal(data, &jsonData); err != nil {
		logx.Logf("[AuthStatus] Failed to parse auth file as JSON: %s", account.FilePath)
		return false
	}

	currentlyDisabled, _ := jsonData["disabled"].(bool)
	if !currentlyDisabled {
		enabledCount := 0
		for _, a := range m.Accounts(account.Type) {
			if !a.Disabled {
				enabledCount++
			}
		}
		if enabledCount <= 1 {
			logx.Logf("[AuthStatus] Refusing to disable last enabled account for %s", account.Type)
			return false
		}
	}

	jsonData["disabled"] = !currentlyDisabled
	// Go marshals map[string]any with sorted keys, matching the Swift
	// JSONSerialization .sortedKeys output.
	updatedData, err := json.Marshal(jsonData)
	if err != nil {
		logx.Logf("[AuthStatus] Failed to toggle disabled state: %v", err)
		return false
	}
	if err := writeAtomic0600(account.FilePath, updatedData); err != nil {
		logx.Logf("[AuthStatus] Failed to toggle disabled state: %v", err)
		return false
	}
	logx.Logf("[AuthStatus] Toggled disabled=%t for: %s", !currentlyDisabled, account.FilePath)
	m.CheckAuthStatus()
	return true
}

// DeleteAccount removes an account's auth file (or meta store entry).
func (m *Manager) DeleteAccount(account Account) bool {
	if account.Type == Meta {
		m.mu.Lock()
		store := m.metaStore
		m.mu.Unlock()
		if store == nil {
			return false
		}
		removed := store.Remove(account.ID)
		if removed {
			m.CheckAuthStatus()
		}
		return removed
	}

	if err := os.Remove(account.FilePath); err != nil {
		logx.Logf("[AuthStatus] Failed to delete auth file: %v", err)
		return false
	}
	logx.Logf("[AuthStatus] Deleted auth file: %s", account.FilePath)
	m.CheckAuthStatus()
	return true
}

func (m *Manager) parseAccount(path, name string) (Account, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Account{}, false
	}
	var jsonData map[string]any
	if err := json.Unmarshal(data, &jsonData); err != nil {
		return Account{}, false
	}
	typeString, _ := jsonData["type"].(string)
	serviceType, ok := ServiceTypeFromAuthFileType(typeString)
	if !ok {
		return Account{}, false
	}

	logx.Logf("[AuthStatus] Found type '%s' in %s", typeString, name)

	var organizationName, claudeSeatLabel string
	if serviceType == Claude {
		fields := claude.OrganizationFieldsFrom(jsonData)
		organizationName = fields.Name
		email, _ := jsonData["email"].(string)
		claudeSeatLabel = claude.SeatDisplayLabelFromJSON(email, jsonData)
	}

	account := Account{
		ID:               name,
		Email:            stringField(jsonData, "email"),
		Login:            stringField(jsonData, "login"),
		Type:             serviceType,
		Expired:          parseExpiry(stringField(jsonData, "expired")),
		FilePath:         path,
		Disabled:         boolField(jsonData, "disabled"),
		OrganizationName: organizationName,
		ClaudeSeatLabel:  claudeSeatLabel,
	}
	return account, true
}

func stringField(jsonData map[string]any, key string) string {
	s, _ := jsonData[key].(string)
	return s
}

func boolField(jsonData map[string]any, key string) bool {
	b, _ := jsonData[key].(bool)
	return b
}

// parseExpiry parses RFC 3339 timestamps, with or without fractional seconds.
func parseExpiry(value string) *time.Time {
	if value == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}
	return &t
}

// writeAtomic0600 writes via a temp file + rename so readers never see a
// half-written auth file and OAuth tokens stay owner-only.
func writeAtomic0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".droidproxy-auth-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(tempName)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(tempName)
		return err
	}
	if err := os.Chmod(tempName, 0o600); err != nil {
		os.Remove(tempName)
		return err
	}
	return os.Rename(tempName, path)
}
