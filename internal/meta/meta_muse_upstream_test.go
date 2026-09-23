package meta

import "testing"

func TestReplacesFactoryUserAgentForMaxReasoning(t *testing.T) {
	headers := HeadersForForwarding([][2]string{
		{"user-agent", "factory-cli/0.223.0"},
		{"Authorization", "Bearer test-client-key"},
		{"Accept", "text/event-stream"},
	})
	want := [][2]string{
		{"Accept", "text/event-stream"},
		{"User-Agent", "muse-build/1.3.0"},
	}
	if len(headers) != len(want) {
		t.Fatalf("headers = %v, want %v", headers, want)
	}
	for i := range headers {
		if headers[i] != want[i] {
			t.Errorf("header %d = %v, want %v", i, headers[i], want[i])
		}
	}
}

func TestAddsMuseUserAgentWhenMissing(t *testing.T) {
	headers := HeadersForForwarding(nil)
	if len(headers) != 1 || headers[0] != [2]string{"User-Agent", "muse-build/1.3.0"} {
		t.Errorf("headers = %v", headers)
	}
}

func TestPreservesNativeMuseUserAgentWithoutDuplicates(t *testing.T) {
	nativeUserAgent := "muse-build/1.3.0 (non-interactive; macos-aarch64)"
	headers := HeadersForForwarding([][2]string{
		{"USER-AGENT", "factory-cli/0.223.0"},
		{"User-Agent", nativeUserAgent},
	})
	if len(headers) != 1 || headers[0] != [2]string{"User-Agent", nativeUserAgent} {
		t.Errorf("headers = %v", headers)
	}
}

func TestRecognizesMuseSparkModels(t *testing.T) {
	cases := map[string]bool{
		"muse-spark-1.3":             true,
		"muse-spark-1.3-contributor": true,
		"muse-spark-1.2":             false,
		"gpt-6-sol":                  false,
		"grok-4.6":                   false,
		"":                           false,
	}
	for model, want := range cases {
		if IsMetaModel(model) != want {
			t.Errorf("IsMetaModel(%q) = %v, want %v", model, !want, want)
		}
	}
}

func TestDetectsResponsesPathsIncludingCompact(t *testing.T) {
	yes := []string{"/v1/responses", "/api/v1/responses", "/v1/responses?stream=true", "/v1/responses/compact"}
	no := []string{"/v1/chat/completions", "/v1/models"}
	for _, p := range yes {
		if !IsResponsesPath(p) {
			t.Errorf("IsResponsesPath(%q) = false", p)
		}
	}
	for _, p := range no {
		if IsResponsesPath(p) {
			t.Errorf("IsResponsesPath(%q) = true", p)
		}
	}
}

func TestTLSForwardsOnlyMuseResponses(t *testing.T) {
	if !ShouldTLSForward("muse-spark-1.3-contributor", "/v1/responses") {
		t.Error("Muse responses must be TLS-forwarded")
	}
	if !ShouldTLSForward("muse-spark-1.3", "/api/v1/responses") {
		t.Error("Muse responses must be TLS-forwarded")
	}
	if ShouldTLSForward("muse-spark-1.3-contributor", "/v1/chat/completions") {
		t.Error("completions must go through the compatibility block")
	}
	if ShouldTLSForward("gpt-5.4", "/v1/responses") {
		t.Error("non-Muse models must not be forwarded")
	}
}

func TestUpstreamPathMatchesGrokNormalization(t *testing.T) {
	if UpstreamPath("/v1/responses") != "/v1/responses" {
		t.Error("UpstreamPath mismatch")
	}
	if UpstreamPath("/api/v1/responses") != "/v1/responses" {
		t.Error("UpstreamPath mismatch")
	}
	if UpstreamPath("/v1/responses/compact") != "/v1/responses/compact" {
		t.Error("UpstreamPath mismatch")
	}
	if APIHost != "api.meta.ai" {
		t.Errorf("APIHost = %q", APIHost)
	}
}
