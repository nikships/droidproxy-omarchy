package proxy

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nikships/droidproxy-omarchy/internal/journal"
	"github.com/nikships/droidproxy-omarchy/internal/paths"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

// rawBackend stands in for CLIProxyAPI: it records each request and lets
// reply write raw response bytes.
func rawBackend(t *testing.T, reply func(net.Conn)) (int, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	requests := make(chan string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				data, err := readHTTPRequest(conn)
				if err != nil {
					return
				}
				requests <- string(data)
				reply(conn)
			}()
		}
	}()
	_, port := splitHostPort(t, ln.Addr().String())
	return port, requests
}

// journalProxy starts a proxy whose journal uses the isolated HOME.
func journalProxy(t *testing.T, backendPort int, enabled bool) (*Proxy, *journal.Journal) {
	t.Helper()
	if enabled {
		if err := prefs.Shared().Set(prefs.KeyRequestJournal, true); err != nil {
			t.Fatal(err)
		}
	}
	j := journal.New(paths.JournalDir(), paths.AuthDir(), prefs.RequestJournal)
	p := New()
	p.Port = freePort(t)
	p.TargetHost = "127.0.0.1"
	p.TargetPort = backendPort
	p.Journal = j
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	return p, j
}

func postRequest(path, body string, headers ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\n", path)
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return b.String()
}

func roundTrip(t *testing.T, p *Proxy, request string) []byte {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// journalRecords stops the proxy, flushes the journal, and returns its lines.
func journalRecords(t *testing.T, p *Proxy, j *journal.Journal) []map[string]any {
	t.Helper()
	p.Stop()
	j.Close()
	files, _ := filepath.Glob(filepath.Join(paths.JournalDir(), "requests-*.jsonl"))
	var records []map[string]any
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var rec map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
				t.Fatal(err)
			}
			records = append(records, rec)
		}
		f.Close()
	}
	return records
}

func onlyRecord(t *testing.T, records []map[string]any) map[string]any {
	t.Helper()
	if len(records) != 1 {
		t.Fatalf("%d journal records, want 1: %v", len(records), records)
	}
	return records[0]
}

func expectFields(t *testing.T, rec map[string]any, want map[string]any) {
	t.Helper()
	for key, value := range want {
		got, _ := json.Marshal(rec[key])
		expected, _ := json.Marshal(value)
		if string(got) != string(expected) {
			t.Errorf("%s = %s, want %s", key, got, expected)
		}
	}
}

func chunked(parts ...string) string {
	var b strings.Builder
	for _, part := range parts {
		fmt.Fprintf(&b, "%x\r\n%s\r\n", len(part), part)
	}
	b.WriteString("0\r\n\r\n")
	return b.String()
}

func TestJournalRecordsStreamedResponse(t *testing.T) {
	isolateHome(t)
	if err := os.MkdirAll(paths.AuthDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(paths.AuthDir(), "claude-seat@example.invalid-org-1.json")
	if err := os.WriteFile(authFile, []byte(`{"type":"claude","email":"seat@example.invalid","organization_uuid":"org-1","access_token":"synthetic"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("claude:" + authFile))
	index := hex.EncodeToString(sum[:8])
	trace := "20261111100000-" + index + "-019a7140-0000-7000-8000-000000000001"
	response := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nX-Cpa-Trace-Id: " + trace +
		"\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n" + chunked(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-haiku-4-5-20251001\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":20,\"cache_creation_input_tokens\":0,\"output_tokens\":1}}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	port, requests := rawBackend(t, func(conn net.Conn) {
		for _, part := range strings.SplitAfter(response, "\n\n") {
			_, _ = io.WriteString(conn, part)
			time.Sleep(2 * time.Millisecond)
		}
	})
	p, j := journalProxy(t, port, true)

	got := roundTrip(t, p, postRequest("/v1/messages", `{"model":"claude-haiku-4-5-20251001","stream":true}`,
		"Accept-Encoding: gzip", "X-DroidProxy-Origin: window-primer", "X-Session-ID: header-session-0001"))
	if string(got) != response {
		t.Fatalf("client bytes differ from upstream bytes:\n%q\n%q", got, response)
	}
	forwarded := strings.ToLower(<-requests)
	if strings.Contains(forwarded, "accept-encoding") || strings.Contains(forwarded, "x-droidproxy-origin") {
		t.Fatalf("forwarded request kept an observation-only header:\n%s", forwarded)
	}

	rec := onlyRecord(t, journalRecords(t, p, j))
	expectFields(t, rec, map[string]any{
		"v": 1, "execution_id": "019a7140-0000-7000-8000-000000000001", "trace_id": trace,
		"origin": "primer", "endpoint": "/v1/messages", "forwarded_endpoint": nil, "provider": "claude",
		"requested_model": "claude-haiku-4-5-20251001", "response_model": "claude-haiku-4-5-20251001",
		"status": 200, "outcome": "complete", "client_session": "header-session-0001",
		"seat":   map[string]any{"auth_index": index, "role": "served", "file": "claude-seat@example.invalid-org-1.json", "email": "seat@example.invalid", "org": "org-1"},
		"tokens": map[string]any{"input_uncached": 10, "cache_read": 20, "cache_creation": 0, "output": 3, "reasoning": nil},
	})
	if rec["ttft_ms"] == nil || rec["ttft_ms"].(float64) > rec["latency_ms"].(float64) {
		t.Errorf("ttft_ms = %v, latency_ms = %v", rec["ttft_ms"], rec["latency_ms"])
	}
}

func TestJournalRecordsNonStreamedResponse(t *testing.T) {
	isolateHome(t)
	body := `{"id":"resp_1","object":"response","model":"gpt-6-luna","status":"completed","usage":{"input_tokens":300,"input_tokens_details":{"cached_tokens":256},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":2}}}`
	response := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	port, _ := rawBackend(t, func(conn net.Conn) { _, _ = io.WriteString(conn, response) })
	p, j := journalProxy(t, port, true)

	got := roundTrip(t, p, postRequest("/v1/responses?trace=1", `{"model":"gpt-6-luna","prompt_cache_key":"session-fixture-0001","input":[]}`))
	if string(got) != response {
		t.Fatalf("client bytes differ from upstream bytes: %q", got)
	}
	expectFields(t, onlyRecord(t, journalRecords(t, p, j)), map[string]any{
		"trace_id": nil, "seat": nil, "endpoint": "/v1/responses", "forwarded_endpoint": nil,
		"provider": "codex", "requested_model": "gpt-6-luna", "response_model": "gpt-6-luna",
		"status": 200, "outcome": "complete", "client_session": "session-fixture-0001", "origin": "client",
		"tokens": map[string]any{"input_uncached": 44, "cache_read": 256, "cache_creation": nil, "output": 7, "reasoning": 2},
	})
}

func TestJournalRecordsDialFailure(t *testing.T) {
	isolateHome(t)
	p, j := journalProxy(t, freePort(t), true)
	got := roundTrip(t, p, postRequest("/v1/messages", `{"model":"claude-haiku-4-5-20251001"}`))
	if !strings.HasPrefix(string(got), "HTTP/1.1 502 ") {
		t.Fatalf("response = %q", got)
	}
	expectFields(t, onlyRecord(t, journalRecords(t, p, j)), map[string]any{
		"outcome": "backend_unreachable", "status": 502, "trace_id": nil, "seat": nil,
		"ttft_ms": nil, "tokens": nil, "response_model": nil, "provider": "claude",
	})
}

func TestJournalRecordsClientDisconnect(t *testing.T) {
	isolateHome(t)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	port, _ := rawBackend(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		event := chunked("event: ping\ndata: {\"type\":\"ping\"}\n\n")
		event = strings.TrimSuffix(event, "0\r\n\r\n")
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if _, err := io.WriteString(conn, event); err != nil {
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	p, j := journalProxy(t, port, true)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, postRequest("/v1/messages", `{"model":"claude-haiku-4-5-20251001"}`)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			break
		}
	}
	_ = conn.(*net.TCPConn).SetLinger(0)
	conn.Close()

	expectFields(t, onlyRecord(t, journalRecords(t, p, j)), map[string]any{
		"outcome": "client_disconnected", "status": 200, "tokens": nil,
	})
}

func TestJournalOffLeavesRelayUnchanged(t *testing.T) {
	isolateHome(t)
	body := `{"ok":true}`
	response := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	port, requests := rawBackend(t, func(conn net.Conn) { _, _ = io.WriteString(conn, response) })
	p, j := journalProxy(t, port, false)

	got := roundTrip(t, p, postRequest("/v1/messages", `{"model":"claude-haiku-4-5-20251001"}`,
		"Accept-Encoding: gzip", "X-DroidProxy-Origin: window-primer"))
	if string(got) != response {
		t.Fatalf("client bytes differ from upstream bytes: %q", got)
	}
	forwarded := <-requests
	if !strings.Contains(forwarded, "\r\nAccept-Encoding: gzip\r\n") {
		t.Fatalf("Accept-Encoding was not forwarded with the journal off:\n%s", forwarded)
	}
	if strings.Contains(strings.ToLower(forwarded), "x-droidproxy-origin") {
		t.Fatalf("origin marker was forwarded:\n%s", forwarded)
	}
	if records := journalRecords(t, p, j); len(records) != 0 {
		t.Fatalf("journal off wrote %v", records)
	}
	if _, err := os.Stat(paths.JournalDir()); !os.IsNotExist(err) {
		t.Fatalf("journal off created %s (%v)", paths.JournalDir(), err)
	}
}

func TestJournalSkipsCountTokens(t *testing.T) {
	isolateHome(t)
	port, _ := rawBackend(t, func(conn net.Conn) {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}")
	})
	p, j := journalProxy(t, port, true)
	roundTrip(t, p, postRequest("/v1/messages/count_tokens", `{"model":"claude-haiku-4-5-20251001"}`))
	if records := journalRecords(t, p, j); len(records) != 0 {
		t.Fatalf("count_tokens was journaled: %v", records)
	}
}
