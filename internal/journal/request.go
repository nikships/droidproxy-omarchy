package journal

import (
	"encoding/json"
	"regexp"
	"strings"
)

// RequestContext is what the relay knows about a request before forwarding
// it to CLIProxyAPI.
type RequestContext struct {
	Method string
	// Path is the client's request target, before any rewrite.
	Path string
	// ForwardedPath is the request target sent to CLIProxyAPI.
	ForwardedPath string
	// Head is the request line and header lines, without the blank line.
	Head string
	// Body is the client's body before alias or service-tier rewrites.
	Body string
}

// admittedPaths are CLIProxyAPI v8.0.61's POST inference routes.
var admittedPaths = map[string]bool{
	"/v1/messages":                         true,
	"/v1/chat/completions":                 true,
	"/v1/completions":                      true,
	"/v1/responses":                        true,
	"/backend-api/codex/responses":         true,
	"/v1/responses/compact":                true,
	"/backend-api/codex/responses/compact": true,
	"/v1/alpha/search":                     true,
	"/backend-api/codex/alpha/search":      true,
	"/v1/images/generations":               true,
	"/v1/images/edits":                     true,
	"/v1/videos":                           true,
	"/v1/videos/generations":               true,
	"/v1/videos/edits":                     true,
	"/v1/videos/extensions":                true,
	"/openai/v1/videos":                    true,
	"/v1/audio/speech":                     true,
	"/v1/tts":                              true,
	"/v1/live":                             true,
	"/v1/realtime":                         true,
	"/v1/realtime/calls":                   true,
	"/v1/realtime/translations":            true,
	"/v1beta/interactions":                 true,
}

func stripQuery(target string) string {
	if i := strings.IndexByte(target, '?'); i >= 0 {
		return target[:i]
	}
	return target
}

func admitted(path string) bool {
	if admittedPaths[path] {
		return true
	}
	_, ok := nativeGeminiModel(path)
	return ok
}

// nativeGeminiModel returns {model} from /v1beta/models/{model}:generateContent
// or :streamGenerateContent. countTokens and other actions are not inference.
func nativeGeminiModel(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/v1beta/models/")
	if !ok {
		return "", false
	}
	i := strings.LastIndexByte(rest, ':')
	if i <= 0 {
		return "", false
	}
	model, action := rest[:i], rest[i+1:]
	if action != "generateContent" && action != "streamGenerateContent" {
		return "", false
	}
	if strings.ContainsAny(model, "/:") {
		return "", false
	}
	return model, true
}

var claudeAliases = map[string]bool{
	"ag-c55s-high":     true,
	"ag-c55o-high":     true,
	"ag-c46s-thinking": true,
	"ag-c46o-thinking": true,
}

// deriveProvider labels the request family from the original model and path.
func deriveProvider(model, path string) string {
	switch {
	case strings.HasPrefix(model, "claude-") || strings.HasPrefix(model, "gemini-claude-") || claudeAliases[model]:
		return "claude"
	case strings.HasPrefix(model, "gemini-") || (admitted(path) && strings.HasPrefix(path, "/v1beta/")):
		return "gemini"
	case strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "codex-") ||
		(admitted(path) && strings.HasPrefix(path, "/backend-api/codex/")) || path == "/v1/alpha/search":
		return "codex"
	case path == "/v1/messages":
		return "claude"
	case admitted(path) && (strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/openai/v1/")):
		return "openai"
	}
	return "unknown"
}

// trimOWS removes HTTP optional whitespace (SP and HTAB) only.
func trimOWS(s string) string {
	return strings.Trim(s, " \t")
}

func parseHeaderLines(head string) [][2]string {
	lines := strings.Split(head, "\r\n")
	var headers [][2]string
	for _, line := range lines[1:] {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		headers = append(headers, [2]string{trimOWS(line[:colon]), trimOWS(line[colon+1:])})
	}
	return headers
}

func headerValues(headers [][2]string, name string) []string {
	var values []string
	for _, h := range headers {
		if strings.EqualFold(h[0], name) {
			values = append(values, h[1])
		}
	}
	return values
}

func isPrimer(headers [][2]string) bool {
	for _, v := range headerValues(headers, "X-DroidProxy-Origin") {
		if v == "window-primer" {
			return true
		}
	}
	return false
}

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

// clientSession picks the first valid explicit session key: X-Session-ID,
// X-Claude-Code-Session-Id, body prompt_cache_key, then the session id in
// body metadata.user_id.
func clientSession(headers [][2]string, body map[string]json.RawMessage) *string {
	for _, name := range []string{"X-Session-ID", "X-Claude-Code-Session-Id"} {
		for _, v := range headerValues(headers, name) {
			if sessionPattern.MatchString(v) {
				return ptr(v)
			}
		}
	}
	if v, ok := jsonString(body["prompt_cache_key"]); ok && sessionPattern.MatchString(v) {
		return ptr(v)
	}
	if userID, ok := jsonString(objectOf(body["metadata"])["user_id"]); ok {
		if v, ok := metadataSessionID(userID); ok && sessionPattern.MatchString(v) {
			return ptr(v)
		}
	}
	return nil
}

// metadataSessionID reads metadata.user_id in its JSON form ({"session_id":
// ...}) or its legacy form (suffix after the final "_session_"). The first
// byte decides the form; a string starting with "{" is never read as legacy.
func metadataSessionID(userID string) (string, bool) {
	if strings.HasPrefix(userID, "{") {
		return jsonString(strictObject([]byte(userID))["session_id"])
	}
	i := strings.LastIndex(userID, "_session_")
	if i < 0 {
		return "", false
	}
	return userID[i+len("_session_"):], true
}

// requestFacts are the record fields read from the request. The body is
// parsed on its own goroutine so the relay never waits for it; done is
// closed once the fields are set.
type requestFacts struct {
	done     chan struct{}
	model    *string
	provider string
	session  *string
}

func readRequest(endpoint string, headers [][2]string, body string) *requestFacts {
	f := &requestFacts{done: make(chan struct{})}
	go func() {
		defer close(f.done)
		obj := strictObject([]byte(body))
		f.model = requestedModel(endpoint, obj)
		model := ""
		if f.model != nil {
			model = *f.model
		}
		f.provider = deriveProvider(model, endpoint)
		f.session = clientSession(headers, obj)
	}()
	return f
}

func requestedModel(endpoint string, body map[string]json.RawMessage) *string {
	if model, ok := nativeGeminiModel(endpoint); ok {
		return ptr(model)
	}
	if model, ok := jsonString(body["model"]); ok && model != "" {
		return ptr(model)
	}
	return nil
}

func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func objectOf(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return obj
}
