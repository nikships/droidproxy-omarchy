package meta

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/auth"
	"github.com/nikships/droidproxy-omarchy/internal/events"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// Credentials are the Meta Model API credentials for a Muse subscription.
// IdentityToken is the OAuth access token obtained from the device-code login;
// it is not sent to the Model API directly but is re-used to mint fresh
// APIKey values without asking the user to sign in again. APIKey is what
// actually authenticates requests to https://api.meta.ai/v1.
//
// APIKeyExpiresAt is stored as unix seconds, matching the Swift encoder's
// .secondsSince1970 date strategy (the on-disk format must stay compatible).
type Credentials struct {
	IdentityToken   string  `json:"identity_token"`
	APIKey          string  `json:"api_key"`
	APIKeyExpiresAt float64 `json:"api_key_expires_at"`
}

// Account is one stored Meta Muse account.
type Account struct {
	ID          string      `json:"id"`
	Email       string      `json:"email,omitempty"`
	Credentials Credentials `json:"credentials"`
	Disabled    bool        `json:"disabled"`
}

// DisplayName matches MetaMuseAccount.displayName.
func (a Account) DisplayName() string {
	if a.Email != "" {
		return a.Email
	}
	prefix := a.ID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return "Meta account " + prefix
}

// CredentialStore is the JSON-backed Meta account store at
// ~/.droidproxy/meta/accounts.json (0600, directory 0700). All mutations are
// serialized, including compare-and-swap refreshes: a late network response
// must never restore an account removed while refreshing.
type CredentialStore struct {
	directory string
	mu        sync.Mutex
}

var (
	sharedMu  sync.Mutex
	shared    *CredentialStore
	sharedDir string
)

// Shared returns the process-wide store for paths.MetaDataDir(). If HOME
// changes (tests), a fresh store is opened for the new path, like prefs.Shared().
func Shared() *CredentialStore {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	dir := paths.MetaDataDir()
	if shared == nil || sharedDir != dir {
		shared = NewCredentialStore(dir)
		sharedDir = dir
	}
	return shared
}

// NewCredentialStore opens a store rooted at directory.
func NewCredentialStore(directory string) *CredentialStore {
	return &CredentialStore{directory: directory}
}

// Directory returns the store's root directory.
func (s *CredentialStore) Directory() string { return s.directory }

// AccountsPath is the accounts.json location.
func (s *CredentialStore) AccountsPath() string { return filepath.Join(s.directory, "accounts.json") }

func (s *CredentialStore) legacyPath() string {
	return filepath.Join(s.directory, "credentials.json")
}

// Accounts returns the stored accounts, or nil when the store is unreadable.
func (s *CredentialStore) Accounts() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts, err := s.load()
	if err != nil {
		logx.Logf("[Meta] Could not load account store: %v", err)
		return nil
	}
	return accounts
}

// HasCredentials reports whether any account is stored.
func (s *CredentialStore) HasCredentials() bool { return len(s.Accounts()) > 0 }

// HasUsableAPIKey reports whether any enabled account has an unexpired key.
func (s *CredentialStore) HasUsableAPIKey() bool {
	return len(UsableAPIKeys(s.Accounts(), time.Now())) > 0
}

func (s *CredentialStore) load() ([]Account, error) {
	if data, err := os.ReadFile(s.AccountsPath()); err == nil {
		var accounts []Account
		if err := json.Unmarshal(data, &accounts); err != nil {
			return nil, err
		}
		return accounts, nil
	}
	data, err := os.ReadFile(s.legacyPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var credentials Credentials
	if err := json.Unmarshal(data, &credentials); err != nil {
		return nil, err
	}
	migrated := []Account{AccountForCredentials(credentials)}
	if err := s.write(migrated); err != nil {
		return nil, err
	}
	// The new store is authoritative even if legacy cleanup fails.
	_ = os.Remove(s.legacyPath())
	return migrated, nil
}

func (s *CredentialStore) write(accounts []Account) error {
	if err := paths.EnsureDir(s.directory, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(accounts)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.directory, ".accounts.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.AccountsPath()); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Chmod(s.AccountsPath(), 0o600)
}

// mutate applies update to a freshly loaded copy and persists it when update
// returns true. Any mutation publishes events.MetaAccountsChanged after the
// lock is released, mirroring the Swift post on the main queue.
func (s *CredentialStore) mutate(update func(accounts *[]Account) bool) bool {
	s.mu.Lock()
	changed := false
	accounts, err := s.load()
	if err != nil {
		logx.Logf("[Meta] Could not update account store: %v", err)
	} else if update(&accounts) {
		if err := s.write(accounts); err != nil {
			logx.Logf("[Meta] Could not update account store: %v", err)
		} else {
			changed = true
		}
	}
	s.mu.Unlock()
	if changed {
		events.Publish(events.MetaAccountsChanged)
	}
	return changed
}

// Save stores credentials, updating the account in place when its id already
// exists (re-authentication keeps the account's position and disabled flag).
func (s *CredentialStore) Save(credentials Credentials) bool {
	account := AccountForCredentials(credentials)
	return s.mutate(func(accounts *[]Account) bool {
		for i := range *accounts {
			if (*accounts)[i].ID == account.ID {
				(*accounts)[i].Credentials = credentials
				if account.Email != "" {
					(*accounts)[i].Email = account.Email
				}
				return true
			}
		}
		*accounts = append(*accounts, account)
		return true
	})
}

// Remove deletes an account by id. It reports false when the id is unknown.
func (s *CredentialStore) Remove(id string) bool {
	return s.mutate(func(accounts *[]Account) bool {
		found := false
		kept := (*accounts)[:0]
		for _, a := range *accounts {
			if a.ID == id {
				found = true
				continue
			}
			kept = append(kept, a)
		}
		if !found {
			return false
		}
		*accounts = kept
		return true
	})
}

// ToggleDisabled flips an account's disabled flag. The last enabled account
// cannot be disabled (at least one account must stay usable).
func (s *CredentialStore) ToggleDisabled(id string) bool {
	return s.mutate(func(accounts *[]Account) bool {
		index := -1
		for i := range *accounts {
			if (*accounts)[i].ID == id {
				index = i
				break
			}
		}
		if index < 0 {
			return false
		}
		if !(*accounts)[index].Disabled {
			enabled := 0
			for _, a := range *accounts {
				if !a.Disabled {
					enabled++
				}
			}
			if enabled <= 1 {
				return false
			}
		}
		(*accounts)[index].Disabled = !(*accounts)[index].Disabled
		return true
	})
}

// UpdateKey applies a minted API key, but only when the account still holds
// exactly the credentials of the snapshot. This makes a stale refresh a no-op:
// it can neither overwrite a newer login nor resurrect a removed account.
func (s *CredentialStore) UpdateKey(snapshot Account, apiKey string, expiresAt time.Time) bool {
	return s.mutate(func(accounts *[]Account) bool {
		for i := range *accounts {
			if (*accounts)[i].ID == snapshot.ID && (*accounts)[i].Credentials == snapshot.Credentials {
				(*accounts)[i].Credentials.APIKey = apiKey
				(*accounts)[i].Credentials.APIKeyExpiresAt = secondsFromTime(expiresAt)
				return true
			}
		}
		return false
	})
}

// AccountForCredentials derives the account identity from the identity token's
// JWT claims. Claims are display/deduplication hints only, never authorization.
func AccountForCredentials(credentials Credentials) Account {
	claims := map[string]any{}
	parts := splitNonEmpty(credentials.IdentityToken, ".")
	if len(parts) == 3 {
		payload := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
		for len(payload)%4 != 0 {
			payload += "="
		}
		if data, err := base64.StdEncoding.DecodeString(payload); err == nil {
			_ = json.Unmarshal(data, &claims)
		}
	}
	subject, _ := claims["sub"].(string)
	iss, _ := claims["iss"].(string)
	identity := credentials.IdentityToken
	if subject != "" {
		identity = iss + ":" + subject
	}
	sum := sha256.Sum256([]byte(identity))
	id := hex.EncodeToString(sum[:])
	email, _ := claims["email"].(string)
	return Account{ID: id, Email: email, Credentials: credentials, Disabled: false}
}

// AuthAccounts maps the store onto the auth manager's account list so Meta
// accounts appear alongside the auth-directory accounts.
func (s *CredentialStore) AuthAccounts() []auth.Account {
	accounts := s.Accounts()
	out := make([]auth.Account, 0, len(accounts))
	for i := range accounts {
		exp := timeFromSeconds(accounts[i].Credentials.APIKeyExpiresAt)
		out = append(out, auth.Account{
			ID:       accounts[i].ID,
			Email:    accounts[i].Email,
			Login:    accounts[i].DisplayName(),
			Type:     auth.Meta,
			Expired:  &exp,
			FilePath: s.AccountsPath(),
			Disabled: accounts[i].Disabled,
		})
	}
	return out
}

// APIKeyExpiredAt reports whether a Model API key with the given unix-seconds
// expiry is expired at now.
func APIKeyExpiredAt(expiresAt float64, now time.Time) bool {
	return !timeFromSeconds(expiresAt).After(now)
}

// UsableAPIKeys returns the API keys of enabled accounts whose key has not
// expired at now, in stored order.
func UsableAPIKeys(accounts []Account, now time.Time) []string {
	var keys []string
	for _, a := range accounts {
		if !a.Disabled && timeFromSeconds(a.Credentials.APIKeyExpiresAt).After(now) && a.Credentials.APIKey != "" {
			keys = append(keys, a.Credentials.APIKey)
		}
	}
	return keys
}

// CompatibilityConfig renders the CLIProxyAPI openai-compatibility block for
// the Meta accounts. The output must stay byte-identical to the macOS app's:
// ServerManager concatenates it into the generated config. Empty when disabled
// or when no enabled account has an unexpired key.
func CompatibilityConfig(accounts []Account, enabled bool, now time.Time) string {
	keys := UsableAPIKeys(accounts, now)
	if !enabled || len(keys) == 0 {
		return ""
	}
	// Completions still go through this compatibility block (and its account
	// failover). Responses are TLS-forwarded by ThinkingProxy — CLIProxyAPI
	// would otherwise rewrite them into /chat/completions.
	// JSON strings are valid YAML scalars, including quotes/control characters.
	// Swift's JSONEncoder escapes "/" as "\/"; match that byte-for-byte.
	entries := make([]string, 0, len(keys))
	for _, key := range keys {
		quoted, err := json.Marshal(key)
		if err != nil {
			continue
		}
		entries = append(entries, "      - api-key: "+strings.ReplaceAll(string(quoted), "/", `\/`))
	}
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString("# Meta Muse subscription (auto-added by DroidProxy)\n")
	b.WriteString("openai-compatibility:\n")
	b.WriteString("  - name: \"meta\"\n")
	b.WriteString("    base-url: \"https://api.meta.ai/v1\"\n")
	b.WriteString("    api-key-entries:\n")
	b.WriteString(strings.Join(entries, "\n"))
	b.WriteString("\n")
	b.WriteString("    models:\n")
	b.WriteString("      - name: \"muse-spark-1.3\"\n")
	b.WriteString("      - name: \"muse-spark-1.3-contributor\"\n")
	return b.String()
}

// secondsFromTime converts a time to unix seconds (the on-disk date format).
func secondsFromTime(t time.Time) float64 {
	return float64(t.Unix()) + float64(t.Nanosecond())/1e9
}

// timeFromSeconds converts unix seconds back to a time.
func timeFromSeconds(s float64) time.Time {
	sec := int64(s)
	nsec := int64((s - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
