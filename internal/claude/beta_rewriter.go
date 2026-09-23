// Package claude holds Claude-specific request rewriting and auth-file helpers.
package claude

import "strings"

// Header is one HTTP header line, kept as an ordered pair so rewriting never
// reorders or merges the headers the client sent.
type Header struct {
	Name  string
	Value string
}

// RedactedThinkingBeta makes Claude emit only signed empty thinking blocks, so
// it is dropped on thinking requests.
const RedactedThinkingBeta = "redact-thinking-2026-02-12"

// VisibleThinkingBetas are appended on thinking requests so Claude emits
// plaintext thinking blocks.
var VisibleThinkingBetas = []string{
	"claude-code-20250219",
	"oauth-2025-04-20",
	"interleaved-thinking-2025-05-14",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
	"structured-outputs-2025-12-15",
	"token-efficient-tools-2026-03-28",
}

// IsFastModeBeta reports whether rawBeta is a fast-mode-* beta token.
func IsFastModeBeta(rawBeta string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawBeta)), "fast-mode-")
}

// RewriteAnthropicBeta rewrites Anthropic-Beta on Claude requests.
//
// CLIProxyAPI 7.2.130 treats any error as request-scoped when the header
// includes fast-mode-*, which blocks OAuth seat failover on ordinary 5-hour
// 429s. Claude has no Fast mode, so this never injects fast-mode-* and always
// strips it if Factory/Droid sent it.
//
// On thinking requests it also drops redact-thinking-2026-02-12 and appends the
// visible-thinking beta list so Claude emits plaintext thinking blocks.
func RewriteAnthropicBeta(headers []Header, requestVisibleThinking bool) []Header {
	if requestVisibleThinking {
		return headersWithVisibleThinkingBetas(headers)
	}
	return headersStrippingFastMode(headers)
}

func headersWithVisibleThinkingBetas(headers []Header) []Header {
	forwarded := make([]Header, 0, len(headers)+1)
	var candidates []string

	for _, h := range headers {
		if strings.EqualFold(h.Name, "anthropic-beta") {
			candidates = append(candidates, parseAnthropicBetas(h.Value)...)
			continue
		}
		forwarded = append(forwarded, h)
	}

	candidates = append(candidates, VisibleThinkingBetas...)

	seen := map[string]bool{}
	var visible []string
	for _, raw := range candidates {
		beta := strings.TrimSpace(raw)
		if beta == "" {
			continue
		}
		normalized := strings.ToLower(beta)
		if normalized == RedactedThinkingBeta || IsFastModeBeta(beta) || seen[normalized] {
			continue
		}
		seen[normalized] = true
		visible = append(visible, beta)
	}

	if len(visible) > 0 {
		forwarded = append(forwarded, Header{Name: "Anthropic-Beta", Value: strings.Join(visible, ",")})
	}
	return forwarded
}

// headersStrippingFastMode is surgical: Anthropic-Beta is rewritten only when
// a fast-mode-* token is present; otherwise the input is returned unchanged.
func headersStrippingFastMode(headers []Header) []Header {
	didChange := false
	rewritten := make([]Header, 0, len(headers))
	for _, h := range headers {
		if !strings.EqualFold(h.Name, "anthropic-beta") {
			rewritten = append(rewritten, h)
			continue
		}

		parts := parseAnthropicBetas(h.Value)
		hasFastMode := false
		for _, p := range parts {
			if IsFastModeBeta(p) {
				hasFastMode = true
				break
			}
		}
		if !hasFastMode {
			rewritten = append(rewritten, h)
			continue
		}

		didChange = true
		var kept []string
		for _, raw := range parts {
			beta := strings.TrimSpace(raw)
			if beta == "" || IsFastModeBeta(beta) {
				continue
			}
			kept = append(kept, beta)
		}
		if len(kept) == 0 {
			continue
		}
		rewritten = append(rewritten, Header{Name: h.Name, Value: strings.Join(kept, ",")})
	}
	if didChange {
		return rewritten
	}
	return headers
}

// parseAnthropicBetas matches Swift's split(separator: ","), which omits empty
// subsequences.
func parseAnthropicBetas(value string) []string {
	var out []string
	for _, p := range strings.Split(value, ",") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
