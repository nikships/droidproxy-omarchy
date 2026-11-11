package contract

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

type goldenCase struct {
	Name   string          `json:"name"`
	Valid  *bool           `json:"valid"`
	Reason string          `json:"reason,omitempty"`
	Record json.RawMessage `json:"record"`
}

func readGoldens(t *testing.T) map[string]goldenCase {
	t.Helper()
	data, err := os.ReadFile("request-journal-v1.golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatal("golden JSONL must end with LF")
	}
	cases := make(map[string]goldenCase)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || !bytes.Equal(line, bytes.TrimSpace(line)) {
			t.Fatal("golden lines must be nonempty, without surrounding whitespace")
		}
		var entry goldenCase
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			t.Fatalf("expected exactly one JSON object per line, got %v", err)
		}
		if entry.Name == "" || entry.Valid == nil || len(entry.Record) == 0 || entry.Record[0] != '{' {
			t.Fatal("golden requires name, explicit validity, and object record")
		}
		if !*entry.Valid && entry.Reason == "" {
			t.Fatalf("%s: invalid golden requires a reason", entry.Name)
		}
		if _, exists := cases[entry.Name]; exists {
			t.Fatalf("duplicate golden name %q", entry.Name)
		}
		cases[entry.Name] = entry
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("empty golden file")
	}
	return cases
}

func TestContractStructure(t *testing.T) {
	data, err := os.ReadFile("request-journal-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatal("schema must declare draft 2020-12")
	}
	valid, invalid := 0, 0
	for _, entry := range readGoldens(t) {
		if *entry.Valid {
			valid++
		} else {
			invalid++
		}
	}
	if valid == 0 || invalid == 0 {
		t.Fatal("goldens must include both valid and invalid records")
	}
	t.Logf("%d valid and %d invalid wrappers; record schema validation is separately required via uvx check-jsonschema", valid, invalid)
}

func TestTranscriptFixtures(t *testing.T) {
	goldens := readGoldens(t)
	data, err := os.ReadFile("request-journal-v1.transcripts.json")
	if err != nil {
		t.Fatal(err)
	}
	var transcripts struct {
		V     int `json:"v"`
		Cases []struct {
			Name    string `json:"name"`
			Request *struct {
				Method   string         `json:"method"`
				Endpoint string         `json:"endpoint"`
				Headers  map[string]any `json:"headers"`
				Body     map[string]any `json:"body"`
			} `json:"request"`
			Response      string          `json:"response"`
			BodyReadError bool            `json:"body_read_error"`
			Expect        json.RawMessage `json:"expect"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &transcripts); err != nil {
		t.Fatal(err)
	}
	if transcripts.V != 1 || len(transcripts.Cases) == 0 {
		t.Fatal("expected v1 transcripts")
	}
	seen := make(map[string]bool)
	t.Cleanup(func() {
		for _, name := range []string{
			"anthropic_sse_merge_usage",
			"chat_sse_cached_exceeds_prompt",
			"session_metadata_json",
			"session_metadata_json_loses_to_header",
			"session_metadata_json_malformed",
			"session_metadata_json_bad_grammar",
			"session_metadata_legacy_suffix",
		} {
			if !seen[name] {
				t.Errorf("required transcript %q is missing", name)
			}
		}
	})
	for _, entry := range transcripts.Cases {
		t.Run(entry.Name, func(t *testing.T) {
			if seen[entry.Name] {
				t.Fatal("duplicate transcript name")
			}
			seen[entry.Name] = true
			golden, ok := goldens[entry.Name]
			if !ok || !*golden.Valid {
				t.Fatal("transcript must have a valid golden record")
			}
			var expected, record any
			if err := json.Unmarshal(entry.Expect, &expected); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(golden.Record, &record); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(expected, record) {
				t.Fatal("transcript expected record differs from its golden")
			}
			if entry.Request == nil || entry.Request.Method != "POST" || entry.Request.Headers == nil || entry.Request.Body == nil {
				t.Fatal("transcript requires a POST request with headers and body objects")
			}
			if endpoint := expected.(map[string]any)["endpoint"]; endpoint != entry.Request.Endpoint {
				t.Fatalf("request endpoint %q differs from expected endpoint %v", entry.Request.Endpoint, endpoint)
			}
			wire := make([]byte, 0, len(entry.Response))
			for _, r := range entry.Response {
				if r > 255 {
					t.Fatal("response contains a non-Latin-1 code point")
				}
				wire = append(wire, byte(r))
			}
			response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(wire)), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			_, err = io.ReadAll(response.Body)
			if entry.BodyReadError {
				if err != io.ErrUnexpectedEOF {
					t.Fatalf("truncated fixture: got %v, want unexpected EOF", err)
				}
			} else if err != nil {
				t.Fatalf("invalid HTTP fixture framing: %v", err)
			}
			if !strings.Contains(entry.Response, "\r\n\r\n") {
				t.Fatal("HTTP fixture lacks CRLF head separator")
			}
		})
	}
}
