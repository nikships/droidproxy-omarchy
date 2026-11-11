package journal

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// deliver relays one buffer that the client write accepted in full.
func deliver(o *Observation, p []byte, readAt, writeAt time.Time) (Record, bool) {
	return o.deliverAt(p, readAt, writeAt)
}

// failWrite relays one buffer whose client write accepted only its first
// accepted bytes and then failed.
func failWrite(o *Observation, p []byte, accepted int, readAt, writeAt time.Time) (Record, bool) {
	return o.writeFailedAt(p, accepted, readAt, writeAt)
}

var testStart = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return testStart.Add(time.Duration(ms) * time.Millisecond) }

const testTrace = "20261111100000-0123456789abcdef-019a7140-0000-7000-8000-000000000001"

func testJournal(t testing.TB) *Journal {
	t.Helper()
	return &Journal{enabled: func() bool { return true }, seats: newSeatIndex(t.TempDir())}
}

func requestFor(path, body string) RequestContext {
	return RequestContext{
		Method:        "POST",
		Path:          path,
		ForwardedPath: path,
		Head:          "POST " + path + " HTTP/1.1\r\nHost: localhost:8317",
		Body:          body,
	}
}

var (
	messagesRequest = requestFor("/v1/messages", `{"model":"claude-haiku-4-5-20251001"}`)
	chatRequest     = requestFor("/v1/chat/completions", `{"model":"gpt-6-luna"}`)
)

// observeReads relays reads delivered in full (reads at 20 ms, writes at
// 100 ms) and then ends with end at 200 ms unless a write finalized first.
func observeReads(j *Journal, ctx RequestContext, reads [][]byte, end End) Record {
	o := j.begin(ctx, testStart)
	for _, p := range reads {
		if len(p) == 0 {
			continue
		}
		if rec, ok := deliver(o, p, at(20), at(100)); ok {
			return rec
		}
	}
	rec, _ := o.finishAt(end, at(200))
	return rec
}

// observeEverySplit relays wire whole, at every two-read split, and one byte
// per read, requires the same record each time, and returns it.
func observeEverySplit(t testing.TB, ctx RequestContext, wire string, end End) Record {
	t.Helper()
	j := testJournal(t)
	b := []byte(wire)
	whole := observeReads(j, ctx, [][]byte{b}, end)
	want := comparable(whole)
	check := func(label string, reads [][]byte) {
		if got := comparable(observeReads(j, ctx, reads, end)); got != want {
			t.Fatalf("%s:\n got %s\nwant %s", label, got, want)
		}
	}
	step := 1
	if len(b) > 64<<10 {
		step = 509
	}
	for k := 0; k <= len(b); k += step {
		check(fmt.Sprintf("split %d", k), [][]byte{b[:k], b[k:]})
	}
	bytewise := make([][]byte, len(b))
	for i := range b {
		bytewise[i] = b[i : i+1]
	}
	check("bytewise", bytewise)
	return whole
}

// comparable encodes rec with a minted execution_id replaced, so records of
// the same response compare equal.
func comparable(rec Record) string {
	if rec.ExecutionID != "" && uuidV4Pattern.MatchString(rec.ExecutionID) {
		rec.ExecutionID = "minted"
	}
	line, err := encodeLine(rec)
	if err != nil {
		panic(err)
	}
	return string(line)
}

func jsonResponse(head, body string) string {
	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n%sContent-Length: %d\r\n\r\n%s", head, len(body), body)
}

func chunkedSSE(head string, events ...string) string {
	var b strings.Builder
	b.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n" + head + "Transfer-Encoding: chunked\r\n\r\n")
	for _, e := range events {
		fmt.Fprintf(&b, "%x\r\n%s\r\n", len(e), e)
	}
	b.WriteString("0\r\n\r\n")
	return b.String()
}

const anthropicMessage = `{"type":"message","model":"claude-haiku-4-5-20251001","usage":{"input_tokens":10,"output_tokens":4}}`

func tokensString(tk *Tokens) string {
	if tk == nil {
		return "null"
	}
	return fmt.Sprintf("%d/%d/%s/%d/%s", tk.InputUncached, tk.CacheRead, intOrNull(tk.CacheCreation), tk.Output, intOrNull(tk.Reasoning))
}

func intOrNull(v *int64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(*v)
}

func statusOrNull(v *int) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(*v)
}

func TestStatusLineGrammar(t *testing.T) {
	for _, c := range []struct {
		line, status string
	}{
		{"HTTP/1.1 200 OK", "200"},
		{"HTTP/1.1 200", "200"},
		{"HTTP/1.1 200 ", "200"},
		{"HTTP/1.0 404 Not Found", "404"},
		{"HTTP/1.1 599 Whatever reason", "599"},
		{"HTTP/1.1 20", "null"},
		{"HTTP/2 200 OK", "null"},
		{"HTTP/1.2 200 OK", "null"},
		{"http/1.1 200 OK", "null"},
		{"HTTP/1.1 200OK", "null"},
		{"HTTP/1.1  200 OK", "null"},
		{"HTTP/1.1 999 Custom", "null"},
		{"HTTP/1.1 099 Low", "null"},
		{"HTTP/1.1 2x0 OK", "null"},
	} {
		rec := observeEverySplit(t, messagesRequest, c.line+"\r\nContent-Length: 0\r\n\r\n", EndEOF)
		wantOutcome := outcomeComplete
		if c.status == "null" {
			wantOutcome = outcomeUpstreamError
		}
		if got := statusOrNull(rec.Status); got != c.status || rec.Outcome != wantOutcome {
			t.Errorf("%q: status %s outcome %s, want %s %s", c.line, got, rec.Outcome, c.status, wantOutcome)
		}
	}
}

func TestMalformedFramingIsUpstreamError(t *testing.T) {
	usageChunk := `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":5}}` + "\n\n"
	for _, c := range []struct {
		name, wire string
		ttft       bool
	}{
		{"header line without colon", "HTTP/1.1 200 OK\r\nBroken\r\nContent-Length: 0\r\n\r\n", false},
		{"empty header name", "HTTP/1.1 200 OK\r\n: value\r\nContent-Length: 0\r\n\r\n", false},
		{"header name with space", "HTTP/1.1 200 OK\r\nBad Name: value\r\nContent-Length: 0\r\n\r\n", false},
		{"chunk data ends in bare LF", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\n0\r\n\r\n", true},
		{"chunk data followed by other bytes", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhelloXY\r\n0\r\n\r\n", true},
		{"last chunk line ends in bare LF", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\n\r\n", true},
		{"usage published before the violation", "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n" +
			fmt.Sprintf("%x\r\n%s\r\n", len(usageChunk), usageChunk) + "e\ndata: [DONE]\n\n\r\n0\r\n\r\n", true},
	} {
		rec := observeEverySplit(t, chatRequest, c.wire, EndEOF)
		if rec.Outcome != outcomeUpstreamError || statusOrNull(rec.Status) != "200" || rec.Tokens != nil || (rec.TTFTMS != nil) != c.ttft {
			t.Errorf("%s: outcome %s status %s tokens %s ttft %v", c.name, rec.Outcome, statusOrNull(rec.Status), tokensString(rec.Tokens), rec.TTFTMS)
		}
	}
	// Bytes after valid chunk data are fine when they are exactly CRLF.
	rec := observeEverySplit(t, chatRequest, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5;ext=1\r\nhello\r\n0\r\nTrailer: x\r\n\r\n", EndEOF)
	if rec.Outcome != outcomeComplete {
		t.Errorf("valid chunked body: outcome %s", rec.Outcome)
	}
}

func padHead(total int) string {
	prefix := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " + fmt.Sprint(len(anthropicMessage)) + "\r\nX-Pad: "
	return prefix + strings.Repeat("a", total-len(prefix)-4) + "\r\n\r\n"
}

func TestHeadCapCountsFromTheFinalHead(t *testing.T) {
	atCap := observeEverySplit(t, messagesRequest, padHead(64<<10)+anthropicMessage, EndEOF)
	if atCap.Outcome != outcomeComplete || tokensString(atCap.Tokens) != "10/0/0/4/null" {
		t.Errorf("head of exactly 64 KiB: outcome %s tokens %s", atCap.Outcome, tokensString(atCap.Tokens))
	}
	interim := observeEverySplit(t, messagesRequest, "HTTP/1.1 100 Continue\r\nX-Pad: "+strings.Repeat("b", 60<<10)+"\r\n\r\n"+padHead(64<<10)+anthropicMessage, EndEOF)
	if interim.Outcome != outcomeComplete || interim.Tokens == nil {
		t.Errorf("interim head before a 64 KiB head: outcome %s tokens %s", interim.Outcome, tokensString(interim.Tokens))
	}
	over := observeEverySplit(t, messagesRequest, padHead(64<<10+1)+anthropicMessage, EndEOF)
	if over.Outcome != outcomeUpstreamError || statusOrNull(over.Status) != "200" || over.Tokens != nil || over.ResponseModel != nil || over.TTFTMS != nil {
		t.Errorf("head one byte over: outcome %s status %s tokens %s model %v ttft %v",
			over.Outcome, statusOrNull(over.Status), tokensString(over.Tokens), over.ResponseModel, over.TTFTMS)
	}
}

func TestInterimHeadsThenEOF(t *testing.T) {
	rec := observeEverySplit(t, messagesRequest, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: x\r\n\r\n", EndEOF)
	if rec.Outcome != outcomeUpstreamClosed || rec.Status != nil {
		t.Errorf("101 then EOF: outcome %s status %s", rec.Outcome, statusOrNull(rec.Status))
	}
}

func TestUndeliveredBytesTeachOnlyStatusAndTrace(t *testing.T) {
	head := "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nX-CPA-TRACE-ID: " + testTrace + "\r\nContent-Type: application/json\r\nContent-Le"
	o := testJournal(t).begin(messagesRequest, testStart)
	rec, ok := failWrite(o, []byte(head), 5, at(20), at(100))
	if !ok || rec.Outcome != outcomeClientDisconnected || statusOrNull(rec.Status) != "200" || rec.TraceID == nil || *rec.TraceID != testTrace ||
		rec.ExecutionID != "019a7140-0000-7000-8000-000000000001" || rec.Seat == nil || *rec.Seat != (Seat{AuthIndex: "0123456789abcdef", Role: roleServed}) || rec.TTFTMS != nil {
		t.Errorf("head cut in undelivered bytes: %s", comparable(rec))
	}

	wire := jsonResponse("X-CPA-TRACE-ID: "+testTrace+"\r\n", anthropicMessage)
	bodyAt := strings.Index(wire, "\r\n\r\n") + 4
	for _, accepted := range []int{0, 10, bodyAt, len(wire) - 1} {
		o := testJournal(t).begin(messagesRequest, testStart)
		rec, _ := failWrite(o, []byte(wire), accepted, at(20), at(100))
		if rec.Outcome != outcomeClientDisconnected || statusOrNull(rec.Status) != "200" || rec.TraceID == nil || rec.Tokens != nil || rec.ResponseModel != nil {
			t.Errorf("accepted %d of a whole response: %s", accepted, comparable(rec))
		}
		if wantTTFT := accepted > bodyAt; (rec.TTFTMS != nil) != wantTTFT {
			t.Errorf("accepted %d: ttft %v", accepted, rec.TTFTMS)
		}
	}

	o = testJournal(t).begin(messagesRequest, testStart)
	rec, _ = failWrite(o, []byte("HTTP/1.1 204 No Content\r\n\r\n"), 0, at(20), at(100))
	if rec.Outcome != outcomeClientDisconnected || statusOrNull(rec.Status) != "204" {
		t.Errorf("undelivered head that would complete framing: %s", comparable(rec))
	}
}

func TestDeliveredPrefixOfFailedWriteKeepsUsage(t *testing.T) {
	usage := `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":5}}` + "\n\n"
	wire := chunkedSSE("", usage, "data: [DONE]\n\n")
	cut := strings.Index(wire, "e\r\ndata: [DONE]")
	o := testJournal(t).begin(chatRequest, testStart)
	rec, _ := failWrite(o, []byte(wire), cut, at(20), at(100))
	if rec.Outcome != outcomeClientDisconnected || tokensString(rec.Tokens) != "9/0/null/5/null" || rec.TTFTMS == nil {
		t.Errorf("record %s", comparable(rec))
	}
}

func TestExecutionIDFromThirdTraceSegment(t *testing.T) {
	for _, c := range []struct {
		trace, executionID string
		seat               bool
	}{
		{testTrace, "019a7140-0000-7000-8000-000000000001", true},
		{"2026111110000-0123456789abcdef-019a7140-0000-7000-8000-000000000002", "019a7140-0000-7000-8000-000000000002", false},
		{"x-y-019a7140-0000-7000-8000-000000000003", "019a7140-0000-7000-8000-000000000003", false},
		{"20261111100000-0123456789abcdef-not-a-uuid", "", false},
		{"20261111100000-0123456789abcdef", "", false},
	} {
		rec := observeEverySplit(t, messagesRequest, jsonResponse("X-CPA-TRACE-ID: "+c.trace+"\r\n", anthropicMessage), EndEOF)
		if c.executionID == "" {
			if !uuidV4Pattern.MatchString(rec.ExecutionID) {
				t.Errorf("%s: execution_id %s is not minted", c.trace, rec.ExecutionID)
			}
		} else if rec.ExecutionID != c.executionID {
			t.Errorf("%s: execution_id %s", c.trace, rec.ExecutionID)
		}
		if (rec.Seat != nil) != c.seat || rec.TraceID == nil || *rec.TraceID != c.trace {
			t.Errorf("%s: seat %v trace %v", c.trace, rec.Seat, rec.TraceID)
		}
	}
}

func TestChatUsagePublishedByQualifyingChunk(t *testing.T) {
	chunk := func(choices, usage string) string {
		return `data: {"object":"chat.completion.chunk","model":"gpt-6-luna","choices":` + choices + `,"usage":` + usage + "}\n\n"
	}
	stop := `[{"index":0,"delta":{},"finish_reason":"stop"}]`
	for _, c := range []struct {
		name, body, tokens, outcome string
	}{
		{"finish reason then EOF", chunk(stop, `{"prompt_tokens":9,"completion_tokens":5}`), "9/0/null/5/null", outcomeUpstreamClosed},
		{"empty choices then EOF", chunk(`[]`, `{"prompt_tokens":9,"completion_tokens":5}`), "9/0/null/5/null", outcomeUpstreamClosed},
		{"later qualifying chunk replaces", chunk(stop, `{"prompt_tokens":9,"completion_tokens":5}`) + chunk(`[]`, `{"prompt_tokens":20,"completion_tokens":6}`) + "data: [DONE]\n\n", "20/0/null/6/null", outcomeComplete},
		{"non-string finish reason", chunk(`[{"index":0,"finish_reason":1}]`, `{"prompt_tokens":9,"completion_tokens":5}`) + "data: [DONE]\n\n", "null", outcomeComplete},
		{"usage without finish reason", chunk(`[{"index":0,"finish_reason":null}]`, `{"prompt_tokens":9,"completion_tokens":5}`) + "data: [DONE]\n\n", "null", outcomeComplete},
	} {
		wire := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" + c.body
		rec := observeEverySplit(t, chatRequest, wire, EndEOF)
		if tokensString(rec.Tokens) != c.tokens || rec.Outcome != c.outcome {
			t.Errorf("%s: tokens %s outcome %s, want %s %s", c.name, tokensString(rec.Tokens), rec.Outcome, c.tokens, c.outcome)
		}
	}
}

func TestProtocolErrors(t *testing.T) {
	for _, c := range []struct {
		event   string
		isError bool
	}{
		{"event: error\ndata: {\"message\":\"overloaded\"}\n\n", true},
		{"event: error\ndata: not json\n\n", true},
		{"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n", true},
		{"data: {\"type\":\"response.failed\",\"response\":{}}\n\n", true},
		{"data: {\"type\":\"response.error\"}\n\n", true},
		{"data: {\"error\":{\"message\":\"x\"}}\n\n", true},
		{"data: {\"error\":null,\"choices\":[]}\n\n", false},
		{"data: {\"type\":\"ping\",\"error\":{\"message\":\"x\"}}\n\n", false},
		{"data: {\"type\":null,\"error\":{\"message\":\"x\"}}\n\n", false},
		{"event: ping\ndata: {\"error\":{\"message\":\"x\"},}\n\n", false},
	} {
		wire := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" + c.event
		rec := observeEverySplit(t, chatRequest, wire, EndEOF)
		want := outcomeUpstreamClosed
		if c.isError {
			want = outcomeUpstreamError
		}
		if rec.Outcome != want {
			t.Errorf("%q: outcome %s, want %s", c.event, rec.Outcome, want)
		}
	}
	rec := observeEverySplit(t, messagesRequest, jsonResponse("", `{"type":"error","error":{"type":"rate_limit_error"}}`), EndEOF)
	if rec.Outcome != outcomeComplete {
		t.Errorf("non-stream error body: outcome %s", rec.Outcome)
	}
}

func TestCompletionOnlyByPayloadTypeOrDone(t *testing.T) {
	for _, c := range []struct {
		event, outcome string
	}{
		{"event: message_stop\ndata: {\"type\":\"ping\"}\n\n", outcomeUpstreamClosed},
		{"data: [DONE] \n\n", outcomeUpstreamClosed},
		{"data: {\"type\":\"message_stop\"}\n\n", outcomeComplete},
		{"data: {\"type\":\"response.incomplete\",\"response\":{}}\n\n", outcomeComplete},
		{"data: [DONE]\n\n", outcomeComplete},
		{"data: [DONE]\n", outcomeUpstreamClosed},
	} {
		wire := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" + c.event
		if rec := observeEverySplit(t, chatRequest, wire, EndEOF); rec.Outcome != c.outcome {
			t.Errorf("%q: outcome %s, want %s", c.event, rec.Outcome, c.outcome)
		}
	}
}

// eventOfSize builds one chat SSE event whose assembled data is exactly n
// bytes, spread over data lines that are each under the line cap.
func eventOfSize(n int, prompt int) string {
	head := fmt.Sprintf(`{"object":"chat.completion.chunk","model":"inside-cap","choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":5},"pad":[`, prompt)
	lines := []string{head}
	size := len(head)
	const piece = 200 << 10
	for size+1+piece+3 < n {
		lines = append(lines, `"`+strings.Repeat("p", piece)+`",`)
		size += 1 + piece + 3
	}
	last := n - size - 1 - 4
	lines = append(lines, `"`+strings.Repeat("q", last)+`"]}`)
	return "data: " + strings.Join(lines, "\ndata: ") + "\n\n"
}

func TestSSEEventDataCap(t *testing.T) {
	const capBytes = 2 << 20
	if got := len(strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(eventOfSize(capBytes, 9), "data: "), "\n\n"), "\ndata: ", "\n")); got != capBytes {
		t.Fatalf("event data is %d bytes", got)
	}
	head := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n"
	// The later event carries no usage, so its model shows that scanning
	// resumed without replacing usage.
	later := `data: {"object":"chat.completion.chunk","model":"after-cap","choices":[{"index":0,"delta":{}}]}` + "\n\n"
	for _, c := range []struct {
		size   int
		tokens string
	}{
		{capBytes, "9/0/null/5/null"},
		{capBytes + 1, "null"},
	} {
		wire := []byte(head + eventOfSize(c.size, 9) + later + "data: [DONE]\n\n")
		j := testJournal(t)
		reads := [][][]byte{{wire}, {wire[:len(head)+7], wire[len(head)+7:]}, {wire[:len(wire)/2], wire[len(wire)/2:]}}
		bytewise := make([][]byte, len(wire))
		for i := range wire {
			bytewise[i] = wire[i : i+1]
		}
		reads = append(reads, bytewise)
		for i, r := range reads {
			rec := observeReads(j, chatRequest, r, EndEOF)
			if tokensString(rec.Tokens) != c.tokens || rec.Outcome != outcomeComplete || deref(rec.ResponseModel) != "after-cap" {
				t.Errorf("event of %d bytes, read pattern %d: tokens %s outcome %s model %q, want %s", c.size, i, tokensString(rec.Tokens), rec.Outcome, deref(rec.ResponseModel), c.tokens)
			}
		}
	}
}

func TestNonStreamEnvelopeFollowsForwardedPath(t *testing.T) {
	chatBody := `{"object":"chat.completion","model":"m","usage":{"prompt_tokens":9,"completion_tokens":5}}`
	for _, c := range []struct {
		forwarded, body, tokens string
	}{
		{"/v1/messages", anthropicMessage, "10/0/0/4/null"},
		{"/v1/chat/completions", anthropicMessage, "null"},
		{"/v1/chat/completions", chatBody, "9/0/null/5/null"},
		{"/v1/completions", chatBody, "null"},
		{"/v1/completions", `{"object":"text_completion","usage":{"prompt_tokens":3,"completion_tokens":2}}`, "3/0/null/2/null"},
		{"/v1/responses", `{"object":"response","usage":{"input_tokens":7,"output_tokens":1}}`, "7/0/null/1/null"},
		{"/backend-api/codex/responses", `{"object":"response","usage":{"input_tokens":7,"output_tokens":1}}`, "7/0/null/1/null"},
		{"/v1/responses/compact", `{"object":"response","usage":{"input_tokens":7,"output_tokens":1}}`, "null"},
		{"/v1/messages", chatBody, "null"},
	} {
		ctx := requestFor("/v1/responses", `{"model":"x"}`)
		ctx.ForwardedPath = c.forwarded
		rec := observeEverySplit(t, ctx, jsonResponse("", c.body), EndEOF)
		if tokensString(rec.Tokens) != c.tokens || rec.Outcome != outcomeComplete {
			t.Errorf("%s with %s: tokens %s outcome %s, want %s", c.forwarded, c.body, tokensString(rec.Tokens), rec.Outcome, c.tokens)
		}
	}
}

func TestTransferAndContentCodings(t *testing.T) {
	for _, c := range []struct {
		name, head, body, outcome, tokens string
	}{
		{"chunked over two header lines after gzip", "Transfer-Encoding: gzip\r\nTransfer-Encoding: Chunked\r\n", "5\r\nhello\r\n0\r\n\r\n", outcomeComplete, "null"},
		{"chunked not last is close-delimited", "Transfer-Encoding: chunked, gzip\r\n", "\x1f\x8bopaque", outcomeComplete, "null"},
		{"identity content coding", "Content-Encoding: identity\r\nContent-Length: " + fmt.Sprint(len(anthropicMessage)) + "\r\n", anthropicMessage, outcomeComplete, "10/0/0/4/null"},
		{"br content coding", "Content-Encoding: br\r\nContent-Length: " + fmt.Sprint(len(anthropicMessage)) + "\r\n", anthropicMessage, outcomeComplete, "null"},
		{"close-delimited JSON", "", anthropicMessage, outcomeComplete, "10/0/0/4/null"},
	} {
		wire := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n" + c.head + "\r\n" + c.body
		rec := observeEverySplit(t, messagesRequest, wire, EndEOF)
		if rec.Outcome != c.outcome || tokensString(rec.Tokens) != c.tokens {
			t.Errorf("%s: outcome %s tokens %s, want %s %s", c.name, rec.Outcome, tokensString(rec.Tokens), c.outcome, c.tokens)
		}
	}
}

func TestStrictJSON(t *testing.T) {
	for _, c := range []struct {
		body, tokens, model string
	}{
		{`{"type":"message","model":"a","model":"b","usage":{"input_tokens":1,"output_tokens":1},"usage":{"input_tokens":2,"output_tokens":3}}`, "2/0/0/3/null", "b"},
		{`{"type":"message","model":"a","usage":{"input_tokens":1,"output_tokens":NaN}}`, "null", ""},
		{`{"type":"message","model":"a",/*c*/"usage":{"input_tokens":1,"output_tokens":1}}`, "null", ""},
		{`{"type":"message","model":"a","usage":{"input_tokens":1,"output_tokens":1}} trailing`, "null", ""},
	} {
		rec := observeEverySplit(t, messagesRequest, jsonResponse("", c.body), EndEOF)
		model := ""
		if rec.ResponseModel != nil {
			model = *rec.ResponseModel
		}
		if tokensString(rec.Tokens) != c.tokens || model != c.model {
			t.Errorf("%s: tokens %s model %q", c.body, tokensString(rec.Tokens), model)
		}
	}
	for _, c := range []struct {
		body, model, session string
	}{
		{`{"model":"claude-a","prompt_cache_key":"session-0001","model":"gpt-b"}`, "gpt-b", "session-0001"},
		{`{"model":"claude-a","prompt_cache_key":"session-0001",}`, "", ""},
		{`{"model":"claude-a"} {"x":1}`, "", ""},
	} {
		o := testJournal(t).begin(requestFor("/v1/responses", c.body), testStart)
		if deref(o.model) != c.model || deref(o.session) != c.session {
			t.Errorf("%s: model %q session %q", c.body, deref(o.model), deref(o.session))
		}
	}
}
