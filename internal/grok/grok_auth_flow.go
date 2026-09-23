package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/logx"
)

// LoginSession is the cancellation handle for an in-flight device login.
type LoginSession struct {
	mu        sync.Mutex
	cancelled bool
	ctx       context.Context
	stop      context.CancelFunc
}

func newLoginSession() *LoginSession {
	ctx, stop := context.WithCancel(context.Background())
	return &LoginSession{ctx: ctx, stop: stop}
}

// Cancel stops the login. The completion callback still fires once, with an
// AuthErrCancelled error.
func (s *LoginSession) Cancel() {
	s.mu.Lock()
	s.cancelled = true
	s.mu.Unlock()
	s.stop()
}

// IsCancelled reports whether Cancel was called.
func (s *LoginSession) IsCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

// sleep waits d or until the session is cancelled.
func (s *LoginSession) sleep(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-s.ctx.Done():
	}
}

type authHTTPResult struct {
	status int
	body   []byte
	err    error
}

func authDo(ctx context.Context, req *http.Request) authHTTPResult {
	resp, err := HTTPClient.Do(req.WithContext(ctx))
	if err != nil {
		return authHTTPResult{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return authHTTPResult{status: resp.StatusCode, err: err}
	}
	return authHTTPResult{status: resp.StatusCode, body: body}
}

// StartDeviceLogin starts the device authorization flow: it requests a
// device/user code, opens the verification URL in the browser, surfaces the
// user code via onPrompt, then polls until the user approves, the request
// fails, is cancelled, or expires. On success the credentials are persisted to
// CredentialsPath() before completion runs. Callbacks run on a background
// goroutine; completion runs exactly once, with a nil error on success.
func StartDeviceLogin(onPrompt func(DeviceAuthorization), completion func(Credentials, *AuthError)) *LoginSession {
	session := newLoginSession()
	finish := func(creds Credentials, authErr *AuthError) {
		if authErr != nil {
			completion(Credentials{}, authErr)
			return
		}
		if err := Persist(creds, CredentialsPath()); err != nil {
			completion(Credentials{}, newAuthError(AuthErrTokenError, "Could not save credentials: "+err.Error()))
			return
		}
		completion(creds, nil)
	}

	go func() {
		req, err := authFormPOST(DeviceCodeURL, map[string]string{
			"client_id": ClientID,
			"scope":     Scope,
		})
		if err != nil {
			finish(Credentials{}, newAuthError(AuthErrDeviceCodeFailed, err.Error()))
			return
		}
		res := authDo(session.ctx, req)
		if session.IsCancelled() {
			finish(Credentials{}, newAuthError(AuthErrCancelled, ""))
			return
		}
		if res.err != nil && res.status == 0 {
			finish(Credentials{}, newAuthError(AuthErrDeviceCodeFailed, res.err.Error()))
			return
		}
		if res.err != nil {
			finish(Credentials{}, newAuthError(AuthErrDeviceCodeFailed,
				fmt.Sprintf("Empty device authorization response (HTTP %d).", res.status)))
			return
		}
		if res.status < 200 || res.status > 299 {
			snippet := strings.TrimSpace(string(res.body))
			detail := fmt.Sprintf("HTTP %d", res.status)
			if snippet != "" {
				detail = fmt.Sprintf("HTTP %d: %s", res.status, authPrefixRunes(snippet, 200))
			}
			finish(Credentials{}, newAuthError(AuthErrDeviceCodeFailed, detail))
			return
		}
		auth, ok := ParseDeviceAuthorization(res.body)
		if !ok {
			finish(Credentials{}, newAuthError(AuthErrDeviceCodeFailed, "Invalid device authorization response."))
			return
		}
		if auth.VerificationURIComplete == "" {
			finish(Credentials{}, newAuthError(AuthErrDeviceCodeFailed, "Device authorization response missing verification URL."))
			return
		}
		if session.IsCancelled() {
			finish(Credentials{}, newAuthError(AuthErrCancelled, ""))
			return
		}
		if _, err := url.Parse(auth.VerificationURIComplete); err == nil {
			if err := OpenBrowser(auth.VerificationURIComplete); err != nil {
				logx.Logf("[GrokAuth] Could not open browser: %v", err)
			}
		}
		if onPrompt != nil {
			onPrompt(auth)
		}
		interval := auth.Interval
		if interval < 1 {
			interval = 1
		}
		deadline := time.Now().Add(secondsDuration(auth.ExpiresIn))
		creds, authErr := pollForToken(auth.DeviceCode, interval, deadline, session)
		finish(creds, authErr)
	}()
	return session
}

func secondsDuration(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

func authPrefixRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func pollForToken(deviceCode string, interval float64, deadline time.Time, session *LoginSession) (Credentials, *AuthError) {
	failures := 0
	for {
		if session.IsCancelled() {
			return Credentials{}, newAuthError(AuthErrCancelled, "")
		}
		if !time.Now().Before(deadline) {
			return Credentials{}, newAuthError(AuthErrExpiredToken, "")
		}
		req, err := authFormPOST(TokenURL, map[string]string{
			"grant_type":  DeviceGrantType,
			"device_code": deviceCode,
			"client_id":   ClientID,
		})
		if err != nil {
			return Credentials{}, newAuthError(AuthErrNetwork, err.Error())
		}
		res := authDo(session.ctx, req)
		if session.IsCancelled() {
			return Credentials{}, newAuthError(AuthErrCancelled, "")
		}
		if res.err != nil {
			failures++
			logx.Logf("[GrokAuth] Token poll network error (%d): %v", failures, res.err)
			// Bound transient retries so persistent offline doesn't look like user inaction.
			if failures >= 8 {
				return Credentials{}, newAuthError(AuthErrNetwork, res.err.Error())
			}
			session.sleep(secondsDuration(interval))
			continue
		}
		creds, authErr := ParseTokenResponse(res.body, res.status, time.Now(), true)
		if authErr == nil {
			return creds, nil
		}
		switch authErr.Kind {
		case AuthErrAuthorizationPending:
			failures = 0
		case AuthErrSlowDown:
			failures = 0
			interval += 5
		default:
			return Credentials{}, authErr
		}
		session.sleep(secondsDuration(interval))
	}
}

// MARK: - Token access for request forwarding

var (
	refreshMu       sync.Mutex
	refreshInFlight bool
	refreshWaiters  []func(string, *AuthError)
)

// EnsureValidAccessToken loads stored credentials and delivers a valid bearer
// access token, refreshing (and persisting) it first when the current token
// has expired. completion may run on the caller's goroutine (fast path) or on
// a background goroutine (refresh).
//
// Refreshes are single-flighted: concurrent requests that arrive while the
// token is expired collapse onto one network refresh instead of each POSTing
// the same refresh token (xAI rotates it, so a second concurrent refresh would
// fail with invalid_grant and surface a spurious 401).
func EnsureValidAccessToken(completion func(token string, err *AuthError)) {
	creds, path, ok := LoadActiveCredentials()
	if !ok {
		completion("", newAuthError(AuthErrNotLoggedIn, ""))
		return
	}
	if !creds.IsAccessExpired(time.Now()) {
		completion(creds.Access, nil)
		return
	}

	refreshMu.Lock()
	if refreshInFlight {
		refreshWaiters = append(refreshWaiters, completion)
		refreshMu.Unlock()
		return
	}
	refreshInFlight = true
	refreshMu.Unlock()

	deliver := func(token string, err *AuthError) {
		refreshMu.Lock()
		waiters := refreshWaiters
		refreshWaiters = nil
		refreshInFlight = false
		refreshMu.Unlock()
		completion(token, err)
		for _, w := range waiters {
			w(token, err)
		}
	}

	// A previous refresh may have persisted a fresh token between our expiry
	// check and winning the in-flight slot; re-read before hitting the network.
	if c, p, ok := LoadActiveCredentials(); ok {
		creds, path = c, p
	}
	if !creds.IsAccessExpired(time.Now()) {
		deliver(creds.Access, nil)
		return
	}

	go func() {
		refreshed, authErr := refreshAccessToken(creds)
		if authErr != nil {
			if authErr.IsTerminalRefreshFailure() {
				QuarantineCredentials(path)
				deliver("", newAuthError(AuthErrReauthRequired, ""))
			} else {
				deliver("", authErr)
			}
			return
		}
		if err := Persist(refreshed, path); err != nil {
			// Still return the token for this request; next expiry will refresh again.
			logx.Logf("[GrokAuth] Refresh succeeded but persist failed: %v", err)
		}
		deliver(refreshed.Access, nil)
	}()
}

// AccessToken is the blocking form of EnsureValidAccessToken.
func AccessToken() (string, *AuthError) {
	type result struct {
		token string
		err   *AuthError
	}
	ch := make(chan result, 1)
	EnsureValidAccessToken(func(token string, err *AuthError) { ch <- result{token, err} })
	r := <-ch
	return r.token, r.err
}

func refreshAccessToken(creds Credentials) (Credentials, *AuthError) {
	req, err := authFormPOST(TokenURL, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     ClientID,
		"refresh_token": creds.Refresh,
	})
	if err != nil {
		return Credentials{}, newAuthError(AuthErrNetwork, err.Error())
	}
	res := authDo(context.Background(), req)
	if res.err != nil {
		return Credentials{}, newAuthError(AuthErrNetwork, res.err.Error())
	}
	refreshed, authErr := ParseTokenResponse(res.body, res.status, time.Now(), false)
	if authErr != nil {
		return Credentials{}, authErr
	}
	// Refresh may omit rotated refresh_token / id_token; keep prior values.
	if refreshed.Refresh == "" {
		refreshed.Refresh = creds.Refresh
	}
	if refreshed.Email == "" {
		refreshed.Email = creds.Email
	}
	return refreshed, nil
}
