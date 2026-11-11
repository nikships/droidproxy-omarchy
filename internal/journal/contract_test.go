package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

const contractDir = "../../contract"

type transcriptCase struct {
	Name    string `json:"name"`
	Request struct {
		Method   string            `json:"method"`
		Endpoint string            `json:"endpoint"`
		Headers  map[string]string `json:"headers"`
		Body     map[string]any    `json:"body"`
	} `json:"request"`
	Response    string `json:"response"`
	Observation struct {
		StartedAt          string `json:"started_at"`
		EndedAt            string `json:"ended_at"`
		FirstBodyElapsedMS int64  `json:"first_body_elapsed_ms"`
		EndSignal          string `json:"end_signal"`
	} `json:"observation"`
	SeatSnapshot           []map[string]string `json:"seat_snapshot"`
	NondeterministicFields []string            `json:"nondeterministic_fields"`
	Expect                 json.RawMessage     `json:"expect"`
}

func loadTranscripts(t *testing.T) []transcriptCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(contractDir, "request-journal-v1.transcripts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []transcriptCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file.Cases
}

type goldenLine struct {
	Name   string          `json:"name"`
	Valid  bool            `json:"valid"`
	Record json.RawMessage `json:"record"`
}

func loadGoldens(t *testing.T) []goldenLine {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(contractDir, "request-journal-v1.golden.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []goldenLine
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var g goldenLine
		if err := json.Unmarshal(scanner.Bytes(), &g); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, g)
	}
	return lines
}

func goldenRecord(t *testing.T, name string) map[string]any {
	t.Helper()
	for _, g := range loadGoldens(t) {
		if g.Name == name {
			return decodeObject(t, g.Record)
		}
	}
	t.Fatalf("no golden %q", name)
	return nil
}

func decodeObject(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// latin1 maps each code point 0-255 of a transcript string back to one byte.
func latin1(t *testing.T, s string) []byte {
	t.Helper()
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 255 {
			t.Fatalf("code point %U is not Latin-1", r)
		}
		out = append(out, byte(r))
	}
	return out
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// contractJournal serves the fixture seats from synthetic auth files. The
// fixture auth indexes are redacted, so each is pointed at the entry the
// real directory scan produced for its file.
func contractJournal(t *testing.T, seats []map[string]string) *Journal {
	t.Helper()
	dir := t.TempDir()
	types := map[string]string{}
	for _, s := range seats {
		content := map[string]string{"email": s["email"], "access_token": "synthetic-not-a-token"}
		switch {
		case s["org"] != "":
			content["type"], content["organization_uuid"] = "claude", s["org"]
		case s["account_id"] != "":
			content["type"], content["account_id"] = "codex", s["account_id"]
		default:
			t.Fatalf("seat %v has no provider identity", s)
		}
		types[s["file"]] = content["type"]
		data, _ := json.Marshal(content)
		if err := os.WriteFile(filepath.Join(dir, s["file"]), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	index := newSeatIndex(dir)
	index.mu.Lock()
	index.refreshLocked(time.Now())
	abs, _ := filepath.Abs(dir)
	for _, s := range seats {
		scanned := index.byIndex[authIndex(types[s["file"]], filepath.Join(abs, s["file"]))]
		if len(scanned) != 1 {
			t.Fatalf("seat file %s was not indexed", s["file"])
		}
		index.byIndex[s["auth_index"]] = scanned
	}
	index.mu.Unlock()
	return &Journal{enabled: func() bool { return true }, seats: index}
}

func transcriptContext(t *testing.T, c transcriptCase) RequestContext {
	t.Helper()
	var head strings.Builder
	fmt.Fprintf(&head, "%s %s HTTP/1.1\r\nHost: localhost:8317", c.Request.Method, c.Request.Endpoint)
	for name, value := range c.Request.Headers {
		fmt.Fprintf(&head, "\r\n%s: %s", name, value)
	}
	body, err := json.Marshal(c.Request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return RequestContext{
		Method:        c.Request.Method,
		Path:          c.Request.Endpoint,
		ForwardedPath: c.Request.Endpoint,
		Head:          head.String(),
		Body:          string(body),
	}
}

// replay feeds chunks at the fixture's first-body time and finalizes at its
// end time with the fixture's end signal.
func replay(t *testing.T, j *Journal, c transcriptCase, chunks [][]byte) Record {
	t.Helper()
	if c.Observation.EndSignal != "eof" {
		t.Fatalf("unsupported end signal %q", c.Observation.EndSignal)
	}
	start := mustTime(t, c.Observation.StartedAt)
	firstBody := start.Add(time.Duration(c.Observation.FirstBodyElapsedMS) * time.Millisecond)
	o := j.begin(transcriptContext(t, c), start)
	for _, chunk := range chunks {
		if len(chunk) > 0 {
			o.feedAt(chunk, false, firstBody)
		}
	}
	rec, ok := o.finishAt(EndEOF, mustTime(t, c.Observation.EndedAt))
	if !ok {
		t.Fatal("observation did not finalize")
	}
	return rec
}

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// compareRecord checks rec against expect. A minted execution_id is only
// checked for UUIDv4 form.
func compareRecord(t *testing.T, label string, rec Record, expect map[string]any, minted bool) {
	t.Helper()
	got := recordObject(t, rec)
	if minted {
		id, _ := got["execution_id"].(string)
		if !uuidV4Pattern.MatchString(id) {
			t.Fatalf("%s: execution_id %q is not a UUIDv4", label, id)
		}
		got["execution_id"] = expect["execution_id"]
	}
	if !reflect.DeepEqual(got, expect) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(expect)
		t.Fatalf("%s:\n got %s\nwant %s", label, gotJSON, wantJSON)
	}
}

func recordObject(t *testing.T, rec Record) map[string]any {
	t.Helper()
	line, err := encodeLine(rec)
	if err != nil {
		t.Fatal(err)
	}
	return decodeObject(t, line)
}

func TestTranscriptsReplayAtEverySplit(t *testing.T) {
	schema := loadSchema(t)
	for _, c := range loadTranscripts(t) {
		t.Run(c.Name, func(t *testing.T) {
			j := contractJournal(t, c.SeatSnapshot)
			wire := latin1(t, c.Response)
			expect := decodeObject(t, c.Expect)
			minted := len(c.NondeterministicFields) > 0
			if minted && !reflect.DeepEqual(c.NondeterministicFields, []string{"execution_id"}) {
				t.Fatalf("unexpected nondeterministic fields %v", c.NondeterministicFields)
			}

			whole := replay(t, j, c, [][]byte{wire})
			if err := schema.validateRecord(recordObject(t, whole)); err != nil {
				t.Fatalf("record fails the schema: %v", err)
			}
			for k := 0; k <= len(wire); k++ {
				compareRecord(t, fmt.Sprintf("split %d", k), replay(t, j, c, [][]byte{wire[:k], wire[k:]}), expect, minted)
			}
			bytewise := make([][]byte, len(wire))
			for i := range wire {
				bytewise[i] = wire[i : i+1]
			}
			compareRecord(t, "bytewise", replay(t, j, c, bytewise), expect, minted)
		})
	}
}

// The goldens without a transcript cover exits that carry no response bytes
// or a request rewrite; they are produced here through the same code path.
func TestGoldenExitPaths(t *testing.T) {
	schema := loadSchema(t)
	cases := loadTranscripts(t)
	byName := map[string]transcriptCase{}
	for _, c := range cases {
		byName[c.Name] = c
	}
	start := mustTime(t, "2026-11-11T10:00:00.000Z")
	at := func(ms int) time.Time { return start.Add(time.Duration(ms) * time.Millisecond) }
	messages := RequestContext{
		Method:        "POST",
		Path:          "/v1/messages",
		ForwardedPath: "/v1/messages",
		Head:          "POST /v1/messages HTTP/1.1\r\nHost: localhost:8317",
		Body:          `{"model":"claude-haiku-4-5-20251001","max_tokens":1}`,
	}
	anthropic := byName["anthropic_sse"]

	t.Run("backend_unreachable_502", func(t *testing.T) {
		o := contractJournal(t, nil).begin(messages, start)
		rec, _ := o.finishAt(EndBackendUnreachable, at(3))
		check(t, schema, "backend_unreachable_502", rec, true)
	})
	t.Run("upstream_error_before_status", func(t *testing.T) {
		o := contractJournal(t, nil).begin(messages, start)
		rec, _ := o.finishAt(EndSendFailed, at(100))
		check(t, schema, "upstream_error_before_status", rec, true)
	})
	t.Run("client_disconnected_before_terminal_usage", func(t *testing.T) {
		wire := latin1(t, anthropic.Response)
		cut := bytes.Index(wire, []byte("136\r\nevent: message_delta"))
		if cut < 0 {
			t.Fatal("message_delta chunk not found")
		}
		o := contractJournal(t, anthropic.SeatSnapshot).begin(messages, start)
		o.feedAt(wire[:cut], false, at(20))
		o.feedAt(wire[cut:], true, at(20))
		rec, _ := o.finishAt(EndClientWriteFailed, at(100))
		check(t, schema, "client_disconnected_before_terminal_usage", rec, false)
	})
	t.Run("gemini_path_rewrite", func(t *testing.T) {
		ctx := RequestContext{
			Method:        "POST",
			Path:          "/v1/responses?x=1",
			ForwardedPath: "/v1/chat/completions?x=1",
			Head:          "POST /v1/responses?x=1 HTTP/1.1\r\nHost: localhost:8317",
			Body:          `{"model":"gemini-3.1-pro-preview","input":[]}`,
		}
		events := "data: {\"object\":\"chat.completion.chunk\",\"model\":\"gemini-3.1-pro-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		wire := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(events), events)
		o := contractJournal(t, nil).begin(ctx, start)
		o.feedAt([]byte(wire), false, at(20))
		rec, _ := o.finishAt(EndEOF, at(100))
		check(t, schema, "gemini_path_rewrite", rec, true)
	})
}

func check(t *testing.T, schema *jsonSchema, name string, rec Record, minted bool) {
	t.Helper()
	if err := schema.validateRecord(recordObject(t, rec)); err != nil {
		t.Fatalf("record fails the schema: %v", err)
	}
	compareRecord(t, name, rec, goldenRecord(t, name), minted)
}

// TestGoldenRecordsRoundTrip decodes every valid golden record into Record
// and requires the encoder to reproduce it byte for byte.
func TestGoldenRecordsRoundTrip(t *testing.T) {
	for _, g := range loadGoldens(t) {
		if !g.Valid {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(g.Record))
		dec.DisallowUnknownFields()
		var rec Record
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("%s: %v", g.Name, err)
		}
		line, err := encodeLine(rec)
		if err != nil {
			t.Fatal(err)
		}
		if got := bytes.TrimSuffix(line, []byte("\n")); !bytes.Equal(got, g.Record) {
			t.Fatalf("%s:\n got %s\nwant %s", g.Name, got, g.Record)
		}
	}
}

// TestSchemaValidatorMatchesGoldens guards the in-test validator: it must
// accept every valid golden and reject every invalid one.
func TestSchemaValidatorMatchesGoldens(t *testing.T) {
	schema := loadSchema(t)
	for _, g := range loadGoldens(t) {
		err := schema.validateRecord(decodeObject(t, g.Record))
		if g.Valid && err != nil {
			t.Errorf("%s: valid golden rejected: %v", g.Name, err)
		}
		if !g.Valid && err == nil {
			t.Errorf("%s: invalid golden accepted", g.Name)
		}
	}
}
