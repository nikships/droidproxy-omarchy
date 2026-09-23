// Package ports the macOS Meta Muse support: credential storage, the
// device-code login / API-key minting flow, and the upstream routing helpers.
package meta

import (
	"strings"

	"github.com/nikships/droidproxy-omarchy/internal/grok"
)

// Muse Spark is Responses-native. The Muse Code CLI posts /v1/responses with
// reasoning.effort and include: ["reasoning.encrypted_content"] so later turns
// can resume thinking. CLIProxyAPI's generic openai-compatibility executor
// translates /v1/responses into /chat/completions (chatcmpl-* ids), which
// drops encrypted reasoning and breaks long agent runs.
//
// ThinkingProxy TLS-forwards Muse Responses traffic straight to api.meta.ai.
// Chat Completions still use the compatibility block so multi-account failover
// keeps working for clients that already speak Completions.

// APIHost is the Meta Model API host.
const APIHost = "api.meta.ai"

// headersForForwarding merges Grok's header filter with the Muse User-Agent:
// Meta rejects reasoning.effort: "max" without a Muse client User-Agent.
// Droid overrides custom-model extraHeaders, so this is set at the TLS boundary.
func HeadersForForwarding(headers [][2]string) [][2]string {
	filtered := grok.FilterClientHeaders(headers)
	var nativeUserAgent string
	for _, h := range filtered {
		if strings.EqualFold(h[0], "User-Agent") && strings.HasPrefix(h[1], "muse-build/") {
			nativeUserAgent = h[1]
			break
		}
	}
	out := make([][2]string, 0, len(filtered)+1)
	for _, h := range filtered {
		if strings.EqualFold(h[0], "User-Agent") {
			continue
		}
		out = append(out, h)
	}
	if nativeUserAgent == "" {
		nativeUserAgent = "muse-build/1.3.0"
	}
	return append(out, [2]string{"User-Agent", nativeUserAgent})
}

// IsMetaModel reports whether the model is Muse Spark (or a variant).
func IsMetaModel(model string) bool {
	return model == "muse-spark-1.3" || strings.HasPrefix(model, "muse-spark-1.3-")
}

// IsResponsesPath reports whether the request path targets a Responses endpoint.
func IsResponsesPath(path string) bool {
	pathOnly := path
	if i := strings.IndexByte(path, '?'); i >= 0 {
		pathOnly = path[:i]
	}
	return strings.Contains(pathOnly, "/responses")
}

// ShouldTLSForward reports whether this request bypasses CLIProxyAPI entirely.
func ShouldTLSForward(model, path string) bool {
	return IsMetaModel(model) && IsResponsesPath(path)
}

// UpstreamPath normalizes the client path using the Grok normalization
// (strip /api/v1, ensure /v1 prefix).
func UpstreamPath(path string) string {
	return grok.NormalizeUpstreamPath(path)
}
