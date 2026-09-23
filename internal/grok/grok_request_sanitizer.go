package grok

// Sanitizes Factory/Droid `/v1/responses` bodies before forwarding to api.x.ai.
//
// xAI rejects:
// - OpenAI-style `"type":"custom"` tool definitions (HTTP 422)
// - `"type":"custom_tool_call"` / `"custom_tool_call_output"` items in `input`
//   (HTTP 422: `data did not match any variant of untagged enum ModelInput`)
// - `tool_choice` / `parallel_tool_calls` when `tools` is empty or absent (HTTP 400)
//
// Client tools are remapped to `"function"`; unsupported tool types are dropped.
// Nested chat-completions function wrappers are flattened. Custom tool-call
// history items become `function_call` / `function_call_output` with object
// `arguments` (xAI rejects non-object argument values). Execute/Bash/Shell
// schemas get a required `command` property when Factory omitted it.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// AllowedToolTypes are the tool `type` values accepted by api.x.ai Responses
// (from the 422 allowlist).
var AllowedToolTypes = map[string]struct{}{
	"function":           {},
	"web_search":         {},
	"x_search":           {},
	"collections_search": {},
	"file_search":        {},
	"code_search":        {},
	"code_execution":     {},
	"code_interpreter":   {},
	"mcp":                {},
	"shell":              {},
}

// SanitizeRequestBody returns a body safe for api.x.ai, or the original bytes
// when unchanged / unparseable. Re-serialization may reorder JSON keys
// (acceptable for api.x.ai; never used on Anthropic paths).
func SanitizeRequestBody(body []byte) []byte {
	root := decodeJSONMap(body)
	if root == nil {
		return body
	}

	changed := false

	if tools, ok := root["tools"].([]any); ok {
		sanitizedTools, toolsChanged := SanitizeToolsArray(tools)
		if toolsChanged {
			changed = true
			if len(sanitizedTools) == 0 {
				delete(root, "tools")
			} else {
				root["tools"] = mapSliceToAny(sanitizedTools)
			}
		}
	}

	if choice, ok := root["tool_choice"].(map[string]any); ok {
		if sanitizedChoice, choiceChanged := sanitizeToolChoice(choice); choiceChanged {
			root["tool_choice"] = sanitizedChoice
			changed = true
		}
	}

	if input, ok := root["input"].([]any); ok {
		sanitizedInput, inputChanged := SanitizeInputArray(input)
		if inputChanged {
			changed = true
			root["input"] = sanitizedInput
		}
	}

	// After tool filtering, drop orphaned tool_choice / parallel_tool_calls.
	// xAI: "A tool_choice was set on the request but no tools were specified."
	if dropOrphanedToolControls(root) {
		changed = true
	}

	if !changed {
		return body
	}
	out, ok := marshalJSONCompact(root)
	if !ok {
		return body
	}
	return []byte(out)
}

// SanitizeToolsArray remaps / drops tools[] entries. Entries that are not
// objects, and tools that cannot be sanitized, mark the array changed.
func SanitizeToolsArray(tools []any) ([]map[string]any, bool) {
	var sanitized []map[string]any
	changed := false

	for _, entry := range tools {
		tool, ok := entry.(map[string]any)
		if !ok {
			changed = true
			continue
		}
		next, ok := SanitizeTool(tool)
		if !ok {
			changed = true
			continue
		}
		sanitized = append(sanitized, next)
		if !reflect.DeepEqual(next, tool) {
			changed = true
		}
	}

	return sanitized, changed
}

// SanitizeTool remaps a single tools[] entry. ok=false means drop.
func SanitizeTool(tool map[string]any) (map[string]any, bool) {
	toolType, _ := tool["type"].(string)

	// Chat-completions wrapper: {"type":"function","function":{name,description,parameters}}
	if toolType == "function" || toolType == "custom" {
		if nested, ok := tool["function"].(map[string]any); ok {
			return flattenFunctionTool(nested)
		}
	}

	if toolType == "custom" || toolType == "function" {
		return flattenFunctionTool(tool)
	}

	if _, allowed := AllowedToolTypes[toolType]; !allowed {
		return nil, false
	}

	// Built-ins: pass through (web_search, x_search, …).
	return tool, true
}

// SanitizeInputArray converts `custom_tool_call` / `custom_tool_call_output`
// history items that Factory/Codex emit but xAI's ModelInput enum does not
// accept.
func SanitizeInputArray(input []any) ([]any, bool) {
	sanitized := make([]any, 0, len(input))
	changed := false

	for _, entry := range input {
		item, ok := entry.(map[string]any)
		if !ok {
			sanitized = append(sanitized, entry)
			continue
		}

		itemType, _ := item["type"].(string)
		switch itemType {
		case "custom_tool_call":
			toolName, _ := item["name"].(string)
			item["type"] = "function_call"
			if raw, present := item["input"]; present {
				item["arguments"] = customToolCallArgumentsJSON(raw, toolName)
				delete(item, "input")
			} else if _, present := item["arguments"]; !present {
				item["arguments"] = "{}"
			} else if argsMap, isMap := item["arguments"].(map[string]any); isMap {
				// xAI expects arguments as a JSON-object *string*.
				if encoded, ok := marshalJSONCompact(argsMap); ok {
					item["arguments"] = encoded
				}
			} else if _, isStr := item["arguments"].(string); !isStr {
				item["arguments"] = customToolCallArgumentsJSON(item["arguments"], toolName)
			}
			changed = true
			sanitized = append(sanitized, item)

		case "custom_tool_call_output":
			item["type"] = "function_call_output"
			changed = true
			sanitized = append(sanitized, item)

		default:
			sanitized = append(sanitized, entry)
		}
	}

	return sanitized, changed
}

func flattenFunctionTool(source map[string]any) (map[string]any, bool) {
	name, _ := source["name"].(string)
	if name == "" {
		return nil, false
	}

	flat := map[string]any{
		"type": "function",
		"name": name,
	}

	if description, ok := source["description"].(string); ok {
		flat["description"] = description
	}

	if parameters, ok := source["parameters"].(map[string]any); ok {
		flat["parameters"] = parameters
	} else if parameters, ok := source["input_schema"].(map[string]any); ok {
		// Anthropic-shaped schema some clients attach to custom tools.
		flat["parameters"] = parameters
	} else {
		flat["parameters"] = emptyToolParameters()
	}

	if strict, ok := source["strict"].(bool); ok {
		flat["strict"] = strict
	}

	EnsureKnownToolParameters(flat)
	return flat, true
}

// EnsureKnownToolParameters pins a required `command` property onto
// Execute/Bash/Shell schemas. Grok often calls Execute with only `summary`
// when `command` is missing from the schema (or buried). Reports whether the
// tool was modified.
func EnsureKnownToolParameters(tool map[string]any) bool {
	name, ok := tool["name"].(string)
	if !ok {
		return false
	}
	params, ok := tool["parameters"].(map[string]any)
	if !ok {
		params = map[string]any{"type": "object"}
	}
	// Copy before mutating so the caller's decoded object is never aliased.
	params = copyStringMap(params)
	props := map[string]any{}
	if p, ok := params["properties"].(map[string]any); ok {
		props = copyStringMap(p)
	}
	var required []any
	if req, ok := params["required"].([]any); ok {
		allStrings := true
		for _, r := range req {
			if _, isStr := r.(string); !isStr {
				allStrings = false
				break
			}
		}
		if allStrings {
			required = req
		}
	}
	changed := false

	if _, isExecute := ExecuteToolNames[name]; isExecute {
		_, hasCommand := props["command"]
		inRequired := false
		for _, r := range required {
			if s, _ := r.(string); s == "command" {
				inRequired = true
				break
			}
		}
		wasMissing := !hasCommand || !inRequired

		if _, has := props["command"]; !has {
			props["command"] = map[string]any{
				"type":        "string",
				"description": "The exact shell command to run. Required. Never omit this field or replace it with summary.",
			}
			changed = true
		}
		if !inRequired {
			required = append([]any{"command"}, required...)
			changed = true
		}

		if wasMissing {
			const suffix = " The command field is required and must be the exact shell string; summary is optional metadata only."
			if description, ok := tool["description"].(string); ok {
				if !strings.Contains(description, "command field is required") {
					tool["description"] = description + suffix
					changed = true
				}
			} else if _, present := tool["description"]; !present {
				tool["description"] = "Run a shell command." + suffix
				changed = true
			}
		}
	}

	if !changed {
		return false
	}
	params["type"] = "object"
	params["properties"] = props
	params["required"] = required
	tool["parameters"] = params
	return true
}

// sanitizeToolChoice returns a rewritten tool_choice when `custom` must become
// `function`; ok=false when unchanged.
func sanitizeToolChoice(choice map[string]any) (map[string]any, bool) {
	choiceType, ok := choice["type"].(string)
	if !ok {
		return nil, false
	}

	if choiceType == "custom" {
		rewritten := copyStringMap(choice)
		rewritten["type"] = "function"
		return rewritten, true
	}

	if choiceType == "function" {
		if _, hasName := choice["name"]; !hasName {
			if nested, ok := choice["function"].(map[string]any); ok {
				if name, ok := nested["name"].(string); ok {
					return map[string]any{"type": "function", "name": name}, true
				}
			}
		}
	}

	return nil, false
}

// dropOrphanedToolControls drops empty `tools` and orphaned `tool_choice` /
// `parallel_tool_calls`. xAI rejects a tool_choice without tools.
func dropOrphanedToolControls(root map[string]any) bool {
	changed := false

	hasTools := false
	if tools, ok := root["tools"].([]any); ok {
		hasTools = len(tools) > 0
		if !hasTools {
			delete(root, "tools")
			changed = true
		}
	}

	if hasTools {
		return changed
	}

	if _, present := root["tool_choice"]; present {
		delete(root, "tool_choice")
		changed = true
	}
	if _, present := root["parallel_tool_calls"]; present {
		delete(root, "parallel_tool_calls")
		changed = true
	}
	return changed
}

// customToolCallArgumentsJSON wraps freeform custom-tool `input` into a
// JSON-object arguments string.
func customToolCallArgumentsJSON(value any, toolName string) string {
	if value == nil {
		return "{}"
	}

	if text, ok := value.(string); ok {
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "{") {
			if obj := decodeJSONMap([]byte(trimmed)); obj != nil {
				return trimmed
			}
		}
		key := "input"
		if _, isExecute := ExecuteToolNames[toolName]; isExecute {
			key = "command"
		}
		if encoded, ok := marshalJSONCompact(map[string]any{key: text}); ok {
			return encoded
		}
		return "{}"
	}

	if obj, ok := value.(map[string]any); ok {
		if encoded, ok := marshalJSONCompact(obj); ok {
			return encoded
		}
	}

	if encoded, ok := marshalJSONCompact(map[string]any{"input": value}); ok {
		return encoded
	}
	return "{}"
}

func emptyToolParameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// --- shared JSON helpers (used by the other Grok transform files) ---

// decodeJSONMap decodes a JSON object, preserving number literals via
// json.Number (Swift's JSONSerialization keeps integer fidelity; a plain
// float64 decode would not).
func decodeJSONMap(data []byte) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil
	}
	return obj
}

// decodeJSONAny decodes any top-level JSON value with number fidelity.
func decodeJSONAny(data []byte) any {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return v
}

// marshalJSONCompact serializes like Swift's JSONSerialization: compact, no
// HTML escaping of <, >, &. Map keys are sorted (matches .sortedKeys output).
func marshalJSONCompact(v any) (string, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

// asDictSlice mirrors Swift's `as? [[String: Any]]`: every element must be an
// object for the cast to succeed.
func asDictSlice(v any) ([]map[string]any, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, len(arr))
	for i, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, false
		}
		out[i] = m
	}
	return out, true
}

func mapSliceToAny(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

func copyStringMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
