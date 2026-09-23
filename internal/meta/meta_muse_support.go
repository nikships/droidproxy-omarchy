package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/logx"
)

// AuthManager reproduces `muse login`'s device-code flow entirely in-process
// (three plain HTTPS calls, no child process): device authorization and token
// polling against auth.meta.com, then Model API key minting against
// api.meta.ai/muse-code/key. The resulting API key is what the backend writes
// into CLIProxyAPI's openai-compatibility config for Completions. Muse
// /v1/responses is TLS-forwarded by ThinkingProxy instead, because the
// compatibility executor translates Responses into Chat Completions.
//
// This contract is reverse-engineered from the installed `muse` CLI binary and
// a working third-party client (pi-meta-oauth), not from official Meta
// documentation. Errors expose status codes, not potentially sensitive bodies.

// Default endpoints. Overridable via AuthManager fields so tests use
// httptest and never touch the network.
const (
	DefaultClientID               = "1031625952748946"
	DefaultDeviceAuthorizationURL = "https://auth.meta.com/oidc/device/authorization/"
	DefaultDeviceTokenURL         = "https://auth.meta.com/oidc/device/token/"
	DefaultMintURL                = "https://api.meta.ai/muse-code/key"
	deviceCodeGrantType           = "urn:ietf:params:oauth:grant-type:device_code"
)

// Timing mirrors the Swift Timing constants.
const (
	// APIKeyValidityInterval: minted keys are treated as valid for this long
	// before a proactive re-mint is due (the reference client re-mints
	// roughly daily).
	APIKeyValidityInterval = 24 * time.Hour
	// RefreshMargin: re-mint this far ahead of the recorded expiry so a
	// request never races an about-to-expire key.
	RefreshMargin = 6 * time.Hour
	// DefaultPollInterval / DefaultExpiresIn fill in server omissions.
	DefaultPollInterval = 5 * time.Second
	DefaultExpiresIn    = 15 * time.Minute
)

// AuthError is a Meta sign-in failure. Error() returns the user-facing
// description shown in the UI.
type AuthErrorCode int

const (
	AuthErrDeviceAuthorizationFailed AuthErrorCode = iota
	AuthErrLoginDenied
	AuthErrLoginExpired
	AuthErrPollFailed
	AuthErrMintFailed
	AuthErrNotAuthenticated
)

type AuthError struct {
	Code   AuthErrorCode
	Detail string
}

func (e *AuthError) Error() string {
	switch e.Code {
	case AuthErrDeviceAuthorizationFailed:
		return "Could not start Meta sign-in: " + e.Detail
	case AuthErrLoginDenied:
		return "Meta sign-in was denied."
	case AuthErrLoginExpired:
		return "Meta sign-in request expired. Please try again."
	case AuthErrPollFailed:
		return "Meta sign-in failed: " + e.Detail
	case AuthErrMintFailed:
		return "Could not obtain a Meta Model API key: " + e.Detail
	case AuthErrNotAuthenticated:
		return "Connect your Meta Muse subscription first."
	}
	return e.Detail
}

// StateKind is the lifecycle of the Meta Muse credential. Unlike Copilot,
// there is no local gateway process to supervise — this only tracks the
// OAuth/mint round trip.
type StateKind int

const (
	StateIdle StateKind = iota
	StateAuthenticating
	StateConnected
	StateFailed
)

// State carries the state plus the failure detail for StateFailed.
type State struct {
	Kind   StateKind
	Detail string
}

// FailureDescription is the message shown for StateFailed; empty otherwise.
func (s State) FailureDescription() string {
	if s.Kind == StateFailed {
		return s.Detail
	}
	return ""
}

func (s State) String() string {
	switch s.Kind {
	case StateIdle:
		return "idle"
	case StateAuthenticating:
		return "authenticating"
	case StateConnected:
		return "connected"
	case StateFailed:
		return "failed(" + s.Detail + ")"
	}
	return "unknown"
}

// AuthManager drives the device-code login and API-key minting lifecycle.
type AuthManager struct {
	store  *CredentialStore
	client *http.Client

	// Injectable endpoint/identity values and browser opener.
	ClientID               string
	DeviceAuthorizationURL string
	DeviceTokenURL         string
	MintURL                string
	OpenVerificationURL    func(string) error

	mu        sync.Mutex
	state     State
	lastError string
	// pollGeneration invalidates in-flight callbacks after a cancel or a new
	// login: a stale response must never change state.
	pollGeneration int
	activeCancel   context.CancelFunc
	// refreshing guards re-mints so an invalid identity token on one account
	// cannot block the others.
	refreshing map[string]bool

	onChange func()
}

// NewAuthManager creates a manager for the store. client may be nil for
// http.DefaultClient.
func NewAuthManager(store *CredentialStore, client *http.Client) *AuthManager {
	m := &AuthManager{
		store:                  store,
		client:                 client,
		ClientID:               DefaultClientID,
		DeviceAuthorizationURL: DefaultDeviceAuthorizationURL,
		DeviceTokenURL:         DefaultDeviceTokenURL,
		MintURL:                DefaultMintURL,
		OpenVerificationURL:    defaultOpenVerificationURL,
		refreshing:             map[string]bool{},
	}
	m.state = m.HasCredentialsState()
	return m
}

func defaultOpenVerificationURL(target string) error {
	cmd := exec.Command("xdg-open", target)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// HasCredentials reports whether the store holds any account.
func (m *AuthManager) HasCredentials() bool { return m.store.HasCredentials() }

// State returns the current lifecycle state.
func (m *AuthManager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// LastError returns the most recent failure description, or "".
func (m *AuthManager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastError
}

// SetOnChange registers a callback invoked after every state/lastError change,
// for UI refreshes.
func (m *AuthManager) SetOnChange(fn func()) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

func (m *AuthManager) notifyChange() {
	if fn := m.onChange; fn != nil {
		// Run on a separate goroutine so subscribers can re-enter the manager.
		go fn()
	}
}

// HasCredentialsState returns the initial state for the store's contents.
func (m *AuthManager) HasCredentialsState() State {
	if m.HasCredentials() {
		return State{Kind: StateConnected}
	}
	return State{Kind: StateIdle}
}

// StartAuthentication begins the device authorization flow. onDeviceCode is
// called once the verification URL has been opened in the browser; completion
// runs exactly once. Errors are *AuthError.
func (m *AuthManager) StartAuthentication(
	onDeviceCode func(code, verificationURL string),
	completion func(error),
) {
	m.mu.Lock()
	if m.state.Kind == StateAuthenticating {
		m.mu.Unlock()
		return
	}
	m.pollGeneration++
	generation := m.pollGeneration
	m.state = State{Kind: StateAuthenticating}
	m.lastError = ""
	m.mu.Unlock()
	m.notifyChange()

	go func() {
		device, err := m.requestDeviceAuthorization(generation)
		if !m.isCurrent(generation) {
			return
		}
		if err != nil {
			m.finishAuthentication(err, completion)
			return
		}
		verificationURL := device.VerificationURIComplete
		if verificationURL == "" {
			verificationURL = device.VerificationURI
		}
		parsed, parseErr := parseHTTPSURL(verificationURL)
		host := ""
		if parseErr == nil {
			host = parsed.Host
		}
		if parseErr != nil || (host != "meta.com" && !strings.HasSuffix(host, ".meta.com")) {
			m.finishAuthentication(&AuthError{Code: AuthErrDeviceAuthorizationFailed, Detail: "invalid verification URL"}, completion)
			return
		}
		if err := m.OpenVerificationURL(verificationURL); err != nil {
			logx.Logf("[Meta] Could not open verification URL: %v", err)
		}
		if onDeviceCode != nil {
			onDeviceCode(device.UserCode, verificationURL)
		}
		interval := device.Interval
		if interval < 1 {
			interval = DefaultPollInterval.Seconds()
		}
		expiresIn := device.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = DefaultExpiresIn.Seconds()
		}
		deadline := time.Now().Add(time.Duration(expiresIn * float64(time.Second)))
		m.pollForToken(device.DeviceCode, interval, deadline, generation, completion)
	}()
}

// CancelAuthentication stops an in-flight sign-in, preserving existing accounts.
func (m *AuthManager) CancelAuthentication() {
	m.mu.Lock()
	if m.state.Kind != StateAuthenticating {
		m.mu.Unlock()
		return
	}
	m.pollGeneration++
	if m.activeCancel != nil {
		m.activeCancel()
		m.activeCancel = nil
	}
	m.state = m.HasCredentialsState()
	m.lastError = ""
	m.mu.Unlock()
	m.notifyChange()
}

// RefreshAPIKeyIfNeeded re-mints keys that are expired or inside the refresh
// margin (or all enabled keys when force is set). Accounts refresh
// independently so one invalid identity token cannot block the others.
func (m *AuthManager) RefreshAPIKeyIfNeeded(force bool, completion func(error)) {
	now := time.Now()
	var accounts []Account
	m.mu.Lock()
	for _, account := range m.store.Accounts() {
		if account.Disabled || m.refreshing[account.ID] {
			continue
		}
		if !force && now.Before(timeFromSeconds(account.Credentials.APIKeyExpiresAt).Add(-RefreshMargin)) {
			continue
		}
		m.refreshing[account.ID] = true
		accounts = append(accounts, account)
	}
	m.mu.Unlock()

	if len(accounts) == 0 {
		if completion != nil {
			completion(nil)
		}
		return
	}

	var (
		wg         sync.WaitGroup
		errMu      sync.Mutex
		firstError error
	)
	for _, account := range accounts {
		account := account
		wg.Add(1)
		go func() {
			defer wg.Done()
			apiKey, err := m.mintAPIKey(account.Credentials.IdentityToken)
			m.mu.Lock()
			delete(m.refreshing, account.ID)
			m.mu.Unlock()
			if err != nil {
				errMu.Lock()
				if firstError == nil {
					firstError = err
				}
				errMu.Unlock()
				return
			}
			// A removed/re-authenticated account makes this a harmless no-op.
			if !m.store.UpdateKey(account, apiKey, now.Add(APIKeyValidityInterval)) {
				for _, current := range m.store.Accounts() {
					if current.ID == account.ID && current.Credentials == account.Credentials {
						errMu.Lock()
						if firstError == nil {
							firstError = &AuthError{Code: AuthErrMintFailed, Detail: "could not save the refreshed key"}
						}
						errMu.Unlock()
						break
					}
				}
			}
		}()
	}
	wg.Wait()

	if firstError != nil {
		m.mu.Lock()
		m.lastError = firstError.Error()
		m.mu.Unlock()
		m.notifyChange()
		if completion != nil {
			completion(firstError)
		}
		return
	}
	if completion != nil {
		completion(nil)
	}
}

// MARK: - Device authorization

type deviceAuthorizationResponse struct {
	DeviceCode              string  `json:"device_code"`
	UserCode                string  `json:"user_code"`
	VerificationURI         string  `json:"verification_uri"`
	VerificationURIComplete string  `json:"verification_uri_complete"`
	ExpiresIn               float64 `json:"expires_in"`
	Interval                float64 `json:"interval"`
}

func (m *AuthManager) isCurrent(generation int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return generation == m.pollGeneration
}

// setActiveCancel stores the cancellation handle for the in-flight request.
func (m *AuthManager) setActiveCancel(cancel context.CancelFunc) {
	m.mu.Lock()
	m.activeCancel = cancel
	m.mu.Unlock()
}

func (m *AuthManager) requestDeviceAuthorization(generation int) (deviceAuthorizationResponse, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m.setActiveCancel(cancel)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.DeviceAuthorizationURL,
		strings.NewReader(formEncodedBody(map[string]string{"client_id": m.ClientID})))
	if err != nil {
		return deviceAuthorizationResponse{}, &AuthError{Code: AuthErrDeviceAuthorizationFailed, Detail: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() != nil || !m.isCurrent(generation) {
			return deviceAuthorizationResponse{}, errCancelled
		}
		return deviceAuthorizationResponse{}, &AuthError{Code: AuthErrDeviceAuthorizationFailed, Detail: err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) == 0 {
		return deviceAuthorizationResponse{}, &AuthError{Code: AuthErrDeviceAuthorizationFailed, Detail: "no response"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return deviceAuthorizationResponse{}, &AuthError{Code: AuthErrDeviceAuthorizationFailed, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	var decoded deviceAuthorizationResponse
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.DeviceCode == "" || decoded.UserCode == "" || decoded.VerificationURI == "" {
		return deviceAuthorizationResponse{}, &AuthError{Code: AuthErrDeviceAuthorizationFailed, Detail: "malformed response"}
	}
	return decoded, nil
}

// MARK: - Token polling

type deviceTokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
}

var errCancelled = errors.New("cancelled")

func (m *AuthManager) pollForToken(deviceCode string, interval float64, deadline time.Time, generation int, completion func(error)) {
	for {
		if !m.isCurrent(generation) {
			return
		}
		if !time.Now().Before(deadline) {
			m.finishAuthentication(&AuthError{Code: AuthErrLoginExpired}, completion)
			return
		}

		accessToken, pollErr := m.pollOnce(deviceCode, generation)
		if !m.isCurrent(generation) {
			return
		}
		if pollErr != nil {
			var slow slowDownError
			if errors.As(pollErr, &slow) {
				// RFC 8628 slow_down: back off by 5s before the next poll.
				interval += 5
				continue
			}
			m.finishAuthentication(pollErr, completion)
			return
		}
		if accessToken != "" {
			m.mintAndPersist(accessToken, generation, completion)
			return
		}

		time.Sleep(time.Duration(interval * float64(time.Second)))
	}
}

// pollOnce performs a single token poll. An empty token with a nil error means
// authorization is still pending; slowDownError makes the caller back off.
type slowDownError struct{}

func (slowDownError) Error() string { return "slow down" }

func (m *AuthManager) pollOnce(deviceCode string, generation int) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m.setActiveCancel(cancel)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.DeviceTokenURL,
		strings.NewReader(formEncodedBody(map[string]string{
			"grant_type":  deviceCodeGrantType,
			"device_code": deviceCode,
			"client_id":   m.ClientID,
		})))
	if err != nil {
		return "", &AuthError{Code: AuthErrPollFailed, Detail: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() != nil || !m.isCurrent(generation) {
			return "", errCancelled
		}
		return "", &AuthError{Code: AuthErrPollFailed, Detail: err.Error()}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil || len(body) == 0 {
		return "", &AuthError{Code: AuthErrPollFailed, Detail: "no response"}
	}
	var decoded deviceTokenResponse
	_ = json.Unmarshal(body, &decoded)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 && decoded.AccessToken != "" {
		return decoded.AccessToken, nil
	}

	switch decoded.Error {
	case "authorization_pending":
		return "", nil
	case "slow_down":
		return "", slowDownError{}
	case "access_denied":
		return "", &AuthError{Code: AuthErrLoginDenied}
	case "expired_token":
		return "", &AuthError{Code: AuthErrLoginExpired}
	default:
		return "", &AuthError{Code: AuthErrPollFailed, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
}

func (m *AuthManager) mintAndPersist(identityToken string, generation int, completion func(error)) {
	if !m.isCurrent(generation) {
		return
	}
	apiKey, err := m.mintAPIKey(identityToken)
	if !m.isCurrent(generation) {
		return
	}
	if err != nil {
		m.finishAuthentication(err, completion)
		return
	}
	credentials := Credentials{
		IdentityToken:   identityToken,
		APIKey:          apiKey,
		APIKeyExpiresAt: secondsFromTime(time.Now().Add(APIKeyValidityInterval)),
	}
	if !m.store.Save(credentials) {
		m.finishAuthentication(&AuthError{Code: AuthErrMintFailed, Detail: "could not save credentials"}, completion)
		return
	}
	m.finishAuthentication(nil, completion)
}

// MARK: - Key minting

type mintResponse struct {
	APIKey    string `json:"api_key"`
	ActionURL string `json:"action_url"`
}

func (m *AuthManager) mintAPIKey(identityToken string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, m.MintURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", &AuthError{Code: AuthErrMintFailed, Detail: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+identityToken)
	req.Header.Set("x-api-version", "1.0.0")
	resp, err := m.client.Do(req)
	if err != nil {
		return "", &AuthError{Code: AuthErrMintFailed, Detail: err.Error()}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", &AuthError{Code: AuthErrMintFailed, Detail: err.Error()}
	}
	var decoded mintResponse
	_ = json.Unmarshal(body, &decoded)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &AuthError{Code: AuthErrMintFailed, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	if decoded.APIKey == "" {
		if decoded.ActionURL != "" {
			return "", &AuthError{Code: AuthErrMintFailed, Detail: "payment method required; complete setup in Meta Muse"}
		}
		return "", &AuthError{Code: AuthErrMintFailed, Detail: "no API key was issued"}
	}
	return decoded.APIKey, nil
}

// MARK: - Helpers

func (m *AuthManager) finishAuthentication(err error, completion func(error)) {
	m.mu.Lock()
	m.activeCancel = nil
	if err != nil {
		description := err.Error()
		m.state = State{Kind: StateFailed, Detail: description}
		m.lastError = description
	} else {
		m.state = State{Kind: StateConnected}
		m.lastError = ""
	}
	m.mu.Unlock()
	m.notifyChange()
	if completion != nil {
		completion(err)
	}
}

// formEncodedBody builds an application/x-www-form-urlencoded body. "+" is
// always encoded as %2B because form decoding treats a bare "+" as a space.
func formEncodedBody(fields map[string]string) string {
	var b strings.Builder
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(fields[k]))
	}
	return b.String()
}

// parseHTTPSURL validates that target is an absolute https URL and returns its host.
func parseHTTPSURL(target string) (*url.URL, error) {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("not an https URL: %q", target)
	}
	return parsed, nil
}
