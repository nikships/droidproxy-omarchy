package journal

import (
	"fmt"
	"testing"
)

func nonStreamTokens(t *testing.T, path, body string) string {
	t.Helper()
	rec := observeReads(testJournal(t), requestFor(path, `{}`), [][]byte{[]byte(jsonResponse("", body))}, EndEOF)
	if rec.Outcome != outcomeComplete {
		t.Fatalf("%s: outcome %s", body, rec.Outcome)
	}
	return tokensString(rec.Tokens)
}

func TestCounterLiterals(t *testing.T) {
	for _, c := range []struct{ value, tokens string }{
		{"0", "1/0/0/0/null"},
		{"9223372036854775807", "1/0/0/9223372036854775807/null"},
		{"9223372036854775808", "null"},
		{"1e3", "null"},
		{"1E3", "null"},
		{"1.0", "null"},
		{"1.5", "null"},
		{"-1", "null"},
		{"-0", "null"},
		{`"5"`, "null"},
		{"true", "null"},
		{"null", "null"},
	} {
		body := fmt.Sprintf(`{"type":"message","usage":{"input_tokens":1,"output_tokens":%s}}`, c.value)
		if got := nonStreamTokens(t, "/v1/messages", body); got != c.tokens {
			t.Errorf("output_tokens %s: tokens %s, want %s", c.value, got, c.tokens)
		}
	}
}

func TestOptionalCounters(t *testing.T) {
	for _, c := range []struct{ path, usage, tokens string }{
		{"/v1/messages", `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":null,"cache_creation_input_tokens":null}`, "1/0/0/2/null"},
		{"/v1/messages", `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":1.5}`, "null"},
		{"/v1/messages", `{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":"3"}`, "null"},
		{"/v1/messages", `{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":50}`, "5/50/0/2/null"},
		{"/v1/chat/completions", `{"prompt_tokens":9,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":null,"cache_write_tokens":null,"cached_creation_tokens":7}}`, "9/0/7/5/null"},
		{"/v1/chat/completions", `{"prompt_tokens":9,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":3,"cached_creation_tokens":7}}`, "5/4/3/5/null"},
		{"/v1/chat/completions", `{"prompt_tokens":9,"completion_tokens":5,"prompt_tokens_details":{"cache_write_tokens":3,"cached_creation_tokens":1.5}}`, "null"},
		{"/v1/chat/completions", `{"prompt_tokens":9,"completion_tokens":5,"prompt_tokens_details":5}`, "null"},
		{"/v1/chat/completions", `{"prompt_tokens":9,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":2}}`, "9/0/null/5/2"},
		{"/v1/chat/completions", `{"prompt_tokens":3,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}`, "null"},
		{"/v1/responses", `{"input_tokens":7,"output_tokens":1,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":5}}`, "5/2/null/1/null"},
		{"/v1/responses", `{"input_tokens":7,"output_tokens":1,"input_tokens_details":{"cache_write_tokens":-1}}`, "null"},
		{"/v1/responses", `{"input_tokens":7,"output_tokens":1,"output_tokens_details":null}`, "7/0/null/1/null"},
	} {
		var envelope string
		switch c.path {
		case "/v1/messages":
			envelope = `{"type":"message","usage":%s}`
		case "/v1/chat/completions":
			envelope = `{"object":"chat.completion","usage":%s}`
		default:
			envelope = `{"object":"response","usage":%s}`
		}
		if got := nonStreamTokens(t, c.path, fmt.Sprintf(envelope, c.usage)); got != c.tokens {
			t.Errorf("%s %s: tokens %s, want %s", c.path, c.usage, got, c.tokens)
		}
	}
}
