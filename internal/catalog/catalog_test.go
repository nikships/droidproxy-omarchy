package catalog

import (
	"strings"
	"testing"

	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

func settingsEntry(t *testing.T, id string) map[string]any {
	t.Helper()
	for _, m := range SettingsModels(nil) {
		if m["id"] == id {
			return m
		}
	}
	return nil
}

func idsOf(models []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, m := range models {
		if id, ok := m["id"].(string); ok {
			out[id] = true
		}
	}
	return out
}

// From AppPreferencesFastModeDefaultsTests: Fast Mode is opt-in, so absent
// keys must read as false.
func TestFastModeDefaultsAreOff(t *testing.T) {
	isolateHome(t)
	for _, key := range []string{
		prefs.KeyGPT6AstraFastMode,
		prefs.KeyGPT61SolFastMode,
		prefs.KeyGPT61TerraFastMode,
		prefs.KeyGPT6LunaFastMode,
	} {
		if prefs.Shared().Has(key) {
			t.Fatalf("fast mode key %s should be unset by default", key)
		}
	}
	if prefs.GPT6AstraFastMode() || prefs.GPT61SolFastMode() || prefs.GPT61TerraFastMode() || prefs.GPT6LunaFastMode() {
		t.Fatal("unset fast mode keys must read as false")
	}
}

func TestMuseVariantsApplyMaxReasoning(t *testing.T) {
	for _, model := range []string{
		"muse-wonder-1.0", "muse-wonder-1.0-contributor",
		"muse-wonder-full-1.0", "muse-wonder-full-1.0-contributor",
	} {
		entry := MuseModel(model, model, model).SettingsEntry()
		if entry["model"] != model {
			t.Errorf("%s: model = %v", model, entry["model"])
		}
		if entry["defaultReasoningEffort"] != "max" {
			t.Errorf("%s: defaultReasoningEffort = %v", model, entry["defaultReasoningEffort"])
		}
		if entry["reasoningEffort"] != "max" {
			t.Errorf("%s: reasoningEffort = %v", model, entry["reasoningEffort"])
		}
		assertStringSlice(t, entry["supportedReasoningEfforts"],
			[]string{"minimal", "low", "medium", "high", "xhigh", "max"})
	}
}

func TestFable51ExposesFullEffortLevels(t *testing.T) {
	fable := mustEntry(t, "custom:droidproxy:fable-5-1")

	if fable["model"] != "claude-fable-5-1" {
		t.Errorf("model = %v", fable["model"])
	}
	if fable["enableThinking"] != true {
		t.Errorf("enableThinking = %v", fable["enableThinking"])
	}
	if fable["reasoningEffort"] != "xhigh" || fable["defaultReasoningEffort"] != "xhigh" {
		t.Errorf("efforts = %v / %v", fable["reasoningEffort"], fable["defaultReasoningEffort"])
	}
	assertStringSlice(t, fable["supportedReasoningEfforts"], []string{"low", "medium", "high", "xhigh", "max"})
	if fable["maxOutputTokens"] != 128000 {
		t.Errorf("maxOutputTokens = %v", fable["maxOutputTokens"])
	}
}

func TestOpus55ExposesFullEffortLevels(t *testing.T) {
	opus := mustEntry(t, "custom:droidproxy:opus-5-5")

	if opus["model"] != "claude-opus-5-5" {
		t.Errorf("model = %v", opus["model"])
	}
	if opus["displayName"] != "DroidProxy: Opus 5.5" {
		t.Errorf("displayName = %v", opus["displayName"])
	}
	if opus["provider"] != "anthropic" {
		t.Errorf("provider = %v", opus["provider"])
	}
	if opus["baseUrl"] != "http://localhost:8317" {
		t.Errorf("baseUrl = %v", opus["baseUrl"])
	}
	if opus["enableThinking"] != true {
		t.Errorf("enableThinking = %v", opus["enableThinking"])
	}
	if opus["reasoningEffort"] != "xhigh" || opus["defaultReasoningEffort"] != "xhigh" {
		t.Errorf("efforts = %v / %v", opus["reasoningEffort"], opus["defaultReasoningEffort"])
	}
	assertStringSlice(t, opus["supportedReasoningEfforts"], []string{"low", "medium", "high", "xhigh", "max"})
	if opus["maxOutputTokens"] != 128000 {
		t.Errorf("maxOutputTokens = %v", opus["maxOutputTokens"])
	}
}

func TestApplyWritesOnlyLatestOpusAndFableForClaudeProvider(t *testing.T) {
	// Apply/Re-apply serializes every enabled definition via SettingsModels;
	// Claude OAuth writes only the latest Opus (5.5) and Fable (5.1).
	claudeIDs := idsOf(SettingsModels(func(key string) bool { return key == "claude" }))
	if !claudeIDs["custom:droidproxy:opus-5-5"] || !claudeIDs["custom:droidproxy:fable-5-1"] {
		t.Fatalf("missing current Claude models: %v", claudeIDs)
	}
	for _, retired := range []string{"opus-5", "opus-4-8", "fable-5"} {
		if claudeIDs["custom:droidproxy:"+retired] {
			t.Errorf("retired model %s still registered", retired)
		}
	}

	opus := mustEntry(t, "custom:droidproxy:opus-5-5")
	if opus["model"] != "claude-opus-5-5" || opus["displayName"] != "DroidProxy: Opus 5.5" {
		t.Errorf("opus entry = %v / %v", opus["model"], opus["displayName"])
	}
}

func TestJunieExposesOnlyOpus55AndSol61(t *testing.T) {
	junieIDs := idsOf(SettingsModels(func(key string) bool { return key == "junie" }))
	if len(junieIDs) != 2 || !junieIDs["custom:droidproxy:junie-claude-opus-5-5"] || !junieIDs["custom:droidproxy:junie-gpt-6.1-sol"] {
		t.Fatalf("junie models = %v", junieIDs)
	}

	sol := mustEntry(t, "custom:droidproxy:junie-gpt-6.1-sol")
	if sol["model"] != "junie-gpt-6.1-sol" || sol["displayName"] != "DroidProxy: Junie Sol 6.1" {
		t.Errorf("sol entry = %v / %v", sol["model"], sol["displayName"])
	}
	if sol["provider"] != "openai" || sol["baseUrl"] != "http://localhost:8317/v1" {
		t.Errorf("sol route = %v / %v", sol["provider"], sol["baseUrl"])
	}
	if sol["defaultReasoningEffort"] != "xhigh" {
		t.Errorf("defaultReasoningEffort = %v", sol["defaultReasoningEffort"])
	}
	assertStringSlice(t, sol["supportedReasoningEfforts"], []string{"low", "medium", "high", "xhigh", "max"})
}

func TestSonnet55UsesNativeModelIDAndExposesFullLevels(t *testing.T) {
	sonnet := mustEntry(t, "custom:droidproxy:sonnet-5-5")
	for _, retired := range []string{"sonnet-5", "junie-claude-sonnet-5-5"} {
		if settingsEntry(t, "custom:droidproxy:"+retired) != nil {
			t.Errorf("retired model %s still registered", retired)
		}
	}

	if sonnet["model"] != "claude-sonnet-5-5" || sonnet["displayName"] != "DroidProxy: Sonnet 5.5" {
		t.Errorf("sonnet entry = %v / %v", sonnet["model"], sonnet["displayName"])
	}
	if sonnet["enableThinking"] != true {
		t.Errorf("enableThinking = %v", sonnet["enableThinking"])
	}
	if sonnet["reasoningEffort"] != "xhigh" || sonnet["defaultReasoningEffort"] != "xhigh" {
		t.Errorf("efforts = %v / %v", sonnet["reasoningEffort"], sonnet["defaultReasoningEffort"])
	}
	assertStringSlice(t, sonnet["supportedReasoningEfforts"], []string{"low", "medium", "high", "xhigh", "max"})
}

func TestHaiku55IsClaudeSubscriptionOnlyAndExposesFullLevels(t *testing.T) {
	haiku := mustEntry(t, "custom:droidproxy:haiku-5-5")

	if haiku["model"] != "claude-haiku-5-5" || haiku["displayName"] != "DroidProxy: Haiku 5.5" {
		t.Errorf("haiku entry = %v / %v", haiku["model"], haiku["displayName"])
	}
	if haiku["provider"] != "anthropic" || haiku["baseUrl"] != "http://localhost:8317" {
		t.Errorf("haiku route = %v / %v", haiku["provider"], haiku["baseUrl"])
	}
	if haiku["maxOutputTokens"] != 128000 || haiku["maxContextLimit"] != 1_000_000 {
		t.Errorf("limits = %v / %v", haiku["maxOutputTokens"], haiku["maxContextLimit"])
	}
	if haiku["reasoningEffort"] != "xhigh" || haiku["defaultReasoningEffort"] != "xhigh" {
		t.Errorf("efforts = %v / %v", haiku["reasoningEffort"], haiku["defaultReasoningEffort"])
	}
	assertStringSlice(t, haiku["supportedReasoningEfforts"], []string{"low", "medium", "high", "xhigh", "max"})

	var providers []string
	for _, d := range Definitions() {
		if strings.Contains(d.BaseModel, "haiku-5-5") {
			providers = append(providers, d.ProviderKey)
		}
	}
	if len(providers) != 1 || providers[0] != "claude" {
		t.Errorf("haiku providers = %v", providers)
	}
}

func TestGpt61SolTerraAndLunaUseNativeModelMetadata(t *testing.T) {
	if settingsEntry(t, "custom:droidproxy:gpt-6-sol") != nil {
		t.Error("retired gpt-6-sol still registered")
	}
	for slug, name := range map[string]string{"gpt-6.1-sol": "GPT 6.1 Sol", "gpt-6.1-terra": "GPT 6.1 Terra", "gpt-6-luna": "GPT 6 Luna"} {
		entry := mustEntry(t, "custom:droidproxy:"+slug)

		if entry["model"] != slug {
			t.Errorf("%s: model = %v", slug, entry["model"])
		}
		if entry["provider"] != "openai" {
			t.Errorf("%s: provider = %v", slug, entry["provider"])
		}
		if entry["baseUrl"] != "http://localhost:8317/v1" {
			t.Errorf("%s: baseUrl = %v", slug, entry["baseUrl"])
		}
		if entry["displayName"] != "DroidProxy: "+name {
			t.Errorf("%s: displayName = %v", slug, entry["displayName"])
		}
		if entry["maxOutputTokens"] != 128000 {
			t.Errorf("%s: maxOutputTokens = %v", slug, entry["maxOutputTokens"])
		}
		if entry["maxContextLimit"] != 272000 {
			t.Errorf("%s: maxContextLimit = %v", slug, entry["maxContextLimit"])
		}
		if entry["enableThinking"] != true {
			t.Errorf("%s: enableThinking = %v", slug, entry["enableThinking"])
		}
		if entry["reasoningEffort"] != "xhigh" || entry["defaultReasoningEffort"] != "xhigh" {
			t.Errorf("%s: efforts = %v / %v", slug, entry["reasoningEffort"], entry["defaultReasoningEffort"])
		}
		assertStringSlice(t, entry["supportedReasoningEfforts"], []string{"low", "medium", "high", "xhigh", "max"})
	}
}

func TestCodexProviderWritesOnlyGpt6Models(t *testing.T) {
	got := idsOf(SettingsModels(func(key string) bool { return key == "codex" }))
	want := map[string]bool{
		"custom:droidproxy:gpt-6-astra":   true,
		"custom:droidproxy:gpt-6.1-sol":   true,
		"custom:droidproxy:gpt-6.1-terra": true,
		"custom:droidproxy:gpt-6-luna":    true,
	}
	if len(got) != len(want) {
		t.Fatalf("codex models = %v", got)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("missing %s", id)
		}
	}
}

func TestGpt6AstraUsesNativeModelMetadata(t *testing.T) {
	astra := mustEntry(t, "custom:droidproxy:gpt-6-astra")

	if astra["model"] != "gpt-6-astra" {
		t.Errorf("model = %v", astra["model"])
	}
	if astra["provider"] != "openai" {
		t.Errorf("provider = %v", astra["provider"])
	}
	if astra["baseUrl"] != "http://localhost:8317/v1" {
		t.Errorf("baseUrl = %v", astra["baseUrl"])
	}
	if astra["displayName"] != "DroidProxy: GPT 6 Astra" {
		t.Errorf("displayName = %v", astra["displayName"])
	}
	if astra["maxOutputTokens"] != 128000 {
		t.Errorf("maxOutputTokens = %v", astra["maxOutputTokens"])
	}
	if astra["maxContextLimit"] != 272000 {
		t.Errorf("maxContextLimit = %v", astra["maxContextLimit"])
	}
	if astra["enableThinking"] != true {
		t.Errorf("enableThinking = %v", astra["enableThinking"])
	}
	if astra["reasoningEffort"] != "xhigh" || astra["defaultReasoningEffort"] != "xhigh" {
		t.Errorf("efforts = %v / %v", astra["reasoningEffort"], astra["defaultReasoningEffort"])
	}
	assertStringSlice(t, astra["supportedReasoningEfforts"], []string{"low", "medium", "high", "xhigh", "max"})
}

func TestKimiK3UsesMaxReasoningMetadata(t *testing.T) {
	kimi := mustEntry(t, "custom:droidproxy:kimi-k3")

	if kimi["model"] != "kimi-k3" {
		t.Errorf("model = %v", kimi["model"])
	}
	if kimi["provider"] != "openai" {
		t.Errorf("provider = %v", kimi["provider"])
	}
	if kimi["baseUrl"] != "http://localhost:8317/v1" {
		t.Errorf("baseUrl = %v", kimi["baseUrl"])
	}
	if kimi["displayName"] != "DroidProxy: Kimi K3" {
		t.Errorf("displayName = %v", kimi["displayName"])
	}
	if kimi["maxOutputTokens"] != 65536 {
		t.Errorf("maxOutputTokens = %v", kimi["maxOutputTokens"])
	}
	if kimi["enableThinking"] != true {
		t.Errorf("enableThinking = %v", kimi["enableThinking"])
	}
	if kimi["reasoningEffort"] != "max" || kimi["defaultReasoningEffort"] != "max" {
		t.Errorf("efforts = %v / %v", kimi["reasoningEffort"], kimi["defaultReasoningEffort"])
	}
	assertStringSlice(t, kimi["supportedReasoningEfforts"], []string{"max"})

	modelsWithoutKimi := SettingsModels(func(key string) bool { return key != "kimi" })
	if idsOf(modelsWithoutKimi)["custom:droidproxy:kimi-k3"] {
		t.Error("kimi-k3 must be filtered out when kimi is disabled")
	}
}

func TestKimiK31ProUsesMaxReasoningMetadata(t *testing.T) {
	for _, slug := range []string{"kimi-k3.1-pro", "kimi-k3.1-pro-highspeed"} {
		kimi := mustEntry(t, "custom:droidproxy:"+slug)
		if kimi["model"] != slug {
			t.Errorf("%s: model = %v", slug, kimi["model"])
		}
		if kimi["provider"] != "openai" {
			t.Errorf("%s: provider = %v", slug, kimi["provider"])
		}
		if kimi["maxOutputTokens"] != 65536 {
			t.Errorf("%s: maxOutputTokens = %v, want 65536 (registry max_completion_tokens)", slug, kimi["maxOutputTokens"])
		}
		if kimi["reasoningEffort"] != "max" || kimi["defaultReasoningEffort"] != "max" {
			t.Errorf("%s: efforts = %v / %v", slug, kimi["reasoningEffort"], kimi["defaultReasoningEffort"])
		}
		assertStringSlice(t, kimi["supportedReasoningEfforts"], []string{"max"})
	}

	k26 := mustEntry(t, "custom:droidproxy:kimi-k2.6")
	if k26["maxOutputTokens"] != 65536 {
		t.Errorf("kimi-k2.6: maxOutputTokens = %v, want 65536 (262144 is its context length)", k26["maxOutputTokens"])
	}
}

func TestGemini38FlashHighUsesAntigravityModelMetadata(t *testing.T) {
	gemini := mustEntry(t, "custom:droidproxy:gemini-3.8-flash-high")

	if gemini["model"] != "gemini-3.8-flash-high" {
		t.Errorf("model = %v", gemini["model"])
	}
	if gemini["provider"] != "openai" {
		t.Errorf("provider = %v", gemini["provider"])
	}
	if gemini["baseUrl"] != "http://localhost:8317/v1" {
		t.Errorf("baseUrl = %v", gemini["baseUrl"])
	}
	if gemini["displayName"] != "DroidProxy: Antigravity: Gemini 3.8 Flash (High)" {
		t.Errorf("displayName = %v", gemini["displayName"])
	}
	if gemini["maxOutputTokens"] != 65536 {
		t.Errorf("maxOutputTokens = %v", gemini["maxOutputTokens"])
	}
	if gemini["enableThinking"] != true {
		t.Errorf("enableThinking = %v", gemini["enableThinking"])
	}
	if gemini["reasoningEffort"] != "high" || gemini["defaultReasoningEffort"] != "high" {
		t.Errorf("efforts = %v / %v", gemini["reasoningEffort"], gemini["defaultReasoningEffort"])
	}
	assertStringSlice(t, gemini["supportedReasoningEfforts"], []string{"high"})
}

func TestGemini4ArgonUsesAntigravityModelMetadata(t *testing.T) {
	gemini := mustEntry(t, "custom:droidproxy:gemini-4-argon-high")

	if gemini["model"] != "gemini-4-argon-high" {
		t.Errorf("model = %v", gemini["model"])
	}
	if gemini["provider"] != "openai" || gemini["baseUrl"] != "http://localhost:8317/v1" {
		t.Errorf("route = %v / %v", gemini["provider"], gemini["baseUrl"])
	}
	if gemini["displayName"] != "DroidProxy: Antigravity: Gemini 4 Argon" {
		t.Errorf("displayName = %v", gemini["displayName"])
	}
	if gemini["maxOutputTokens"] != 65536 {
		t.Errorf("maxOutputTokens = %v", gemini["maxOutputTokens"])
	}
	if _, ok := gemini["maxContextLimit"]; ok {
		t.Errorf("maxContextLimit = %v, want unset", gemini["maxContextLimit"])
	}
	if gemini["reasoningEffort"] != "high" || gemini["defaultReasoningEffort"] != "high" {
		t.Errorf("efforts = %v / %v", gemini["reasoningEffort"], gemini["defaultReasoningEffort"])
	}
	assertStringSlice(t, gemini["supportedReasoningEfforts"], []string{"low", "medium", "high"})
}

func TestAntigravityClaudeModelMetadata(t *testing.T) {
	for _, m := range []struct {
		slug, displayName string
		maxOutput         int
		context           int
	}{
		{"ag-c55s-high", "Claude Sonnet 5.5 (High)", 128000, 1_000_000},
		{"ag-c55o-high", "Claude Opus 5.5 (High)", 128000, 1_000_000},
		{"ag-c46s-thinking", "Claude Sonnet 4.6 (Thinking)", 64000, 0},
		{"ag-c46o-thinking", "Claude Opus 4.6 (Thinking)", 64000, 0},
	} {
		entry := mustEntry(t, "custom:droidproxy:"+m.slug)
		if entry["model"] != m.slug {
			t.Errorf("%s: model = %v", m.slug, entry["model"])
		}
		if entry["provider"] != "openai" || entry["baseUrl"] != "http://localhost:8317/v1" {
			t.Errorf("%s: route = %v / %v", m.slug, entry["provider"], entry["baseUrl"])
		}
		if entry["displayName"] != "DroidProxy: Antigravity: "+m.displayName {
			t.Errorf("%s: displayName = %v", m.slug, entry["displayName"])
		}
		if entry["maxOutputTokens"] != m.maxOutput {
			t.Errorf("%s: maxOutputTokens = %v", m.slug, entry["maxOutputTokens"])
		}
		context, hasContext := entry["maxContextLimit"]
		if (m.context == 0 && hasContext) || (m.context != 0 && context != m.context) {
			t.Errorf("%s: maxContextLimit = %v", m.slug, context)
		}
		assertStringSlice(t, entry["supportedReasoningEfforts"], []string{"high"})
	}
	for _, retired := range []string{"antigravity-gemini-3.1-pro", "gemini-3.1-pro-low", "gpt-oss-120b-medium"} {
		if settingsEntry(t, "custom:droidproxy:"+retired) != nil {
			t.Errorf("retired model %s still registered", retired)
		}
	}
}

func TestGrok47UsesOpenAIProviderAndApiXAIProxy(t *testing.T) {
	grok := mustEntry(t, "custom:droidproxy:grok-4.7")

	if grok["model"] != "grok-4.7" {
		t.Errorf("model = %v", grok["model"])
	}
	if grok["provider"] != "openai" {
		t.Errorf("provider = %v", grok["provider"])
	}
	if grok["baseUrl"] != "http://localhost:8317/v1" {
		t.Errorf("baseUrl = %v", grok["baseUrl"])
	}
	if grok["displayName"] != "DroidProxy: Grok 4.7" {
		t.Errorf("displayName = %v", grok["displayName"])
	}
	assertStringSlice(t, grok["supportedReasoningEfforts"], []string{"low", "medium", "high", "xhigh"})
	if grok["defaultReasoningEffort"] != "xhigh" || grok["reasoningEffort"] != "xhigh" {
		t.Errorf("efforts = %v / %v", grok["defaultReasoningEffort"], grok["reasoningEffort"])
	}
	if grok["maxContextLimit"] != 500000 {
		t.Errorf("maxContextLimit = %v", grok["maxContextLimit"])
	}
}

func TestGrok47FastUsesBuildProxyModelID(t *testing.T) {
	grok := mustEntry(t, "custom:droidproxy:grok-4.7-build-fast")

	if grok["model"] != "grok-4.7-build-fast" {
		t.Errorf("model = %v", grok["model"])
	}
	if grok["provider"] != "openai" {
		t.Errorf("provider = %v", grok["provider"])
	}
	if grok["baseUrl"] != "http://localhost:8317/v1" {
		t.Errorf("baseUrl = %v", grok["baseUrl"])
	}
	if grok["displayName"] != "DroidProxy: Grok 4.7 Fast" {
		t.Errorf("displayName = %v", grok["displayName"])
	}
	assertStringSlice(t, grok["supportedReasoningEfforts"], []string{"low", "medium", "high", "xhigh"})
	if grok["defaultReasoningEffort"] != "xhigh" || grok["reasoningEffort"] != "xhigh" {
		t.Errorf("efforts = %v / %v", grok["defaultReasoningEffort"], grok["reasoningEffort"])
	}
	if grok["maxContextLimit"] != 500000 {
		t.Errorf("maxContextLimit = %v", grok["maxContextLimit"])
	}
}

func TestGrokProviderModelsAreRegistered(t *testing.T) {
	grokModels := SettingsModels(func(key string) bool { return key == "grok" })
	var got []string
	for _, m := range grokModels {
		got = append(got, m["id"].(string))
	}
	want := []string{"custom:droidproxy:grok-4.7", "custom:droidproxy:grok-4.7-build-fast"}
	if len(got) != len(want) {
		t.Fatalf("grok models = %v", got)
	}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("grok models[%d] = %s, want %s", i, got[i], id)
		}
	}
}

func TestGrokContextLimitsMatchXAIDocs(t *testing.T) {
	grok := mustEntry(t, "custom:droidproxy:grok-4.7")
	if grok["maxContextLimit"] != 500000 {
		t.Errorf("maxContextLimit = %v", grok["maxContextLimit"])
	}
}

func TestCursorModelsAreNotRegistered(t *testing.T) {
	for _, id := range []string{
		"custom:droidproxy:cursor-composer-2.5",
		"custom:droidproxy:cursor-grok-4.6",
		"custom:droidproxy:cursor-grok-4.6-fast",
		"custom:droidproxy:cursor-grok-4.5",
		"custom:droidproxy:grok-4.5",
		"custom:droidproxy:grok-4.6",
	} {
		if settingsEntry(t, id) != nil {
			t.Errorf("%s must not be registered", id)
		}
	}
}

// The macOS app exposes Muse Wonder only with a usable key; here the checker is
// injected, so verify both the gating and the Contributor Mode exclusivity.
func TestMetaContributorModeExclusivity(t *testing.T) {
	isolateHome(t)
	defer SetMetaKeyChecker(nil)

	SetMetaKeyChecker(func() bool { return false })
	for _, m := range SettingsModels(nil) {
		if id, _ := m["id"].(string); strings.Contains(id, "muse-wonder") {
			t.Error("muse models must be hidden without a usable key")
		}
	}

	SetMetaKeyChecker(func() bool { return true })
	defer func() { prefs.Shared().Delete(prefs.KeyMetaContributorMode) }()

	museIDs := func() map[string]bool {
		out := map[string]bool{}
		for _, m := range SettingsModels(nil) {
			if id, ok := m["id"].(string); ok && strings.Contains(id, "muse-wonder") {
				out[id] = true
			}
		}
		return out
	}

	prefs.Shared().Set(prefs.KeyMetaContributorMode, false)
	got := museIDs()
	if len(got) != 2 || !got["custom:droidproxy:muse-wonder-1.0"] || !got["custom:droidproxy:muse-wonder-full-1.0"] {
		t.Errorf("non-contributor muse models = %v", got)
	}

	prefs.Shared().Set(prefs.KeyMetaContributorMode, true)
	got = museIDs()
	if len(got) != 2 || !got["custom:droidproxy:muse-wonder-1.0-contributor"] || !got["custom:droidproxy:muse-wonder-full-1.0-contributor"] {
		t.Errorf("contributor muse models = %v", got)
	}
}

func mustEntry(t *testing.T, id string) map[string]any {
	t.Helper()
	entry := settingsEntry(t, id)
	if entry == nil {
		t.Fatalf("no settings entry for %s", id)
	}
	return entry
}

func assertStringSlice(t *testing.T, got any, want []string) {
	t.Helper()
	slice, ok := got.([]string)
	if !ok {
		t.Fatalf("expected []string, got %T (%v)", got, got)
	}
	if len(slice) != len(want) {
		t.Fatalf("got %v, want %v", slice, want)
	}
	for i := range want {
		if slice[i] != want[i] {
			t.Fatalf("got %v, want %v", slice, want)
		}
	}
}
