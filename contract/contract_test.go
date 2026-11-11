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
	"time"
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
	t.Logf("%d valid and %d invalid wrappers; records are validated against the schema by TestSchemaValidatorMatchesGoldens in internal/journal", valid, invalid)
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
				BodyRaw  *string        `json:"body_raw"`
			} `json:"request"`
			Observation     *transcriptObservation `json:"observation"`
			Response        string                 `json:"response"`
			BodyReadError   bool                   `json:"body_read_error"`
			NonstandardWire string                 `json:"nonstandard_wire"`
			Expect          json.RawMessage        `json:"expect"`
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
			"chat_sse_bytes_after_done",
			"partial_head_after_429_status",
			"head_over_64k",
			"header_line_without_colon",
			"chunk_size_line_bare_lf",
			"transfer_encoding_gzip_chunked",
			"sse_event_error_field",
			"chat_sse_untyped_top_level_error",
			"chat_sse_null_optional_counters",
			"anthropic_json_unrecognized_envelope",
			"anthropic_json_trailing_comma",
			"request_body_trailing_comma",
			"sse_overlong_line_skipped",
			"sse_multiline_event_over_line_cap",
			"chat_sse_finish_usage_then_eof",
			"anthropic_sse_late_cache_delta",
			"trace_index_uppercase",
			"anthropic_sse_delta_model",
			"anthropic_json_max_counter",
			"anthropic_json_fractional_counter",
			"client_write_fails_before_done",
			"client_write_fails_on_last_byte",
			"status_line_without_reason",
			"status_line_reason_without_space",
			"chat_cache_write_null_uses_cached_creation",
			"chat_null_finish_usage_then_eof",
			"request_body_duplicate_model",
			"response_model_three_locations",
			"status_code_out_of_range",
			"close_delimited_sse_usage_then_eof",
			"event_message_stop_with_ping_payload",
			"sse_line_exactly_at_cap",
			"sse_line_one_over_cap",
			"header_line_bare_lf",
			"status_line_bare_lf",
			"trailer_line_without_colon",
			"trailer_line_bare_lf",
			"chunk_size_line_padded",
			"trace_header_repeated_first_wins",
			"trace_header_malformed_then_valid",
			"request_body_depth_512",
			"request_body_depth_513",
			"response_body_depth_512",
			"response_body_depth_513",
			"head_exactly_64k_unterminated",
			"trace_header_empty_then_valid",
			"anthropic_sse_on_chat_completions",
			"chat_sse_on_messages",
			"anthropic_sse_on_responses",
			"anthropic_sse_on_responses_compact",
			"responses_sse_on_responses_compact",
			"responses_sse_on_gemini_stream",
			"forwarded_chat_sse_from_messages",
			"forwarded_anthropic_sse_from_messages",
			"chunk_size_line_exactly_64k",
			"chunk_size_line_one_over_64k",
			"chunk_size_int64_max_then_eof",
			"chunk_size_over_int64",
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
			if entry.Request == nil || entry.Request.Method != "POST" || entry.Request.Headers == nil {
				t.Fatal("transcript requires a POST request with a headers object")
			}
			if (entry.Request.Body == nil) == (entry.Request.BodyRaw == nil) {
				t.Fatal("transcript request needs exactly one of body and body_raw")
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
			checkObservation(t, entry.Observation, wire)
			if entry.NonstandardWire != "" {
				if !bytes.HasPrefix(wire, []byte("HTTP/1.")) {
					t.Fatal("nonstandard fixture must still start with an HTTP/1.x status line")
				}
				return
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

type transcriptObservation struct {
	StartedAt          string `json:"started_at"`
	EndedAt            string `json:"ended_at"`
	FirstBodyElapsedMS *int64 `json:"first_body_elapsed_ms"`
	EndSignal          string `json:"end_signal"`
	EndSignalElapsedMS *int64 `json:"end_signal_elapsed_ms"`
	ClientWriteFailsAt *int   `json:"client_write_fails_at"`
}

func checkObservation(t *testing.T, o *transcriptObservation, wire []byte) {
	t.Helper()
	if o == nil || o.FirstBodyElapsedMS == nil {
		t.Fatal("transcript requires an observation with first_body_elapsed_ms")
	}
	started, err := time.Parse(time.RFC3339Nano, o.StartedAt)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := time.Parse(time.RFC3339Nano, o.EndedAt)
	if err != nil {
		t.Fatal(err)
	}
	if o.EndSignalElapsedMS != nil && time.Duration(*o.EndSignalElapsedMS)*time.Millisecond < ended.Sub(started) {
		t.Fatal("end signal cannot arrive before ended_at")
	}
	switch o.EndSignal {
	case "eof":
		if o.ClientWriteFailsAt != nil {
			t.Fatal("client_write_fails_at requires end_signal client_write_failed")
		}
	case "client_write_failed":
		head := bytes.Index(wire, []byte("\r\n\r\n"))
		if o.ClientWriteFailsAt == nil || head < 0 || *o.ClientWriteFailsAt < head+4 || *o.ClientWriteFailsAt >= len(wire) {
			t.Fatal("client_write_fails_at must be a response offset after the head terminator")
		}
	default:
		t.Fatalf("unknown end_signal %q", o.EndSignal)
	}
}
