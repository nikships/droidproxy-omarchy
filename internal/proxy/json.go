package proxy

import (
	"strings"
	"unicode"
)

// fieldLocation is one key/value pair inside a JSON object. Ranges are byte
// offsets into the source string (JSON is UTF-8; keys and structure are ASCII).
type fieldLocation struct {
	pairLo, pairHi   int
	valueLo, valueHi int
}

type requestJSONFields struct {
	model           string
	hasModel        bool
	modelLoc        fieldLocation
	thinkingType    string
	hasThinkingType bool
	thinkingLoc     fieldLocation
	hasThinkingLoc  bool
	serviceTierLoc  fieldLocation
	hasServiceTier  bool
}

var routingInspectionKeys = map[string]bool{
	"model":        true,
	"service_tier": true,
	"thinking":     true,
}

var reasoningLogInspectionKeys = map[string]bool{
	"reasoning":        true,
	"reasoning_effort": true,
	"output_config":    true,
	"generationConfig": true,
}

var reasoningSummaryOrder = []string{
	"reasoning",
	"reasoning_effort",
	"thinking",
	"output_config",
	"service_tier",
	"generationConfig",
}

const reasoningSummarySnippetLimit = 512

func inspectRequestJSONFields(body string) (requestJSONFields, bool) {
	locations, ok := findObjectFieldLocations(body, 0, len(body), routingInspectionKeys)
	if !ok {
		return requestJSONFields{}, false
	}
	var fields requestJSONFields
	if loc, ok := locations["model"]; ok {
		fields.modelLoc = loc
		if v, ok := topLevelStringValue(body, loc); ok {
			fields.model = v
			fields.hasModel = true
		}
	}
	if loc, ok := locations["thinking"]; ok {
		fields.thinkingLoc = loc
		fields.hasThinkingLoc = true
		if v, ok := objectStringField(body, loc.valueLo, loc.valueHi, "type"); ok {
			fields.thinkingType = v
			fields.hasThinkingType = true
		}
	}
	if loc, ok := locations["service_tier"]; ok {
		fields.serviceTierLoc = loc
		fields.hasServiceTier = true
	}
	return fields, true
}

func reasoningSummaryLog(body string, fields requestJSONFields) string {
	locations, _ := findObjectFieldLocations(body, 0, len(body), reasoningLogInspectionKeys)
	if locations == nil {
		locations = map[string]fieldLocation{}
	}
	if fields.hasThinkingLoc {
		locations["thinking"] = fields.thinkingLoc
	}
	if fields.hasServiceTier {
		locations["service_tier"] = fields.serviceTierLoc
	}

	var parts []string
	if fields.hasModel {
		parts = append(parts, "model="+fields.model)
	}
	for _, key := range reasoningSummaryOrder {
		loc, ok := locations[key]
		if !ok {
			continue
		}
		raw := body[loc.valueLo:loc.valueHi]
		if len(raw) > reasoningSummarySnippetLimit {
			raw = raw[:reasoningSummarySnippetLimit]
		}
		snippet := strings.ReplaceAll(strings.ReplaceAll(raw, "\r", " "), "\n", " ")
		parts = append(parts, key+"="+snippet)
	}
	if len(parts) <= 1 {
		return ""
	}
	return strings.Join(parts, " ")
}

func topLevelStringValue(json string, loc fieldLocation) (string, bool) {
	if loc.valueLo >= len(json) || json[loc.valueLo] != '"' {
		return "", false
	}
	value, valueEnd, ok := parseJSONStringToken(json, loc.valueLo, loc.valueHi)
	if !ok || valueEnd != loc.valueHi {
		return "", false
	}
	return value, true
}

func objectStringField(json string, objectLo, objectHi int, key string) (string, bool) {
	loc, ok := findObjectFieldLocation(json, objectLo, objectHi, key)
	if !ok || loc.valueLo >= len(json) || json[loc.valueLo] != '"' {
		return "", false
	}
	value, valueEnd, ok := parseJSONStringToken(json, loc.valueLo, loc.valueHi)
	if !ok || valueEnd != loc.valueHi {
		return "", false
	}
	return value, true
}

func findObjectFieldLocation(json string, objectLo, objectHi int, key string) (fieldLocation, bool) {
	locs, ok := findObjectFieldLocations(json, objectLo, objectHi, map[string]bool{key: true})
	if !ok {
		return fieldLocation{}, false
	}
	loc, ok := locs[key]
	return loc, ok
}

func findObjectFieldLocations(json string, objectLo, objectHi int, targetKeys map[string]bool) (map[string]fieldLocation, bool) {
	index, ok := firstNonWhitespaceIndex(json, objectLo, objectHi)
	if !ok || json[index] != '{' {
		return nil, false
	}
	if len(targetKeys) == 0 {
		return map[string]fieldLocation{}, true
	}

	locations := map[string]fieldLocation{}
	index++

	for {
		keyStart, ok := firstNonWhitespaceIndex(json, index, objectHi)
		if !ok {
			return nil, false
		}
		token := json[keyStart]
		if token == '}' {
			return locations, true
		}
		if token != '"' {
			return nil, false
		}

		key, keyEnd, ok := parseJSONStringToken(json, keyStart, objectHi)
		if !ok {
			return nil, false
		}
		colonIndex, ok := firstNonWhitespaceIndex(json, keyEnd, objectHi)
		if !ok || json[colonIndex] != ':' {
			return nil, false
		}
		valueStart, ok := firstNonWhitespaceIndex(json, colonIndex+1, objectHi)
		if !ok {
			return nil, false
		}
		valueEnd, ok := consumeJSONValue(json, valueStart, objectHi)
		if !ok {
			return nil, false
		}

		if targetKeys[key] {
			if _, exists := locations[key]; !exists {
				locations[key] = fieldLocation{
					pairLo:  keyStart,
					pairHi:  valueEnd,
					valueLo: valueStart,
					valueHi: valueEnd,
				}
				if len(locations) == len(targetKeys) {
					return locations, true
				}
			}
		}

		delimiterIndex, ok := firstNonWhitespaceIndex(json, valueEnd, objectHi)
		if !ok {
			return nil, false
		}
		switch json[delimiterIndex] {
		case ',':
			index = delimiterIndex + 1
		case '}':
			return locations, true
		default:
			return nil, false
		}
	}
}

func firstNonWhitespaceIndex(json string, start, end int) (int, bool) {
	index := start
	for index < end {
		r := rune(json[index])
		if r >= utf8ASCIIThreshold {
			// Multibyte runes are never JSON whitespace.
			return index, true
		}
		if !unicode.IsSpace(r) {
			return index, true
		}
		index++
	}
	return 0, false
}

// utf8ASCIIThreshold: bytes >= 0x80 start a multibyte UTF-8 sequence.
const utf8ASCIIThreshold = 0x80

func parseJSONStringToken(json string, startQuote, end int) (string, int, bool) {
	if startQuote >= end || json[startQuote] != '"' {
		return "", 0, false
	}
	index := startQuote + 1
	escaped := false
	for index < end {
		char := json[index]
		if escaped {
			escaped = false
		} else if char == '\\' {
			escaped = true
		} else if char == '"' {
			return json[startQuote+1 : index], index + 1, true
		}
		index++
	}
	return "", 0, false
}

func consumeJSONValue(json string, start, end int) (int, bool) {
	if start >= end {
		return 0, false
	}
	first := json[start]
	if first == '"' {
		_, valueEnd, ok := parseJSONStringToken(json, start, end)
		return valueEnd, ok
	}
	if first == '{' || first == '[' {
		return consumeCompositeJSONValue(json, start, end)
	}
	index := start
	for index < end {
		char := json[index]
		if char == ',' || char == '}' || char == ']' || unicode.IsSpace(rune(char)) {
			break
		}
		index++
	}
	if index > start {
		return index, true
	}
	return 0, false
}

func consumeCompositeJSONValue(json string, start, end int) (int, bool) {
	index := start
	depth := 0
	inString := false
	escaped := false
	for index < end {
		char := json[index]
		if inString {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
		} else {
			switch char {
			case '"':
				inString = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return index + 1, true
				}
				if depth < 0 {
					return 0, false
				}
			}
		}
		index++
	}
	return 0, false
}
