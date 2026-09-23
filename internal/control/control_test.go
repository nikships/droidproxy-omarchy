package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeProvider struct {
	state State
	calls []string
	// onCall can rewrite the result for a specific method.
	onCall func(method string, params json.RawMessage) CallResult
}

func (f *fakeProvider) State() State { return f.state }

func (f *fakeProvider) Call(_ context.Context, method string, params json.RawMessage) CallResult {
	f.calls = append(f.calls, method)
	if f.onCall != nil {
		return f.onCall(method, params)
	}
	return OK("did " + method)
}

func startServer(t *testing.T, p Provider) (*Server, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "control.sock")
	srv := NewServer(p)
	if err := srv.Listen(sock); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { _ = srv.Close })
	return srv, sock
}

func TestStateRoundTrip(t *testing.T) {
	p := &fakeProvider{state: sampleState()}
	_, sock := startServer(t, p)

	st, err := NewClient(sock).State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.App.Version != "1.0.3" || st.Server.ProxyPort != 8317 {
		t.Fatalf("unexpected state: %+v", st)
	}
	// Slices must never encode as null.
	data, _ := json.Marshal(st)
	for _, s := range []string{`"providers":null`, `"accounts":null`, `"windows":null`, `"availableModels":null`, `"selectedModels":null`} {
		if bytes.Contains(data, []byte(s)) {
			t.Errorf("state contains %s: %s", s, data)
		}
	}
}

func TestStateJSONShape(t *testing.T) {
	data, err := json.Marshal(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"app", "server", "settings", "paths", "factory", "providers", "copilot", "meta", "grok", "usage", "update"} {
		if _, ok := m[key]; !ok {
			t.Errorf("state snapshot missing key %q", key)
		}
	}
}

func TestCall(t *testing.T) {
	p := &fakeProvider{}
	_, sock := startServer(t, p)
	c := NewClient(sock)

	res, err := c.Call(context.Background(), "server.start", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.OK || res.Message != "did server.start" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if p.calls[0] != "server.start" {
		t.Fatalf("provider got %v", p.calls)
	}

	// ok=false is a value, not a transport error.
	res, err = c.Call(context.Background(), "provider.connect", map[string]any{"provider": "grok"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
}

func TestEventsInitialStateAndMessages(t *testing.T) {
	p := &fakeProvider{state: sampleState()}
	srv, sock := startServer(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 16)
	go func() {
		_ = NewClient(sock).Watch(ctx, func(ev Event) { events <- ev })
	}()

	select {
	case ev := <-events:
		if ev.Type != EventState || ev.State == nil || ev.State.App.Version != "1.0.3" {
			t.Fatalf("expected initial state event, got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial state")
	}

	srv.PostMessage("Authentication Result", "connected", LevelInfo)
	select {
	case ev := <-events:
		if ev.Type != EventMessage || ev.Title != "Authentication Result" || ev.Body != "connected" || ev.Level != LevelInfo {
			t.Fatalf("unexpected message event: %+v", ev)
		}
		if ev.ID == "" {
			t.Error("message event should have an id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestNotifyStateChangedCoalesces(t *testing.T) {
	p := &fakeProvider{state: sampleState()}
	srv, sock := startServer(t, p)
	srv.coalesce = 150 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	count := make(chan Event, 64)
	go func() {
		_ = NewClient(sock).Watch(ctx, func(ev Event) { count <- ev })
	}()
	<-count // initial state

	for i := 0; i < 20; i++ {
		srv.NotifyStateChanged()
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)

	n := 0
	drained := false
	for !drained {
		select {
		case ev := <-count:
			if ev.Type == EventState {
				n++
			}
		default:
			drained = true
		}
	}
	if n > 3 {
		t.Errorf("20 notifications coalesced into %d state events; want <=3", n)
	}
}

func TestClientDisconnect(t *testing.T) {
	p := &fakeProvider{state: sampleState()}
	srv, sock := startServer(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewClient(sock).Watch(ctx, func(Event) {}) }()

	// Give the subscriber time to register, then disconnect.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("Watch returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after client disconnect")
	}

	// The server side drops the subscriber asynchronously after the
	// disconnect; give it a moment.
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.mu.Lock()
		n := len(srv.subs)
		srv.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server still holds %d subscribers after disconnect", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStaleSocketRemoved(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	// A stale socket with nothing listening behind it.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()

	srv := NewServer(&fakeProvider{})
	if err := srv.Listen(sock); err != nil {
		t.Fatalf("Listen on stale socket: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close })
}

func TestRefusesLiveDaemon(t *testing.T) {
	_, sock := startServer(t, &fakeProvider{})

	srv2 := NewServer(&fakeProvider{})
	err := srv2.Listen(sock)
	if err == nil {
		t.Fatal("expected ErrAlreadyRunning")
	}
	if err != ErrAlreadyRunning {
		t.Fatalf("got %v, want ErrAlreadyRunning", err)
	}
}

func TestSocketPermissions(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sub", "control.sock")
	srv := NewServer(&fakeProvider{})
	if err := srv.Listen(sock); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close })

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
	di, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("socket dir mode = %o, want 700", perm)
	}
}

func TestOfflineClient(t *testing.T) {
	// A socket path that does not exist at all.
	c := NewClient(filepath.Join(t.TempDir(), "missing.sock"))
	if _, err := c.State(); !IsOffline(err) {
		t.Errorf("State err = %v, want ErrOffline", err)
	}
	if _, err := c.Call(context.Background(), "x", nil); !IsOffline(err) {
		t.Errorf("Call err = %v, want ErrOffline", err)
	}
	if err := c.Watch(context.Background(), func(Event) {}); !IsOffline(err) {
		t.Errorf("Watch err = %v, want ErrOffline", err)
	}
}

func TestEventJSONShapes(t *testing.T) {
	st := sampleState()
	line, _ := json.Marshal(Event{Type: EventState, State: &st})
	if !bytes.HasPrefix(line, []byte(`{"type":"state","state":{`)) {
		t.Errorf("state event line: %s", line)
	}
	line, _ = json.Marshal(Event{Type: EventMessage, ID: "b3c1", Title: "T", Body: "B", Level: LevelInfo})
	want := `{"type":"message","id":"b3c1","title":"T","body":"B","level":"info"}`
	if string(line) != want {
		t.Errorf("message line = %s, want %s", line, want)
	}
	line, _ = json.Marshal(Event{Type: EventOffline})
	if string(line) != `{"type":"offline"}` {
		t.Errorf("offline line = %s", line)
	}
}

func TestCallBodyShapes(t *testing.T) {
	p := &fakeProvider{}
	_, sock := startServer(t, p)
	c := NewClient(sock)

	// Empty body counts as {}.
	req, _ := http.NewRequest(http.MethodPost, c.url("/v1/call/server.start"), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("empty body status = %d", resp.StatusCode)
	}

	// Non-object body is rejected.
	req, _ = http.NewRequest(http.MethodPost, c.url("/v1/call/server.start"), bytes.NewBufferString(`[1,2]`))
	resp, err = c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("array body status = %d, want 400", resp.StatusCode)
	}
}

func sampleState() State {
	return State{
		App: AppInfo{
			Name: "DroidProxy", Version: "1.0.3", Commit: "abc1234",
			RepoURL: "https://github.com/nikships/droidproxy-omarchy",
		},
		Server: ServerState{Running: true, ProxyPort: 8317, BackendPort: 8318, URL: "http://localhost:8317"},
		Settings: Settings{
			LaunchAtLogin: true, BackgroundOpacity: 0.55,
			AutoCheckUpdates: true,
		},
		Paths:     PathsInfo{AuthDir: "/home/u/.cli-proxy-api"},
		Providers: []ProviderState{{ID: "claude", Name: "Claude Code", Kind: KindStandard, Enabled: true}},
		Copilot:   CopilotState{GatewayState: GatewayRunning, Port: 8319, MaxSelected: 3},
		Usage:     UsageState{Visible: true, Accounts: []UsageAccount{{Provider: "codex", ProviderName: "Codex"}}},
		Update:    UpdateState{State: UpdateIdle, CurrentVersion: "1.0.3"},
	}
}
