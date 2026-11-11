package journal

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// longContextBody is a chat request of about size bytes: many messages of
// prose with escapes, the shape of a long-context coding session.
func longContextBody(size int) string {
	var b strings.Builder
	b.WriteString(`{"model":"claude-sonnet-4-5","prompt_cache_key":"session-0001","metadata":{"user_id":"user_1_acc__session_01a06a06-e830-7da9-a866-98470a94389c"},"messages":[`)
	text := strings.Repeat(`func main() {\n\tfmt.Println(\"hello, world\")\n}\n// caf\u00e9 \"quoted\" text with ordinary words in it. `, 40)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		fmt.Fprintf(&b, `{"role":%q,"content":[{"type":"text","text":"%s"}]}`, role, text)
	}
	b.WriteString(`],"max_tokens":4096,"stream":true}`)
	return b.String()
}

// numberHeavyBody is a request of about size bytes that is mostly numbers.
func numberHeavyBody(size int) string {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-6-luna","input":[`)
	for i := 0; b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d.%06d,-%de-3", i%977, (i*7919)%1000000, i%89)
	}
	b.WriteString(`]}`)
	return b.String()
}

var requestBodyShapes = []struct {
	name string
	body func() string
}{
	{"chat_100KB", func() string { return longContextBody(100 << 10) }},
	{"chat_1MB", func() string { return longContextBody(1 << 20) }},
	{"chat_5MB", func() string { return longContextBody(5 << 20) }},
	{"chat_20MB", func() string { return longContextBody(20 << 20) }},
	{"numbers_5MB", func() string { return numberHeavyBody(5 << 20) }},
}

// BenchmarkRelayPathRequestBody measures the journal work the relay
// goroutine does for one request: Begin before forwarding and finalization
// after the response. Completing the record happens off the relay and is
// left out of the timing.
func BenchmarkRelayPathRequestBody(b *testing.B) {
	for _, shape := range requestBodyShapes {
		ctx := requestFor("/v1/messages", shape.body())
		b.Run(shape.name, func(b *testing.B) {
			j := testJournal(b)
			b.SetBytes(int64(len(ctx.Body)))
			for range b.N {
				o := j.begin(ctx, testStart)
				fin, _ := o.finishAt(EndEOF, at(100))
				b.StopTimer()
				fin.record(nil)
				b.StartTimer()
			}
		})
	}
}

// BenchmarkRequestFields measures parsing the request body for the record's
// request fields, which happens off the relay.
func BenchmarkRequestFields(b *testing.B) {
	for _, shape := range requestBodyShapes {
		ctx := requestFor("/v1/messages", shape.body())
		b.Run(shape.name, func(b *testing.B) {
			j := testJournal(b)
			b.SetBytes(int64(len(ctx.Body)))
			for b.Loop() {
				fin, _ := j.begin(ctx, testStart).finishAt(EndEOF, at(100))
				fin.record(nil)
			}
		})
	}
}

// The relay's share of journal work must not grow with the request body:
// a 20 MB body that takes tens of milliseconds to parse must not delay
// forwarding or finalization.
func TestRelayPathDoesNotParseTheRequestBody(t *testing.T) {
	ctx := requestFor("/v1/messages", numberHeavyBody(20<<20))
	j := testJournal(t)
	fastest := time.Hour
	for range 3 {
		start := time.Now()
		o := j.begin(ctx, testStart)
		fin, _ := o.finishAt(EndEOF, at(100))
		fastest = min(fastest, time.Since(start))
		if rec, _ := fin.record(nil); deref(rec.RequestedModel) != "gpt-6-luna" || rec.Provider != "codex" {
			t.Fatalf("request fields: model %q provider %s", deref(rec.RequestedModel), rec.Provider)
		}
	}
	if fastest > 10*time.Millisecond {
		t.Fatalf("Begin and finalization took %v on the relay for a 20 MB body", fastest)
	}
}
