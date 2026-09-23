package control

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// Provider is what the daemon implements to back the API.
type Provider interface {
	// State returns the current full snapshot. It is called on every
	// GET /v1/state and for every state event, so it must be cheap and must
	// not block on slow work.
	State() State
	// Call runs one action. ctx is the request context: it is cancelled when
	// the client disconnects, so long-running work that must survive that
	// (installs, logins) should detach onto its own goroutine and report its
	// outcome with Server.PostMessage.
	Call(ctx context.Context, method string, params json.RawMessage) CallResult
}

// ErrAlreadyRunning is returned by Listen when another live daemon answers on
// the control socket.
var ErrAlreadyRunning = errors.New("another DroidProxy daemon is already running")

// StateCoalesceInterval is the minimum gap between two state events sent to
// one subscriber.
const StateCoalesceInterval = 100 * time.Millisecond

const (
	maxCallBody      = 1 << 20
	subscriberBuffer = 64
)

// Server serves the control API for one Provider.
type Server struct {
	provider Provider

	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	ln     net.Listener
	srv    *http.Server
	path   string
	closed bool

	coalesce time.Duration
}

type subscriber struct {
	dirty chan struct{}
	msgs  chan Event
}

// NewServer returns a server for p. Call Listen then Serve (or ListenAndServe).
func NewServer(p Provider) *Server {
	return &Server{
		provider: p,
		subs:     map[*subscriber]struct{}{},
		coalesce: StateCoalesceInterval,
	}
}

// Handler returns the HTTP handler (exposed for tests and embedding).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/state", s.handleState)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("POST /v1/call/{method...}", s.handleCall)
	return mux
}

// Listen binds the Unix socket at path ("" means paths.ControlSocketPath()).
// The parent directory is created 0700 and the socket is chmodded 0600. A
// stale socket file left by a crashed daemon is removed; a live one makes
// Listen fail with ErrAlreadyRunning.
func (s *Server) Listen(path string) error {
	if path == "" {
		path = paths.ControlSocketPath()
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) || dir == paths.RuntimeDir() {
		if err := paths.EnsureDir(dir, 0o700); err != nil {
			return fmt.Errorf("create socket directory: %w", err)
		}
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and is not a socket", path)
		}
		if socketAlive(path) {
			return ErrAlreadyRunning
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	s.mu.Lock()
	s.ln = ln
	s.path = path
	s.mu.Unlock()
	return nil
}

func socketAlive(path string) bool {
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Serve accepts connections until Close. It returns nil after Close.
func (s *Server) Serve() error {
	s.mu.Lock()
	ln := s.ln
	if ln == nil {
		s.mu.Unlock()
		return errors.New("control: Serve called before Listen")
	}
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          nil,
	}
	srv := s.srv
	s.mu.Unlock()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ListenAndServe binds path and serves until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, path string) error {
	if err := s.Listen(path); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return s.Serve()
}

// Close stops the server, ends every event stream, and removes the socket.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srv, ln := s.srv, s.ln
	for sub := range s.subs {
		close(sub.msgs)
	}
	s.subs = map[*subscriber]struct{}{}
	s.mu.Unlock()

	var err error
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = srv.Shutdown(ctx)
		cancel()
		if err != nil {
			err = srv.Close()
		}
	} else if ln != nil {
		err = ln.Close()
	}
	return err
}

// SocketPath returns the bound socket path ("" before Listen).
func (s *Server) SocketPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// NotifyStateChanged schedules a state event for every subscriber. It never
// blocks and may be called from any goroutine at any rate.
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

// PostMessage broadcasts a "message" event (level "info" or "error") to every
// connected subscriber. Messages are not queued for clients that connect
// later. It returns the message id.
func (s *Server) PostMessage(title, body, level string) string {
	if level != LevelError {
		level = LevelInfo
	}
	ev := Event{Type: EventMessage, ID: newID(), Title: title, Body: body, Level: level}
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		select {
		case sub.msgs <- ev:
		default:
			logx.Logf("[Control] Dropping message for slow event subscriber")
		}
	}
	return ev.ID
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Server) subscribe() (*subscriber, bool) {
	sub := &subscriber{dirty: make(chan struct{}, 1), msgs: make(chan Event, subscriberBuffer)}
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
		writeJSON(w, http.StatusNotFound, Fail("Missing method name."))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCallBody+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Fail("Could not read request body."))
		return
	}
	if len(body) > maxCallBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, Fail("Request body is too large."))
		return
	}
	params, ok := normalizeParams(body)
	if !ok {
		writeJSON(w, http.StatusBadRequest, Fail("Parameters must be a JSON object."))
		return
	}
	writeJSON(w, http.StatusOK, s.provider.Call(r.Context(), method, params))
}

// normalizeParams turns an empty body into {} and rejects anything that is
// not a JSON object.
func normalizeParams(body []byte) (json.RawMessage, bool) {
	trimmed := bytesTrimSpace(body)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage("{}"), true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return nil, false
	}
	return json.RawMessage(trimmed), true
}

func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\r' || c == '\t' }

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

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	send := func(ev Event) bool {
		if err := enc.Encode(ev); err != nil {
			return false
		}
		if err := bw.Flush(); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	sendState := func() bool {
		st := s.provider.State()
		return send(Event{Type: EventState, State: &st})
	}

	if !sendState() {
		return
	}
	lastState := time.Now()

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
			wait := s.coalesce - time.Since(lastState)
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
