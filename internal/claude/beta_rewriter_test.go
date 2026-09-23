package claude

import (
	"reflect"
	"strings"
	"testing"
)

var inboundFastModeHeader = Header{
	Name:  "Anthropic-Beta",
	Value: "claude-code-20250219,fast-mode-2026-02-01,redact-thinking-2026-02-12,interleaved-thinking-2025-05-14",
}

func anthropicBetas(headers []Header) []string {
	var out []string
	for _, h := range headers {
		if !strings.EqualFold(h.Name, "anthropic-beta") {
			continue
		}
		for _, p := range strings.Split(h.Value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func containsFastMode(headers []Header) bool {
	for _, b := range anthropicBetas(headers) {
		if IsFastModeBeta(b) {
			return true
		}
	}
	return false
}

func containsBeta(headers []Header, expected string) bool {
	for _, b := range anthropicBetas(headers) {
		if strings.EqualFold(b, expected) {
			return true
		}
	}
	return false
}

func headerValue(headers []Header, name string) (string, bool) {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value, true
		}
	}
	return "", false
}

func TestThinkingAdaptiveStripsInboundFastModeAndDoesNotInjectIt(t *testing.T) {
	rewritten := RewriteAnthropicBeta([]Header{inboundFastModeHeader, {"X-Request-Id", "abc"}}, true)

	if containsFastMode(rewritten) {
		t.Fatal("fast mode present")
	}
	if !containsBeta(rewritten, "interleaved-thinking-2025-05-14") {
		t.Fatal("missing interleaved-thinking")
	}
	if !containsBeta(rewritten, "prompt-caching-scope-2026-01-05") {
		t.Fatal("missing prompt-caching-scope")
	}
	if v, _ := headerValue(rewritten, "X-Request-Id"); v != "abc" {
		t.Fatalf("X-Request-Id = %q", v)
	}
}

func TestThinkingEnabledStripsFastMode20260212(t *testing.T) {
	rewritten := RewriteAnthropicBeta([]Header{{"anthropic-beta", "fast-mode-2026-02-12,oauth-2025-04-20"}}, true)

	if containsFastMode(rewritten) {
		t.Fatal("fast mode present")
	}
	if !containsBeta(rewritten, "oauth-2025-04-20") {
		t.Fatal("missing oauth beta")
	}
}

func TestThinkingAutoWithoutInboundFastModeDoesNotInjectIt(t *testing.T) {
	rewritten := RewriteAnthropicBeta([]Header{{"Anthropic-Beta", "oauth-2025-04-20"}}, true)

	if containsFastMode(rewritten) {
		t.Fatal("fast mode present")
	}
	for _, b := range VisibleThinkingBetas {
		if IsFastModeBeta(b) {
			t.Fatalf("visible-thinking list contains fast mode beta %q", b)
		}
	}
}

func TestThinkingStripsRedactThinkingAndKeepsOtherVisibleBetas(t *testing.T) {
	rewritten := RewriteAnthropicBeta([]Header{inboundFastModeHeader}, true)

	if containsBeta(rewritten, RedactedThinkingBeta) {
		t.Fatal("redact-thinking still present")
	}
	for _, beta := range VisibleThinkingBetas {
		if !containsBeta(rewritten, beta) {
			t.Errorf("missing visible-thinking beta %s", beta)
		}
	}
	if containsFastMode(rewritten) {
		t.Fatal("fast mode present")
	}
}

func TestNonThinkingClaudeRequestUnchangedWhenNoFastMode(t *testing.T) {
	headers := []Header{
		{"Anthropic-Beta", "oauth-2025-04-20,redact-thinking-2026-02-12"},
		{"X-Request-Id", "abc"},
	}

	rewritten := RewriteAnthropicBeta(headers, false)

	if !reflect.DeepEqual(rewritten, headers) {
		t.Fatalf("headers changed: %#v", rewritten)
	}
	if !containsBeta(rewritten, "redact-thinking-2026-02-12") {
		t.Fatal("redact-thinking removed")
	}
	if containsBeta(rewritten, "prompt-caching-scope-2026-01-05") {
		t.Fatal("visible betas injected")
	}
	if containsFastMode(rewritten) {
		t.Fatal("fast mode present")
	}
}

func TestNonThinkingStripsInboundFastModeAndLeavesOtherBetas(t *testing.T) {
	rewritten := RewriteAnthropicBeta([]Header{inboundFastModeHeader, {"X-Request-Id", "abc"}}, false)

	if containsFastMode(rewritten) {
		t.Fatal("fast mode present")
	}
	for _, b := range []string{"claude-code-20250219", "redact-thinking-2026-02-12", "interleaved-thinking-2025-05-14"} {
		if !containsBeta(rewritten, b) {
			t.Errorf("missing %s", b)
		}
	}
	if v, _ := headerValue(rewritten, "X-Request-Id"); v != "abc" {
		t.Fatalf("X-Request-Id = %q", v)
	}
	if containsBeta(rewritten, "prompt-caching-scope-2026-01-05") {
		t.Fatal("visible betas injected")
	}
}

func TestNonThinkingDropsAnthropicBetaHeaderWhenOnlyFastModeWasPresent(t *testing.T) {
	rewritten := RewriteAnthropicBeta([]Header{{"Anthropic-Beta", "fast-mode-2026-02-01"}, {"Accept", "application/json"}}, false)

	if containsFastMode(rewritten) {
		t.Fatal("fast mode present")
	}
	if _, ok := headerValue(rewritten, "Anthropic-Beta"); ok {
		t.Fatal("Anthropic-Beta should be dropped")
	}
	if v, _ := headerValue(rewritten, "Accept"); v != "application/json" {
		t.Fatalf("Accept = %q", v)
	}
}

func TestThinkingWithNoInboundBetaStillOmitsFastMode(t *testing.T) {
	rewritten := RewriteAnthropicBeta(nil, true)

	if containsFastMode(rewritten) {
		t.Fatal("fast mode present")
	}
	if !containsBeta(rewritten, "interleaved-thinking-2025-05-14") {
		t.Fatal("missing interleaved-thinking")
	}
	if !containsBeta(rewritten, "prompt-caching-scope-2026-01-05") {
		t.Fatal("missing prompt-caching-scope")
	}
	if containsBeta(rewritten, RedactedThinkingBeta) {
		t.Fatal("redact-thinking present")
	}
}

func TestThinkingDeduplicatesCaseInsensitivelyAndKeepsFirstSpelling(t *testing.T) {
	rewritten := RewriteAnthropicBeta([]Header{{"Anthropic-Beta", " OAuth-2025-04-20 ,,custom-beta"}}, true)
	v, _ := headerValue(rewritten, "Anthropic-Beta")
	want := "OAuth-2025-04-20,custom-beta,claude-code-20250219,interleaved-thinking-2025-05-14," +
		"context-management-2025-06-27,prompt-caching-scope-2026-01-05,structured-outputs-2025-12-15," +
		"token-efficient-tools-2026-03-28"
	if v != want {
		t.Fatalf("got %q\nwant %q", v, want)
	}
}
