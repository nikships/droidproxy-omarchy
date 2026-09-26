// Package webui serves the DroidProxy settings web app: a localhost-only HTTP
// server with an embedded single-page frontend plus a small JSON API that
// mirrors the control API. The Omarchy bar icon opens it in the default
// browser; it replaces the old QML settings panel.
package webui

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/control"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
)

//go:embed static
var staticFiles embed.FS

const (
	// Port is the localhost-only settings UI port. 8317 is ThinkingProxy,
	// 8318 is CLIProxyAPI; 8319 stays reserved for a future Copilot gateway
	// (macOS parity), so the UI takes the next one.
	Port = 8320
	// Host is the only interface the UI ever binds: loopback only, no
	// remote access regardless of the remote-management settings.
	Host = "127.0.0.1"
)

// URL is the address the bar icon and `droidproxy open` open.
func URL() string { return "http://" + net.JoinHostPort(Host, strconv.Itoa(Port)) }

// Provider backs the API. The daemon implements it.
type Provider interface {
	State() control.State
	Call(ctx context.Context, method string, params json.RawMessage) control.CallResult
}

const (
	maxCallBody      = 1 << 20
	subscriberBuffer = 16
	// stateCoalesce is the minimum gap between two state events per SSE
	// subscriber, matching the control server.
	stateCoalesce = 100 * time.Millisecond
)

// Server serves the settings web app for one Provider.
type Server struct {
	provider Provider

	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	srv    *http.Server
	closed bool
}

type subscriber struct {
	dirty chan struct{}
	msgs  chan control.Event
}

// NewServer returns a server for p.
func NewServer(p Provider) *Server {
	return &Server{provider: p, subs: map[*subscriber]struct{}{}}
}

// Handler returns the HTTP handler (exposed for tests and embedding).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/call/{method...}", s.handleCall)

	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("webui: embedded static dir missing: " + err.Error())
	}
	files := http.FileServer(http.FS(static))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			http.Redirect(w, r, "/icons/icon-active.png", http.StatusFound)
			return
		}
		if r.URL.Path != "/" && !isStaticPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		// The frontend is one page; keep caching off so updates apply
		// immediately after an upgrade.
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
	return secureHeaders(mux)
}

func isStaticPath(path string) bool {
	return path == "/app.js" ||
		path == "/styles.css" ||
		strings.HasPrefix(path, "/icons/")
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe binds Host:Port and serves until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(Host, strconv.Itoa(Port)))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	srv := s.srv
	s.mu.Unlock()
	logx.Logf("[WebUI] Listening on %s", URL())

	go func() {
		<-ctx.Done()
		s.Close()
	}()
	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Close stops the server and ends every event stream.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srv := s.srv
	for sub := range s.subs {
		close(sub.msgs)
	}
	s.subs = map[*subscriber]struct{}{}
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return srv.Close()
	}
	return nil
}

// NotifyStateChanged schedules a state event for every SSE subscriber. It
// never blocks and may be called from any goroutine at any rate.
func (s *Server) NotifyStateChanged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		select {
		case sub.dirty <- struct{}{}:
		default:
		}
	}
}

// PostMessage broadcasts a "message" event to every SSE subscriber.
func (s *Server) PostMessage(title, body, level string) {
	ev := control.Event{Type: control.EventMessage, Title: title, Body: body, Level: level}
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		select {
		case sub.msgs <- ev:
		default:
			logx.Logf("[WebUI] Dropping message for slow event subscriber")
		}
	}
}

func (s *Server) subscribe() (*subscriber, bool) {
	sub := &subscriber{dirty: make(chan struct{}, 1), msgs: make(chan control.Event, subscriberBuffer)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	s.subs[sub] = struct{}{}
	return sub, true
}

func (s *Server) unsubscribe(sub *subscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subs, sub)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.provider.State())
}

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	if method == "" {
		writeJSON(w, http.StatusNotFound, control.Fail("Missing method name."))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCallBody+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, control.Fail("Could not read request body."))
		return
	}
	if len(body) > maxCallBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, control.Fail("Request body is too large."))
		return
	}
	params, ok := normalizeParams(body)
	if !ok {
		writeJSON(w, http.StatusBadRequest, control.Fail("Parameters must be a JSON object."))
		return
	}
	writeJSON(w, http.StatusOK, s.provider.Call(r.Context(), method, params))
}

func normalizeParams(body []byte) (json.RawMessage, bool) {
	trimmed := []byte(strings.TrimSpace(string(body)))
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage("{}"), true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return nil, false
	}
	return json.RawMessage(trimmed), true
}

// handleEvents streams server-sent events: a state event immediately on
// connect and after every change (coalesced), plus message events for async
// flow results. The frontend falls back to polling /api/state when SSE is
// unavailable.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sub, ok := s.subscribe()
	if !ok {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func(ev control.Event) bool {
		data, err := json.Marshal(ev)
		if err != nil {
			return false
		}
		if _, err := w.Write([]byte("data: " + string(data) + "\n\n")); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	sendState := func() bool {
		st := s.provider.State()
		return send(control.Event{Type: control.EventState, State: &st})
	}

	if !sendState() {
		return
	}
	lastState := time.Now()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-sub.msgs:
			if !ok {
				return
			}
			if !send(ev) {
				return
			}
		case <-sub.dirty:
			if timerC != nil {
				continue
			}
			wait := stateCoalesce - time.Since(lastState)
			if wait <= 0 {
				if !sendState() {
					return
				}
				lastState = time.Now()
				continue
			}
			timer = time.NewTimer(wait)
			timerC = timer.C
		case <-timerC:
			timerC = nil
			timer = nil
			if !sendState() {
				return
			}
			lastState = time.Now()
		}
	}
}
