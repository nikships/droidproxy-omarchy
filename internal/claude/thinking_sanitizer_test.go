package claude

import (
	"encoding/json"
	"testing"
)

func roleSequence(t *testing.T, body string) []string {
	t.Helper()
	var object struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &object); err != nil {
		return nil
	}
	roles := make([]string, 0, len(object.Messages))
	for _, m := range object.Messages {
		roles = append(roles, m.Role)
	}
	return roles
}

func TestPreservesThinkingForTrailingToolResults(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"sig","thinking":"keep me"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"pwd"}},{"type":"tool_use","id":"toolu_2","name":"Read","input":{"file":"a"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"tool_result","tool_use_id":"toolu_2","content":"ok"}]}]}`

	if sanitized := SanitizeThinkingBlocks(request); sanitized != request {
		t.Fatalf("unexpected change:\n%s\n%s", request, sanitized)
	}
}

func TestPreservesRedactedThinkingForTrailingToolResults(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"keep me"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"pwd"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`

	if sanitized := SanitizeThinkingBlocks(request); sanitized != request {
		t.Fatalf("unexpected change:\n%s\n%s", request, sanitized)
	}
}

func TestStripsThinkingAfterNormalUserContent(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"sig","thinking":"strip me"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"pwd"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]},{"role":"user","content":[{"type":"text","text":"continue"}]}]}`

	sanitized := SanitizeThinkingBlocks(request)

	if containsSubstring(sanitized, `"type":"thinking"`) {
		t.Fatal("thinking block still present")
	}
	if !containsSubstring(sanitized, `"type":"tool_use","id":"toolu_1"`) {
		t.Fatal("tool_use removed")
	}
	if !containsSubstring(sanitized, `"type":"tool_result","tool_use_id":"toolu_1"`) {
		t.Fatal("tool_result removed")
	}
	if !containsSubstring(sanitized, `"type":"text","text":"continue"`) {
		t.Fatal("user text removed")
	}
}

func TestStripsRedactedThinkingAfterNormalUserContent(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"strip me"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"pwd"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]},{"role":"user","content":[{"type":"text","text":"continue"}]}]}`

	sanitized := SanitizeThinkingBlocks(request)

	if containsSubstring(sanitized, `"type":"redacted_thinking"`) {
		t.Fatal("redacted_thinking block still present")
	}
	if !containsSubstring(sanitized, `"type":"tool_use","id":"toolu_1"`) {
		t.Fatal("tool_use removed")
	}
	if !containsSubstring(sanitized, `"type":"tool_result","tool_use_id":"toolu_1"`) {
		t.Fatal("tool_result removed")
	}
}

func TestStripsEarlierAssistantThinkingWhenLatestToolResultCycleIsPreserved(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"old","thinking":"strip old"},{"type":"tool_use","id":"toolu_old","name":"Bash","input":{"cmd":"pwd"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_old","content":"ok"}]},{"role":"user","content":[{"type":"text","text":"next task"}]},{"role":"assistant","content":[{"type":"thinking","signature":"new","thinking":"keep new"},{"type":"tool_use","id":"toolu_new","name":"Bash","input":{"cmd":"ls"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_new","content":"ok"}]}]}`

	sanitized := SanitizeThinkingBlocks(request)

	if containsSubstring(sanitized, `"signature":"old"`) {
		t.Fatal("old thinking not stripped")
	}
	if !containsSubstring(sanitized, `"signature":"new"`) {
		t.Fatal("new thinking stripped")
	}
	if !containsSubstring(sanitized, `"tool_use_id":"toolu_old"`) || !containsSubstring(sanitized, `"tool_use_id":"toolu_new"`) {
		t.Fatal("tool results removed")
	}
}

// The trailing user turn carries a tool_result plus a text block. The preceding
// assistant is still the active tool-use turn, so its thinking must be preserved.
func TestPreservesThinkingWhenTrailingToolResultIncludesText(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"sig","thinking":"keep me"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"pwd"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"text","text":"also do this"}]}]}`

	sanitized := SanitizeThinkingBlocks(request)

	if sanitized != request {
		t.Fatalf("unexpected change:\n%s\n%s", request, sanitized)
	}
	if !containsSubstring(sanitized, `"thinking":"keep me"`) {
		t.Fatal("thinking stripped")
	}
}

// A stale assistant message whose only block is thinking must not become
// `"content":[]`; a placeholder keeps the assistant turn and role alternation.
func TestReplacesEmptiedAssistantContentWithPlaceholder(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":[{"type":"thinking","signature":"s","thinking":"x"}]},{"role":"user","content":[{"type":"text","text":"go"}]}]}`

	sanitized := SanitizeThinkingBlocks(request)

	if containsSubstring(sanitized, `"content":[]`) {
		t.Fatal("empty content array left behind")
	}
	if containsSubstring(sanitized, `"type":"thinking"`) {
		t.Fatal("thinking block still present")
	}
	if !containsSubstring(sanitized, `"role":"assistant"`) {
		t.Fatal("assistant turn dropped")
	}
	if !containsSubstring(sanitized, `"type":"text","text":"go"`) {
		t.Fatal("user text removed")
	}
	if !json.Valid([]byte(sanitized)) {
		t.Fatal("output is not valid JSON")
	}
	if roles := roleSequence(t, sanitized); len(roles) != 3 || roles[0] != "user" || roles[1] != "assistant" || roles[2] != "user" {
		t.Fatalf("roles = %v", roles)
	}
}

// A clustered merge (every thinking block before every tool_use block) must be
// stripped even in the latest assistant turn: the signed sequence is invalid.
func TestStripsClusteredThinkingInLatestAssistantTurn(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"sigA","thinking":"first"},{"type":"thinking","signature":"sigB","thinking":"second"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"pwd"}},{"type":"tool_use","id":"toolu_2","name":"Read","input":{"file":"a"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"tool_result","tool_use_id":"toolu_2","content":"ok"}]}]}`

	sanitized := SanitizeThinkingBlocks(request)

	if containsSubstring(sanitized, `"type":"thinking"`) ||
		containsSubstring(sanitized, `"signature":"sigA"`) ||
		containsSubstring(sanitized, `"signature":"sigB"`) {
		t.Fatal("clustered thinking not stripped")
	}
	for _, want := range []string{
		`"type":"tool_use","id":"toolu_1"`,
		`"type":"tool_use","id":"toolu_2"`,
		`"type":"tool_result","tool_use_id":"toolu_1"`,
		`"type":"tool_result","tool_use_id":"toolu_2"`,
	} {
		if !containsSubstring(sanitized, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if !json.Valid([]byte(sanitized)) {
		t.Fatal("invalid JSON")
	}
	if roles := roleSequence(t, sanitized); len(roles) != 2 || roles[0] != "assistant" || roles[1] != "user" {
		t.Fatalf("roles = %v", roles)
	}
}

// Same clustered shape, but with a text block between the thinking blocks and
// the tool_use blocks. The interposed text must not stop the strip.
func TestStripsClusteredThinkingWithInterposedTextBlock(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"sigA","thinking":"first"},{"type":"thinking","signature":"sigB","thinking":"second"},{"type":"text","text":"working on it"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"pwd"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`

	sanitized := SanitizeThinkingBlocks(request)

	if containsSubstring(sanitized, `"type":"thinking"`) {
		t.Fatal("clustered thinking not stripped")
	}
	if !containsSubstring(sanitized, `"type":"text","text":"working on it"`) {
		t.Fatal("text block removed")
	}
	if !containsSubstring(sanitized, `"type":"tool_use","id":"toolu_1"`) ||
		!containsSubstring(sanitized, `"type":"tool_result","tool_use_id":"toolu_1"`) {
		t.Fatal("tool blocks removed")
	}
	if !json.Valid([]byte(sanitized)) {
		t.Fatal("invalid JSON")
	}
}

// redacted_thinking clusters the same way and must also be stripped.
func TestStripsClusteredRedactedThinkingInLatestAssistantTurn(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"first"},{"type":"redacted_thinking","data":"second"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"pwd"}},{"type":"tool_use","id":"toolu_2","name":"Read","input":{"file":"a"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"tool_result","tool_use_id":"toolu_2","content":"ok"}]}]}`

	sanitized := SanitizeThinkingBlocks(request)

	if containsSubstring(sanitized, `"type":"redacted_thinking"`) {
		t.Fatal("clustered redacted_thinking not stripped")
	}
	if !containsSubstring(sanitized, `"type":"tool_use","id":"toolu_1"`) ||
		!containsSubstring(sanitized, `"type":"tool_use","id":"toolu_2"`) ||
		!containsSubstring(sanitized, `"type":"tool_result","tool_use_id":"toolu_2"`) {
		t.Fatal("tool blocks removed")
	}
	if !json.Valid([]byte(sanitized)) {
		t.Fatal("invalid JSON")
	}
}

// Two thinking blocks correctly interleaved with their tool_use blocks are a
// valid signed sequence and must be preserved untouched.
func TestPreservesInterleavedThinkingInLatestAssistantTurn(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"sigA","thinking":"first"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"pwd"}},{"type":"thinking","signature":"sigB","thinking":"second"},{"type":"tool_use","id":"toolu_2","name":"Read","input":{"file":"a"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"tool_result","tool_use_id":"toolu_2","content":"ok"}]}]}`

	if sanitized := SanitizeThinkingBlocks(request); sanitized != request {
		t.Fatalf("unexpected change:\n%s\n%s", request, sanitized)
	}
}

// A single leading thinking block followed by parallel tool_use blocks is a
// normal, valid shape. The >= 2 thinking guard keeps it preserved.
func TestPreservesSingleLeadingThinkingBeforeParallelToolUse(t *testing.T) {
	request := `{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"thinking","signature":"sig","thinking":"plan"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"pwd"}},{"type":"tool_use","id":"toolu_2","name":"Read","input":{"file":"a"}},{"type":"tool_use","id":"toolu_3","name":"Read","input":{"file":"b"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"tool_result","tool_use_id":"toolu_2","content":"ok"},{"type":"tool_result","tool_use_id":"toolu_3","content":"ok"}]}]}`

	sanitized := SanitizeThinkingBlocks(request)

	if sanitized != request {
		t.Fatalf("unexpected change:\n%s\n%s", request, sanitized)
	}
	if !containsSubstring(sanitized, `"thinking":"plan"`) {
		t.Fatal("thinking stripped")
	}
}
