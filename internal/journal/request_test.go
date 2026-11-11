package journal

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBeginAdmitsInferencePostsOnly(t *testing.T) {
	on := true
	j := &Journal{enabled: func() bool { return on }}
	begin := func(method, forwarded string) *Observation {
		return j.Begin(RequestContext{Method: method, Path: forwarded, ForwardedPath: forwarded, Head: method + " " + forwarded + " HTTP/1.1"})
	}
	for _, path := range []string{
		"/v1/messages", "/v1/messages?beta=true", "/v1/chat/completions", "/v1/completions",
		"/v1/responses", "/backend-api/codex/responses", "/v1/responses/compact",
		"/backend-api/codex/responses/compact", "/v1/alpha/search", "/backend-api/codex/alpha/search",
		"/v1/images/generations", "/v1/images/edits", "/v1/videos", "/v1/videos/generations",
		"/v1/videos/edits", "/v1/videos/extensions", "/openai/v1/videos", "/v1/audio/speech", "/v1/tts",
		"/v1/live", "/v1/realtime", "/v1/realtime/calls", "/v1/realtime/translations",
		"/v1beta/interactions", "/v1beta/models/gemini-3-pro:generateContent",
		"/v1beta/models/gemini-3-pro:streamGenerateContent?alt=sse",
	} {
		if begin("POST", path) == nil {
			t.Errorf("POST %s not admitted", path)
		}
	}
	for _, path := range []string{
		"/v1/messages/count_tokens", "/v1beta/models/gemini-3-pro:countTokens", "/v1beta/models/:generateContent",
		"/v1beta/models/a/b:generateContent", "/api/v1/responses", "/v1/models", "/v1/realtime/client_secrets",
		"/v1/realtime/sessions", "/v1/messages/batches", "/v0/management/usage", "/healthz", "/v1/responsesx",
		"/droidproxy/usage",
	} {
		if begin("POST", path) != nil {
			t.Errorf("POST %s admitted", path)
		}
	}
	if begin("GET", "/v1/responses") != nil {
		t.Error("GET admitted")
	}
	on = false
	if begin("POST", "/v1/messages") != nil {
		t.Error("admitted while the setting is off")
	}
	var off *Journal
	if off.Begin(RequestContext{Method: "POST", ForwardedPath: "/v1/messages"}) != nil {
		t.Error("nil journal returned an observation")
	}
}

func TestDeriveProvider(t *testing.T) {
	for _, c := range []struct{ model, path, want string }{
		{"claude-opus-4-6", "/v1/chat/completions", "claude"},
		{"gemini-claude-sonnet-4-6", "/v1/messages", "claude"},
		{"ag-c55o-high", "/v1/chat/completions", "claude"},
		{"gemini-3.1-pro-preview", "/v1/responses", "gemini"},
		{"", "/v1beta/models/x:generateContent", "gemini"},
		{"", "/v1beta/interactions", "gemini"},
		{"gpt-6-luna", "/v1/chat/completions", "codex"},
		{"codex-mini", "/v1/responses", "codex"},
		{"custom-alias", "/backend-api/codex/responses", "codex"},
		{"", "/v1/alpha/search", "codex"},
		{"custom-alias", "/v1/messages", "claude"},
		{"custom-alias", "/v1/responses", "openai"},
		{"", "/openai/v1/videos", "openai"},
		{"kimi-k3", "/v1/chat/completions", "openai"},
		{"Claude-opus", "/api/v1/x", "unknown"},
	} {
		if got := deriveProvider(c.model, c.path); got != c.want {
			t.Errorf("deriveProvider(%q, %q) = %s, want %s", c.model, c.path, got, c.want)
		}
	}
}

func TestRequestedModel(t *testing.T) {
	body := func(s string) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(s), &m)
		return m
	}
	for _, c := range []struct {
		path, body string
		want       any
	}{
		{"/v1/messages", `{"model":"ag-c46s-thinking"}`, "ag-c46s-thinking"},
		{"/v1/messages", `{"model":""}`, nil},
		{"/v1/messages", `{"model":7}`, nil},
		{"/v1/messages", `{"messages":[]}`, nil},
		{"/v1/images/edits", "--boundary\r\nmultipart", nil},
		{"/v1beta/models/gemini-3-pro:generateContent", `{"model":"other"}`, "gemini-3-pro"},
	} {
		got := requestedModel(c.path, body(c.body))
		if (got == nil) != (c.want == nil) || (got != nil && *got != c.want) {
			t.Errorf("requestedModel(%q, %s) = %v, want %v", c.path, c.body, deref(got), c.want)
		}
	}
}

func TestClientSession(t *testing.T) {
	for _, c := range []struct {
		name    string
		headers [][2]string
		body    string
		want    string
	}{
		{"x-session-id first", [][2]string{{"X-Claude-Code-Session-Id", "claude-session-1"}, {"x-session-id", "header-session-1"}}, `{"prompt_cache_key":"cache-key-0001"}`, "header-session-1"},
		{"claude code header", [][2]string{{"x-claude-code-session-id", "claude-session-1"}}, `{"prompt_cache_key":"cache-key-0001"}`, "claude-session-1"},
		{"malformed header skipped", [][2]string{{"X-Session-ID", "short"}, {"X-Session-ID", "second-value-ok"}}, `{}`, "second-value-ok"},
		{"header value with space", [][2]string{{"X-Session-ID", "bad session"}}, `{"prompt_cache_key":"11111111-2222-4333-8444-555555555555"}`, "11111111-2222-4333-8444-555555555555"},
		{"prompt_cache_key before metadata", nil, `{"prompt_cache_key":"cache-key-0001","metadata":{"user_id":"{\"session_id\":\"json-session-0001\"}"}}`, "cache-key-0001"},
		{"prompt_cache_key not trimmed", nil, `{"prompt_cache_key":" cache-key-0001"}`, ""},
		{"prompt_cache_key non-string", nil, `{"prompt_cache_key":12345678}`, ""},
		{"metadata json", nil, `{"metadata":{"user_id":"{\"session_id\":\"json-session-0001\"}"}}`, "json-session-0001"},
		{"metadata json ignores parents", nil, `{"metadata":{"user_id":"{\"parent_session_id\":\"parent-session-1\"}"}}`, ""},
		{"metadata json malformed", nil, `{"metadata":{"user_id":"{\"session_id\":\"json-session-0001\""}}`, ""},
		{"metadata json never legacy", nil, `{"metadata":{"user_id":"{x_session_legacy-session-1"}}`, ""},
		{"metadata json non-string id", nil, `{"metadata":{"user_id":"{\"session_id\":12345678}"}}`, ""},
		{"metadata legacy", nil, `{"metadata":{"user_id":"user_123_acc__session_01a06a06-e830-7da9-a866-98470a94389c"}}`, "01a06a06-e830-7da9-a866-98470a94389c"},
		{"metadata legacy last marker", nil, `{"metadata":{"user_id":"a_session_first-one-x_session_second-one"}}`, "second-one"},
		{"metadata without marker", nil, `{"metadata":{"user_id":"user_123_account"}}`, ""},
		{"too long", [][2]string{{"X-Session-ID", string(make129())}}, `{}`, ""},
		{"no candidate", nil, `{}`, ""},
	} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(c.body), &body)
		got := clientSession(c.headers, body)
		if deref(got) != c.want {
			t.Errorf("%s: got %q, want %q", c.name, deref(got), c.want)
		}
	}
}

func TestBeginReadsRawHeaders(t *testing.T) {
	j := &Journal{enabled: func() bool { return true }, seats: newSeatIndex(t.TempDir())}
	o := j.Begin(RequestContext{
		Method:        "POST",
		Path:          "/v1/messages",
		ForwardedPath: "/v1/messages",
		Head:          "POST /v1/messages HTTP/1.1\r\nX-Session-ID: \t header-session-1 \r\nx-droidproxy-origin: window-primer",
		Body:          `{"model":"claude-haiku-4-5-20251001"}`,
	})
	rec, _ := finish(o, EndEOF, time.Now())
	if deref(rec.ClientSession) != "header-session-1" || rec.Origin != "primer" || rec.Provider != "claude" {
		t.Fatalf("session=%q origin=%s provider=%s", deref(rec.ClientSession), rec.Origin, rec.Provider)
	}
}

func make129() []byte {
	b := make([]byte, 129)
	for i := range b {
		b[i] = 'a'
	}
	return b
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
