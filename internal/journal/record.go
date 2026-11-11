// Package journal is the opt-in request journal: one JSON line per inference
// request relayed to CLIProxyAPI, naming the credential seat from the
// X-CPA-TRACE-ID response header, the client session key, and the token usage
// seen in the relayed response. contract/request-journal-v1.md is the
// definition; nothing here alters relayed bytes.
package journal

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Record is one journal line. Field order and nulls follow
// contract/request-journal-v1.schema.json.
type Record struct {
	V                 int     `json:"v"`
	ExecutionID       string  `json:"execution_id"`
	TraceID           *string `json:"trace_id"`
	StartedAt         string  `json:"started_at"`
	EndedAt           string  `json:"ended_at"`
	LatencyMS         int64   `json:"latency_ms"`
	TTFTMS            *int64  `json:"ttft_ms"`
	Origin            string  `json:"origin"`
	Endpoint          string  `json:"endpoint"`
	ForwardedEndpoint *string `json:"forwarded_endpoint"`
	Provider          string  `json:"provider"`
	RequestedModel    *string `json:"requested_model"`
	ResponseModel     *string `json:"response_model"`
	Status            *int    `json:"status"`
	Outcome           string  `json:"outcome"`
	Seat              *Seat   `json:"seat"`
	ClientSession     *string `json:"client_session"`
	Tokens            *Tokens `json:"tokens"`
}

// Seat is the trace-selected CLIProxyAPI auth. Identity fields are omitted
// when they cannot be bound safely.
type Seat struct {
	AuthIndex string `json:"auth_index"`
	Role      string `json:"role"`
	File      string `json:"file,omitempty"`
	Email     string `json:"email,omitempty"`
	Org       string `json:"org,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}

// Tokens is terminal usage normalized across the Anthropic, Responses and
// chat/completions shapes.
type Tokens struct {
	InputUncached int64  `json:"input_uncached"`
	CacheRead     int64  `json:"cache_read"`
	CacheCreation *int64 `json:"cache_creation"`
	Output        int64  `json:"output"`
	Reasoning     *int64 `json:"reasoning"`
}

const (
	outcomeComplete           = "complete"
	outcomeUpstreamError      = "upstream_error"
	outcomeUpstreamClosed     = "upstream_closed_early"
	outcomeClientDisconnected = "client_disconnected"
	outcomeBackendUnreachable = "backend_unreachable"

	roleServed    = "served"
	roleLastTried = "last_tried"
)

func encodeLine(rec Record) ([]byte, error) {
	line, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func millis(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// parseTrace splits X-CPA-TRACE-ID ("YYYYMMDDHHMMSS-<auth index>-<request
// UUID>"). Only a fully well-formed value yields an index and request id.
func parseTrace(trace string) (index, requestID string, ok bool) {
	parts := strings.SplitN(trace, "-", 3)
	if len(parts) != 3 || !isDigits(parts[0], 14) || !isLowerHex(parts[1], 16) || !uuidPattern.MatchString(parts[2]) {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func isDigits(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func ptr[T any](v T) *T { return &v }
