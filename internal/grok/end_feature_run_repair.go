package grok

// Repairs Grok tool calls that Factory then rejects:
// - `EndFeatureRun` without `handoff`, or with `validatorsPassed` as a thought-string
// - `Execute` / `Bash` / `Shell` without `command` (often only `summary`, or `cmd`/`input`)
// - `Read` with `path` instead of `file_path`, `Grep` with `regex` instead of `pattern`
//
// This repairs the arguments before the client sees them. It does not invent a
// shell command from an English summary.

import (
	"regexp"
	"strings"
)

var (
	// EndFeatureRunToolNames / ExecuteToolNames / ReadToolNames / GrepToolNames
	// are the tool name sets the repair pass understands.
	EndFeatureRunToolNames = map[string]struct{}{"EndFeatureRun": {}, "end_feature_run": {}}
	ExecuteToolNames       = map[string]struct{}{"Execute": {}, "Bash": {}, "Shell": {}}
	ReadToolNames          = map[string]struct{}{"Read": {}}
	GrepToolNames          = map[string]struct{}{"Grep": {}}
)

// RepairResult is the outcome of repairing one tool call.
type RepairResult struct {
	Arguments map[string]any
	Changed   bool
	Notes     []string
}

// IsRepairable reports whether the repair pass handles this tool name.
func IsRepairable(name string) bool {
	if _, ok := EndFeatureRunToolNames[name]; ok {
		return true
	}
	if _, ok := ExecuteToolNames[name]; ok {
		return true
	}
	if _, ok := ReadToolNames[name]; ok {
		return true
	}
	if _, ok := GrepToolNames[name]; ok {
		return true
	}
	return false
}

// RepairToolCall repairs one tool call's arguments. An empty assistantText
// means "unknown" (Swift's nil).
func RepairToolCall(name string, arguments map[string]any, assistantText string) RepairResult {
	if _, ok := ExecuteToolNames[name]; ok {
		return repairExecute(arguments, assistantText)
	}
	if _, ok := ReadToolNames[name]; ok {
		return repairRead(arguments)
	}
	if _, ok := GrepToolNames[name]; ok {
		return repairGrep(arguments)
	}
	if _, ok := EndFeatureRunToolNames[name]; !ok {
		return RepairResult{Arguments: arguments, Changed: false, Notes: nil}
	}

	args := copyStringMap(arguments)
	var notes []string
	changed := false

	if raw, ok := args["validatorsPassed"].(string); ok {
		args["validatorsPassed"] = coerceBool(raw)
		changed = true
		notes = append(notes, "coerced validatorsPassed")
	}

	if raw, ok := args["returnToOrchestrator"].(string); ok {
		args["returnToOrchestrator"] = coerceBool(raw)
		changed = true
	}

	if _, hasHandoff := args["handoff"]; !hasHandoff {
		nested := map[string]any{}
		for _, key := range []string{
			"salientSummary",
			"whatWasImplemented",
			"whatWasLeftUndone",
			"discoveredIssues",
			"verification",
			"tests",
			"skillFeedback",
		} {
			if value, present := args[key]; present {
				delete(args, key)
				nested[key] = value
			}
		}
		if len(nested) > 0 {
			args["handoff"] = nested
			changed = true
			notes = append(notes, "nested top-level handoff fields")
		}
	}

	if raw, ok := args["handoff"].(string); ok {
		if object := decodeJSONMap([]byte(raw)); object != nil {
			args["handoff"] = object
			changed = true
			notes = append(notes, "parsed string handoff")
		}
	}

	if _, hasHandoff := args["handoff"]; !hasHandoff {
		args["handoff"] = stubHandoff(assistantText)
		changed = true
		notes = append(notes, "inserted stub handoff")
	}

	if successState, _ := args["successState"].(string); successState == "success" {
		if _, hasValidators := args["validatorsPassed"]; !hasValidators {
			args["validatorsPassed"] = true
			if handoff, ok := args["handoff"].(map[string]any); ok {
				issues := issuesArray(handoff["discoveredIssues"])
				issues = append(issues, map[string]any{
					"severity":    "non_blocking",
					"description": "Grok omitted validatorsPassed on a success EndFeatureRun. DroidProxy set it true so the mission could close. Re-check gates independently.",
				})
				handoff["discoveredIssues"] = issues
				args["handoff"] = handoff
			}
			changed = true
			notes = append(notes, "defaulted validatorsPassed")
		}
	}

	return RepairResult{Arguments: args, Changed: changed, Notes: notes}
}

// RepairArgumentsJSON repairs a JSON-encoded arguments string for a named
// tool. ok=false when the arguments are unparseable or need no repair.
func RepairArgumentsJSON(name string, argumentsJSON string, assistantText string) (string, bool) {
	if !IsRepairable(name) {
		return "", false
	}
	object := decodeJSONMap([]byte(argumentsJSON))
	if object == nil {
		return "", false
	}
	result := RepairToolCall(name, object, assistantText)
	if !result.Changed {
		return "", false
	}
	out, ok := marshalJSONCompact(result.Arguments)
	if !ok {
		return "", false
	}
	return out, true
}

// RepairArgumentsJSONInferred repairs an arguments string when the SSE event
// omitted the tool name; the tool is inferred from the argument shape.
func RepairArgumentsJSONInferred(argumentsJSON string, assistantText string) (string, bool) {
	object := decodeJSONMap([]byte(argumentsJSON))
	if object == nil {
		return "", false
	}
	if _, has := object["successState"]; has {
		return RepairArgumentsJSON("EndFeatureRun", argumentsJSON, assistantText)
	}
	if looksLikeExecute(object) {
		return RepairArgumentsJSON("Execute", argumentsJSON, assistantText)
	}
	if _, hasFilePath := object["file_path"]; !hasFilePath {
		if _, hasPath := object["path"]; hasPath {
			return RepairArgumentsJSON("Read", argumentsJSON, "")
		}
	}
	if _, hasPattern := object["pattern"]; !hasPattern {
		_, hasRegex := object["regex"]
		_, hasQuery := object["query"]
		if hasRegex || hasQuery {
			return RepairArgumentsJSON("Grep", argumentsJSON, "")
		}
	}
	return "", false
}

// RepairJSONBody repairs chat.completion or Responses API JSON.
// ok=false when unchanged or unparseable.
func RepairJSONBody(jsonBody string, assistantText string) (string, bool) {
	root := decodeJSONMap([]byte(jsonBody))
	if root == nil {
		return "", false
	}

	changed := false
	text := assistantText
	if text == "" {
		text = extractAssistantText(root)
	}

	if choices, ok := asDictSlice(root["choices"]); ok && len(choices) > 0 {
		choice := choices[0]
		if message, ok := choice["message"].(map[string]any); ok {
			if toolCalls, ok := asDictSlice(message["tool_calls"]); ok {
				repaired, repairedChanged := repairToolCalls(toolCalls, text)
				if repairedChanged {
					message["tool_calls"] = mapSliceToAny(repaired)
					choice["message"] = message
					choices[0] = choice
					root["choices"] = mapSliceToAny(choices)
					changed = true
				}
			}
		}
	}

	if output, ok := root["output"].([]any); ok {
		repaired, repairedChanged := repairOutputItems(output, text)
		if repairedChanged {
			root["output"] = repaired
			changed = true
		}
	}

	if !changed {
		return "", false
	}
	out, ok := marshalJSONCompact(root)
	if !ok {
		return "", false
	}
	return out, true
}

// RepairSSE repairs streamed chat-completion or Responses SSE.
// ok=false when unchanged.
func RepairSSE(sse string) (string, bool) {
	lines := strings.Split(sse, "\n")
	currentEvent := ""
	changed := false
	skipArgumentDeltas := false
	var rebuilt []string

	// First pass: do we need to drop argument deltas?
	for _, rawLine := range lines {
		line := rawLine
		if strings.HasSuffix(line, "\r") {
			line = line[:len(line)-1]
		}
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.Trim(line[6:], " \t")
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := line[5:]
		if strings.HasPrefix(payload, " ") {
			payload = payload[1:]
		}
		if payload == "[DONE]" {
			continue
		}
		obj := decodeJSONMap([]byte(payload))
		if obj == nil {
			continue
		}
		if scanNeedsRepair(obj) {
			skipArgumentDeltas = true
			break
		}
		if currentEvent == "response.function_call_arguments.done" {
			if args, ok := obj["arguments"].(string); ok {
				name, _ := obj["name"].(string)
				var repaired string
				if name != "" {
					if r, ok := RepairArgumentsJSON(name, args, ""); ok {
						repaired = r
					}
				}
				if repaired == "" {
					if r, ok := RepairArgumentsJSONInferred(args, ""); ok {
						repaired = r
					}
				}
				if repaired != "" {
					skipArgumentDeltas = true
					break
				}
			}
		}
	}

	currentEvent = ""
	for _, rawLine := range lines {
		line := rawLine
		hadCR := strings.HasSuffix(line, "\r")
		if hadCR {
			line = line[:len(line)-1]
		}

		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.Trim(line[6:], " \t")
			if skipArgumentDeltas && currentEvent == "response.function_call_arguments.delta" {
				continue
			}
			rebuilt = append(rebuilt, withCR(line, hadCR))
			continue
		}

		if skipArgumentDeltas && currentEvent == "response.function_call_arguments.delta" {
			continue
		}

		if !strings.HasPrefix(line, "data:") {
			rebuilt = append(rebuilt, withCR(line, hadCR))
			continue
		}

		payload := line[5:]
		if strings.HasPrefix(payload, " ") {
			payload = payload[1:]
		}
		if payload == "[DONE]" {
			rebuilt = append(rebuilt, withCR(line, hadCR))
			continue
		}

		obj := decodeJSONMap([]byte(payload))
		if obj == nil {
			rebuilt = append(rebuilt, withCR(line, hadCR))
			continue
		}

		if repairPayload(obj) {
			changed = true
			if encoded, ok := marshalJSONCompact(obj); ok {
				rebuilt = append(rebuilt, "data: "+encoded)
				continue
			}
		}
		rebuilt = append(rebuilt, withCR(line, hadCR))
	}

	if !changed && !skipArgumentDeltas {
		return "", false
	}
	return strings.Join(rebuilt, "\n"), true
}

// --- internals ---

func withCR(line string, hadCR bool) string {
	if hadCR {
		return line + "\r"
	}
	return line
}

func repairToolCalls(toolCalls []map[string]any, assistantText string) ([]map[string]any, bool) {
	changed := false
	next := make([]map[string]any, len(toolCalls))
	for i, call := range toolCalls {
		function, ok := call["function"].(map[string]any)
		if !ok {
			function = map[string]any{}
		}
		name, _ := function["name"].(string)
		if name == "" {
			name, _ = call["name"].(string)
		}
		if !IsRepairable(name) {
			next[i] = call
			continue
		}

		rawArgs, ok := function["arguments"].(string)
		if !ok {
			rawArgs = "{}"
		}
		if repaired, ok := RepairArgumentsJSON(name, rawArgs, assistantText); ok {
			function["arguments"] = repaired
			call["function"] = function
			next[i] = call
			changed = true
			continue
		}
		next[i] = call
	}
	return next, changed
}

func repairOutputItems(items []any, assistantText string) ([]any, bool) {
	changed := false
	next := make([]any, len(items))
	for i, item := range items {
		next[i] = item
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemType, _ := object["type"].(string)
		name, _ := object["name"].(string)
		if !IsRepairable(name) {
			continue
		}
		_ = itemType // the Swift type guard is subsumed by the name check

		if raw, ok := object["arguments"].(string); ok {
			if repaired, ok := RepairArgumentsJSON(name, raw, assistantText); ok {
				object["arguments"] = repaired
				next[i] = object
				changed = true
				continue
			}
		}
		if args, ok := object["arguments"].(map[string]any); ok {
			result := RepairToolCall(name, args, assistantText)
			if result.Changed {
				object["arguments"] = result.Arguments
				next[i] = object
				changed = true
				continue
			}
		}
		if raw, ok := object["input"].(string); ok {
			if repaired, ok := RepairArgumentsJSON(name, raw, assistantText); ok {
				object["input"] = repaired
				next[i] = object
				changed = true
				continue
			}
		}
	}
	return next, changed
}

// repairPayload repairs one decoded SSE data-line object in place.
func repairPayload(obj map[string]any) bool {
	changed := false
	text := extractAssistantText(obj)

	if choices, ok := asDictSlice(obj["choices"]); ok && len(choices) > 0 {
		choice := choices[0]
		if message, ok := choice["message"].(map[string]any); ok {
			if toolCalls, ok := asDictSlice(message["tool_calls"]); ok {
				repaired, repairedChanged := repairToolCalls(toolCalls, text)
				if repairedChanged {
					message["tool_calls"] = mapSliceToAny(repaired)
					choice["message"] = message
					choices[0] = choice
					obj["choices"] = mapSliceToAny(choices)
					changed = true
				}
			}
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if toolCalls, ok := asDictSlice(delta["tool_calls"]); ok {
				repaired, repairedChanged := repairToolCalls(toolCalls, text)
				if repairedChanged {
					delta["tool_calls"] = mapSliceToAny(repaired)
					choice["delta"] = delta
					choices[0] = choice
					obj["choices"] = mapSliceToAny(choices)
					changed = true
				}
			}
		}
	}

	if item, ok := obj["item"].(map[string]any); ok {
		name, _ := item["name"].(string)
		if IsRepairable(name) {
			if raw, ok := item["arguments"].(string); ok {
				if repaired, ok := RepairArgumentsJSON(name, raw, text); ok {
					item["arguments"] = repaired
					obj["item"] = item
					changed = true
				}
			}
		}
	}

	repairedByName := false
	if name, ok := obj["name"].(string); ok && IsRepairable(name) {
		if raw, ok := obj["arguments"].(string); ok {
			if repaired, ok := RepairArgumentsJSON(name, raw, text); ok {
				obj["arguments"] = repaired
				changed = true
				repairedByName = true
			}
		}
	}

	if !repairedByName {
		if raw, ok := obj["arguments"].(string); ok {
			if repaired, ok := RepairArgumentsJSONInferred(raw, text); ok {
				obj["arguments"] = repaired
				changed = true
			}
		}
	}

	return changed
}

func scanNeedsRepair(obj map[string]any) bool {
	return repairPayload(obj)
}

func stubHandoff(assistantText string) map[string]any {
	summary := ""
	if text := strings.TrimSpace(assistantText); text != "" {
		summary = truncateRunes(text, 400)
	} else {
		summary = "Grok omitted the required handoff object. DroidProxy inserted a stub so EndFeatureRun could close."
	}
	return map[string]any{
		"salientSummary":     summary,
		"whatWasImplemented": "",
		"whatWasLeftUndone":  "",
		"discoveredIssues": []any{map[string]any{
			"severity":    "non_blocking",
			"description": "Grok omitted the required handoff object. DroidProxy inserted a stub so the mission could close. Verify the commit and working tree before trusting this summary.",
		}},
	}
}

func repairExecute(arguments map[string]any, assistantText string) RepairResult {
	args := copyStringMap(arguments)
	var notes []string
	changed := false

	if _, ok := commandString(args["command"]); !ok {
		for _, alias := range []string{"cmd", "shell", "bash", "script", "input"} {
			if value, ok := commandString(args[alias]); ok {
				args["command"] = value
				changed = true
				notes = append(notes, "remapped "+alias+" to command")
				break
			}
		}
	}

	if _, ok := commandString(args["command"]); !ok {
		for key, value := range args {
			if key == "command" {
				continue
			}
			lowered := strings.ToLower(key)
			if !strings.HasPrefix(lowered, "command") {
				continue
			}
			if value2, ok := commandString(value); ok {
				args["command"] = value2
				changed = true
				notes = append(notes, "normalized "+key+" to command")
				break
			}
			if colon := strings.Index(key, ":"); colon >= 0 {
				inline := strings.TrimSpace(key[colon+1:])
				if inline != "" {
					args["command"] = inline
					changed = true
					notes = append(notes, "split inline command key")
					break
				}
			}
		}
	}

	if _, ok := commandString(args["command"]); !ok {
		for _, value := range args {
			if text, isStr := value.(string); isStr {
				if extracted := extractEmbeddedCommand(text); extracted != "" {
					args["command"] = extracted
					changed = true
					notes = append(notes, "extracted command from argument text")
					break
				}
			}
		}
	}

	if _, ok := commandString(args["command"]); !ok {
		if summary, isStr := args["summary"].(string); isStr && looksLikeShellCommand(summary) {
			args["command"] = strings.TrimSpace(summary)
			changed = true
			notes = append(notes, "promoted shell-like summary to command")
		}
	}

	if _, ok := commandString(args["command"]); !ok {
		if assistantText != "" {
			if extracted := extractFencedCommand(assistantText); extracted != "" {
				args["command"] = extracted
				changed = true
				notes = append(notes, "extracted command from assistant text")
			}
		}
	}

	return RepairResult{Arguments: args, Changed: changed, Notes: notes}
}

func repairRead(arguments map[string]any) RepairResult {
	args := copyStringMap(arguments)
	if _, ok := commandString(args["file_path"]); !ok {
		for _, alias := range []string{"path", "file", "filename"} {
			if value, ok := commandString(args[alias]); ok {
				args["file_path"] = value
				return RepairResult{Arguments: args, Changed: true, Notes: []string{"remapped " + alias + " to file_path"}}
			}
		}
	}
	return RepairResult{Arguments: args, Changed: false, Notes: nil}
}

func repairGrep(arguments map[string]any) RepairResult {
	args := copyStringMap(arguments)
	if _, ok := commandString(args["pattern"]); !ok {
		for _, alias := range []string{"regex", "query", "search"} {
			if value, ok := commandString(args[alias]); ok {
				args["pattern"] = value
				return RepairResult{Arguments: args, Changed: true, Notes: []string{"remapped " + alias + " to pattern"}}
			}
		}
	}
	return RepairResult{Arguments: args, Changed: false, Notes: nil}
}

// looksLikeExecute infers Execute only from a command-shaped field.
// `summary` / `riskLevel` alone are too common on other tools and must not
// steal the repair path.
func looksLikeExecute(object map[string]any) bool {
	if _, has := object["cmd"]; has {
		return true
	}
	if _, has := object["shell"]; has {
		return true
	}
	if _, has := object["bash"]; has {
		return true
	}
	if _, has := object["script"]; has {
		return true
	}
	_, has := object["command"]
	return has
}

// commandString accepts only non-empty trimmed strings (Swift's String-only
// coercion of command-like values).
func commandString(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

var (
	embeddedCommandNewlinePattern = regexp.MustCompile(`(?m)^command\s*\n`)
	embeddedCommandColonPattern   = regexp.MustCompile(`(?im)^command:\s+`)
)

func extractEmbeddedCommand(raw string) string {
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	if loc := embeddedCommandNewlinePattern.FindStringIndex(text); loc != nil {
		rest := strings.TrimSpace(text[loc[1]:])
		if rest != "" {
			return rest
		}
	}
	if loc := embeddedCommandColonPattern.FindStringIndex(text); loc != nil {
		rest := strings.TrimSpace(text[loc[1]:])
		if rest != "" {
			return rest
		}
	}
	return ""
}

// looksLikeShellCommand's prefix list is byte-identical to the Swift one; the
// "python" and "/" entries deliberately have no trailing space.
var shellCommandPrefixes = []string{
	"pwd", "ls", "cd ", "cat ", "git ", "node ", "npx ", "python",
	"swift ", "npm ", "pnpm ", "yarn ", "bun ", "cargo ", "go ",
	"rg ", "grep ", "find ", "echo ", "head ", "tail ", "curl ",
	"jq ", "mkdir ", "./", "/", "~/", "brew ",
}

func looksLikeShellCommand(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	for _, prefix := range shellCommandPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

var fencedCommandPattern = regexp.MustCompile("```(?:bash|sh|zsh|shell)?\n([\\s\\S]*?)```")

func extractFencedCommand(text string) string {
	matches := fencedCommandPattern.FindAllStringSubmatchIndex(text, -1)
	// Swift iterates matches in reverse; so must we to pick the same fence.
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		if len(m) < 4 || m[2] < 0 {
			continue
		}
		body := strings.TrimSpace(text[m[2]:m[3]])
		if body != "" {
			return body
		}
	}
	return ""
}

func coerceBool(raw string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "true" {
		return true
	}
	if trimmed == "false" {
		return false
	}
	if strings.HasPrefix(trimmed, "true") {
		return true
	}
	if strings.HasPrefix(trimmed, "false") {
		return false
	}
	return false
}

func issuesArray(value any) []any {
	if issues, ok := value.([]any); ok {
		allMaps := true
		for _, e := range issues {
			if _, isMap := e.(map[string]any); !isMap {
				allMaps = false
				break
			}
		}
		if allMaps {
			return issues
		}
	}
	return nil
}

func extractAssistantText(root map[string]any) string {
	if choices, ok := asDictSlice(root["choices"]); ok && len(choices) > 0 {
		if message, ok := choices[0]["message"].(map[string]any); ok {
			if text, ok := message["content"].(string); ok && text != "" {
				return text
			}
		}
	}
	if output, ok := root["output"].([]any); ok {
		var pieces []string
		for _, itemAny := range output {
			item, ok := itemAny.(map[string]any)
			if !ok {
				continue
			}
			content, ok := item["content"].([]any)
			if !ok {
				continue
			}
			for _, blockAny := range content {
				block, ok := blockAny.(map[string]any)
				if !ok {
					continue
				}
				if text, ok := block["text"].(string); ok {
					pieces = append(pieces, text)
				}
			}
		}
		if len(pieces) > 0 {
			return strings.Join(pieces, " ")
		}
	}
	return ""
}

// truncateRunes mirrors Swift's String.prefix(_:), which counts characters,
// not bytes.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
