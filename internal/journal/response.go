package journal

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Observation limits. They bound memory only; the relay never waits on them.
const (
	maxHeadBytes     = 64 << 10
	maxSSELineBytes  = 256 << 10
	maxJSONBodyBytes = 2 << 20
)

type framing int

const (
	framingNone framing = iota
	framingLength
	framingChunked
	framingClose
)

type chunkState int

const (
	chunkSize chunkState = iota
	chunkData
	chunkDataEnd
	chunkTrailer
)

// response follows the bytes the client received from CLIProxyAPI: HTTP
// head, entity framing, and the SSE or JSON body that carries usage.
type response struct {
	head     []byte
	headDone bool
	status   int
	trace    string

	sse    bool
	opaque bool

	framing   framing
	remaining int64
	chunk     chunkState
	line      []byte

	sawBody bool

	// completed: framing ended or a protocol terminal event arrived.
	// failed: malformed framing or an explicit protocol error event.
	// Whichever happens first decides; later bytes are not observed.
	completed bool
	failed    bool

	body         []byte
	bodyOverflow bool

	sseLine     []byte
	sseLineLong bool
	sseData     []byte
	sseHasData  bool
	sseBroken   bool

	model          string
	anthropicUsage map[string]json.RawMessage
	chatUsage      json.RawMessage
	tokens         *Tokens
}

func (r *response) settled() bool { return r.completed || r.failed }

// feed consumes relayed bytes and reports whether they completed the final
// response head and whether they held the first entity-body byte. headOnly
// stops after the head: those bytes never reached the client.
func (r *response) feed(p []byte, headOnly bool) (headNow, bodyNow bool) {
	for len(p) > 0 && !r.settled() {
		if !r.headDone {
			p = p[r.feedHead(p):]
			headNow = headNow || r.headDone
			continue
		}
		if headOnly {
			break
		}
		before := r.sawBody
		r.feedBody(p)
		p = nil
		bodyNow = !before && r.sawBody
	}
	return headNow, bodyNow
}

func (r *response) feedHead(p []byte) int {
	start := max(len(r.head)-3, 0)
	r.head = append(r.head, p...)
	idx := bytes.Index(r.head[start:], []byte("\r\n\r\n"))
	if idx < 0 {
		if len(r.head) > maxHeadBytes {
			r.failed = true
			r.head = nil
		}
		return len(p)
	}
	end := start + idx + 4
	consumed := len(p) - (len(r.head) - end)
	head := string(r.head[:end-4])
	r.head = nil
	r.parseHead(head)
	return consumed
}

// parseHead reads a status line and headers. Interim 1xx heads are skipped.
func (r *response) parseHead(head string) {
	lines := strings.Split(head, "\r\n")
	status, ok := parseStatusLine(lines[0])
	if !ok {
		r.failed = true
		return
	}
	if status < 200 {
		return
	}
	r.status = status
	r.headDone = true

	var transferCodings []string
	contentLength := int64(-1)
	for _, line := range lines[1:] {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name, value := trimOWS(line[:colon]), trimOWS(line[colon+1:])
		switch strings.ToLower(name) {
		case "content-length":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 || (contentLength >= 0 && n != contentLength) {
				r.failed = true
				return
			}
			contentLength = n
		case "transfer-encoding":
			for _, coding := range strings.Split(value, ",") {
				if coding = strings.ToLower(trimOWS(coding)); coding != "" {
					transferCodings = append(transferCodings, coding)
				}
			}
		case "content-encoding":
			for _, coding := range strings.Split(value, ",") {
				if coding = strings.ToLower(trimOWS(coding)); coding != "" && coding != "identity" {
					r.opaque = true
				}
			}
		case "content-type":
			mediaType, _, _ := strings.Cut(value, ";")
			r.sse = strings.EqualFold(trimOWS(mediaType), "text/event-stream")
		case "x-cpa-trace-id":
			if r.trace == "" {
				r.trace = value
			}
		}
	}

	switch {
	case status == 204 || status == 304:
		r.framing = framingNone
	case len(transferCodings) > 0:
		r.framing = framingClose
		if transferCodings[len(transferCodings)-1] == "chunked" {
			r.framing = framingChunked
		}
		if len(transferCodings) > 1 || r.framing != framingChunked {
			r.opaque = true
		}
	case contentLength >= 0:
		r.framing = framingLength
		r.remaining = contentLength
	default:
		r.framing = framingClose
	}
	if r.framing == framingNone || (r.framing == framingLength && r.remaining == 0) {
		r.framingEnded()
	}
}

func parseStatusLine(line string) (int, bool) {
	if !strings.HasPrefix(line, "HTTP/") {
		return 0, false
	}
	_, rest, ok := strings.Cut(line, " ")
	if !ok || len(rest) < 3 || (len(rest) > 3 && rest[3] != ' ') || !isDigits(rest[:3], 3) {
		return 0, false
	}
	status, _ := strconv.Atoi(rest[:3])
	if status < 100 || status > 599 {
		return 0, false
	}
	return status, true
}

func (r *response) feedBody(p []byte) {
	switch r.framing {
	case framingLength:
		n := min(int64(len(p)), r.remaining)
		r.entity(p[:n])
		r.remaining -= n
		if r.remaining == 0 {
			r.framingEnded()
		}
	case framingClose:
		r.entity(p)
	case framingChunked:
		r.feedChunked(p)
	}
}

func (r *response) feedChunked(p []byte) {
	for len(p) > 0 && !r.settled() {
		if r.chunk == chunkData {
			n := min(int64(len(p)), r.remaining)
			r.entity(p[:n])
			p = p[n:]
			r.remaining -= n
			if r.remaining == 0 {
				r.chunk = chunkDataEnd
			}
			continue
		}
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			r.line = append(r.line, p...)
			if len(r.line) > maxHeadBytes {
				r.failed = true
			}
			return
		}
		r.line = append(r.line, p[:i]...)
		p = p[i+1:]
		line := strings.TrimSuffix(string(r.line), "\r")
		r.line = r.line[:0]
		switch r.chunk {
		case chunkSize:
			size, ok := parseChunkSize(line)
			if !ok {
				r.failed = true
				return
			}
			if size == 0 {
				r.chunk = chunkTrailer
			} else {
				r.remaining = size
				r.chunk = chunkData
			}
		case chunkDataEnd:
			if line != "" {
				r.failed = true
				return
			}
			r.chunk = chunkSize
		case chunkTrailer:
			if line == "" {
				r.framingEnded()
				return
			}
		}
	}
}

func parseChunkSize(line string) (int64, bool) {
	size, _, _ := strings.Cut(line, ";")
	size = trimOWS(size)
	if size == "" || len(size) > 15 {
		return 0, false
	}
	for i := 0; i < len(size); i++ {
		c := size[i] | 0x20
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(size, 16, 64)
	return n, err == nil
}

// entity receives decoded entity-body bytes.
func (r *response) entity(b []byte) {
	if len(b) == 0 {
		return
	}
	r.sawBody = true
	switch {
	case r.opaque:
	case r.sse:
		r.scanSSE(b)
	case !r.bodyOverflow:
		if len(r.body)+len(b) > maxJSONBodyBytes {
			r.bodyOverflow = true
			r.body = nil
			return
		}
		r.body = append(r.body, b...)
	}
}

// framingEnded marks the HTTP framing complete and reads a non-stream body.
func (r *response) framingEnded() {
	if r.settled() {
		return
	}
	r.completed = true
	if r.sse || r.opaque || r.bodyOverflow || len(r.body) == 0 {
		return
	}
	var obj map[string]json.RawMessage
	err := json.Unmarshal(r.body, &obj)
	r.body = nil
	if err != nil {
		return
	}
	r.noteModel(obj, nil)
	switch {
	case jsonStringOr(obj["type"]) == "message":
		r.tokens = anthropicTokens(objectOf(obj["usage"]))
	case jsonStringOr(obj["object"]) == "response":
		r.tokens = responsesTokens(obj["usage"])
	case isChatObject(obj):
		r.tokens = chatTokens(obj["usage"])
	}
}

func (r *response) scanSSE(b []byte) {
	for len(b) > 0 && !r.settled() {
		i := bytes.IndexByte(b, '\n')
		seg := b
		if i >= 0 {
			seg = b[:i]
		}
		if !r.sseLineLong {
			if len(r.sseLine)+len(seg) > maxSSELineBytes {
				r.sseLineLong = true
				r.sseLine = r.sseLine[:0]
			} else {
				r.sseLine = append(r.sseLine, seg...)
			}
		}
		if i < 0 {
			return
		}
		b = b[i+1:]
		if r.sseLineLong {
			r.sseLineLong = false
			r.sseBroken = true
		} else {
			r.sseFieldLine(bytes.TrimSuffix(r.sseLine, []byte("\r")))
		}
		r.sseLine = r.sseLine[:0]
	}
}

func (r *response) sseFieldLine(line []byte) {
	if len(line) == 0 {
		if r.sseHasData && !r.sseBroken {
			r.sseEvent(r.sseData)
		}
		r.sseData = r.sseData[:0]
		r.sseHasData = false
		r.sseBroken = false
		return
	}
	name, value, _ := bytes.Cut(line, []byte(":"))
	if string(name) != "data" {
		return
	}
	value = bytes.TrimPrefix(value, []byte(" "))
	if len(r.sseData)+len(value)+1 > maxSSELineBytes {
		r.sseBroken = true
		return
	}
	if r.sseHasData {
		r.sseData = append(r.sseData, '\n')
	}
	r.sseData = append(r.sseData, value...)
	r.sseHasData = true
}

// sseEvent applies one dispatched SSE data payload.
func (r *response) sseEvent(data []byte) {
	if string(data) == "[DONE]" {
		r.tokens = nil
		if r.chatUsage != nil {
			r.tokens = chatTokens(r.chatUsage)
		}
		r.completed = true
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return
	}
	eventType := jsonStringOr(obj["type"])
	switch eventType {
	case "message_start":
		message := objectOf(obj["message"])
		r.noteModel(obj, message)
		r.anthropicUsage = objectOf(message["usage"])
	case "message_delta":
		usage := objectOf(obj["usage"])
		if usage == nil {
			return
		}
		if r.anthropicUsage == nil {
			r.anthropicUsage = map[string]json.RawMessage{}
		}
		for k, v := range usage {
			r.anthropicUsage[k] = v
		}
		if _, ok := usage["output_tokens"]; ok {
			r.tokens = anthropicTokens(r.anthropicUsage)
		}
	case "message_stop":
		r.completed = true
	case "response.completed", "response.incomplete":
		inner := objectOf(obj["response"])
		r.noteModel(obj, inner)
		r.tokens = responsesTokens(inner["usage"])
		r.completed = true
	case "error", "response.failed":
		r.failed = true
	default:
		if strings.HasPrefix(eventType, "response.") {
			r.noteModel(obj, objectOf(obj["response"]))
			return
		}
		if eventType == "" && len(obj["error"]) > 0 && string(obj["error"]) != "null" {
			r.failed = true
			return
		}
		r.noteModel(obj, nil)
		if isChatObject(obj) && len(obj["usage"]) > 0 && string(obj["usage"]) != "null" {
			r.chatUsage = obj["usage"]
		}
	}
}

// noteModel keeps the last explicit nonempty model the response reports.
func (r *response) noteModel(obj, inner map[string]json.RawMessage) {
	if m := jsonStringOr(inner["model"]); m != "" {
		r.model = m
		return
	}
	if m := jsonStringOr(obj["model"]); m != "" {
		r.model = m
	}
}

func isChatObject(obj map[string]json.RawMessage) bool {
	object := jsonStringOr(obj["object"])
	if strings.HasPrefix(object, "chat.completion") || object == "text_completion" {
		return true
	}
	_, hasChoices := obj["choices"]
	return object == "" && hasChoices
}

func jsonStringOr(raw json.RawMessage) string {
	s, _ := jsonString(raw)
	return s
}
