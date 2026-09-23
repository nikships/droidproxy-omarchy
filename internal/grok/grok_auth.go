package grok

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// xAI Grok OAuth 2.0 Device Authorization Grant (RFC 8628), token refresh, and
// credential storage compatible with the auth manager's auth-directory scan.
//
// Uses the public Grok CLI OAuth client (token_endpoint_auth_method: none) so
// the proxy can call api.x.ai with a bearer token instead of an XAI_API_KEY.

const (
	// ClientID is the public OAuth client id used by the Grok CLI. Not a
	// secret: the device grant uses no client authentication.
	ClientID        = "b1a00492-073a-47ea-816f-4c329264a828"
	Scope           = "openid profile email offline_access grok-cli:access api:access"
	DeviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// APIHost is the public xAI API host. Serves grok-4.7.
	APIHost = "api.x.ai"
	// BuildProxyHost is the Grok Build chat proxy. Serves grok-4.7-build-fast,
	// which the public API does not. Routing uses x-grok-model-override; the
	// JSON model field is the same id.
	BuildProxyHost = "cli-chat-proxy.grok.com"
	FastModelID    = "grok-4.7-build-fast"
	// BuildProxyClientVersion must be sent: the Build proxy rejects requests
	// that omit it ("version (none) is outdated").
	BuildProxyClientVersion = "1.0.40"

	// AuthFileType is the auth file "type" tag scanned by the auth manager.
	AuthFileType = "grok-cli"
	AuthFileName = "grok-cli.json"

	// RefreshSkewMs refreshes this many ms before access-token expiry so long
	// Droid sessions stay warm without mid-turn 401s. Kept well below typical
	// expires_in (often 1h) so a fresh token is not treated as expired
	// immediately. Applied at check time, never baked into the stored expiry.
	RefreshSkewMs float64 = 300_000
)

// Endpoints and I/O hooks. Tests point these at httptest servers and stubs.
var (
	DeviceCodeURL = "https://auth.x.ai/oauth2/device/code"
	TokenURL      = "https://auth.x.ai/oauth2/token"
	// HTTPClient is used for every auth request.
	HTTPClient = &http.Client{Timeout: 60 * time.Second}
	// OpenBrowser opens the verification URL. It must not block.
	OpenBrowser = defaultOpenBrowser
)

func defaultOpenBrowser(target string) error {
	cmd := exec.Command("xdg-open", target)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// excludedUpstreamHeaderNames are client headers dropped when building the
// upstream request.
var excludedUpstreamHeaderNames = map[string]struct{}{
	"host": {}, "content-length": {}, "connection": {}, "transfer-encoding": {},
	"authorization": {}, "content-type": {}, "anthropic-beta": {}, "anthropic-version": {},
	"accept-encoding": {}, "x-api-key": {}, "x-xai-token-auth": {}, "x-grok-model-override": {},
	"x-grok-client-version": {}, "x-grok-client-identifier": {},
}

// DeviceAuthorization is the parsed device-code response.
type DeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURIComplete string
	Interval                float64 // seconds
	ExpiresIn               float64 // seconds
}

// Credentials are the stored Grok OAuth tokens.
type Credentials struct {
	Access  string
	Refresh string
	// ExpiresAtMs is the absolute access-token expiry in epoch milliseconds.
	// Skew is applied in IsAccessExpired, not stored here.
	ExpiresAtMs float64
	// Email is empty when unknown.
	Email string
}

func epochMs(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e6
}

// IsAccessExpired reports whether the access token is expired (or within the
// default refresh skew of expiring) at now.
func (c Credentials) IsAccessExpired(now time.Time) bool {
	return c.IsAccessExpiredWithSkew(now, RefreshSkewMs)
}

// IsAccessExpiredWithSkew is IsAccessExpired with an explicit skew.
func (c Credentials) IsAccessExpiredWithSkew(now time.Time, skewMs float64) bool {
	return epochMs(now)+skewMs >= c.ExpiresAtMs
}

// AuthErrorKind distinguishes GrokAuthError cases.
type AuthErrorKind int

const (
	AuthErrNotLoggedIn AuthErrorKind = iota
	AuthErrReauthRequired
	AuthErrDeviceCodeFailed
	AuthErrAuthorizationPending
	AuthErrSlowDown
	AuthErrAccessDenied
	AuthErrExpiredToken
	AuthErrTokenError
	AuthErrNetwork
	AuthErrCancelled
)

// AuthError is a Grok auth failure. Detail is only meaningful for
// DeviceCodeFailed, TokenError and Network.
type AuthError struct {
	Kind   AuthErrorKind
	Detail string
}

func newAuthError(kind AuthErrorKind, detail string) *AuthError {
	return &AuthError{Kind: kind, Detail: detail}
}

// Error is the user-facing description shown in the UI.
func (e *AuthError) Error() string {
	switch e.Kind {
	case AuthErrNotLoggedIn:
		return "Not logged in to Grok."
	case AuthErrReauthRequired:
		return "Grok session expired. Reconnect Grok in DroidProxy settings."
	case AuthErrDeviceCodeFailed:
		return "Could not start Grok login: " + e.Detail
	case AuthErrAuthorizationPending:
		return "Authorization pending."
	case AuthErrSlowDown:
		return "Polling too fast."
	case AuthErrAccessDenied:
		return "Authorization was denied."
	case AuthErrExpiredToken:
		return "The login request expired. Please try again."
	case AuthErrTokenError:
		return e.Detail
	case AuthErrNetwork:
		return "Network error: " + e.Detail
	case AuthErrCancelled:
		return "Login cancelled."
	}
	return e.Detail
}

// IsTerminalRefreshFailure reports refresh failures that should stop retrying
// until the user reconnects.
func (e *AuthError) IsTerminalRefreshFailure() bool {
	switch e.Kind {
	case AuthErrReauthRequired, AuthErrAccessDenied, AuthErrExpiredToken:
		return true
	case AuthErrTokenError:
		lower := strings.ToLower(e.Detail)
		return strings.Contains(lower, "invalid_grant") ||
			strings.Contains(lower, "invalid_token") ||
			strings.Contains(lower, "invalid_request")
	}
	return false
}

// MARK: - Pure parsing helpers

// authJSONNumber coerces a decoded JSON number (float64 or json.Number).
func authJSONNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	}
	return 0, false
}

func authJSONObject(data []byte) map[string]any {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil
	}
	return obj
}

// ParseDeviceAuthorization parses the device-code endpoint response.
func ParseDeviceAuthorization(data []byte) (DeviceAuthorization, bool) {
	obj := authJSONObject(data)
	deviceCode, _ := obj["device_code"].(string)
	userCode, _ := obj["user_code"].(string)
	if deviceCode == "" || userCode == "" {
		return DeviceAuthorization{}, false
	}
	complete, ok := obj["verification_uri_complete"].(string)
	if !ok {
		complete, _ = obj["verification_uri"].(string)
	}
	interval, ok := authJSONNumber(obj["interval"])
	if !ok {
		interval = 5
	}
	expiresIn, ok := authJSONNumber(obj["expires_in"])
	if !ok {
		expiresIn = 900
	}
	return DeviceAuthorization{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURIComplete: complete,
		Interval:                interval,
		ExpiresIn:               expiresIn,
	}, true
}

// ParseTokenResponse parses an RFC 8628 token poll / refresh response.
// Standard polling outcomes map to typed errors so callers can drive the poll
// loop.
//
// When requireRefreshToken is true (device-code success), an empty
// refresh_token is rejected so login cannot look connected then 401 later.
// Refresh responses may omit a rotated refresh token; pass false there.
func ParseTokenResponse(data []byte, statusCode int, now time.Time, requireRefreshToken bool) (Credentials, *AuthError) {
	obj := authJSONObject(data)

	if access, _ := obj["access_token"].(string); statusCode == 200 && access != "" {
		refresh, _ := obj["refresh_token"].(string)
		if requireRefreshToken && refresh == "" {
			return Credentials{}, newAuthError(AuthErrTokenError, "Login response missing refresh_token. Try connecting Grok again.")
		}
		expiresIn, ok := authJSONNumber(obj["expires_in"])
		if !ok {
			expiresIn = 3600
		}
		idToken, _ := obj["id_token"].(string)
		return Credentials{
			Access:      access,
			Refresh:     refresh,
			ExpiresAtMs: epochMs(now) + expiresIn*1000,
			Email:       EmailFromIDToken(idToken),
		}, nil
	}

	errCode, isString := obj["error"].(string)
	if !isString {
		if statusCode == 401 || statusCode == 403 {
			return Credentials{}, newAuthError(AuthErrReauthRequired, "")
		}
		return Credentials{}, newAuthError(AuthErrTokenError, fmt.Sprintf("Unexpected token response (HTTP %d)", statusCode))
	}
	switch errCode {
	case "authorization_pending":
		return Credentials{}, newAuthError(AuthErrAuthorizationPending, "")
	case "slow_down":
		return Credentials{}, newAuthError(AuthErrSlowDown, "")
	case "access_denied":
		return Credentials{}, newAuthError(AuthErrAccessDenied, "")
	case "expired_token":
		return Credentials{}, newAuthError(AuthErrExpiredToken, "")
	case "invalid_grant", "invalid_token":
		return Credentials{}, newAuthError(AuthErrReauthRequired, "")
	}
	if desc, ok := obj["error_description"].(string); ok {
		return Credentials{}, newAuthError(AuthErrTokenError, desc)
	}
	return Credentials{}, newAuthError(AuthErrTokenError, errCode)
}

// UpstreamHost keeps grok-4.7 on api.x.ai. The fast variant is a separate
// model id on the Grok Build proxy, using the same SuperGrok OAuth bearer.
func UpstreamHost(model string) string {
	if model == FastModelID {
		return BuildProxyHost
	}
	return APIHost
}

// UpstreamAuthHeaders are the headers the Build proxy requires to route the
// fast model. Empty for api.x.ai.
func UpstreamAuthHeaders(model string) [][2]string {
	if model != FastModelID {
		return nil
	}
	return [][2]string{
		{"X-XAI-Token-Auth", "xai-grok-cli"},
		{"x-grok-model-override", FastModelID},
		{"x-grok-client-version", BuildProxyClientVersion},
		{"x-grok-client-identifier", "grok-shell"},
	}
}

// NormalizeUpstreamPath normalizes a client path to /v1/... for api.x.ai,
// stripping a leading /api/v1 so Factory /api/v1/responses becomes /v1/responses.
func NormalizeUpstreamPath(path string) string {
	pathOnly, query := path, ""
	if q := strings.IndexByte(path, '?'); q >= 0 {
		pathOnly, query = path[:q], path[q:]
	}
	normalized := pathOnly
	switch {
	case strings.HasPrefix(normalized, "/api/v1/"):
		normalized = "/v1/" + strings.TrimPrefix(normalized, "/api/v1/")
	case normalized == "/api/v1":
		normalized = "/v1"
	case !(strings.HasPrefix(normalized, "/v1/") || normalized == "/v1"):
		if strings.HasPrefix(normalized, "/") {
			normalized = "/v1" + normalized
		} else {
			normalized = "/v1/" + normalized
		}
	}
	return normalized + query
}

// FilterClientHeaders drops hop-by-hop / auth / Content-Type headers so the
// upstream request carries a single Content-Type: application/json (api.x.ai
// returns 415 otherwise).
func FilterClientHeaders(headers [][2]string) [][2]string {
	out := make([][2]string, 0, len(headers))
	for _, h := range headers {
		if _, drop := excludedUpstreamHeaderNames[strings.ToLower(h[0])]; !drop {
			out = append(out, h)
		}
	}
	return out
}

// EmailFromIDToken is a best-effort extraction of the email claim from an OIDC
// id_token JWT. Returns "" when absent.
func EmailFromIDToken(idToken string) string {
	if idToken == "" {
		return ""
	}
	parts := authSplitNonEmpty(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	b64 := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	for len(b64)%4 != 0 {
		b64 += "="
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return ""
	}
	email, _ := authJSONObject(data)["email"].(string)
	return email
}

// authSplitNonEmpty mirrors Swift's split(separator:), which omits empty pieces.
func authSplitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// CredentialsJSON is the on-disk auth file object. ISO "expired" is omitted:
// the auth manager treats a past "expired" as dead, but a refresh token keeps
// the account usable past the short-lived "expires".
func CredentialsJSON(c Credentials) map[string]any {
	email := c.Email
	if email == "" {
		email = "grok-user"
	}
	return map[string]any{
		"type":     AuthFileType,
		"access":   c.Access,
		"refresh":  c.Refresh,
		"expires":  c.ExpiresAtMs,
		"disabled": false,
		"email":    email,
	}
}

// CredentialsFromJSON parses an auth file object; access and refresh must be
// non-empty.
func CredentialsFromJSON(obj map[string]any) (Credentials, bool) {
	access, _ := obj["access"].(string)
	refresh, _ := obj["refresh"].(string)
	if access == "" || refresh == "" {
		return Credentials{}, false
	}
	expires, _ := authJSONNumber(obj["expires"])
	email, _ := obj["email"].(string)
	return Credentials{Access: access, Refresh: refresh, ExpiresAtMs: expires, Email: email}, true
}

// MARK: - Storage

var authIOMu sync.Mutex

func authReadJSONFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, fmt.Errorf("not a JSON object")
	}
	return obj, nil
}

// LoadActiveCredentials returns the newest enabled Grok credential file in the
// auth directory (paths.AuthDir()).
func LoadActiveCredentials() (Credentials, string, bool) {
	return LoadActiveCredentialsIn(paths.AuthDir())
}

// LoadActiveCredentialsIn is LoadActiveCredentials for an explicit directory.
func LoadActiveCredentialsIn(dir string) (Credentials, string, bool) {
	authIOMu.Lock()
	defer authIOMu.Unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Credentials{}, "", false
	}
	var (
		best         Credentials
		bestPath     string
		bestModified time.Time
		found        bool
	)
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		obj, err := authReadJSONFile(path)
		if err != nil {
			continue
		}
		if t, _ := obj["type"].(string); strings.ToLower(t) != AuthFileType {
			continue
		}
		if disabled, _ := obj["disabled"].(bool); disabled {
			continue
		}
		creds, ok := CredentialsFromJSON(obj)
		if !ok {
			continue
		}
		var modified time.Time
		if info, err := os.Stat(path); err == nil {
			modified = info.ModTime()
		}
		if !found || modified.After(bestModified) {
			best, bestPath, bestModified, found = creds, path, modified, true
		}
	}
	return best, bestPath, found
}

// CredentialsPath is the canonical credential file (~/.cli-proxy-api/grok-cli.json).
func CredentialsPath() string {
	return filepath.Join(paths.AuthDir(), AuthFileName)
}

func authWriteJSONAtomic(path string, obj map[string]any) error {
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

// Persist atomically writes credentials, preserving an existing disabled flag.
// It returns an error on I/O failure so login/refresh cannot report success
// after a failed write.
func Persist(c Credentials, path string) error {
	authIOMu.Lock()
	defer authIOMu.Unlock()
	obj := CredentialsJSON(c)
	if existing, err := authReadJSONFile(path); err == nil {
		if disabled, ok := existing["disabled"].(bool); ok {
			obj["disabled"] = disabled
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return authWriteJSONAtomic(path, obj)
}

// QuarantineCredentials marks a credential file disabled so dead refresh
// tokens are not retried until the user reconnects (device login overwrites
// the file).
func QuarantineCredentials(path string) {
	authIOMu.Lock()
	defer authIOMu.Unlock()
	obj, err := authReadJSONFile(path)
	if err != nil {
		logx.Logf("[GrokAuth] Failed to quarantine credentials: unreadable file at %s", path)
		return
	}
	obj["disabled"] = true
	if err := authWriteJSONAtomic(path, obj); err != nil {
		logx.Logf("[GrokAuth] Failed to quarantine credentials: %v", err)
		return
	}
	logx.Logf("[GrokAuth] Quarantined Grok credentials (disabled=true) at %s", path)
}

// MARK: - Helpers

func authFormPOST(endpoint string, params map[string]string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(FormBody(params)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// FormBody builds an application/x-www-form-urlencoded body. "+" is always
// percent-encoded as %2B because form decoding treats a bare "+" as a space,
// which would corrupt device_code / refresh_token values.
func FormBody(params map[string]string) []byte {
	values := url.Values{}
	for k, v := range params {
		values.Set(k, v)
	}
	return []byte(values.Encode())
}
