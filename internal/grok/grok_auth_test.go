package grok

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDeviceAuthorizationAcceptsIntegerExpiresIn(t *testing.T) {
	json := `{"device_code":"dc","user_code":"ABCD-EFGH","verification_uri_complete":"https://auth.x.ai/device","interval":5,"expires_in":900}`

	auth, ok := ParseDeviceAuthorization([]byte(json))
	if !ok {
		t.Fatal("expected success")
	}
	if auth.DeviceCode != "dc" || auth.UserCode != "ABCD-EFGH" || auth.Interval != 5 || auth.ExpiresIn != 900 {
		t.Errorf("unexpected parse result: %+v", auth)
	}
}

func TestParseDeviceAuthorizationFallsBackToVerificationURIAndDefaults(t *testing.T) {
	json := `{"device_code":"dc","user_code":"CODE","verification_uri":"https://auth.x.ai/device"}`

	auth, ok := ParseDeviceAuthorization([]byte(json))
	if !ok {
		t.Fatal("expected success")
	}
	if auth.VerificationURIComplete != "https://auth.x.ai/device" {
		t.Errorf("VerificationURIComplete = %q", auth.VerificationURIComplete)
	}
	if auth.Interval != 5 || auth.ExpiresIn != 900 {
		t.Errorf("defaults not applied: %+v", auth)
	}
}

// idTokenWithEmail builds a JWT-shaped token whose payload carries the email claim.
func idTokenWithEmail(email string) string {
	payload, err := json.Marshal(map[string]string{"email": email})
	if err != nil {
		panic(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestParseTokenResponseStoresAbsoluteExpiryWithoutSkew(t *testing.T) {
	json := `{"access_token":"access","refresh_token":"refresh","expires_in":7200,"id_token":"` + idTokenWithEmail("u@x.ai") + `"}`
	now := time.Unix(1_700_000_000, 0)
	creds, err := ParseTokenResponse([]byte(json), 200, now, false)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if creds.Access != "access" || creds.Refresh != "refresh" || creds.Email != "u@x.ai" {
		t.Errorf("unexpected credentials: %+v", creds)
	}
	want := float64(now.UnixNano())/1e6 + 7200*1000
	if creds.ExpiresAtMs != want {
		t.Errorf("ExpiresAtMs = %v, want %v", creds.ExpiresAtMs, want)
	}
}

func TestIsAccessExpiredAppliesSkewAtCheckTime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	creds := Credentials{Access: "a", Refresh: "r", ExpiresAtMs: epochMs(now) + 7200*1000}

	if creds.IsAccessExpiredWithSkew(now, RefreshSkewMs) {
		t.Error("fresh token reported expired")
	}
	// Exactly at the 5m skew threshold → expired (>=).
	if !creds.IsAccessExpiredWithSkew(now.Add((7200-300)*time.Second), RefreshSkewMs) {
		t.Error("token at skew threshold should be expired")
	}
	// One ms before the skew threshold → still valid.
	if creds.IsAccessExpiredWithSkew(now.Add(time.Duration((7200-300)*1000-1)*time.Millisecond), RefreshSkewMs) {
		t.Error("token 1ms before skew threshold should be valid")
	}
	// Absolute expiry boundary with skew disabled.
	if !creds.IsAccessExpiredWithSkew(now.Add(7200*time.Second), 0) {
		t.Error("expired token with zero skew should be expired")
	}
	if creds.IsAccessExpiredWithSkew(now.Add(7200*time.Second-time.Millisecond), 0) {
		t.Error("token 1ms before absolute expiry should be valid")
	}
}

func TestShortLivedTokenIsExpiredImmediatelyUnderDefaultSkew(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// Lifetime shorter than RefreshSkewMs (5m) → expired as soon as issued.
	creds := Credentials{Access: "a", Refresh: "r", ExpiresAtMs: epochMs(now) + 240*1000}
	if !creds.IsAccessExpired(now) {
		t.Error("short-lived token should be expired immediately")
	}
}

func TestParseTokenResponseRequiresRefreshTokenWhenRequested(t *testing.T) {
	json := `{"access_token":"access","expires_in":3600}`
	_, err := ParseTokenResponse([]byte(json), 200, time.Now(), true)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "refresh_token") {
		t.Fatalf("expected tokenError mentioning refresh_token, got %v", err)
	}
}

func TestParseTokenResponseAllowsEmptyRefreshWhenNotRequired(t *testing.T) {
	json := `{"access_token":"access","expires_in":3600}`
	creds, err := ParseTokenResponse([]byte(json), 200, time.Now(), false)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if creds.Refresh != "" {
		t.Errorf("Refresh = %q, want empty", creds.Refresh)
	}
}

func TestParseTokenResponseErrorMatrix(t *testing.T) {
	cases := []struct {
		body     string
		status   int
		wantKind AuthErrorKind
	}{
		{`{"error":"authorization_pending"}`, 400, AuthErrAuthorizationPending},
		{`{"error":"slow_down"}`, 400, AuthErrSlowDown},
		{`{"error":"access_denied"}`, 400, AuthErrAccessDenied},
		{`{"error":"expired_token"}`, 400, AuthErrExpiredToken},
		{`{"error":"invalid_grant"}`, 400, AuthErrReauthRequired},
		{`{"error":"invalid_token"}`, 401, AuthErrReauthRequired},
	}
	for _, c := range cases {
		_, err := ParseTokenResponse([]byte(c.body), c.status, time.Now(), false)
		if err == nil {
			t.Fatalf("expected failure for %s", c.body)
		}
		if err.Kind != c.wantKind {
			t.Errorf("%s: kind = %v, want %v", c.body, err.Kind, c.wantKind)
		}
	}

	_, err := ParseTokenResponse([]byte(`{"error":"server_error","error_description":"boom"}`), 500, time.Now(), false)
	if err == nil || err.Kind != AuthErrTokenError || err.Error() != "boom" {
		t.Fatalf("expected tokenError with description, got %v", err)
	}

	_, err = ParseTokenResponse([]byte(`{}`), 401, time.Now(), false)
	if err == nil || err.Kind != AuthErrReauthRequired {
		t.Fatalf("expected reauthRequired for bare 401, got %v", err)
	}
}

func TestCredentialsRoundTrip(t *testing.T) {
	creds := Credentials{Access: "a", Refresh: "r", ExpiresAtMs: 123, Email: "u@x.ai"}
	obj := CredentialsJSON(creds)
	if obj["type"] != "grok-cli" {
		t.Errorf("type = %v", obj["type"])
	}
	if _, has := obj["expired"]; has {
		t.Error("expired must be omitted so the auth manager never treats the account as dead")
	}
	parsed, ok := CredentialsFromJSON(obj)
	if !ok {
		t.Fatal("expected parse success")
	}
	if parsed != creds {
		t.Errorf("round trip mismatch: %+v vs %+v", parsed, creds)
	}
}

func TestCredentialsFromRejectsEmptyRefresh(t *testing.T) {
	if _, ok := CredentialsFromJSON(map[string]any{"access": "a", "refresh": "", "expires": float64(1)}); ok {
		t.Error("expected rejection for empty refresh")
	}
}

func writeTestCreds(t *testing.T, path string, creds Credentials, disabled bool, modTime *time.Time) {
	t.Helper()
	obj := CredentialsJSON(creds)
	obj["disabled"] = disabled
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if modTime != nil {
		if err := os.Chtimes(path, *modTime, *modTime); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadActiveCredentialsSkipsDisabledAndPrefersNewest(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "older.json")
	newer := filepath.Join(dir, "newer.json")
	disabled := filepath.Join(dir, "disabled.json")

	writeTestCreds(t, older, Credentials{Access: "old", Refresh: "r1", ExpiresAtMs: 1, Email: "old@x.ai"}, false, &time.Time{})
	// Ensure distinct mtimes.
	oldTime := time.Unix(1_000, 0)
	newTime := time.Unix(2_000, 0)
	if err := os.Chtimes(older, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	writeTestCreds(t, newer, Credentials{Access: "new", Refresh: "r2", ExpiresAtMs: 2, Email: "new@x.ai"}, false, &newTime)
	writeTestCreds(t, disabled, Credentials{Access: "off", Refresh: "r3", ExpiresAtMs: 3, Email: "off@x.ai"}, true, &newTime)

	creds, path, ok := LoadActiveCredentialsIn(dir)
	if !ok {
		t.Fatal("expected credentials")
	}
	if creds.Access != "new" {
		t.Errorf("Access = %q, want new", creds.Access)
	}
	if filepath.Base(path) != "newer.json" {
		t.Errorf("path = %q, want newer.json", path)
	}
}

func TestQuarantineCredentialsSetsDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grok-cli.json")
	writeTestCreds(t, path, Credentials{Access: "a", Refresh: "r", ExpiresAtMs: 1, Email: "u@x.ai"}, false, nil)

	QuarantineCredentials(path)

	obj, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(obj, &m); err != nil {
		t.Fatal(err)
	}
	if m["disabled"] != true {
		t.Errorf("disabled = %v, want true", m["disabled"])
	}
	if _, _, ok := LoadActiveCredentialsIn(dir); ok {
		t.Error("quarantined credentials must not load")
	}
}

func TestNormalizeUpstreamPath(t *testing.T) {
	cases := map[string]string{
		"/v1/responses":           "/v1/responses",
		"/v1":                     "/v1",
		"/api/v1/responses":       "/v1/responses",
		"/api/v1":                 "/v1",
		"/responses":              "/v1/responses",
		"responses":               "/v1/responses",
		"/v1/chat/completions":    "/v1/chat/completions",
		"/api/v1/responses?foo=1": "/v1/responses?foo=1",
	}
	for in, want := range cases {
		if got := NormalizeUpstreamPath(in); got != want {
			t.Errorf("NormalizeUpstreamPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilterClientHeadersDropsContentTypeAndAuth(t *testing.T) {
	filtered := FilterClientHeaders([][2]string{
		{"Content-Type", "application/json"},
		{"content-type", "text/plain"},
		{"Authorization", "Bearer client"},
		{"Host", "localhost"},
		{"X-Request-Id", "abc"},
		{"User-Agent", "Droid"},
	})
	names := map[string]bool{}
	for _, h := range filtered {
		names[strings.ToLower(h[0])] = true
	}
	if names["content-type"] || names["authorization"] || names["host"] {
		t.Errorf("excluded headers leaked: %v", names)
	}
	if len(filtered) != 2 {
		t.Errorf("expected exactly X-Request-Id and User-Agent, got %v", filtered)
	}
}

func TestAPIHostIsPublicXAI(t *testing.T) {
	if APIHost != "api.x.ai" {
		t.Errorf("APIHost = %q", APIHost)
	}
}

func TestFastModelRoutesToBuildProxy(t *testing.T) {
	if UpstreamHost("grok-4.7") != "api.x.ai" || UpstreamHost("") != "api.x.ai" {
		t.Error("grok-4.7 must stay on api.x.ai")
	}
	if UpstreamHost("grok-4.7-build-fast") != "cli-chat-proxy.grok.com" {
		t.Error("fast model must route to the Build proxy")
	}
	if headers := UpstreamAuthHeaders("grok-4.7"); len(headers) != 0 {
		t.Errorf("api.x.ai must need no extra headers, got %v", headers)
	}
	wantNames := []string{"X-XAI-Token-Auth", "x-grok-model-override", "x-grok-client-version", "x-grok-client-identifier"}
	wantValues := []string{"xai-grok-cli", "grok-4.7-build-fast", "1.0.40", "grok-shell"}
	headers := UpstreamAuthHeaders("grok-4.7-build-fast")
	for i, h := range headers {
		if h[0] != wantNames[i] || h[1] != wantValues[i] {
			t.Errorf("header %d = %v, want %s: %s", i, h, wantNames[i], wantValues[i])
		}
	}
}

func TestFormBodyPercentEncodesPlusAsFormURLEncoded(t *testing.T) {
	body := string(FormBody(map[string]string{
		"refresh_token": "abc+def/ghi=",
		"grant_type":    "refresh_token",
	}))
	// "+" must become %2B (form-urlencoded space trap); "=" may be %3D.
	if !strings.Contains(body, "refresh_token=abc%2Bdef") || !strings.Contains(body, "%2B") {
		t.Errorf("body must encode + as %%2B: %s", body)
	}
	if strings.Contains(body, "abc+def") {
		t.Errorf("bare + would decode as a space: %s", body)
	}
	if !strings.Contains(body, "grant_type=refresh_token") {
		t.Errorf("missing grant_type: %s", body)
	}
}

func TestOneHourTokenIsNotExpiredImmediatelyUnderDefaultSkew(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	creds := Credentials{Access: "a", Refresh: "r", ExpiresAtMs: epochMs(now) + 3600*1000}
	if creds.IsAccessExpired(now) {
		t.Error("one-hour token reported expired at issue time")
	}
	if !creds.IsAccessExpired(now.Add((3600 - 300) * time.Second)) {
		t.Error("one-hour token should be expired inside the skew window")
	}
}

func TestTerminalRefreshFailureDetection(t *testing.T) {
	if !(&AuthError{Kind: AuthErrReauthRequired}).IsTerminalRefreshFailure() {
		t.Error("reauthRequired must be terminal")
	}
	if !(&AuthError{Kind: AuthErrTokenError, Detail: "invalid_grant"}).IsTerminalRefreshFailure() {
		t.Error("invalid_grant must be terminal")
	}
	if (&AuthError{Kind: AuthErrNetwork, Detail: "timeout"}).IsTerminalRefreshFailure() {
		t.Error("network errors are not terminal")
	}
	if (&AuthError{Kind: AuthErrNotLoggedIn}).IsTerminalRefreshFailure() {
		t.Error("notLoggedIn is not a refresh failure")
	}
}
