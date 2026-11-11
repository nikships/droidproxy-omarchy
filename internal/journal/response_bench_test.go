package journal

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// proseText is about n bytes of JSON-escaped model output, never more.
func proseText(n int) string {
	unit := `Here is the change:\n\n` + "```go\\nfunc main() {\\n\\tfmt.Println(\\\"hello\\\")\\n}\\n```" + `\nThe caf\u00e9 test now passes with ordinary words in it. `
	return strings.Repeat(unit, n/len(unit))
}

// splitReads cuts each wire piece into reads of at most size bytes.
func splitReads(size int, pieces ...string) [][]byte {
	var reads [][]byte
	for _, piece := range pieces {
		for len(piece) > 0 {
			n := min(size, len(piece))
			reads = append(reads, []byte(piece[:n]))
			piece = piece[n:]
		}
	}
	return reads
}

// chunkedSSEReads frames each event as its own chunk and read, cut into
// reads of at most 32 KiB.
func chunkedSSEReads(events []string) [][]byte {
	pieces := []string{"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n"}
	for _, e := range events {
		pieces = append(pieces, fmt.Sprintf("%x\r\n%s\r\n", len(e), e))
	}
	return splitReads(32<<10, append(pieces, "0\r\n\r\n")...)
}

func sseEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func messagesJSONReads() [][]byte {
	prefix := `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"`
	suffix := `"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"cache_read_input_tokens":20,"output_tokens":4}}`
	body := prefix + proseText(maxJSONBodyBytes-len(prefix)-len(suffix)) + suffix
	return splitReads(32<<10, jsonResponse("", body))
}

func responsesSSEReads(textBytes int) [][]byte {
	text := proseText(textBytes)
	item := `{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[],"text":"` + text + `"}]}`
	events := []string{sseEvent("response.created", `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","model":"gpt-6-luna","output":[]}}`)}
	for i := 1; i <= 200; i++ {
		events = append(events, sseEvent("response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","sequence_number":%d,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"some words "}`, i)))
	}
	events = append(events,
		sseEvent("response.output_text.done", `{"type":"response.output_text.done","sequence_number":201,"item_id":"msg_1","output_index":0,"content_index":0,"text":"`+text+`"}`),
		sseEvent("response.output_item.done", `{"type":"response.output_item.done","sequence_number":202,"output_index":0,"item":`+item+`}`),
		sseEvent("response.completed", `{"type":"response.completed","sequence_number":203,"response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-6-luna","output":[`+item+`],"usage":{"input_tokens":300,"input_tokens_details":{"cached_tokens":256},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":2}}}}`))
	return chunkedSSEReads(events)
}

func anthropicSSEReads(deltas int) [][]byte {
	events := []string{
		sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"stop_reason":null,"usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":0,"output_tokens":1}}}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	}
	for range deltas {
		events = append(events, sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"some more words "}}`))
	}
	events = append(events,
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2000}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`))
	return chunkedSSEReads(events)
}

var responseShapes = []struct {
	name   string
	ctx    RequestContext
	reads  func() [][]byte
	tokens string
}{
	{"messages_json_2MiB", messagesRequest, messagesJSONReads, "10/20/0/4/null"},
	// Each event's data is one line, as CLIProxyAPI sends it.
	{"responses_sse_256KiB", requestFor("/v1/responses", `{"model":"gpt-6-luna"}`), func() [][]byte { return responsesSSEReads(254 << 10) }, "44/256/null/7/2"},
	{"responses_sse_1MiB", requestFor("/v1/responses", `{"model":"gpt-6-luna"}`), func() [][]byte { return responsesSSEReads(1 << 20) }, "44/256/null/7/2"},
	{"anthropic_sse_2000_deltas", messagesRequest, func() [][]byte { return anthropicSSEReads(2000) }, "10/20/0/2000/null"},
}

// deliverAll relays reads as Delivered does, until one finalizes, and keeps
// the slowest single call in worst when worst is not nil.
func deliverAll(o *Observation, reads [][]byte, worst *time.Duration) (pending, bool) {
	for _, p := range reads {
		start := time.Now()
		fin, ok := o.deliverAt(p, start, time.Now())
		if worst != nil {
			*worst = max(*worst, time.Since(start))
		}
		if ok {
			return fin, true
		}
	}
	return pending{}, false
}

// BenchmarkDelivered measures the journal work Delivered does on the relay
// goroutine for one whole response. Begin is left out of the timing.
func BenchmarkDelivered(b *testing.B) {
	for _, shape := range responseShapes {
		reads := shape.reads()
		b.Run(shape.name, func(b *testing.B) {
			j := testJournal(b)
			fin, ok := deliverAll(j.begin(shape.ctx, testStart), reads, nil)
			if rec, _ := fin.record(nil); !ok || tokensString(rec.Tokens) != shape.tokens || rec.Outcome != outcomeComplete {
				b.Fatalf("finalized %v outcome %s tokens %s, want %s", ok, rec.Outcome, tokensString(rec.Tokens), shape.tokens)
			}
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				o := j.begin(shape.ctx, testStart)
				b.StartTimer()
				deliverAll(o, reads, nil)
			}
			b.StopTimer()
			worst := make([]time.Duration, 50)
			for i := range worst {
				deliverAll(j.begin(shape.ctx, testStart), reads, &worst[i])
			}
			slices.Sort(worst)
			b.ReportMetric(float64(worst[len(worst)/2].Microseconds()), "median-worst-µs")
			b.ReportMetric(float64(worst[len(worst)-1].Microseconds()), "max-worst-µs")
		})
	}
}
