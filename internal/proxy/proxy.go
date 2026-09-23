// Package proxy is the ThinkingProxy port: the user-facing HTTP proxy on
// localhost:8317. It forwards most traffic to CLIProxyAPI (:8318), TLS-forwards
// Grok and Meta Muse Responses upstream, rewrites Claude Anthropic-Beta
// headers, injects Codex fast-mode service_tier, and repairs Grok responses.
//
// Junie / Copilot / Cursor / Kimi-specific paths from the macOS app are not
// ported (out of scope on Omarchy).
package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

const (
	// Port is the user-facing ThinkingProxy port.
	Port = 8317
	// DefaultTargetPort is the CLIProxyAPI listener.
	DefaultTargetPort = 8318
	DefaultTargetHost = "127.0.0.1"

	grokRewriteBufferLimit = 8 * 1024 * 1024
	maxRequestBytes        = 64 * 1024 * 1024
	readChunkSize          = 65536
)

// Proxy is the ThinkingProxy. Start/Stop/IsRunning match the daemon seam.
type Proxy struct {
	Port       int
	TargetHost string
	TargetPort int

	// DialBackend connects to CLIProxyAPI. Tests point this at a local listener.
	DialBackend func(network, address string) (net.Conn, error)
	// DialTLS connects to Grok/Meta. Tests point this at a local TLS or plain listener.
	DialTLS func(network, address string, cfg *tls.Config) (net.Conn, error)
	// GrokAccessToken resolves a Grok OAuth bearer. Defaults to grok.AccessToken.
	GrokAccessToken func() (string, error)
	// MetaAPIKey returns the first usable Muse Model API key.
	MetaAPIKey func() (string, bool)

	mu       sync.Mutex
	listener net.Listener
	running  atomic.Bool
	wg       sync.WaitGroup
}

// New returns a Proxy with production defaults.
func New() *Proxy {
	return &Proxy{
		Port:       Port,
		TargetHost: DefaultTargetHost,
		TargetPort: DefaultTargetPort,
	}
}

// Start binds and begins accepting connections. Mirrors ThinkingProxy.start /
// startListener, including a one-shot fallback when a custom bind address fails.
func (p *Proxy) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running.Load() {
		logx.Logf("[ThinkingProxy] Already running")
		return nil
	}
	return p.startListenerLocked(true)
}

func (p *Proxy) startListenerLocked(allowCustomBindAddress bool) error {
	bindAddress := prefs.BindAddress()
	useCustomBind := allowCustomBindAddress && bindAddress != "0.0.0.0"

	var addr string
	if useCustomBind {
		addr = net.JoinHostPort(bindAddress, fmt.Sprintf("%d", p.port()))
		logx.Logf("[ThinkingProxy] Binding to %s:%d", bindAddress, p.port())
	} else if !allowCustomBindAddress {
		addr = net.JoinHostPort("0.0.0.0", fmt.Sprintf("%d", p.port()))
		logx.Logf("[ThinkingProxy] Falling back to all interfaces after bind failure: %d", p.port())
	} else {
		addr = net.JoinHostPort("0.0.0.0", fmt.Sprintf("%d", p.port()))
		logx.Logf("[ThinkingProxy] Binding to all interfaces (0.0.0.0):%d", p.port())
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logx.Logf("[ThinkingProxy] Failed to start: %v", err)
		if useCustomBind {
			logx.Logf("[ThinkingProxy] Retrying on all interfaces after start failure")
			return p.startListenerLocked(false)
		}
		return err
	}

	p.listener = ln
	p.running.Store(true)
	logx.Logf("[ThinkingProxy] Listening on port %d", p.port())

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.acceptLoop(ln)
	}()
	return nil
}

func (p *Proxy) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if p.running.Load() {
				logx.Logf("[ThinkingProxy] Accept error: %v", err)
			}
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handleConnection(conn)
		}()
	}
}

// Stop closes the listener and waits for in-flight handlers to finish.
func (p *Proxy) Stop() {
	p.mu.Lock()
	if !p.running.Load() {
		p.mu.Unlock()
		return
	}
	p.running.Store(false)
	ln := p.listener
	p.listener = nil
	p.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	p.wg.Wait()
	logx.Logf("[ThinkingProxy] Stopped")
}

// IsRunning reports whether the proxy is accepting connections.
func (p *Proxy) IsRunning() bool { return p.running.Load() }

func (p *Proxy) port() int {
	if p.Port == 0 {
		return Port
	}
	return p.Port
}

func (p *Proxy) targetHost() string {
	if p.TargetHost == "" {
		return DefaultTargetHost
	}
	return p.TargetHost
}

func (p *Proxy) targetPort() int {
	if p.TargetPort == 0 {
		return DefaultTargetPort
	}
	return p.TargetPort
}

func (p *Proxy) dialBackend() (net.Conn, error) {
	addr := net.JoinHostPort(p.targetHost(), fmt.Sprintf("%d", p.targetPort()))
	if p.DialBackend != nil {
		return p.DialBackend("tcp", addr)
	}
	return net.DialTimeout("tcp", addr, 10*time.Second)
}

func (p *Proxy) dialTLS(host string) (net.Conn, error) {
	addr := net.JoinHostPort(host, "443")
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if p.DialTLS != nil {
		return p.DialTLS("tcp", addr, cfg)
	}
	return tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, cfg)
}

func (p *Proxy) handleConnection(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))
	data, err := readHTTPRequest(conn)
	if err != nil {
		logx.Logf("[ThinkingProxy] Failed to read request: %v", err)
		sendError(conn, 400, "Invalid request")
		return
	}
	p.processRequest(data, conn)
}

func readHTTPRequest(conn net.Conn) ([]byte, error) {
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, readChunkSize)
	headerEnd := -1
	contentLength := -1

	for {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > maxRequestBytes {
				return nil, errors.New("request too large")
			}
		}
		if headerEnd < 0 {
			if idx := indexOfCRLFCRLF(buf); idx >= 0 {
				headerEnd = idx + 4
				contentLength = parseContentLength(buf[:headerEnd])
			}
		}
		if headerEnd >= 0 {
			bodyReceived := len(buf) - headerEnd
			if contentLength >= 0 {
				if bodyReceived >= contentLength {
					return buf[:headerEnd+contentLength], nil
				}
			} else if err == io.EOF {
				// No Content-Length: body ends when the client closes.
				return buf, nil
			}
		}
		if err == io.EOF {
			if headerEnd >= 0 {
				return buf, nil
			}
			return nil, io.ErrUnexpectedEOF
		}
		if err != nil {
			return nil, err
		}
	}
}

func indexOfCRLFCRLF(b []byte) int {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == '\r' && b[i+1] == '\n' && b[i+2] == '\r' && b[i+3] == '\n' {
			return i
		}
	}
	return -1
}

func parseContentLength(headerData []byte) int {
	for _, line := range strings.Split(string(headerData), "\r\n") {
		if len(line) == 0 {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if !strings.EqualFold(name, "Content-Length") {
			continue
		}
		value := strings.TrimSpace(line[colon+1:])
		var n int
		for _, c := range value {
			if c < '0' || c > '9' {
				return -1
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	return -1
}
