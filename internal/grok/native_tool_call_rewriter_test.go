package grok

// Port of GrokNativeToolCallRewriterTests.swift (1:1 case mapping).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestParsesExecuteFromStalledSession(t *testing.T) {
	text := "Checking the working tree and remaining over-cap files so I can continue the editorial demotions from the last batch.<|tool_calls_begin|><|tool_call_begin|>\nExecute\n<|tool_sep|>summary\nCheck git status and remaining diffs\n<|tool_sep|>command\ncd /tmp && git status\n<|tool_sep|>timeout\n30\n<|tool_sep|>riskLevel\nlow\n<|tool_call_end|><|tool_calls_end|>"

	parsed, ok := ParseNativeMarkup(text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if !strings.HasPrefix(parsed.Prefix, "Checking the working tree") {
		t.Errorf("prefix = %q", parsed.Prefix)
	}
	if strings.Contains(parsed.Prefix, "<|tool_calls_begin|>") {
		t.Errorf("prefix contains markup: %q", parsed.Prefix)
	}
	if len(parsed.Calls) != 1 {
		t.Fatalf("calls count = %d, want 1", len(parsed.Calls))
	}
	call := parsed.Calls[0]
	if call.Name != "Execute" {
		t.Errorf("name = %v", call.Name)
	}
	if call.Arguments["summary"] != "Check git status and remaining diffs" {
		t.Errorf("summary = %v", call.Arguments["summary"])
	}
	if call.Arguments["command"] != "cd /tmp && git status" {
		t.Errorf("command = %v", call.Arguments["command"])
	}
	if call.Arguments["timeout"] != 30 {
		t.Errorf("timeout = %v (%T), want int 30", call.Arguments["timeout"], call.Arguments["timeout"])
	}
	if call.Arguments["riskLevel"] != "low" {
		t.Errorf("riskLevel = %v", call.Arguments["riskLevel"])
	}
}

func TestParsesEndFeatureRunWithJSONHandoff(t *testing.T) {
	text := "<|tool_calls_begin|><|tool_call_begin|>\nEndFeatureRun\n<|tool_sep|>successState\npartial\n<|tool_sep|>returnToOrchestrator\ntrue\n<|tool_sep|>validatorsPassed\nfalse\n<|tool_sep|>handoff\n{\"salientSummary\": \"Paused mid-feature\", \"whatWasImplemented\": \"Measured counts\"}\n<|tool_call_end|><|tool_calls_end|>"

	parsed, ok := ParseNativeMarkup("Handing that state back now." + text[:0] + text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if parsed.Prefix != "Handing that state back now." {
		t.Errorf("prefix = %q", parsed.Prefix)
	}
	if len(parsed.Calls) != 1 {
		t.Fatalf("calls count = %d", len(parsed.Calls))
	}
	call := parsed.Calls[0]
	if call.Name != "EndFeatureRun" {
		t.Errorf("name = %v", call.Name)
	}
	if call.Arguments["successState"] != "partial" {
		t.Errorf("successState = %v", call.Arguments["successState"])
	}
	if call.Arguments["returnToOrchestrator"] != true {
		t.Errorf("returnToOrchestrator = %v (%T)", call.Arguments["returnToOrchestrator"], call.Arguments["returnToOrchestrator"])
	}
	if call.Arguments["validatorsPassed"] != false {
		t.Errorf("validatorsPassed = %v (%T)", call.Arguments["validatorsPassed"], call.Arguments["validatorsPassed"])
	}
	handoff, ok := call.Arguments["handoff"].(map[string]any)
	if !ok {
		t.Fatalf("handoff = %v, want object", call.Arguments["handoff"])
	}
	if handoff["salientSummary"] != "Paused mid-feature" {
		t.Errorf("salientSummary = %v", handoff["salientSummary"])
	}
}

func TestParsesSingleArgSkillCall(t *testing.T) {
	text := "<|tool_calls_begin|><|tool_call_begin|>\nSkill\n<|tool_sep|>skill\nmission-worker-base\n<|tool_call_end|><|tool_calls_end|>"

	parsed, ok := ParseNativeMarkup(text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if parsed.Prefix != "" {
		t.Errorf("prefix = %q", parsed.Prefix)
	}
	if parsed.Calls[0].Name != "Skill" {
		t.Errorf("name = %v", parsed.Calls[0].Name)
	}
	if parsed.Calls[0].Arguments["skill"] != "mission-worker-base" {
		t.Errorf("skill = %v", parsed.Calls[0].Arguments["skill"])
	}
}

func TestParsesMultipleToolCalls(t *testing.T) {
	text := "<|tool_calls_begin|><|tool_call_begin|>\nGrep\n<|tool_sep|>pattern\nuppercase\n<|tool_call_end|><|tool_call_begin|>\nRead\n<|tool_sep|>path\n/tmp/a.swift\n<|tool_call_end|><|tool_calls_end|>"

	parsed, ok := ParseNativeMarkup(text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if len(parsed.Calls) != 2 {
		t.Fatalf("calls count = %d, want 2", len(parsed.Calls))
	}
	if parsed.Calls[0].Name != "Grep" || parsed.Calls[1].Name != "Read" {
		t.Errorf("names = %v, %v", parsed.Calls[0].Name, parsed.Calls[1].Name)
	}
	if parsed.Calls[0].Arguments["pattern"] != "uppercase" {
		t.Errorf("pattern = %v", parsed.Calls[0].Arguments["pattern"])
	}
	if parsed.Calls[1].Arguments["path"] != "/tmp/a.swift" {
		t.Errorf("path = %v", parsed.Calls[1].Arguments["path"])
	}
}

func TestParsesInlineCommandKey(t *testing.T) {
	text := "<|tool_calls_begin|><|tool_call_begin|>\nExecute\n<|tool_sep|>command: pwd\n<|tool_sep|>summary\nPrint working directory\n<|tool_call_end|><|tool_calls_end|>"

	parsed, ok := ParseNativeMarkup(text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if parsed.Calls[0].Arguments["command"] != "pwd" {
		t.Errorf("command = %v", parsed.Calls[0].Arguments["command"])
	}
	if parsed.Calls[0].Arguments["summary"] != "Print working directory" {
		t.Errorf("summary = %v", parsed.Calls[0].Arguments["summary"])
	}
}

func TestRemapsFactoryWriteMarkupToCreate(t *testing.T) {
	text := "<|tool_calls_begin|><|tool_call_begin|>\nWrite\n<|tool_sep|>path\n/tmp/ping.txt\n<|tool_sep|>contents\nhello-from-droid-tool-test\n<|tool_call_end|><|tool_calls_end|>"

	parsed, ok := ParseNativeMarkup(text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if parsed.Calls[0].Name != "Create" {
		t.Errorf("name = %v", parsed.Calls[0].Name)
	}
	if parsed.Calls[0].Arguments["file_path"] != "/tmp/ping.txt" {
		t.Errorf("file_path = %v", parsed.Calls[0].Arguments["file_path"])
	}
	if parsed.Calls[0].Arguments["content"] != "hello-from-droid-tool-test" {
		t.Errorf("content = %v", parsed.Calls[0].Arguments["content"])
	}
}

func TestParsesTruncatedCallMissingEndTags(t *testing.T) {
	text := "keep going<|tool_calls_begin|><|tool_call_begin|>\nExecute\n<|tool_sep|>command\nls"

	parsed, ok := ParseNativeMarkup(text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if parsed.Prefix != "keep going" {
		t.Errorf("prefix = %q", parsed.Prefix)
	}
	if parsed.Calls[0].Name != "Execute" {
		t.Errorf("name = %v", parsed.Calls[0].Name)
	}
	if parsed.Calls[0].Arguments["command"] != "ls" {
		t.Errorf("command = %v", parsed.Calls[0].Arguments["command"])
	}
}

func TestShouldRewriteCoversCatalogAndUpstreamGrokIds(t *testing.T) {
	for _, model := range []string{
		"grok-4.6",
		"grok-4.6-fast",
		"cursor-grok-4.6",
		"cursor-grok-4.6-fast",
	} {
		if !ShouldRewrite(model) {
			t.Errorf("expected rewrite for %s", model)
		}
	}
	for _, model := range []string{"cursor-composer-2.5", "claude-opus-4-6", ""} {
		if ShouldRewrite(model) {
			t.Errorf("expected no rewrite for %q", model)
		}
	}
}

func TestEncodeToolCallsRepairsReadPathAlias(t *testing.T) {
	calls := []NativeCall{{Name: "Read", Arguments: map[string]any{"path": "/tmp/a.swift"}}}
	encoded := EncodeToolCalls(calls)
	function, ok := encoded[0]["function"].(map[string]any)
	if !ok {
		t.Fatalf("function = %v", encoded[0]["function"])
	}
	args, ok := function["arguments"].(string)
	if !ok {
		t.Fatalf("arguments = %v", function["arguments"])
	}
	parsed := sanitizeTestJSON(t, args)
	if parsed["file_path"] != "/tmp/a.swift" {
		t.Errorf("file_path = %v", parsed["file_path"])
	}
}

func TestParsesFencedJSONWriteCall(t *testing.T) {
	text := "```json\n{\"name\":\"Write\",\"arguments\":{\"path\":\"/tmp/droidproxy-composer-markup.txt\",\"contents\":\"hello-composer\"}}\n```"

	parsed, ok := ParseNativeMarkup(text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if len(parsed.Calls) != 1 {
		t.Fatalf("calls count = %d", len(parsed.Calls))
	}
	if parsed.Calls[0].Name != "Create" {
		t.Errorf("name = %v", parsed.Calls[0].Name)
	}
	if parsed.Calls[0].Arguments["file_path"] != "/tmp/droidproxy-composer-markup.txt" {
		t.Errorf("file_path = %v", parsed.Calls[0].Arguments["file_path"])
	}
	if parsed.Calls[0].Arguments["content"] != "hello-composer" {
		t.Errorf("content = %v", parsed.Calls[0].Arguments["content"])
	}
}

func TestParsesJSONWriteFilePathAlias(t *testing.T) {
	text := "```json\n{\"name\":\"Write\",\"arguments\":{\"file_path\":\"/tmp/x.txt\",\"content\":\"hi\"}}\n```"

	parsed, ok := ParseNativeMarkup(text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if parsed.Calls[0].Name != "Create" {
		t.Errorf("name = %v", parsed.Calls[0].Name)
	}
	if parsed.Calls[0].Arguments["file_path"] != "/tmp/x.txt" {
		t.Errorf("file_path = %v", parsed.Calls[0].Arguments["file_path"])
	}
	if parsed.Calls[0].Arguments["content"] != "hi" {
		t.Errorf("content = %v", parsed.Calls[0].Arguments["content"])
	}
}

func TestParsesBareJSONFunctionCall(t *testing.T) {
	text := `here {"name":"Delete","arguments":{"path":"/tmp/x.txt"}}`
	parsed, ok := ParseNativeMarkup(text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if parsed.Prefix != "here" {
		t.Errorf("prefix = %q", parsed.Prefix)
	}
	if parsed.Calls[0].Name != "Execute" {
		t.Errorf("name = %v", parsed.Calls[0].Name)
	}
	if parsed.Calls[0].Arguments["command"] != "rm -f '/tmp/x.txt'" {
		t.Errorf("command = %v", parsed.Calls[0].Arguments["command"])
	}
}

func TestParsesSequentialJSONWriteThenDelete(t *testing.T) {
	text := "```json\n{\"name\":\"Write\",\"arguments\":{\"path\":\"/tmp/ping.txt\",\"contents\":\"hello\"}}\n```\n```json\n{\"name\":\"Delete\",\"arguments\":{\"path\":\"/tmp/ping.txt\"}}\n```"

	parsed, ok := ParseNativeMarkup(text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if len(parsed.Calls) != 2 {
		t.Fatalf("calls count = %d, want 2", len(parsed.Calls))
	}
	if parsed.Calls[0].Name != "Create" {
		t.Errorf("calls[0] name = %v", parsed.Calls[0].Name)
	}
	if parsed.Calls[0].Arguments["content"] != "hello" {
		t.Errorf("content = %v", parsed.Calls[0].Arguments["content"])
	}
	if parsed.Calls[1].Name != "Execute" {
		t.Errorf("calls[1] name = %v", parsed.Calls[1].Name)
	}
	if parsed.Calls[1].Arguments["command"] != "rm -f '/tmp/ping.txt'" {
		t.Errorf("command = %v", parsed.Calls[1].Arguments["command"])
	}
}

func TestDedupesRepeatedJSONWriteCall(t *testing.T) {
	text := "{\"name\":\"Write\",\"arguments\":{\"path\":\"/tmp/ping.txt\",\"contents\":\"hello\"}}\n{\"name\":\"Write\",\"arguments\":{\"path\":\"/tmp/ping.txt\",\"contents\":\"hello\"}}"

	parsed, ok := ParseNativeMarkup(text)
	if !ok {
		t.Fatalf("expected parse")
	}
	if len(parsed.Calls) != 1 {
		t.Fatalf("calls count = %d, want 1", len(parsed.Calls))
	}
	if parsed.Calls[0].Name != "Create" {
		t.Errorf("name = %v", parsed.Calls[0].Name)
	}
}

func TestJSONRewriteLiftsIntoOpenAIToolCalls(t *testing.T) {
	content := "```json\n{\"name\":\"Write\",\"arguments\":{\"path\":\"/tmp/a.txt\",\"contents\":\"hi\"}}\n```"
	escaped, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := `{"choices":[{"message":{"role":"assistant","content":` + string(escaped) + `},"finish_reason":"stop"}]}`

	rewritten, ok := RewriteChatCompletionJSON(body)
	if !ok {
		t.Fatalf("expected rewrite")
	}
	root := sanitizeTestJSON(t, rewritten)
	choice := root["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	toolCalls := message["tool_calls"].([]any)
	function := toolCalls[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "Create" {
		t.Errorf("name = %v", function["name"])
	}
}

func TestRewriterLeavesPlainTextUnchanged(t *testing.T) {
	if _, ok := ParseNativeMarkup("just a sentence"); ok {
		t.Errorf("expected no parse")
	}
	if _, ok := RewriteChatCompletionJSON(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`); ok {
		t.Errorf("expected no rewrite")
	}
}

func TestRewritesNonStreamingChatCompletion(t *testing.T) {
	body := `{"id":"chatcmpl_abc","choices":[{"index":0,"message":{"role":"assistant","content":"Working.<|tool_calls_begin|><|tool_call_begin|>\nExecute\n<|tool_sep|>command\nls\n<|tool_call_end|><|tool_calls_end|>"},"finish_reason":"stop"}]}`

	rewritten, ok := RewriteChatCompletionJSON(body)
	if !ok {
		t.Fatalf("expected rewrite")
	}
	root := sanitizeTestJSON(t, rewritten)
	choice := root["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["content"] != "Working." {
		t.Errorf("content = %v", message["content"])
	}
	toolCalls := message["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls count = %d", len(toolCalls))
	}
	entry := toolCalls[0].(map[string]any)
	if entry["type"] != "function" {
		t.Errorf("type = %v", entry["type"])
	}
	function := entry["function"].(map[string]any)
	if function["name"] != "Execute" {
		t.Errorf("name = %v", function["name"])
	}
	args := sanitizeTestJSON(t, function["arguments"].(string))
	if args["command"] != "ls" {
		t.Errorf("command = %v", args["command"])
	}
}

func TestDoesNotRewriteWhenToolCallsAlreadyPresent(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"x<|tool_calls_begin|>","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{}"}}]}}]}`
	if _, ok := RewriteChatCompletionJSON(body); ok {
		t.Errorf("expected no rewrite")
	}
}

func TestRewritesSSEContentDeltasIntoToolCalls(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"grok-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Working.\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"grok-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"<|tool_calls_begin|><|tool_call_begin|>\\nExecute\\n<|tool_sep|>command\\nls\\n<|tool_call_end|><|tool_calls_end|>\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"grok-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

	rewritten, ok := RewriteSSEBody(sse)
	if !ok {
		t.Fatalf("expected rewrite")
	}
	if !strings.Contains(rewritten, `"finish_reason":"tool_calls"`) {
		t.Errorf("missing finish_reason: %s", rewritten)
	}
	if !strings.Contains(rewritten, `"name":"Execute"`) {
		t.Errorf("missing Execute: %s", rewritten)
	}
	if !strings.Contains(rewritten, "Working.") {
		t.Errorf("missing prefix content: %s", rewritten)
	}
	if strings.Contains(rewritten, "tool_calls_begin") {
		t.Errorf("markup leaked: %s", rewritten)
	}
	if !strings.Contains(rewritten, "data: [DONE]") {
		t.Errorf("missing [DONE]: %s", rewritten)
	}
}

func TestRewritesHTTPJSONResponseAndUpdatesContentLength(t *testing.T) {
	jsonBody := `{"choices":[{"message":{"role":"assistant","content":"<|tool_calls_begin|><|tool_call_begin|>\nSkill\n<|tool_sep|>skill\natlas-builder\n<|tool_call_end|><|tool_calls_end|>"},"finish_reason":"stop"}]}`
	raw := []byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(jsonBody)))
	raw = append(raw, []byte(jsonBody)...)

	rewritten := RewriteHTTPResponse(raw)
	if bytes.Equal(rewritten, raw) {
		t.Fatalf("expected rewrite")
	}
	text := string(rewritten)
	if !strings.Contains(text, "tool_calls") {
		t.Errorf("missing tool_calls: %s", text)
	}
	if !strings.Contains(text, "atlas-builder") {
		t.Errorf("missing atlas-builder: %s", text)
	}
	if !strings.Contains(text, "Content-Length:") {
		t.Errorf("missing Content-Length: %s", text)
	}
	if strings.Contains(text, "tool_calls_begin") {
		t.Errorf("markup leaked: %s", text)
	}
}

func TestHTTPPassthroughWhenNoMarkup(t *testing.T) {
	jsonBody := `{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`
	raw := []byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(jsonBody)))
	raw = append(raw, []byte(jsonBody)...)
	if got := RewriteHTTPResponse(raw); !bytes.Equal(got, raw) {
		t.Errorf("expected passthrough, got %q", got)
	}
}

func TestRewritesChunkedSSEHTTPResponseAndRebuildsContentLength(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"grok-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Working.\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"grok-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"<|tool_calls_begin|><|tool_call_begin|>\\nExecute\\n<|tool_sep|>command\\nls\\n<|tool_call_end|><|tool_calls_end|>\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"grok-4.6-fast\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

	body := []byte(sse)
	chunked := []byte(fmt.Sprintf("%x\r\n", len(body)))
	chunked = append(chunked, body...)
	chunked = append(chunked, []byte("\r\n0\r\n\r\n")...)

	raw := []byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
	raw = append(raw, chunked...)

	rewritten := RewriteHTTPResponse(raw)
	if bytes.Equal(rewritten, raw) {
		t.Fatalf("expected rewrite")
	}
	text := string(rewritten)
	if !strings.Contains(text, "Content-Type: text/event-stream") {
		t.Errorf("missing event-stream content type: %s", text)
	}
	if !strings.Contains(text, "Content-Length:") {
		t.Errorf("missing Content-Length: %s", text)
	}
	if strings.Contains(text, "Transfer-Encoding:") {
		t.Errorf("Transfer-Encoding should be dropped: %s", text)
	}
	if !strings.Contains(text, `"finish_reason":"tool_calls"`) {
		t.Errorf("missing finish_reason: %s", text)
	}
	if !strings.Contains(text, `"name":"Execute"`) {
		t.Errorf("missing Execute: %s", text)
	}
	if strings.Contains(text, "tool_calls_begin") {
		t.Errorf("markup leaked: %s", text)
	}
}
