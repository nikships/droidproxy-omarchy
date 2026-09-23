package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"

	"github.com/nikships/droidproxy-omarchy/internal/paths"
)

// ErrOffline means the daemon is not reachable (no socket or nothing
// listening). Callers should treat it like the CLI's {"type":"offline"}.
var ErrOffline = errors.New("daemon is not running")

// Client talks to the daemon's control API over its Unix socket. It is safe
// for concurrent use.
type Client struct {
	socket string
	http   *http.Client
}

// SocketPath returns the socket this client dials.
func (c *Client) SocketPath() string { return c.socket }

// NewClient returns a client for the socket at path ("" means
// paths.ControlSocketPath()).
func NewClient(path string) *Client {
	if path == "" {
		path = paths.ControlSocketPath()
	}
	return &Client{
		socket: path,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", path)
				},
			},
		},
	}
}

// DefaultClient dials paths.ControlSocketPath().
func DefaultClient() *Client { return NewClient("") }

// IsOffline reports whether err means the daemon is not reachable.
func IsOffline(err error) bool { return errors.Is(err, ErrOffline) }

// isOfflineErr maps dial failures (missing socket, connection refused) onto
// ErrOffline so callers can distinguish "daemon down" from other errors.
func isOfflineErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrOffline) {
		return true
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		if errors.Is(oe.Err, syscall.ENOENT) || errors.Is(oe.Err, syscall.ECONNREFUSED) || errors.Is(oe.Err, syscall.ECONNRESET) {
			return true
		}
	}
	// Fall back to text matching for wrapped errors that lose their errno.
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such file or directory") ||
		strings.Contains(msg, "socket has been disconnected")
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if isOfflineErr(err) {
			return nil, ErrOffline
		}
		return nil, err
	}
	return resp, nil
}

// State fetches the current snapshot.
func (c *Client) State() (State, error) {
	req, err := http.NewRequest(http.MethodGet, c.url("/v1/state"), nil)
	if err != nil {
		return State{}, err
	}
	resp, err := c.do(req)
	if err != nil {
		return State{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return State{}, fmt.Errorf("control: GET /v1/state returned %d", resp.StatusCode)
	}
	var st State
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return State{}, err
	}
	return st, nil
}

// Call runs one action. params may be nil (sent as {}). A result with ok ==
// false is returned as a value, not as an error.
func (c *Client) Call(ctx context.Context, method string, params any) (CallResult, error) {
	body := []byte("{}")
	if params != nil {
		var err error
		body, err = json.Marshal(params)
		if err != nil {
			return CallResult{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.url("/v1/call/"+url.PathEscape(method)), strings.NewReader(string(body)))
	if err != nil {
		return CallResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return CallResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return CallResult{}, fmt.Errorf("control: unknown method %q", method)
	}
	if resp.StatusCode != http.StatusOK {
		return CallResult{}, fmt.Errorf("control: POST /v1/call/%s returned %d", method, resp.StatusCode)
	}
	var cr CallResult
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return CallResult{}, err
	}
	return cr, nil
}

// Watch streams events to fn until ctx is done or the daemon drops the
// stream. It returns ctx.Err() on context end, ErrOffline if the daemon is
// unreachable at dial time, and the connection error otherwise (reconnecting
// is the caller's job; the CLI does).
func (c *Client) Watch(ctx context.Context, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/v1/events"), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("control: GET /v1/events returned %d", resp.StatusCode)
	}
	r := bufio.NewReader(resp.Body)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var ev Event
			if jerr := json.Unmarshal(line, &ev); jerr == nil {
				fn(ev)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (c *Client) url(p string) string {
	// Host is ignored over Unix sockets but must be non-empty.
	return "http://droidproxy" + p
}
