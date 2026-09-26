package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nikships/droidproxy-omarchy/internal/control"
)

type stubProvider struct {
	state control.State
	calls []string
}

func (s *stubProvider) State() control.State { return s.state }

func (s *stubProvider) Call(_ context.Context, method string, _ json.RawMessage) control.CallResult {
	s.calls = append(s.calls, method)
	return control.OK("ok:" + method)
}

func testServer() (*Server, *stubProvider) {
	p := &stubProvider{state: control.State{
		App:    control.AppInfo{Name: "DroidProxy", Version: "9.9.9"},
		Server: control.ServerState{Running: true, ProxyPort: 8317},
	}}
	return NewServer(p), p
}

func TestIndexServesHTML(t *testing.T) {
	s, _ := testServer()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	res := rec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "DroidProxy") || !strings.Contains(string(body), "/app.js") {
		t.Fatal("index does not look like the settings app")
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestStaticAssetsServed(t *testing.T) {
	s, _ := testServer()
	for _, path := range []string{"/app.js", "/styles.css", "/icons/icon-active.png", "/icons/icon-claude.png", "/icons/icon-grok.svg"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Result().StatusCode != 200 {
			t.Fatalf("%s status = %d", path, rec.Result().StatusCode)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", rec.Result().StatusCode)
	}
}

func TestAPIState(t *testing.T) {
	s, _ := testServer()
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var st control.State
	if err := json.NewDecoder(rec.Result().Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.App.Version != "9.9.9" || !st.Server.Running {
		t.Fatalf("state = %+v", st)
	}
}

func TestAPICall(t *testing.T) {
	s, p := testServer()
	req := httptest.NewRequest(http.MethodPost, "/api/call/server.toggle", strings.NewReader(`{"a":1}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var result control.CallResult
	if err := json.NewDecoder(rec.Result().Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Message != "ok:server.toggle" {
		t.Fatalf("result = %+v", result)
	}
	if len(p.calls) != 1 || p.calls[0] != "server.toggle" {
		t.Fatalf("calls = %v", p.calls)
	}
}

func TestAPICallRejectsNonObject(t *testing.T) {
	s, _ := testServer()
	req := httptest.NewRequest(http.MethodPost, "/api/call/x", strings.NewReader(`[1]`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Result().StatusCode)
	}
}
