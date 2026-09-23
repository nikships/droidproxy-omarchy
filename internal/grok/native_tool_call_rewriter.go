package grok

// Converts Factory/Droid native Grok tool markup leaked into `message.content`
// into OpenAI `tool_calls` so the chat-completions route actually executes
// them.
//
// Grok-via-DroidProxy often writes:
//
//	prefix text<|tool_calls_begin|><|tool_call_begin|>
//	Execute
//	<|tool_sep|>command
//	ls
//	<|tool_call_end|><|tool_calls_end|>
//
// Factory's first-party Grok adapter parses that. The custom OpenAI-compatible
// route does not, so the turn ends as plain text and the worker goes quiet.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Native Grok tool markup markers.
const (
	CallsBegin = "<|tool_calls_begin|>"
	CallBegin  = "<|tool_call_begin|>"
	CallEnd    = "<|tool_call_end|>"
	CallsEnd   = "<|tool_calls_end|>"
	ToolSep    = "<|tool_sep|>"
)

// ShouldRewrite reports whether a model id carries native Grok tool markup
// (any id containing "grok", e.g. grok-4.7). Composer and Junie do not.
func ShouldRewrite(model string) bool {
	if model == "" {
		return false
	}
	return strings.Contains(strings.ToLower(model), "grok")
}

// NativeCall is one parsed native tool call.
type NativeCall struct {
	Name      string
	Arguments map[string]any
}

// Equal compares name and arguments deeply (Swift's Equatable conformance).
func (c NativeCall) Equal(other NativeCall) bool {
	return c.Name == other.Name && reflect.DeepEqual(c.Arguments, other.Arguments)
}

// ParsedMarkup is the leading prose plus the tool calls parsed out of a model
// message.
type ParsedMarkup struct {
	Prefix string
	Calls  []NativeCall
}

// ParseNativeMarkup parses Factory markup, falling back to fenced/bare JSON
// function calls. Consecutive identical calls are collapsed.
func ParseNativeMarkup(text string) (ParsedMarkup, bool) {
	raw, ok := parseFactoryMarkup(text)
	if !ok {
		raw, ok = ParseJSONFunctionCall(text)
		if !ok {
			return ParsedMarkup{}, false
		}
	}
	calls := dedupeConsecutive(raw.Calls)
	if len(calls) == 0 {
		return ParsedMarkup{}, false
	}
	return ParsedMarkup{Prefix: raw.Prefix, Calls: calls}, true
}

// dedupeConsecutive collapses repeated identical calls: SSE/CLI stream
// parsers often repeat the same JSON object, and Droid must not run Create
// twice.
func dedupeConsecutive(calls []NativeCall) []NativeCall {
	var out []NativeCall
	for _, call := range calls {
		if len(out) > 0 && out[len(out)-1].Equal(call) {
			continue
		}
		out = append(out, call)
	}
	return out
}

func parseFactoryMarkup(text string) (ParsedMarkup, bool) {
	beginIdx := strings.Index(text, CallsBegin)
	if beginIdx < 0 {
		return ParsedMarkup{}, false
	}

	prefix := text[:beginIdx]
	cursor := beginIdx + len(CallsBegin)
	var calls []NativeCall

	for cursor < len(text) {
		if strings.HasPrefix(text[cursor:], CallsEnd) {
			break
		}

		callStart := nextMarkerIndex(text, cursor, []string{CallBegin}, len(text))
		if callStart < 0 {
			break
		}
		cursor = callStart + len(CallBegin)

		callLimit := nextMarkerIndex(text, cursor, []string{CallEnd, CallsEnd, CallBegin}, len(text))
		if callLimit < 0 {
			callLimit = len(text)
		}

		parsed, end, ok := parseOneCall(text, cursor, callLimit)
		if !ok {
			cursor = callLimit
			if callLimit < len(text) && strings.HasPrefix(text[callLimit:], CallEnd) {
				cursor = callLimit + len(CallEnd)
			}
			continue
		}

		calls = append(calls, parsed)
		cursor = end
		if cursor < len(text) && strings.HasPrefix(text[cursor:], CallEnd) {
			cursor += len(CallEnd)
		}
	}

	if len(calls) == 0 {
		return ParsedMarkup{}, false
	}
	return ParsedMarkup{Prefix: prefix, Calls: calls}, true
}

// ParseJSONFunctionCall lifts a fenced `{"name","arguments"}` object (emitted
// by Composer and some Grok turns) into the same parse result. Sequential JSON
// objects (Write then Delete) are collected in order.
func ParseJSONFunctionCall(text string) (ParsedMarkup, bool) {
	var calls []NativeCall
	prefix := ""
	rest := strings.TrimSpace(text)
	capturedPrefix := false
	for {
		extractedPrefix, value, remainder, ok := extractJSONValue(rest)
		if !ok {
			break
		}
		if len(remainder) >= len(rest) {
			break
		}
		more := nativeCallsFromJSON(value)
		if len(more) == 0 {
			rest = strings.TrimSpace(remainder)
			continue
		}
		if !capturedPrefix {
			prefix = extractedPrefix
			capturedPrefix = true
		}
		calls = append(calls, more...)
		rest = strings.TrimSpace(remainder)
		if rest == "" {
			break
		}
	}
	if len(calls) == 0 {
		return ParsedMarkup{}, false
	}
	return ParsedMarkup{Prefix: prefix, Calls: calls}, true
}

func extractJSONValue(text string) (prefix string, value any, remainder string, ok bool) {
	body := text
	prefix = ""
	var remainderAfterFence string
	hasFence := false
	if fence, found := firstCodeFence(text); found {
		prefix = strings.TrimSpace(text[:fence.start])
		body = fence.body
		remainderAfterFence = text[fence.end:]
		hasFence = true
	}
	start := strings.IndexAny(body, "{[")
	if start < 0 {
		return "", nil, "", false
	}
	if prefix == "" && start > 0 {
		prefix = strings.TrimSpace(body[:start])
	}
	jsonSlice, found := matchingJSONSlice(body, start)
	if !found {
		return "", nil, "", false
	}
	decoded := decodeJSONAny([]byte(jsonSlice))
	if decoded == nil {
		return "", nil, "", false
	}
	var rem string
	if hasFence {
		rem = remainderAfterFence
	} else {
		rem = body[start+len(jsonSlice):]
	}
	return prefix, decoded, rem, true
}

type fenceRange struct {
	start int
	end   int
	body  string
}

func firstCodeFence(text string) (fenceRange, bool) {
	opener := strings.Index(text, "```")
	if opener < 0 {
		return fenceRange{}, false
	}
	cursor := opener + 3
	if strings.HasPrefix(text[cursor:], "json") {
		cursor += 4
	}
	if cursor < len(text) && (text[cursor] == '\n' || text[cursor] == '\r') {
		cursor++
	}
	rel := strings.Index(text[cursor:], "```")
	if rel < 0 {
		return fenceRange{}, false
	}
	closer := cursor + rel
	body := strings.TrimSpace(text[cursor:closer])
	return fenceRange{start: opener, end: closer + 3, body: body}, true
}

// matchingJSONSlice finds the balanced JSON object/array starting at start,
// respecting strings and escapes.
func matchingJSONSlice(text string, start int) (string, bool) {
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			if escape {
				escape = false
			} else if ch == '\\' {
				escape = true
			} else if ch == '"' {
				inString = false
			}
		} else {
			switch ch {
			case '"':
				inString = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return text[start : i+1], true
				}
			}
		}
	}
	return "", false
}

func nativeCallsFromJSON(value any) []NativeCall {
	if object, ok := value.(map[string]any); ok {
		if call, ok := nativeCallFromObject(object); ok {
			return []NativeCall{call}
		}
		return nil
	}
	if array, ok := value.([]any); ok {
		var out []NativeCall
		for _, item := range array {
			object, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if call, ok := nativeCallFromObject(object); ok {
				out = append(out, call)
			}
		}
		return out
	}
	return nil
}

func nativeCallFromObject(object map[string]any) (NativeCall, bool) {
	if _, has := object["choices"]; has {
		return NativeCall{}, false
	}
	_, hasID := object["id"]
	_, hasObject := object["object"]
	if hasID && hasObject {
		return NativeCall{}, false
	}
	name, hasName := object["name"].(string)
	if !hasName {
		if tool, ok := object["tool"].(string); ok {
			name = tool
			hasName = true
		}
	}
	var arguments map[string]any
	if nested, ok := object["function"].(map[string]any); ok {
		if !hasName {
			if nestedName, ok := nested["name"].(string); ok {
				name = nestedName
				hasName = true
			}
		}
		arguments = jsonObjectArguments(nested["arguments"])
	}
	if arguments == nil {
		arguments = jsonObjectArguments(object["arguments"])
	}
	if !hasName || !isToolName(name) {
		return NativeCall{}, false
	}
	if len(arguments) > 0 {
		return CanonicalizeToolCall(name, arguments), true
	}
	rest := copyStringMap(object)
	delete(rest, "name")
	delete(rest, "tool")
	delete(rest, "type")
	delete(rest, "function")
	delete(rest, "id")
	if len(rest) == 0 {
		return NativeCall{}, false
	}
	return CanonicalizeToolCall(name, rest), true
}

// CanonicalizeToolCall maps Grok's file-tool names onto Droid's: Write/Create
// become Create; Delete/Remove become an Execute `rm -f`. Remaining calls get
// path/contents aliases normalized.
func CanonicalizeToolCall(name string, arguments map[string]any) NativeCall {
	args := copyStringMap(arguments)
	if name == "Write" || name == "write" || name == "Create" {
		remapString(args, "file_path", []string{"file_path", "path", "filePath", "filename"})
		remapString(args, "content", []string{"content", "contents", "body", "text"})
		create := map[string]any{}
		if v, ok := args["file_path"]; ok {
			create["file_path"] = v
		}
		if v, ok := args["content"]; ok {
			create["content"] = v
		}
		if len(create) == 0 {
			return NativeCall{Name: "Create", Arguments: args}
		}
		return NativeCall{Name: "Create", Arguments: create}
	}
	if name == "Delete" || name == "delete" || name == "Remove" {
		remapString(args, "path", []string{"path", "file_path", "filePath", "filename"})
		if path, ok := args["path"].(string); ok && path != "" {
			return NativeCall{Name: "Execute", Arguments: map[string]any{
				"command": "rm -f " + shellSingleQuote(path),
				"summary": "Delete " + path,
			}}
		}
	}
	if _, has := args["path"]; !has {
		for _, alias := range []string{"file_path", "filePath", "filename"} {
			if v, ok := args[alias].(string); ok && v != "" {
				args["path"] = v
				break
			}
		}
	}
	if _, has := args["contents"]; !has {
		for _, alias := range []string{"content", "body", "text"} {
			if v, ok := args[alias]; ok {
				args["contents"] = v
				break
			}
		}
	}
	return NativeCall{Name: name, Arguments: args}
}

func remapString(args map[string]any, dest string, aliases []string) {
	if existing, ok := args[dest].(string); ok && existing != "" {
		return
	}
	for _, alias := range aliases {
		if v, ok := args[alias].(string); ok && v != "" {
			args[dest] = v
			return
		}
	}
}

func shellSingleQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

func jsonObjectArguments(raw any) map[string]any {
	if object, ok := raw.(map[string]any); ok {
		return object
	}
	if text, ok := raw.(string); ok {
		if object := decodeJSONMap([]byte(text)); object != nil {
			return object
		}
	}
	return nil
}

var toolNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

func isToolName(name string) bool {
	if utf8.RuneCountInString(name) > 64 || !toolNamePattern.MatchString(name) {
		return false
	}
	switch strings.ToLower(name) {
	case "object", "choices", "message", "assistant", "system", "user":
		return false
	}
	return true
}

// parseOneCall parses one `<|tool_call_begin|>…` body between start and limit.
func parseOneCall(text string, start int, limit int) (NativeCall, int, bool) {
	cursor := start
	skipWhitespace(text, &cursor, limit)
	if cursor >= limit {
		return NativeCall{}, 0, false
	}

	nameEnd := nextMarkerIndex(text, cursor, []string{ToolSep, CallEnd, CallsEnd, CallBegin}, limit)
	if nameEnd < 0 {
		if nl := nextNewlineIndex(text, cursor, limit); nl >= 0 {
			nameEnd = nl
		} else {
			nameEnd = limit
		}
	}
	name := strings.TrimSpace(text[cursor:nameEnd])
	if name == "" {
		return NativeCall{}, 0, false
	}

	cursor = nameEnd
	if cursor < limit && text[cursor] == '\r' {
		cursor++
	}
	if cursor < limit && text[cursor] == '\n' {
		cursor++
	}

	arguments := map[string]any{}
	for cursor < limit && strings.HasPrefix(text[cursor:], ToolSep) {
		cursor += len(ToolSep)
		skipHorizontalWhitespace(text, &cursor, limit)
		if cursor < limit && text[cursor] == '\r' {
			cursor++
		}
		if cursor < limit && text[cursor] == '\n' {
			cursor++
		}

		keyEnd := nextNewlineIndex(text, cursor, limit)
		if keyEnd < 0 {
			keyEnd = nextMarkerIndex(text, cursor, []string{ToolSep, CallEnd, CallsEnd}, limit)
		}
		if keyEnd < 0 {
			keyEnd = limit
		}
		key := strings.TrimSpace(text[cursor:keyEnd])
		cursor = keyEnd
		if cursor < limit && text[cursor] == '\r' {
			cursor++
		}
		if cursor < limit && text[cursor] == '\n' {
			cursor++
		}

		valueEnd := nextMarkerIndex(text, cursor, []string{ToolSep, CallEnd, CallsEnd, CallBegin}, limit)
		if valueEnd < 0 {
			valueEnd = limit
		}
		value := text[cursor:valueEnd]
		if strings.HasSuffix(value, "\r\n") {
			value = value[:len(value)-2]
		} else if strings.HasSuffix(value, "\n") || strings.HasSuffix(value, "\r") {
			value = value[:len(value)-1]
		}
		if colon := strings.Index(key, ":"); colon >= 0 {
			inline := strings.TrimSpace(key[colon+1:])
			key = strings.TrimSpace(key[:colon])
			if inline != "" && strings.TrimSpace(value) == "" {
				value = inline
			}
		}
		if key != "" {
			arguments[key] = CoerceJSONValue(value)
		}
		cursor = valueEnd
	}

	return CanonicalizeToolCall(name, arguments), cursor, true
}

// --- chat completion / SSE / HTTP ---

// RewriteChatCompletionJSON rewrites a non-streaming chat.completion JSON
// body. ok=false when unchanged.
func RewriteChatCompletionJSON(jsonBody string) (string, bool) {
	root := decodeJSONMap([]byte(jsonBody))
	if root == nil {
		return "", false
	}
	choices, ok := asDictSlice(root["choices"])
	if !ok || len(choices) == 0 {
		return "", false
	}

	choice := choices[0]
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return "", false
	}

	if _, has := message["tool_calls"]; has {
		return "", false
	}

	content, ok := message["content"].(string)
	if !ok {
		return "", false
	}
	parsed, ok := ParseNativeMarkup(content)
	if !ok {
		return "", false
	}

	if parsed.Prefix == "" {
		message["content"] = nil
	} else {
		message["content"] = parsed.Prefix
	}
	message["tool_calls"] = mapSliceToAny(EncodeToolCalls(parsed.Calls))
	choice["message"] = message
	choice["finish_reason"] = "tool_calls"
	choices[0] = choice
	root["choices"] = mapSliceToAny(choices)

	out, ok := marshalJSONCompact(root)
	if !ok {
		return "", false
	}
	return out, true
}

// RewriteSSEBody rewrites an OpenAI SSE body into a fresh tool-calls stream.
// ok=false when unchanged.
func RewriteSSEBody(sse string) (string, bool) {
	events := sseDataPayloads(sse)
	if len(events) == 0 {
		return "", false
	}

	content := ""
	alreadyHasToolCalls := false
	id := "chatcmpl-grok-native"
	model := "grok-4.6"
	created := int(time.Now().Unix())
	var usage any
	hasUsage := false

	for _, event := range events {
		if event.payload == "[DONE]" {
			continue
		}
		obj := decodeJSONMap([]byte(event.payload))
		if obj == nil {
			continue
		}
		if event.index == 0 || id == "chatcmpl-grok-native" {
			if v, ok := obj["id"].(string); ok {
				id = v
			}
			if v, ok := obj["model"].(string); ok {
				model = v
			}
			if n, ok := obj["created"].(json.Number); ok {
				if i, err := n.Int64(); err == nil {
					created = int(i)
				}
			}
		}
		if v, present := obj["usage"]; present {
			usage = v
			hasUsage = true
		}
		choices, ok := asDictSlice(obj["choices"])
		if !ok || len(choices) == 0 {
			continue
		}
		choice := choices[0]
		if message, ok := choice["message"].(map[string]any); ok {
			if _, has := message["tool_calls"]; has {
				alreadyHasToolCalls = true
			}
			if text, ok := message["content"].(string); ok {
				content += text
			}
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if _, has := delta["tool_calls"]; has {
				alreadyHasToolCalls = true
			}
			if text, ok := delta["content"].(string); ok {
				content += text
			}
		}
	}

	if alreadyHasToolCalls {
		return "", false
	}
	parsed, ok := ParseNativeMarkup(content)
	if !ok {
		return "", false
	}

	var out strings.Builder
	if parsed.Prefix != "" {
		out.WriteString(sseLine(chatChunk(id, model, created,
			map[string]any{"role": "assistant", "content": parsed.Prefix}, "", usage, hasUsage)))
	} else {
		out.WriteString(sseLine(chatChunk(id, model, created,
			map[string]any{"role": "assistant"}, "", usage, hasUsage)))
	}

	toolCalls := EncodeToolCalls(parsed.Calls)
	indexed := make([]any, len(toolCalls))
	for i, call := range toolCalls {
		call["index"] = i
		indexed[i] = call
	}
	out.WriteString(sseLine(chatChunk(id, model, created,
		map[string]any{"tool_calls": indexed}, "", usage, hasUsage)))
	out.WriteString(sseLine(chatChunk(id, model, created,
		map[string]any{}, "tool_calls", usage, hasUsage)))
	out.WriteString("data: [DONE]\n\n")
	return out.String(), true
}

// RewriteHTTPResponse rewrites a complete HTTP response. Returns the original
// bytes when unchanged.
func RewriteHTTPResponse(raw []byte) []byte {
	parsed := splitHTTPResponse(raw)
	if parsed == nil || parsed.statusCode != 200 {
		return raw
	}

	body := parsed.body
	if parsed.isChunked {
		decoded, ok := decodeChunkedBody(body)
		if !ok {
			return raw
		}
		body = decoded
	}

	if !utf8.Valid(body) {
		return raw
	}
	bodyText := string(body)
	changed := false
	if parsed.isEventStream || strings.HasPrefix(bodyText, "data:") || strings.Contains(bodyText, "\ndata:") {
		if next, ok := RewriteSSEBody(bodyText); ok {
			bodyText = next
			changed = true
		}
		if next, ok := RepairSSE(bodyText); ok {
			bodyText = next
			changed = true
		}
	} else {
		if next, ok := RewriteChatCompletionJSON(bodyText); ok {
			bodyText = next
			changed = true
		}
		if next, ok := RepairJSONBody(bodyText, ""); ok {
			bodyText = next
			changed = true
		}
	}
	if !changed {
		return raw
	}

	return buildHTTPResponse(
		parsed.statusLine,
		parsed.headers,
		bodyText,
		parsed.isEventStream || strings.HasPrefix(bodyText, "data:"),
	)
}

// EncodeToolCalls converts parsed calls into OpenAI tool_calls entries,
// running the EndFeatureRun/Execute/Read/Grep repair pass on the arguments.
func EncodeToolCalls(calls []NativeCall) []map[string]any {
	out := make([]map[string]any, len(calls))
	for i, call := range calls {
		args := RepairToolCall(call.Name, call.Arguments, "").Arguments
		argsJSON := "{}"
		if encoded, ok := marshalJSONCompact(args); ok {
			argsJSON = encoded
		}
		out[i] = map[string]any{
			"id":   toolCallID(call.Name, i),
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": argsJSON,
			},
		}
	}
	return out
}

// CoerceJSONValue coerces a raw markup value to bool / null / int / float /
// nested JSON when it unambiguously parses as one; otherwise the raw string.
func CoerceJSONValue(raw string) any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "true" {
		return true
	}
	if trimmed == "false" {
		return false
	}
	if trimmed == "null" {
		return nil
	}
	if i, err := strconv.ParseInt(trimmed, 10, 64); err == nil && strconv.FormatInt(i, 10) == trimmed {
		return int(i)
	}
	if strings.Contains(trimmed, ".") {
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return f
		}
	}
	if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
		(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
		if obj := decodeJSONAny([]byte(trimmed)); obj != nil {
			return obj
		}
	}
	return raw
}

// --- internals ---

func toolCallID(name string, index int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return fmt.Sprintf("call_grok_native_%d_%s", index, b.String())
}

type ssePayload struct {
	index   int
	payload string
}

func sseDataPayloads(sse string) []ssePayload {
	var payloads []ssePayload
	for i, rawLine := range strings.Split(sse, "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if strings.HasPrefix(line, "data:") {
			payload := line[5:]
			if strings.HasPrefix(payload, " ") {
				payload = payload[1:]
			}
			payloads = append(payloads, ssePayload{index: i, payload: payload})
		}
	}
	return payloads
}

func chatChunk(id string, model string, created int, delta map[string]any, finishReason string, usage any, hasUsage bool) map[string]any {
	var fr any
	if finishReason != "" {
		fr = finishReason
	}
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": fr,
	}
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
	if hasUsage {
		obj["usage"] = usage
	}
	return obj
}

func sseLine(object map[string]any) string {
	if str, ok := marshalJSONCompact(object); ok {
		return "data: " + str + "\n\n"
	}
	return ""
}

type httpResponseParts struct {
	statusLine    string
	statusCode    int
	headers       [][2]string
	body          []byte
	isChunked     bool
	isEventStream bool
}

func splitHTTPResponse(raw []byte) *httpResponseParts {
	separator := []byte("\r\n\r\n")
	idx := bytes.Index(raw, separator)
	if idx < 0 {
		return nil
	}
	headerData := raw[:idx]
	if !utf8.Valid(headerData) {
		return nil
	}
	headerText := string(headerData)
	body := raw[idx+4:]

	lines := strings.Split(headerText, "\r\n")
	if len(lines) == 0 {
		return nil
	}
	statusLine := lines[0]
	parts := strings.Fields(statusLine)
	statusCode := 0
	if len(parts) >= 2 {
		if n, err := strconv.Atoi(parts[1]); err == nil {
			statusCode = n
		}
	}

	var headers [][2]string
	isChunked := false
	isEventStream := false
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		name := line[:colon]
		value := strings.Trim(line[colon+1:], " \t")
		lower := strings.ToLower(name)
		if lower == "transfer-encoding" && strings.Contains(strings.ToLower(value), "chunked") {
			isChunked = true
		}
		if lower == "content-type" && strings.Contains(strings.ToLower(value), "text/event-stream") {
			isEventStream = true
		}
		headers = append(headers, [2]string{name, value})
	}

	return &httpResponseParts{
		statusLine:    statusLine,
		statusCode:    statusCode,
		headers:       headers,
		body:          body,
		isChunked:     isChunked,
		isEventStream: isEventStream,
	}
}

func decodeChunkedBody(data []byte) ([]byte, bool) {
	offset := 0
	var decoded []byte

	for offset < len(data) {
		lineEnd := bytes.Index(data[offset:], []byte("\r\n"))
		if lineEnd < 0 {
			return nil, false
		}
		sizeText := string(data[offset : offset+lineEnd])
		if semi := strings.Index(sizeText, ";"); semi >= 0 {
			sizeText = sizeText[:semi]
		}
		sizeText = strings.TrimSpace(sizeText)
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil {
			return nil, false
		}
		offset += lineEnd + 2
		if size == 0 {
			return decoded, true
		}
		next := offset + int(size)
		if next > len(data) {
			next = len(data)
		}
		decoded = append(decoded, data[offset:next]...)
		offset = next
		if offset < len(data) && data[offset] == 13 {
			offset++
		}
		if offset < len(data) && data[offset] == 10 {
			offset++
		}
	}
	return decoded, true
}

func buildHTTPResponse(statusLine string, headers [][2]string, body string, eventStream bool) []byte {
	bodyData := []byte(body)
	var out strings.Builder
	out.WriteString(statusLine + "\r\n")
	dropped := map[string]struct{}{
		"content-length":    {},
		"transfer-encoding": {},
		"connection":        {},
		"content-encoding":  {},
	}
	for _, h := range headers {
		lower := strings.ToLower(h[0])
		if _, skip := dropped[lower]; skip {
			continue
		}
		if lower == "content-type" && eventStream {
			continue
		}
		out.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	if eventStream {
		out.WriteString("Content-Type: text/event-stream\r\n")
	}
	out.WriteString(fmt.Sprintf("Content-Length: %d\r\n", len(bodyData)))
	out.WriteString("Connection: close\r\n\r\n")

	return append([]byte(out.String()), bodyData...)
}

func nextMarkerIndex(text string, start int, markers []string, limit int) int {
	best := -1
	for _, marker := range markers {
		if marker == "" || start >= limit {
			continue
		}
		if idx := strings.Index(text[start:limit], marker); idx >= 0 {
			pos := start + idx
			if best < 0 || pos < best {
				best = pos
			}
		}
	}
	return best
}

func nextNewlineIndex(text string, start int, limit int) int {
	for i := start; i < limit; i++ {
		if text[i] == '\n' || text[i] == '\r' {
			return i
		}
	}
	return -1
}

func skipWhitespace(text string, cursor *int, limit int) {
	for *cursor < limit {
		r, size := utf8.DecodeRuneInString(text[*cursor:])
		if !unicode.IsSpace(r) {
			break
		}
		*cursor += size
	}
}

func skipHorizontalWhitespace(text string, cursor *int, limit int) {
	for *cursor < limit && (text[*cursor] == ' ' || text[*cursor] == '\t') {
		*cursor++
	}
}
