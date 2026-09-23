package claude

import "sort"

// span is a half-open byte range [lo, hi) into the request body.
type span struct{ lo, hi int }

type fieldLocation struct {
	value span
}

type replacement struct {
	r    span
	text string
}

type messageInfo struct {
	role             string
	hasRole          bool
	content          span
	hasContent       bool
	isToolResultTurn bool
}

func isRemovableThinkingType(t string) bool {
	return t == "thinking" || t == "redacted_thinking"
}

// emptyContentPlaceholder is substituted for a content array that would
// otherwise become [] after stripping. Replacing (rather than dropping the
// message) keeps the assistant turn in place so user/assistant roles still
// alternate.
const emptyContentPlaceholder = `[{"type":"text","text":"..."}]`

// SanitizeThinkingBlocks strips stale thinking / redacted_thinking blocks from
// assistant turns of an Anthropic Messages request. Only the latest assistant
// turn that is followed exclusively by tool_result user turns keeps its signed
// thinking, because Anthropic requires it unmodified for the active tool loop.
//
// The body is edited as raw text so key order and formatting (and therefore the
// Anthropic prompt cache) are preserved. Anything unparseable is returned
// unchanged.
func SanitizeThinkingBlocks(json string) string {
	messagesLocation, ok := findObjectFieldLocation(json, "messages", span{0, len(json)})
	if !ok {
		return json
	}
	messages, ok := arrayElementRanges(json, messagesLocation.value)
	if !ok {
		return json
	}

	infos := make([]messageInfo, len(messages))
	for i, m := range messages {
		infos[i] = buildMessageInfo(json, m)
	}
	preserve := latestAssistantIndexWithTrailingToolResults(infos)

	// Droid sometimes squashes several reasoning/tool steps into a single
	// assistant message, producing a "clustered" layout where every thinking
	// block sits before every tool_use block instead of the valid interleaved
	// ordering (thinking -> tool_use -> thinking -> tool_use ...). Anthropic
	// rejects that turn with a 400 ("`thinking` ... blocks ... cannot be
	// modified") because the signed thinking sequence is out of order, so
	// preserving it is exactly what triggers the failure. Stripping it leaves
	// the tool_use blocks (and matching tool_result ids) untouched, which keeps
	// the request valid; clean interleaved turns are still preserved.
	if preserve >= 0 && infos[preserve].hasContent && isClusteredThinkingMerge(json, infos[preserve].content) {
		preserve = -1
	}

	var replacements []replacement
	for index, info := range infos {
		if !info.hasRole || info.role != "assistant" || index == preserve || !info.hasContent {
			continue
		}
		blocks, ok := arrayElementRanges(json, info.content)
		if !ok {
			continue
		}

		var removable []int
		for i, b := range blocks {
			if t, ok := objectStringField(json, b, "type"); ok && isRemovableThinkingType(t) {
				removable = append(removable, i)
			}
		}
		if len(removable) == 0 {
			continue
		}

		// Removing every block would leave "content":[], which Anthropic
		// rejects; the placeholder keeps the assistant turn instead.
		if len(removable) == len(blocks) {
			replacements = append(replacements, replacement{r: info.content, text: emptyContentPlaceholder})
		} else {
			for _, r := range rangesForRemoving(removable, blocks) {
				replacements = append(replacements, replacement{r: r})
			}
		}
	}

	if len(replacements) == 0 {
		return json
	}

	sort.SliceStable(replacements, func(i, j int) bool { return replacements[i].r.lo < replacements[j].r.lo })
	out := make([]byte, 0, len(json))
	cursor := 0
	for _, rep := range replacements {
		if cursor > rep.r.lo {
			continue
		}
		out = append(out, json[cursor:rep.r.lo]...)
		out = append(out, rep.text...)
		cursor = rep.r.hi
	}
	out = append(out, json[cursor:]...)
	return string(out)
}

func buildMessageInfo(json string, r span) messageInfo {
	role, hasRole := objectStringField(json, r, "role")
	content, ok := findObjectFieldLocation(json, "content", r)
	if !ok {
		return messageInfo{role: role, hasRole: hasRole}
	}
	return messageInfo{
		role:             role,
		hasRole:          hasRole,
		content:          content.value,
		hasContent:       true,
		isToolResultTurn: hasRole && role == "user" && contentHasAnyToolResult(json, content.value),
	}
}

// latestAssistantIndexWithTrailingToolResults returns -1 when there is none.
func latestAssistantIndexWithTrailingToolResults(messages []messageInfo) int {
	index := len(messages) - 1
	sawTrailingToolResults := false
	for index >= 0 {
		m := messages[index]
		if !m.hasRole || m.role != "user" || !m.isToolResultTurn {
			break
		}
		sawTrailingToolResults = true
		index--
	}
	if !sawTrailingToolResults || index < 0 || !messages[index].hasRole || messages[index].role != "assistant" {
		return -1
	}
	return index
}

// isClusteredThinkingMerge reports an assistant turn where two or more
// thinking (or redacted_thinking) blocks all appear before the first tool_use
// block. A single leading thinking block followed by parallel tool_use blocks
// is valid, hence the >= 2 check.
func isClusteredThinkingMerge(json string, content span) bool {
	blocks, ok := arrayElementRanges(json, content)
	if !ok {
		return false
	}
	thinkingCount := 0
	lastThinking := -1
	firstToolUse := -1
	for i, b := range blocks {
		t, ok := objectStringField(json, b, "type")
		if !ok {
			continue
		}
		if isRemovableThinkingType(t) {
			thinkingCount++
			lastThinking = i
		} else if t == "tool_use" && firstToolUse < 0 {
			firstToolUse = i
		}
	}
	return thinkingCount >= 2 && firstToolUse >= 0 && lastThinking < firstToolUse
}

func contentHasAnyToolResult(json string, r span) bool {
	blocks, ok := arrayElementRanges(json, r)
	if !ok || len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		if t, ok := objectStringField(json, b, "type"); ok && t == "tool_result" {
			return true
		}
	}
	return false
}

// rangesForRemoving groups consecutive indexes and returns the byte ranges to
// delete, including the separating comma on one side.
func rangesForRemoving(indexes []int, elements []span) []span {
	if len(indexes) == 0 {
		return nil
	}
	var ranges []span
	appendGroup := func(start, end int) {
		switch {
		case start == 0 && end == len(elements)-1:
			ranges = append(ranges, span{elements[start].lo, elements[end].hi})
		case start == 0:
			ranges = append(ranges, span{elements[start].lo, elements[end+1].lo})
		default:
			ranges = append(ranges, span{elements[start-1].hi, elements[end].hi})
		}
	}
	groupStart, previous := indexes[0], indexes[0]
	for _, index := range indexes[1:] {
		if index == previous+1 {
			previous = index
			continue
		}
		appendGroup(groupStart, previous)
		groupStart, previous = index, index
	}
	appendGroup(groupStart, previous)
	return ranges
}

func findObjectFieldLocation(json, targetKey string, object span) (fieldLocation, bool) {
	index, ok := firstNonWhitespaceIndex(json, object.lo, object.hi)
	if !ok || json[index] != '{' {
		return fieldLocation{}, false
	}
	index++
	for {
		keyStart, ok := firstNonWhitespaceIndex(json, index, object.hi)
		if !ok || json[keyStart] == '}' || json[keyStart] != '"' {
			return fieldLocation{}, false
		}
		key, keyEnd, ok := parseJSONStringToken(json, keyStart, object.hi)
		if !ok {
			return fieldLocation{}, false
		}
		colon, ok := firstNonWhitespaceIndex(json, keyEnd, object.hi)
		if !ok || json[colon] != ':' {
			return fieldLocation{}, false
		}
		valueStart, ok := firstNonWhitespaceIndex(json, colon+1, object.hi)
		if !ok {
			return fieldLocation{}, false
		}
		valueEnd, ok := consumeJSONValue(json, valueStart, object.hi)
		if !ok {
			return fieldLocation{}, false
		}
		if key == targetKey {
			return fieldLocation{value: span{valueStart, valueEnd}}, true
		}
		delim, ok := firstNonWhitespaceIndex(json, valueEnd, object.hi)
		if !ok || json[delim] != ',' {
			return fieldLocation{}, false
		}
		index = delim + 1
	}
}

func objectStringField(json string, object span, key string) (string, bool) {
	loc, ok := findObjectFieldLocation(json, key, object)
	if !ok || json[loc.value.lo] != '"' {
		return "", false
	}
	value, end, ok := parseJSONStringToken(json, loc.value.lo, loc.value.hi)
	if !ok || end != loc.value.hi {
		return "", false
	}
	return value, true
}

func arrayElementRanges(json string, array span) ([]span, bool) {
	index, ok := firstNonWhitespaceIndex(json, array.lo, array.hi)
	if !ok || json[index] != '[' {
		return nil, false
	}
	elements := []span{}
	index++
	for {
		valueStart, ok := firstNonWhitespaceIndex(json, index, array.hi)
		if !ok {
			return nil, false
		}
		if json[valueStart] == ']' {
			return elements, true
		}
		valueEnd, ok := consumeJSONValue(json, valueStart, array.hi)
		if !ok {
			return nil, false
		}
		elements = append(elements, span{valueStart, valueEnd})
		delim, ok := firstNonWhitespaceIndex(json, valueEnd, array.hi)
		if !ok {
			return nil, false
		}
		switch json[delim] {
		case ',':
			index = delim + 1
		case ']':
			return elements, true
		default:
			return nil, false
		}
	}
}

func isJSONWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func firstNonWhitespaceIndex(json string, start, end int) (int, bool) {
	i := start
	for i < end && isJSONWhitespace(json[i]) {
		i++
	}
	return i, i < end
}

// parseJSONStringToken returns the raw (still escaped) contents of the string
// starting at startQuote and the index just past its closing quote.
func parseJSONStringToken(json string, startQuote, end int) (string, int, bool) {
	if json[startQuote] != '"' {
		return "", 0, false
	}
	escaped := false
	for i := startQuote + 1; i < end; i++ {
		c := json[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			return json[startQuote+1 : i], i + 1, true
		}
	}
	return "", 0, false
}

func consumeJSONValue(json string, start, end int) (int, bool) {
	if start >= end {
		return 0, false
	}
	switch json[start] {
	case '"':
		_, e, ok := parseJSONStringToken(json, start, end)
		return e, ok
	case '{', '[':
		return consumeCompositeJSONValue(json, start, end)
	}
	i := start
	for i < end {
		c := json[i]
		if c == ',' || c == '}' || c == ']' || isJSONWhitespace(c) {
			break
		}
		i++
	}
	return i, i > start
}

func consumeCompositeJSONValue(json string, start, end int) (int, bool) {
	depth := 0
	inString, escaped := false, false
	for i := start; i < end; i++ {
		c := json[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	return 0, false
}
