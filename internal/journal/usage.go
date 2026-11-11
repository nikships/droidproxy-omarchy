package journal

import (
	"encoding/json"
	"math"
	"strconv"
)

// counter reads a nonnegative integral JSON number. Missing and null are
// absent; any other value that is not such a number is malformed.
func counter(raw json.RawMessage) (n int64, present, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, true
	}
	if v, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
		return v, true, v >= 0
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || f < 0 || f != math.Trunc(f) || f > 1<<53 {
		return 0, true, false
	}
	return int64(f), true, true
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
	if !ok5 || !ok6 || valueOrZero(cached) > input {
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
	creationKey := "cache_write_tokens"
	if _, ok := promptDetails[creationKey]; !ok {
		creationKey = "cached_creation_tokens"
	}
	cacheCreation, ok7 := optionalCounter(promptDetails[creationKey])
	if !ok5 || !ok6 || !ok7 || valueOrZero(cached) > prompt {
		return nil
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
