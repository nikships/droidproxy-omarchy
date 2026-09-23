package grok

// Port of GrokRequestSanitizerTests.swift (1:1 case mapping).

import (
	"encoding/json"
	"strings"
	"testing"
)

func sanitizeTestJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	root := decodeJSONMap([]byte(s))
	if root == nil {
		t.Fatalf("could not parse JSON: %s", s)
	}
	return root
}

func TestSanitizeRemapsCustomToolToFunction(t *testing.T) {
	request := `{"model":"grok-4.6","input":"hi","tools":[{"type":"custom","name":"Read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	tools := root["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools count = %d, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("type = %v, want function", tool["type"])
	}
	if tool["name"] != "Read" {
		t.Errorf("name = %v, want Read", tool["name"])
	}
	if tool["description"] != "Read a file" {
		t.Errorf("description = %v", tool["description"])
	}
	if _, ok := tool["parameters"].(map[string]any); !ok {
		t.Errorf("parameters missing")
	}
	if strings.Contains(string(sanitized), `"type":"custom"`) {
		t.Errorf("output still contains custom type: %s", sanitized)
	}
}

func TestSanitizePinsRequiredCommandOnExecuteSchema(t *testing.T) {
	request := `{"model":"grok-4.6","tools":[{"type":"custom","name":"Execute","description":"Run a command","parameters":{"type":"object","properties":{"summary":{"type":"string"}}}}]}`
	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	tools := root["tools"].([]any)
	tool := tools[0].(map[string]any)
	parameters := tool["parameters"].(map[string]any)
	properties := parameters["properties"].(map[string]any)
	required := parameters["required"].([]any)
	if _, ok := properties["command"]; !ok {
		t.Errorf("command property missing")
	}
	found := false
	for _, r := range required {
		if r == "command" {
			found = true
		}
	}
	if !found {
		t.Errorf("required = %v, want command", required)
	}
	if desc, _ := tool["description"].(string); !strings.Contains(desc, "command field is required") {
		t.Errorf("description = %q", desc)
	}
}

func TestSanitizeFlattensChatCompletionsFunctionWrapper(t *testing.T) {
	request := `{"model":"grok-4.6","tools":[{"type":"function","function":{"name":"Bash","description":"Run","parameters":{"type":"object","properties":{}}}}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	tools := root["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools count = %d, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("type = %v", tool["type"])
	}
	if tool["name"] != "Bash" {
		t.Errorf("name = %v", tool["name"])
	}
	if _, ok := tool["function"]; ok {
		t.Errorf("nested function key still present")
	}
}

func TestSanitizeKeepsBuiltinSearchTools(t *testing.T) {
	request := `{"model":"grok-4.6","tools":[{"type":"web_search"},{"type":"x_search"},{"type":"function","name":"Read","parameters":{"type":"object","properties":{}}}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	tools := root["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools count = %d, want 3", len(tools))
	}
	if tools[0].(map[string]any)["type"] != "web_search" {
		t.Errorf("tools[0] type = %v", tools[0])
	}
	if tools[1].(map[string]any)["type"] != "x_search" {
		t.Errorf("tools[1] type = %v", tools[1])
	}
	if tools[2].(map[string]any)["type"] != "function" {
		t.Errorf("tools[2] type = %v", tools[2])
	}
}

func TestSanitizeDropsUnknownToolTypes(t *testing.T) {
	request := `{"model":"grok-4.6","tools":[{"type":"computer_use_preview"},{"type":"function","name":"Read","parameters":{"type":"object","properties":{}}}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	tools := root["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools count = %d, want 1", len(tools))
	}
	if tools[0].(map[string]any)["name"] != "Read" {
		t.Errorf("tools[0] = %v", tools[0])
	}
}

func TestSanitizeRemovesToolsKeyWhenAllDropped(t *testing.T) {
	request := `{"model":"grok-4.6","input":"hi","tools":[{"type":"computer_use_preview"}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	if _, ok := root["tools"]; ok {
		t.Errorf("tools key still present")
	}
	if root["model"] != "grok-4.6" {
		t.Errorf("model = %v", root["model"])
	}
}

func TestSanitizeLeavesBodyWithoutToolsUnchanged(t *testing.T) {
	request := `{"model":"grok-4.6","input":"hello"}`
	if got := string(SanitizeRequestBody([]byte(request))); got != request {
		t.Errorf("got %q, want %q", got, request)
	}
}

func TestSanitizeRewritesCustomToolChoice(t *testing.T) {
	request := `{"model":"grok-4.6","tool_choice":{"type":"custom","name":"Read"},"tools":[{"type":"custom","name":"Read","parameters":{"type":"object","properties":{}}}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	choice := root["tool_choice"].(map[string]any)
	if choice["type"] != "function" {
		t.Errorf("type = %v", choice["type"])
	}
	if choice["name"] != "Read" {
		t.Errorf("name = %v", choice["name"])
	}
}

func TestSanitizeFlattensNestedFunctionToolChoice(t *testing.T) {
	request := `{"model":"grok-4.6","tool_choice":{"type":"function","function":{"name":"Read"}},"tools":[{"type":"function","name":"Read","parameters":{"type":"object","properties":{}}}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	choice := root["tool_choice"].(map[string]any)
	if choice["type"] != "function" {
		t.Errorf("type = %v", choice["type"])
	}
	if choice["name"] != "Read" {
		t.Errorf("name = %v", choice["name"])
	}
	if _, ok := choice["function"]; ok {
		t.Errorf("nested function key still present")
	}
}

func TestSanitizeMapsInputSchemaToParameters(t *testing.T) {
	request := `{"model":"grok-4.6","tools":[{"type":"custom","name":"Read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	tools := root["tools"].([]any)
	tool := tools[0].(map[string]any)
	parameters := tool["parameters"].(map[string]any)
	if parameters["type"] != "object" {
		t.Errorf("parameters type = %v", parameters["type"])
	}
	if _, ok := parameters["properties"].(map[string]any); !ok {
		t.Errorf("parameters properties missing")
	}
	if _, ok := tool["input_schema"]; ok {
		t.Errorf("input_schema still present")
	}
}

func TestSanitizeDropsNamelessCustomToolAndDefaultsEmptyParameters(t *testing.T) {
	request := `{"model":"grok-4.6","tools":[{"type":"custom","description":"no name"},{"type":"function","name":"Bare"}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	tools := root["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools count = %d, want 1", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "Bare" {
		t.Errorf("name = %v", tool["name"])
	}
	parameters := tool["parameters"].(map[string]any)
	if parameters["type"] != "object" {
		t.Errorf("parameters type = %v", parameters["type"])
	}
}

func TestSanitizeLeavesInvalidJSONUnchanged(t *testing.T) {
	request := "{not-json"
	if got := string(SanitizeRequestBody([]byte(request))); got != request {
		t.Errorf("got %q, want %q", got, request)
	}
}

func TestSanitizeDropsOrphanedStringToolChoiceWithoutTools(t *testing.T) {
	request := `{"model":"grok-4.6","tool_choice":"auto","parallel_tool_calls":true}`
	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	if root["model"] != "grok-4.6" {
		t.Errorf("model = %v", root["model"])
	}
	if _, ok := root["tool_choice"]; ok {
		t.Errorf("tool_choice still present")
	}
	if _, ok := root["parallel_tool_calls"]; ok {
		t.Errorf("parallel_tool_calls still present")
	}
	if _, ok := root["tools"]; ok {
		t.Errorf("tools still present")
	}
}

func TestSanitizeConvertsCustomToolCallInputItems(t *testing.T) {
	request := `{"model":"grok-4.6","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"custom_tool_call","call_id":"c0","name":"ApplyPatch","input":"*** Begin Patch"},{"type":"custom_tool_call_output","call_id":"c0","output":"done"}],"tools":[],"tool_choice":"auto","parallel_tool_calls":true}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	input := root["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input count = %d, want 3", len(input))
	}
	call := input[1].(map[string]any)
	if call["type"] != "function_call" {
		t.Errorf("type = %v", call["type"])
	}
	if call["name"] != "ApplyPatch" {
		t.Errorf("name = %v", call["name"])
	}
	if call["call_id"] != "c0" {
		t.Errorf("call_id = %v", call["call_id"])
	}
	if _, ok := call["input"]; ok {
		t.Errorf("input key still present")
	}
	arguments, ok := call["arguments"].(string)
	if !ok {
		t.Fatalf("arguments = %v, want string", call["arguments"])
	}
	argsObj := sanitizeTestJSON(t, arguments)
	if argsObj["input"] != "*** Begin Patch" {
		t.Errorf("argsObj[input] = %v", argsObj["input"])
	}
	output := input[2].(map[string]any)
	if output["type"] != "function_call_output" {
		t.Errorf("output type = %v", output["type"])
	}
	if output["output"] != "done" {
		t.Errorf("output = %v", output["output"])
	}
	if _, ok := root["tools"]; ok {
		t.Errorf("tools still present")
	}
	if _, ok := root["tool_choice"]; ok {
		t.Errorf("tool_choice still present")
	}
	if _, ok := root["parallel_tool_calls"]; ok {
		t.Errorf("parallel_tool_calls still present")
	}
}

func TestSanitizeExecuteCustomToolCallStringInputBecomesCommand(t *testing.T) {
	request := `{"model":"grok-4.6","input":[{"type":"custom_tool_call","call_id":"c2","name":"Execute","input":"pwd"}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	input := root["input"].([]any)
	arguments, ok := input[0].(map[string]any)["arguments"].(string)
	if !ok {
		t.Fatalf("arguments = %v, want string", input[0].(map[string]any)["arguments"])
	}
	argsObj := sanitizeTestJSON(t, arguments)
	if argsObj["command"] != "pwd" {
		t.Errorf("command = %v", argsObj["command"])
	}
	if _, ok := argsObj["input"]; ok {
		t.Errorf("input key still present in arguments")
	}
}

func TestSanitizeCustomToolCallObjectInputBecomesArgumentsString(t *testing.T) {
	request := `{"model":"grok-4.6","input":[{"type":"custom_tool_call","call_id":"c1","name":"Read","input":{"file_path":"/tmp/a"}}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	input := root["input"].([]any)
	call := input[0].(map[string]any)
	if call["type"] != "function_call" {
		t.Errorf("type = %v", call["type"])
	}
	arguments, ok := call["arguments"].(string)
	if !ok {
		t.Fatalf("arguments = %v, want string", call["arguments"])
	}
	argsObj := sanitizeTestJSON(t, arguments)
	if argsObj["file_path"] != "/tmp/a" {
		t.Errorf("file_path = %v", argsObj["file_path"])
	}
}

func TestSanitizeLeavesExistingFunctionCallInputUnchanged(t *testing.T) {
	request := `{"model":"grok-4.6","input":[{"type":"function_call","call_id":"f0","name":"Read","arguments":"{}"}]}`
	if got := string(SanitizeRequestBody([]byte(request))); got != request {
		t.Errorf("got %q, want %q", got, request)
	}
}

func TestSanitizeKeepsStringToolChoiceWhenToolsPresent(t *testing.T) {
	request := `{"model":"grok-4.6","tool_choice":"auto","tools":[{"type":"function","name":"Read","parameters":{"type":"object","properties":{}}}]}`

	sanitized := SanitizeRequestBody([]byte(request))
	root := sanitizeTestJSON(t, string(sanitized))
	if root["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v", root["tool_choice"])
	}
	tools := root["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("tools count = %d, want 1", len(tools))
	}
}

// Extra: number fidelity must survive decode + re-encode.
func TestSanitizePreservesLargeIntegerLiterals(t *testing.T) {
	request := `{"model":"grok-4.6","tools":[{"type":"function","name":"Read","parameters":{"type":"object","properties":{}}}],"big":9007199254740993}`
	sanitized := SanitizeRequestBody([]byte(request))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(sanitized, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := string(raw["big"]); got != "9007199254740993" {
		t.Errorf("big = %s, want 9007199254740993", got)
	}
}
