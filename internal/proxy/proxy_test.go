package proxy

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	prefs.Shared().Reload()
}

func TestInspectRequestJSONFields(t *testing.T) {
	body := `{"model":"claude-opus-4-6","thinking":{"type":"adaptive"},"messages":[]}`
	fields, ok := inspectRequestJSONFields(body)
	if !ok {
		t.Fatal("expected fields")
	}
	if fields.model != "claude-opus-4-6" || fields.thinkingType != "adaptive" {
		t.Fatalf("fields = %+v", fields)
	}
	if fields.hasServiceTier {
		t.Fatal("unexpected service_tier")
	}
}

func TestProcessOpenAIFastModeInjectsServiceTier(t *testing.T) {
	for model, key := range map[string]string{
		"gpt-6.1-sol":   prefs.KeyGPT61SolFastMode,
		"gpt-6.1-terra": prefs.KeyGPT61TerraFastMode,
	} {
		isolateHome(t)
		body := `{"model":"` + model + `","input":[]}`
		fields, ok := inspectRequestJSONFields(body)
		if !ok {
			t.Fatal("fields")
		}
		if _, ok := processOpenAIFastMode(body, "/v1/responses", fields, true); ok {
			t.Fatalf("%s: injected with fast mode off", model)
		}
		_ = prefs.Shared().Set(key, true)
		got, ok := processOpenAIFastMode(body, "/v1/responses", fields, true)
		if !ok {
			t.Fatalf("%s: expected injection", model)
		}
		if !strings.Contains(got, `"model":"`+model+`","service_tier":"priority"`) {
			t.Fatalf("%s: injection position wrong: %s", model, got)
		}
	}
}

func TestProcessOpenAIFastModeSkipsWhenAlreadySet(t *testing.T) {
	isolateHome(t)
	_ = prefs.Shared().Set(prefs.KeyGPT61SolFastMode, true)

	body := `{"model":"gpt-6.1-sol","service_tier":"default"}`
	fields, ok := inspectRequestJSONFields(body)
	if !ok {
		t.Fatal("fields")
	}
	if _, ok := processOpenAIFastMode(body, "/v1/responses", fields, true); ok {
		t.Fatal("should not inject when service_tier present")
	}
}

func TestRewriteAntigravityModelAlias(t *testing.T) {
	isolateHome(t)
	for alias, backend := range map[string]string{
		"ag-c55s-high":     "claude-sonnet-5-5-high",
		"ag-c55o-high":     "claude-opus-5-5-high",
		"ag-c46s-thinking": "claude-sonnet-4-6",
		"ag-c46o-thinking": "claude-opus-4-6-thinking",
	} {
		body := `{"model":"` + alias + `","messages":[]}`
		fields, ok := inspectRequestJSONFields(body)
		if !ok {
			t.Fatal("fields")
		}
		got, ok := rewriteAntigravityModelAlias(body, fields, true)
		if !ok {
			t.Fatalf("%s: expected rewrite", alias)
		}
		if !strings.Contains(got, `"model":"`+backend+`"`) {
			t.Fatalf("%s: got %s", alias, got)
		}
	}
}

func TestOAuthGeminiPathRewrite(t *testing.T) {
	fields := requestJSONFields{model: "gemini-3.1-pro-preview", hasModel: true}
	if !isOAuthCodeAssistGeminiModel(fields, true) {
		t.Fatal("expected oauth gemini")
	}
	if isOAuthCodeAssistGeminiModel(requestJSONFields{model: "gemini-3.8-flash-high", hasModel: true}, true) {
		t.Fatal("antigravity gemini must not rewrite")
	}
	if !isResponsesAPIPath("/v1/responses?foo=1") {
		t.Fatal("query should still match")
	}
}

func TestReasoningSummaryLog(t *testing.T) {
	body := `{"model":"gpt-6.1-sol","reasoning":{"effort":"high"},"messages":[]}`
	fields, ok := inspectRequestJSONFields(body)
	if !ok {
		t.Fatal("fields")
	}
	summary := reasoningSummaryLog(body, fields)
	if !strings.Contains(summary, "model=gpt-6.1-sol") || !strings.Contains(summary, "reasoning=") {
		t.Fatalf("summary=%q", summary)
	}
}

func TestProxyForwardsToBackend(t *testing.T) {
	isolateHome(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"claude-sonnet-4-6"`) {
			t.Errorf("body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(backend.Close)

	host, port := splitHostPort(t, backend.Listener.Addr().String())
	p := New()
	p.Port = freePort(t)
	p.TargetHost = host
	p.TargetPort = port
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	body := `{"model":"claude-sonnet-4-6","messages":[]}`
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", p.Port), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(got), `"ok":true`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
}

func TestProxyRewritesOAuthGeminiResponsesPath(t *testing.T) {
	isolateHome(t)

	var sawPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(backend.Close)

	host, port := splitHostPort(t, backend.Listener.Addr().String())
	p := New()
	p.Port = freePort(t)
	p.TargetHost = host
	p.TargetPort = port
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	body := `{"model":"gemini-3-flash-preview","input":[]}`
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/responses", p.Port), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sawPath != "/v1/chat/completions" {
		t.Fatalf("path = %s", sawPath)
	}
}

func TestProxyGrokTLSForward(t *testing.T) {
	isolateHome(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	}))
	t.Cleanup(upstream.Close)

	upHost, upPort := splitHostPort(t, upstream.Listener.Addr().String())
	p := New()
	p.Port = freePort(t)
	p.GrokAccessToken = func() (string, error) { return "test-token", nil }
	p.DialTLS = func(network, _ string, _ *tls.Config) (net.Conn, error) {
		return net.Dial(network, net.JoinHostPort(upHost, fmt.Sprintf("%d", upPort)))
	}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	body := `{"model":"grok-4.7","messages":[]}`
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", p.Port), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(got), `"content":"hi"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
}

func TestProxyMetaResponsesTLSForward(t *testing.T) {
	isolateHome(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer muse-key" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"id":"resp_1"}`)
	}))
	t.Cleanup(upstream.Close)

	upHost, upPort := splitHostPort(t, upstream.Listener.Addr().String())
	p := New()
	p.Port = freePort(t)
	p.MetaAPIKey = func() (string, bool) { return "muse-key", true }
	p.DialTLS = func(network, _ string, _ *tls.Config) (net.Conn, error) {
		return net.Dial(network, net.JoinHostPort(upHost, fmt.Sprintf("%d", upPort)))
	}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	body := `{"model":"muse-wonder-1.0","input":[]}`
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/responses", p.Port), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(got), `resp_1`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
}

func TestProxyAnswersGetWithoutContentLength(t *testing.T) {
	isolateHome(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("backend saw %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	t.Cleanup(backend.Close)

	host, port := splitHostPort(t, backend.Listener.Addr().String())
	p := New()
	p.Port = freePort(t)
	p.TargetHost = host
	p.TargetPort = port
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", p.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(got), `"data"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

func TestUsageEndpointIsAnsweredLocally(t *testing.T) {
	isolateHome(t)
	p := New()
	p.DialBackend = func(network, address string) (net.Conn, error) {
		t.Error("usage endpoint was forwarded to the backend")
		return nil, fmt.Errorf("unexpected dial")
	}
	p.UsageEndpoint = func(requestPath string) (int, []byte, bool) {
		if requestPath != "/droidproxy/usage?provider=codex" {
			return 0, nil, false
		}
		return 200, []byte(`{"accounts":[]}`), true
	}
	client, server := net.Pipe()
	go func() {
		p.processRequest([]byte("GET /droidproxy/usage?provider=codex HTTP/1.1\r\nHost: localhost\r\n\r\n"), server)
		server.Close()
	}()
	raw, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	response := string(raw)
	if !strings.HasPrefix(response, "HTTP/1.1 200 OK\r\n") ||
		!strings.Contains(response, "Content-Type: application/json\r\n") ||
		!strings.HasSuffix(response, "\r\n\r\n"+`{"accounts":[]}`) {
		t.Fatalf("response = %q", response)
	}
}
