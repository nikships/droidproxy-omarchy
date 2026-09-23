package proxy

import (
	"net"
	"strings"

	"github.com/nikships/droidproxy-omarchy/internal/claude"
	"github.com/nikships/droidproxy-omarchy/internal/grok"
	"github.com/nikships/droidproxy-omarchy/internal/logx"
	"github.com/nikships/droidproxy-omarchy/internal/meta"
	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

var antigravityModelAliases = map[string]string{
	"ag-c46s-thinking": "claude-sonnet-4-6",
	"ag-c46o-thinking": "claude-opus-4-6-thinking",
}

var responsesAPIPaths = map[string]bool{
	"/v1/responses":     true,
	"/api/v1/responses": true,
}

var fastTierEligibleResponsePaths = map[string]bool{
	"/v1/responses":     true,
	"/api/v1/responses": true,
}

func (p *Proxy) processRequest(data []byte, conn net.Conn) {
	requestString := string(data)
	lines := strings.Split(requestString, "\r\n")
	if len(lines) == 0 {
		sendError(conn, 400, "Invalid request line")
		return
	}
	requestLine := lines[0]
	parts := strings.Split(requestLine, " ")
	if len(parts) < 3 {
		sendError(conn, 400, "Invalid request format")
		return
	}
	method, path, httpVersion := parts[0], parts[1], parts[2]
	logx.Logf("[ThinkingProxy] Incoming request: %s %s", method, path)

	var headers [][2]string
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		value := strings.TrimSpace(line[colon+1:])
		headers = append(headers, [2]string{name, value})
	}

	sep := strings.Index(requestString, "\r\n\r\n")
	if sep < 0 {
		logx.Logf("[ThinkingProxy] Error: Could not find body separator in request")
		sendError(conn, 400, "Invalid request format - no body separator")
		return
	}
	bodyString := requestString[sep+4:]

	rewrittenPath := path
	modifiedBody := bodyString
	var fields requestJSONFields
	var hasFields bool
	if bodyString != "" {
		fields, hasFields = inspectRequestJSONFields(bodyString)
	}

	if method == "POST" && bodyString != "" {
		logx.Debugf("INCOMING REQUEST: %s %s", method, rewrittenPath)
		if result, ok := rewriteAntigravityModelAlias(modifiedBody, fields, hasFields); ok {
			modifiedBody = result
			fields, hasFields = inspectRequestJSONFields(modifiedBody)
		}
		if isGrokModel(fields, hasFields) {
			if !prefs.IsProviderEnabled("grok") {
				logx.Logf("[ThinkingProxy] Warning: Grok model requested but the provider is disabled in settings.")
				sendError(conn, 400, "Grok provider is disabled in DroidProxy settings.")
				return
			}
			grokBody := string(grok.SanitizeRequestBody([]byte(modifiedBody)))
			if grokBody != modifiedBody {
				logx.Debugf("SANITIZED GROK: remapped custom tools/calls and dropped unsupported fields before Grok upstream")
			}
			p.forwardToGrok(method, rewrittenPath, httpVersion, headers, grokBody, fieldsModel(fields, hasFields), conn)
			return
		}
		if isMetaModel(fields, hasFields) {
			if !prefs.IsProviderEnabled("meta") {
				logx.Logf("[ThinkingProxy] Warning: Meta Muse model requested but the provider is disabled in settings.")
				sendError(conn, 400, "Meta Muse provider is disabled in DroidProxy settings.")
				return
			}
			model := fieldsModel(fields, hasFields)
			if meta.ShouldTLSForward(model, rewrittenPath) {
				p.forwardToMeta(method, rewrittenPath, httpVersion, headers, modifiedBody, conn)
				return
			}
		}
		if result, ok := processOpenAIFastMode(modifiedBody, rewrittenPath, fields, hasFields); ok {
			modifiedBody = result
			fields, hasFields = inspectRequestJSONFields(modifiedBody)
		}
		if hasFields && fields.hasModel && isClaudeModel(fields.model) {
			sanitizedBody := claude.SanitizeThinkingBlocks(modifiedBody)
			if sanitizedBody != modifiedBody {
				logx.Debugf("SANITIZED CLAUDE THINKING BLOCKS: stripped stale assistant thinking before forwarding")
				modifiedBody = sanitizedBody
				fields, hasFields = inspectRequestJSONFields(modifiedBody)
			}
		}
		if summary := reasoningSummaryLog(modifiedBody, fields); summary != "" {
			logx.Debugf("REQUEST REASONING: %s", summary)
		}
	}

	if isResponsesAPIPath(rewrittenPath) && isOAuthCodeAssistGeminiModel(fields, hasFields) {
		newPath := strings.Replace(rewrittenPath, "/responses", "/chat/completions", 1)
		logx.Logf("[ThinkingProxy] Rewriting OAuth-Gemini responses path: %s -> %s", rewrittenPath, newPath)
		logx.Debugf("REWRITE PATH: %s -> %s (OAuth Code Assist Gemini model)", rewrittenPath, newPath)
		rewrittenPath = newPath
	}

	forwardHeaders := headersForForwarding(headers, fields, hasFields)
	p.forwardRequest(method, rewrittenPath, httpVersion, forwardHeaders, modifiedBody, conn)
}

func fieldsModel(fields requestJSONFields, hasFields bool) string {
	if hasFields && fields.hasModel {
		return fields.model
	}
	return ""
}

func headersForForwarding(headers [][2]string, fields requestJSONFields, hasFields bool) [][2]string {
	if !hasFields || !fields.hasModel || !isClaudeModel(fields.model) {
		return headers
	}
	visibleThinking := shouldRequestVisibleClaudeThinking(fields, hasFields)
	if visibleThinking {
		logx.Debugf("CLAUDE visible thinking enabled: removing %s from Anthropic-Beta", claude.RedactedThinkingBeta)
	}
	for _, h := range headers {
		if strings.EqualFold(h[0], "anthropic-beta") {
			for _, part := range strings.Split(h[1], ",") {
				if claude.IsFastModeBeta(part) {
					logx.Logf("[ThinkingProxy] Stripped fast-mode beta from Anthropic-Beta so seat-cap 429s can fail over")
					logx.Debugf("CLAUDE stripped fast-mode beta from Anthropic-Beta so seat-cap 429s can fail over")
					break
				}
			}
		}
	}
	claudeHeaders := make([]claude.Header, len(headers))
	for i, h := range headers {
		claudeHeaders[i] = claude.Header{Name: h[0], Value: h[1]}
	}
	rewritten := claude.RewriteAnthropicBeta(claudeHeaders, visibleThinking)
	out := make([][2]string, len(rewritten))
	for i, h := range rewritten {
		out[i] = [2]string{h.Name, h.Value}
	}
	return out
}

func shouldRequestVisibleClaudeThinking(fields requestJSONFields, hasFields bool) bool {
	if !hasFields || !fields.hasModel || !isClaudeModel(fields.model) || !fields.hasThinkingType {
		return false
	}
	switch fields.thinkingType {
	case "enabled", "adaptive", "auto":
		return true
	default:
		return false
	}
}

func isClaudeModel(model string) bool {
	return strings.HasPrefix(model, "claude-") || strings.HasPrefix(model, "gemini-claude-")
}

func rewriteAntigravityModelAlias(jsonString string, fields requestJSONFields, hasFields bool) (string, bool) {
	if !hasFields || !fields.hasModel {
		return "", false
	}
	backendModel, ok := antigravityModelAliases[fields.model]
	if !ok {
		return "", false
	}
	result := jsonString[:fields.modelLoc.valueLo] + `"` + backendModel + `"` + jsonString[fields.modelLoc.valueHi:]
	logx.Debugf("REWRITE MODEL: %s -> %s (Antigravity alias)", fields.model, backendModel)
	return result, true
}

func isResponsesAPIPath(path string) bool {
	normalized := path
	if i := strings.IndexByte(path, '?'); i >= 0 {
		normalized = path[:i]
	}
	return responsesAPIPaths[normalized]
}

func isOAuthCodeAssistGeminiModel(fields requestJSONFields, hasFields bool) bool {
	if !hasFields || !fields.hasModel {
		return false
	}
	return strings.HasPrefix(fields.model, "gemini-") && strings.HasSuffix(fields.model, "-preview")
}

func processOpenAIFastMode(jsonString, path string, fields requestJSONFields, hasFields bool) (string, bool) {
	normalized := path
	if i := strings.IndexByte(path, '?'); i >= 0 {
		normalized = path[:i]
	}
	if !fastTierEligibleResponsePaths[normalized] {
		return "", false
	}
	if !hasFields || !fields.hasModel {
		return "", false
	}
	switch fields.model {
	case "gpt-6-astra":
		if !prefs.GPT6AstraFastMode() {
			return "", false
		}
	case "gpt-6-sol":
		if !prefs.GPT6SolFastMode() {
			return "", false
		}
	case "gpt-6-luna":
		if !prefs.GPT6LunaFastMode() {
			return "", false
		}
	default:
		return "", false
	}
	if fields.hasServiceTier {
		return "", false
	}
	result := jsonString[:fields.modelLoc.pairHi] + `,"service_tier":"priority"` + jsonString[fields.modelLoc.pairHi:]
	logx.Logf("[ThinkingProxy] Injected service_tier=priority for model '%s' on path %s", fields.model, path)
	logx.Debugf("INJECTED service_tier=priority for model %s", fields.model)
	return result, true
}

func isGrokModel(fields requestJSONFields, hasFields bool) bool {
	return hasFields && fields.hasModel && strings.HasPrefix(fields.model, "grok-")
}

func isMetaModel(fields requestJSONFields, hasFields bool) bool {
	return hasFields && fields.hasModel && meta.IsMetaModel(fields.model)
}
