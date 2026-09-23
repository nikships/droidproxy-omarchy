package proxy

import (
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/grok"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/meta"
)

func (p *Proxy) forwardRequest(method, path, version string, headers [][2]string, body string, client net.Conn) {
	target, err := p.dialBackend()
	if err != nil {
		logx.Logf("[ThinkingProxy] Target connection failed: %v", err)
		sendError(client, 502, "Bad Gateway")
		return
	}
	defer target.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s\r\n", method, path, version)
	excluded := map[string]bool{
		"content-length": true, "host": true, "transfer-encoding": true,
	}
	for _, h := range headers {
		if !excluded[strings.ToLower(h[0])] {
			fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
		}
	}
	fmt.Fprintf(&b, "Host: %s:%d\r\n", p.targetHost(), p.targetPort())
	b.WriteString("Connection: close\r\n")
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.WriteString(body)

	if _, err := io.WriteString(target, b.String()); err != nil {
		logx.Logf("[ThinkingProxy] Send error: %v", err)
		return
	}
	streamProxy(target, client)
}

func (p *Proxy) forwardToGrok(method, path, version string, headers [][2]string, body, model string, client net.Conn) {
	token, err := p.resolveGrokToken()
	if err != nil {
		logx.Logf("[ThinkingProxy] Grok auth error: %s", err.Error())
		status, message := grokAuthHTTPStatus(err)
		sendError(client, status, message)
		return
	}
	p.sendGrokUpstream(method, path, version, headers, body, model, token, client)
}

func (p *Proxy) resolveGrokToken() (string, error) {
	if p.GrokAccessToken != nil {
		return p.GrokAccessToken()
	}
	token, authErr := grok.AccessToken()
	if authErr != nil {
		return "", authErr
	}
	return token, nil
}

func grokAuthHTTPStatus(err error) (int, string) {
	if ae, ok := err.(*grok.AuthError); ok {
		switch ae.Kind {
		case grok.AuthErrNotLoggedIn:
			return 401, "Not logged in to Grok. Connect Grok in DroidProxy settings."
		case grok.AuthErrReauthRequired:
			return 401, ae.Error()
		}
	}
	return 502, "Grok authentication failed: " + err.Error()
}

func (p *Proxy) sendGrokUpstream(method, path, version string, headers [][2]string, body, model, accessToken string, client net.Conn) {
	host := grok.UpstreamHost(model)
	upstreamPath := grok.NormalizeUpstreamPath(path)
	target, err := p.dialTLS(host)
	if err != nil {
		logx.Logf("[ThinkingProxy] Connection to %s failed: %v", host, err)
		sendError(client, 502, "Bad Gateway - Could not connect to "+host)
		return
	}
	defer target.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s\r\n", method, upstreamPath, version)
	for _, h := range grok.FilterClientHeaders(headers) {
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	fmt.Fprintf(&b, "Host: %s\r\n", host)
	fmt.Fprintf(&b, "Authorization: Bearer %s\r\n", accessToken)
	for _, h := range grok.UpstreamAuthHeaders(model) {
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	b.WriteString("Content-Type: application/json\r\n")
	b.WriteString("Accept-Encoding: identity\r\n")
	b.WriteString("Connection: close\r\n")
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.WriteString(body)

	logx.Debugf("FORWARD GROK: %s %s -> %s", method, upstreamPath, host)
	if _, err := io.WriteString(target, b.String()); err != nil {
		logx.Logf("[ThinkingProxy] Send error to %s: %v", host, err)
		return
	}
	p.relayUpstreamResponse(target, client, "Grok", true)
}

func (p *Proxy) forwardToMeta(method, path, version string, headers [][2]string, body string, client net.Conn) {
	apiKey, ok := p.resolveMetaAPIKey()
	if !ok {
		logx.Logf("[ThinkingProxy] Error: No active Meta Muse API key found")
		sendError(client, 401, "No active Meta Muse API key found. Connect Meta Muse in DroidProxy settings.")
		return
	}

	host := meta.APIHost
	upstreamPath := meta.UpstreamPath(path)
	target, err := p.dialTLS(host)
	if err != nil {
		logx.Logf("[ThinkingProxy] Connection to %s failed: %v", host, err)
		sendError(client, 502, "Bad Gateway - Could not connect to "+host)
		return
	}
	defer target.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s\r\n", method, upstreamPath, version)
	for _, h := range meta.HeadersForForwarding(headers) {
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}
	fmt.Fprintf(&b, "Host: %s\r\n", host)
	fmt.Fprintf(&b, "Authorization: Bearer %s\r\n", apiKey)
	b.WriteString("Content-Type: application/json\r\n")
	b.WriteString("Accept-Encoding: identity\r\n")
	b.WriteString("Connection: close\r\n")
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.WriteString(body)

	logx.Debugf("FORWARD META: %s %s -> %s", method, upstreamPath, host)
	if _, err := io.WriteString(target, b.String()); err != nil {
		logx.Logf("[ThinkingProxy] Send error to %s: %v", host, err)
		return
	}
	p.relayUpstreamResponse(target, client, "Meta", false)
}

func (p *Proxy) resolveMetaAPIKey() (string, bool) {
	if p.MetaAPIKey != nil {
		return p.MetaAPIKey()
	}
	keys := meta.UsableAPIKeys(meta.Shared().Accounts(), time.Now())
	if len(keys) == 0 {
		return "", false
	}
	return keys[0], true
}

func (p *Proxy) relayUpstreamResponse(target, client net.Conn, label string, rewriteGrokNativeToolCalls bool) {
	if rewriteGrokNativeToolCalls {
		p.accumulateAndRewriteGrokResponse(target, client, label)
		return
	}
	streamProxy(target, client)
}

func (p *Proxy) accumulateAndRewriteGrokResponse(target, client net.Conn, label string) {
	var accumulated []byte
	buf := make([]byte, readChunkSize)
	for {
		n, err := target.Read(buf)
		if n > 0 {
			accumulated = append(accumulated, buf[:n]...)
			if len(accumulated) > grokRewriteBufferLimit {
				logx.Debugf("GROK REWRITE ABORTED: response exceeded %d bytes; relaying unchanged", grokRewriteBufferLimit)
				if _, werr := client.Write(accumulated); werr != nil {
					logx.Logf("[ThinkingProxy] Send %s response error: %v", label, werr)
					return
				}
				streamProxy(target, client)
				return
			}
		}
		if err == io.EOF {
			p.sendRewrittenGrokResponse(accumulated, client, label)
			return
		}
		if err != nil {
			logx.Logf("[ThinkingProxy] Receive %s response error: %v", label, err)
			if len(accumulated) > 0 {
				p.sendRewrittenGrokResponse(accumulated, client, label)
			}
			return
		}
	}
}

func (p *Proxy) sendRewrittenGrokResponse(raw []byte, client net.Conn, label string) {
	rewritten := grok.RewriteHTTPResponse(raw)
	if string(rewritten) != string(raw) {
		logx.Debugf("REWROTE %s GROK RESPONSE (native tool markup and/or EndFeatureRun repair)", label)
		logx.Logf("[ThinkingProxy] Rewrote %s Grok response (native tool markup and/or EndFeatureRun repair)", label)
	}
	if _, err := client.Write(rewritten); err != nil {
		logx.Logf("[ThinkingProxy] Send %s rewritten response error: %v", label, err)
	}
}

func streamProxy(from, to net.Conn) {
	buf := make([]byte, readChunkSize)
	for {
		n, err := from.Read(buf)
		if n > 0 {
			if _, werr := to.Write(buf[:n]); werr != nil {
				logx.Logf("[ThinkingProxy] Send response error: %v", werr)
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				logx.Logf("[ThinkingProxy] Receive response error: %v", err)
			}
			return
		}
	}
}

func sendError(conn net.Conn, statusCode int, message string) {
	body := []byte(message)
	headers := fmt.Sprintf(
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		statusCode, message, len(body),
	)
	_, _ = conn.Write(append([]byte(headers), body...))
}
