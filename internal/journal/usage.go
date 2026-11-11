package journal

import (
	"encoding/json"
	"strconv"
)

// counter reads a JSON integer literal from 0 through the int64 maximum.
// Missing and null are absent; anything else, including a fraction or an
// exponent, is malformed.
func counter(raw json.RawMessage) (n int64, present, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, true
	}
	if !isDigits(string(raw), len(raw)) {
		return 0, true, false
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	return v, true, err == nil
}

func requiredCounter(raw json.RawMessage) (int64, bool) {
	n, present, ok := counter(raw)
	return n, present && ok
}

func optionalCounter(raw json.RawMessage) (*int64, bool) {
	n, present, ok := counter(raw)
	if !ok {
		return nil, false
	}
	if !present {
		return nil, true
	}
	return &n, true
}

// detailsObject returns a details object, nil when absent or null, and false
// when it is present but not an object.
func detailsObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	obj := objectOf(raw)
	return obj, obj != nil
}

// anthropicTokens normalizes Anthropic usage. input_tokens already excludes
// cache reads and creations; missing cache counters mean zero.
func anthropicTokens(usage map[string]json.RawMessage) *Tokens {
	if usage == nil {
		return nil
	}
	input, ok1 := requiredCounter(usage["input_tokens"])
	output, ok2 := requiredCounter(usage["output_tokens"])
	cacheRead, ok3 := optionalCounter(usage["cache_read_input_tokens"])
	cacheCreation, ok4 := optionalCounter(usage["cache_creation_input_tokens"])
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil
	}
	return &Tokens{
		InputUncached: input,
		CacheRead:     valueOrZero(cacheRead),
		CacheCreation: ptr(valueOrZero(cacheCreation)),
		Output:        output,
	}
}

// responsesTokens normalizes Responses usage, whose input_tokens include
// cached tokens.
func responsesTokens(raw json.RawMessage) *Tokens {
	usage := objectOf(raw)
	if usage == nil {
		return nil
	}
	input, ok1 := requiredCounter(usage["input_tokens"])
	output, ok2 := requiredCounter(usage["output_tokens"])
	inDetails, ok3 := detailsObject(usage["input_tokens_details"])
	outDetails, ok4 := detailsObject(usage["output_tokens_details"])
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil
	}
	cached, ok5 := optionalCounter(inDetails["cached_tokens"])
	reasoning, ok6 := optionalCounter(outDetails["reasoning_tokens"])
	_, ok7 := optionalCounter(inDetails["cache_write_tokens"])
	_, ok8 := optionalCounter(inDetails["cached_creation_tokens"])
	if !ok5 || !ok6 || !ok7 || !ok8 || valueOrZero(cached) > input {
		return nil
	}
	return &Tokens{
		InputUncached: input - valueOrZero(cached),
		CacheRead:     valueOrZero(cached),
		Output:        output,
		Reasoning:     reasoning,
	}
}

// chatTokens normalizes chat/text completions usage, whose prompt_tokens
// include cached tokens.
func chatTokens(raw json.RawMessage) *Tokens {
	usage := objectOf(raw)
	if usage == nil {
		return nil
	}
	prompt, ok1 := requiredCounter(usage["prompt_tokens"])
	completion, ok2 := requiredCounter(usage["completion_tokens"])
	promptDetails, ok3 := detailsObject(usage["prompt_tokens_details"])
	completionDetails, ok4 := detailsObject(usage["completion_tokens_details"])
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil
	}
	cached, ok5 := optionalCounter(promptDetails["cached_tokens"])
	reasoning, ok6 := optionalCounter(completionDetails["reasoning_tokens"])
	cacheWrite, ok7 := optionalCounter(promptDetails["cache_write_tokens"])
	cachedCreation, ok8 := optionalCounter(promptDetails["cached_creation_tokens"])
	if !ok5 || !ok6 || !ok7 || !ok8 || valueOrZero(cached) > prompt {
		return nil
	}
	cacheCreation := cacheWrite
	if cacheCreation == nil {
		cacheCreation = cachedCreation
	}
	return &Tokens{
		InputUncached: prompt - valueOrZero(cached),
		CacheRead:     valueOrZero(cached),
		CacheCreation: cacheCreation,
		Output:        completion,
		Reasoning:     reasoning,
	}
}

func valueOrZero(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
