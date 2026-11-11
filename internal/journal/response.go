package journal

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Observation limits. They bound memory only; the relay never waits on them.
const (
	maxHeadBytes      = 64 << 10
	maxChunkLineBytes = 64 << 10
	maxSSELineBytes   = 256 << 10
	maxSSEEventBytes  = 2 << 20
	maxJSONBodyBytes  = 2 << 20
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

// envelope is the non-stream JSON body shape that carries usage for the
// path forwarded to CLIProxyAPI.
type envelope int

const (
	envelopeNone envelope = iota
	envelopeAnthropic
	envelopeResponses
	envelopeChat
	envelopeText
)

func envelopeFor(forwardedPath string) envelope {
	switch forwardedPath {
	case "/v1/messages":
		return envelopeAnthropic
	case "/v1/responses", "/backend-api/codex/responses":
		return envelopeResponses
	case "/v1/chat/completions":
		return envelopeChat
	case "/v1/completions":
		return envelopeText
	}
	return envelopeNone
}

// response follows the bytes the client received from CLIProxyAPI: HTTP
// head, entity framing, and the SSE or JSON body that carries usage.
type response struct {
	envelope envelope

	// line holds a partial head line, chunk-size or trailer line, or the
	// CRLF expected after chunk data.
	line      []byte
	headBytes int
	headLines int
	interim   bool
	headDone  bool
	status    int
	trace     string
	traceSeen bool

	contentLength   int64
	hasLength       bool
	transferCodings []string
	sse             bool
	opaque          bool

	framing   framing
	remaining int64
	chunk     chunkState

	sawBody bool

	// completed: protocol or HTTP framing completion. protocolError: an SSE
	// error event. Either one finalizes the record at the write that
	// delivered it. malformed: a framing violation or a head over the cap;
	// observation stops and the outcome is upstream_error at finalization.
	completed     bool
	protocolError bool
	malformed     bool

	body         []byte
	bodyOverflow bool

	sseLine      []byte
	sseLineLong  bool
	sseData      []byte
	sseHasData   bool
	sseEventName string
	sseDiscard   bool

	model             string
	anthropicUsage    map[string]json.RawMessage
	anthropicTerminal bool
	chatPublished     bool
	tokens            *Tokens
}

func (r *response) settled() bool { return r.completed || r.protocolError }

func (r *response) stopped() bool { return r.settled() || r.malformed }

// feed observes delivered bytes in wire order and stops at the first point
// that settles or stops the observation. It reports whether the bytes
// completed the X-CPA-TRACE-ID header line and whether they held the first
// entity-body byte.
func (r *response) feed(p []byte) (traceNow, bodyNow bool) {
	for len(p) > 0 && !r.stopped() {
		if !r.headDone {
			n, t := r.feedHead(p, false)
			traceNow = traceNow || t
			p = p[n:]
			continue
		}
		before := r.sawBody
		r.feedBody(p)
		bodyNow = !before && r.sawBody
		break
	}
	return traceNow, bodyNow
}

// learnHead reads bytes the client never received for the final status line
// and the X-CPA-TRACE-ID header only, while the head is incomplete. It never
// marks the response malformed and never starts the entity.
func (r *response) learnHead(p []byte) (traceNow bool) {
	if r.headDone || r.stopped() {
		return false
	}
	_, traceNow = r.feedHead(p, true)
	return traceNow
}

// feedHead consumes head bytes line by line. Lines end in CRLF; the head
// ends at an empty line. A head that reaches maxHeadBytes, counted from its
// first byte, without its terminator is over the cap at that byte.
func (r *response) feedHead(p []byte, learn bool) (n int, traceNow bool) {
	for n < len(p) {
		chunk := p[n:]
		if room := maxHeadBytes - r.headBytes; len(chunk) > room {
			chunk = chunk[:room]
		}
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			r.line = append(r.line, chunk...)
			r.headBytes += len(chunk)
			n += len(chunk)
		} else {
			r.line = append(r.line, chunk[:i+1]...)
			r.headBytes += i + 1
			n += i + 1
			if !bytes.HasSuffix(r.line, []byte("\r\n")) {
				r.malformed = r.malformed || !learn
				return n, traceNow
			}
			ok, t := r.headLine(r.line[:len(r.line)-2], learn)
			r.line = r.line[:0]
			traceNow = traceNow || t
			if !ok || r.headDone {
				return n, traceNow
			}
		}
		if r.headBytes >= maxHeadBytes {
			r.malformed = r.malformed || !learn
			return n, traceNow
		}
	}
	return n, traceNow
}

// headLine applies one complete head line and reports whether to continue.
func (r *response) headLine(line []byte, learn bool) (ok, traceNow bool) {
	r.headLines++
	if r.headLines == 1 {
		status, valid := parseStatusLine(line)
		if !valid {
			r.malformed = r.malformed || !learn
			return false, false
		}
		r.interim = status < 200
		if !r.interim {
			r.status = status
		}
		return true, false
	}
	if len(line) == 0 {
		if r.interim {
			r.nextHead()
			return true, false
		}
		if learn {
			return false, false
		}
		r.headDone = true
		r.startEntity()
		return true, false
	}
	name, value, valid := splitHeader(line)
	if !valid {
		r.malformed = r.malformed || !learn
		return false, false
	}
	if r.interim {
		return true, false
	}
	if strings.EqualFold(name, "X-CPA-TRACE-ID") {
		if r.traceSeen {
			return true, false
		}
		r.traceSeen, r.trace = true, value
		return true, value != ""
	}
	if learn {
		return true, false
	}
	switch strings.ToLower(name) {
	case "content-length":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 || !isDigits(value, len(value)) || (r.hasLength && n != r.contentLength) {
			r.malformed = true
			return false, false
		}
		r.contentLength, r.hasLength = n, true
	case "transfer-encoding":
		for _, coding := range strings.Split(value, ",") {
			if coding = strings.ToLower(trimOWS(coding)); coding != "" {
				r.transferCodings = append(r.transferCodings, coding)
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
	}
	return true, false
}

// nextHead starts the head that follows an interim 1xx head.
func (r *response) nextHead() {
	r.headBytes, r.headLines, r.interim = 0, 0, false
}

func (r *response) startEntity() {
	switch {
	case r.status == 204 || r.status == 304:
		r.framing = framingNone
	case len(r.transferCodings) > 0:
		r.framing = framingClose
		if r.transferCodings[len(r.transferCodings)-1] == "chunked" {
			r.framing = framingChunked
		}
		for _, coding := range r.transferCodings {
			if coding != "chunked" {
				r.opaque = true
			}
		}
	case r.hasLength:
		r.framing = framingLength
		r.remaining = r.contentLength
	default:
		r.framing = framingClose
	}
	if r.framing == framingNone || (r.framing == framingLength && r.remaining == 0) {
		r.framingEnded()
	}
}

// parseStatusLine accepts "HTTP/1.0" or "HTTP/1.1", a space, a status from
// 100 through 599, and optionally a space and any reason text.
func parseStatusLine(line []byte) (int, bool) {
	s := string(line)
	if !strings.HasPrefix(s, "HTTP/1.1 ") && !strings.HasPrefix(s, "HTTP/1.0 ") {
		return 0, false
	}
	rest := s[len("HTTP/1.1 "):]
	if len(rest) < 3 || !isDigits(rest[:3], 3) || (len(rest) > 3 && rest[3] != ' ') {
		return 0, false
	}
	status, _ := strconv.Atoi(rest[:3])
	if status < 100 || status > 599 {
		return 0, false
	}
	return status, true
}

// splitHeader splits a header line at its first colon. The name must be a
// nonempty HTTP token.
func splitHeader(line []byte) (name, value string, ok bool) {
	colon := bytes.IndexByte(line, ':')
	if colon <= 0 {
		return "", "", false
	}
	for _, c := range line[:colon] {
		if !isTokenChar(c) {
			return "", "", false
		}
	}
	return string(line[:colon]), trimOWS(string(line[colon+1:])), true
}

func isTokenChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

func (r *response) feedBody(p []byte) {
	switch r.framing {
	case framingLength:
		n := min(int64(len(p)), r.remaining)
		r.entity(p[:n])
		if r.stopped() {
			return
		}
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
	for len(p) > 0 && !r.stopped() {
		switch r.chunk {
		case chunkData:
			n := min(int64(len(p)), r.remaining)
			r.entity(p[:n])
			p = p[n:]
			r.remaining -= n
			if r.remaining == 0 {
				r.chunk = chunkDataEnd
			}
		case chunkDataEnd:
			if p[0] != "\r\n"[len(r.line)] {
				r.malformed = true
				return
			}
			r.line = append(r.line, p[0])
			p = p[1:]
			if len(r.line) == 2 {
				r.line = r.line[:0]
				r.chunk = chunkSize
			}
		default:
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				r.line = append(r.line, p...)
				if len(r.line) > maxChunkLineBytes {
					r.malformed = true
				}
				return
			}
			r.line = append(r.line, p[:i]...)
			p = p[i+1:]
			line, crlf := bytes.CutSuffix(r.line, []byte("\r"))
			if r.chunk == chunkTrailer {
				ended := len(line) == 0
				_, _, valid := splitHeader(line)
				r.line = r.line[:0]
				switch {
				case !crlf || (!ended && !valid):
					r.malformed = true
					return
				case ended:
					r.framingEnded()
					return
				}
				continue
			}
			size, ok := parseChunkSize(string(line))
			r.line = r.line[:0]
			if !crlf || !ok {
				r.malformed = true
				return
			}
			if size == 0 {
				r.chunk = chunkTrailer
			} else {
				r.remaining = size
				r.chunk = chunkData
			}
		}
	}
}

// parseChunkSize reads a chunk-size line without its CRLF: one or more hex
// digits, then optionally ";" and an extension. A size too large for int64
// is malformed.
func parseChunkSize(line string) (int64, bool) {
	size, _, _ := strings.Cut(line, ";")
	if size == "" {
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
	if r.stopped() {
		return
	}
	r.completed = true
	if r.sse || r.opaque || r.bodyOverflow {
		return
	}
	obj := strictObject(r.body)
	r.body = nil
	if obj == nil {
		return
	}
	r.noteModel(obj)
	switch {
	case r.envelope == envelopeAnthropic && jsonStringOr(obj["type"]) == "message":
		r.tokens = anthropicTokens(objectOf(obj["usage"]))
	case r.envelope == envelopeResponses && jsonStringOr(obj["object"]) == "response":
		r.tokens = responsesTokens(obj["usage"])
	case r.envelope == envelopeChat && jsonStringOr(obj["object"]) == "chat.completion",
		r.envelope == envelopeText && jsonStringOr(obj["object"]) == "text_completion":
		r.tokens = chatTokens(obj["usage"])
	}
}

// scanSSE splits entity bytes into SSE lines. A line longer than the cap,
// not counting its CR or LF, discards the event it belongs to.
func (r *response) scanSSE(b []byte) {
	for len(b) > 0 && !r.stopped() {
		i := bytes.IndexByte(b, '\n')
		seg := b
		if i >= 0 {
			seg = b[:i]
		}
		if !r.sseLineLong {
			// One byte of room for a CR that may turn out to end the line.
			if len(r.sseLine)+len(seg) > maxSSELineBytes+1 {
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
		line := bytes.TrimSuffix(r.sseLine, []byte("\r"))
		if r.sseLineLong || len(line) > maxSSELineBytes {
			r.discardEvent()
		} else {
			r.sseFieldLine(line)
		}
		r.sseLineLong = false
		r.sseLine = r.sseLine[:0]
	}
}

// discardEvent drops the current event; lines are ignored until the blank
// line that ends it.
func (r *response) discardEvent() {
	r.sseDiscard = true
	r.sseData = r.sseData[:0]
	r.sseHasData = false
	r.sseEventName = ""
}

func (r *response) sseFieldLine(line []byte) {
	if len(line) == 0 {
		if r.sseHasData && !r.sseDiscard {
			r.sseDispatch()
		}
		r.sseData = r.sseData[:0]
		r.sseHasData = false
		r.sseEventName = ""
		r.sseDiscard = false
		return
	}
	if r.sseDiscard {
		return
	}
	name, value, _ := bytes.Cut(line, []byte(":"))
	value = bytes.TrimPrefix(value, []byte(" "))
	switch string(name) {
	case "data":
		size := len(r.sseData) + len(value)
		if r.sseHasData {
			size++
		}
		if size > maxSSEEventBytes {
			r.discardEvent()
			return
		}
		if r.sseHasData {
			r.sseData = append(r.sseData, '\n')
		}
		r.sseData = append(r.sseData, value...)
		r.sseHasData = true
	case "event":
		r.sseEventName = string(value)
	}
}

// sseDispatch applies one dispatched SSE event.
func (r *response) sseDispatch() {
	done := string(r.sseData) == "[DONE]"
	var obj map[string]json.RawMessage
	if !done {
		obj = strictObject(r.sseData)
	}
	if obj != nil {
		r.noteModel(obj)
	}
	switch {
	case r.sseEventName == "error":
		r.protocolError = true
		return
	case done:
		r.completed = true
		return
	case obj == nil:
		return
	}
	rawType, hasType := obj["type"]
	switch jsonStringOr(rawType) {
	case "error", "response.failed", "response.error":
		r.protocolError = true
	case "message_start":
		r.mergeAnthropicUsage(objectOf(objectOf(obj["message"])["usage"]), false)
	case "message_delta":
		usage := objectOf(obj["usage"])
		_, carriesOutput := usage["output_tokens"]
		r.mergeAnthropicUsage(usage, carriesOutput)
	case "message_stop":
		r.completed = true
	case "response.completed", "response.incomplete":
		r.tokens = responsesTokens(objectOf(obj["response"])["usage"])
		r.completed = true
	default:
		if !hasType && isPresent(obj["error"]) {
			r.protocolError = true
			return
		}
		r.chatChunk(obj)
	}
}

// mergeAnthropicUsage merges usage members field by field, later values
// replacing earlier ones. Once a message_delta has carried output_tokens,
// every merge re-normalizes the terminal usage.
func (r *response) mergeAnthropicUsage(usage map[string]json.RawMessage, carriesOutput bool) {
	if usage == nil {
		return
	}
	if r.anthropicUsage == nil {
		r.anthropicUsage = map[string]json.RawMessage{}
	}
	for k, v := range usage {
		r.anthropicUsage[k] = v
	}
	r.anthropicTerminal = r.anthropicTerminal || carriesOutput
	if r.anthropicTerminal {
		r.tokens = anthropicTokens(r.anthropicUsage)
	}
}

// chatChunk publishes chat/text completions usage from a chunk that has a
// non-null string finish_reason or empty choices; once published, any later
// chunk with non-null usage replaces it, whatever its choices.
func (r *response) chatChunk(obj map[string]json.RawMessage) {
	if !isPresent(obj["usage"]) {
		return
	}
	if r.chatPublished {
		r.tokens = chatTokens(obj["usage"])
		return
	}
	raw := obj["choices"]
	var choices []map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &choices) != nil {
		return
	}
	qualifies := len(choices) == 0
	for _, choice := range choices {
		if _, ok := jsonString(choice["finish_reason"]); ok {
			qualifies = true
		}
	}
	if qualifies {
		r.tokens = chatTokens(obj["usage"])
		r.chatPublished = true
	}
}

// noteModel keeps the last explicit nonempty model a payload reports, at
// top-level model, message.model, or response.model in that order.
func (r *response) noteModel(obj map[string]json.RawMessage) {
	for _, m := range []string{
		jsonStringOr(obj["model"]),
		jsonStringOr(objectOf(obj["message"])["model"]),
		jsonStringOr(objectOf(obj["response"])["model"]),
	} {
		if m != "" {
			r.model = m
		}
	}
}

// maxJSONDepth is the deepest nesting of arrays and objects the journal
// accepts as strict JSON; the outermost value is depth 1.
const maxJSONDepth = 512

// strictObject parses data as one strict JSON object nested at most
// maxJSONDepth deep. Repeated members keep their last value.
func strictObject(data []byte) map[string]json.RawMessage {
	var obj map[string]json.RawMessage
	if !nestedWithin(data, maxJSONDepth) || json.Unmarshal(data, &obj) != nil {
		return nil
	}
	return obj
}

// nestedWithin reports whether arrays and objects in data, outside strings,
// nest at most limit deep. It does not validate data.
func nestedWithin(data []byte, limit int) bool {
	depth := 0
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '"':
			for i++; i < len(data) && data[i] != '"'; i++ {
				if data[i] == '\\' {
					i++
				}
			}
		case '{', '[':
			if depth++; depth > limit {
				return false
			}
		case '}', ']':
			depth--
		}
	}
	return true
}

func isPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func jsonStringOr(raw json.RawMessage) string {
	s, _ := jsonString(raw)
	return s
}
