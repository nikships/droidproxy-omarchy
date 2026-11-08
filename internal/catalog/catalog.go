// Package catalog is the authoritative list of models DroidProxy exposes to
// Factory's Droid CLI, plus the logic that merges them into
// ~/.factory/settings.json as custom models.
//
// Reasoning effort is owned by Droid CLI, not the proxy: every entry carries
// Factory's native reasoning metadata (enableThinking,
// supportedReasoningEfforts, defaultReasoningEffort, reasoningEffort) so
// Droid's per-session selector exposes every level the model supports.
//
// GitHub Copilot models are not part of the Linux port.
package catalog

import (
	"sync"

	"github.com/nikships/droidproxy-omarchy/internal/prefs"
)

// Kind groups models by upstream family. It only affects the Factory display
// name prefix.
type Kind int

const (
	KindClaudeAdaptive Kind = iota
	KindCodex
	KindKimi
	KindAntigravity
	KindJunie
	KindGrok
	KindMeta
)

// ThinkingLevel is one selectable reasoning effort.
type ThinkingLevel struct {
	Value       string
	DisplayName string
}

// ModelDefinition describes one DroidProxy custom model.
type ModelDefinition struct {
	BaseModel       string
	IDSlug          string
	DisplayName     string
	MaxOutputTokens int
	// MaxContextLimit is Factory's optional maxContextLimit; 0 omits it.
	MaxContextLimit int
	Provider        string
	// ProviderKey is matched against auth.ServiceTypeFromAuthFileType to
	// decide whether the model's provider is enabled.
	ProviderKey       string
	BaseURL           string
	Kind              Kind
	Levels            []ThinkingLevel
	DefaultLevelValue string
	NoImageSupport    bool
}

// SimpleID is the Factory custom model id ("custom:droidproxy:<slug>").
func (d ModelDefinition) SimpleID() string {
	return "custom:droidproxy:" + d.IDSlug
}

func (d ModelDefinition) settingsDisplayName() string {
	switch d.Kind {
	case KindAntigravity:
		return "Antigravity: " + d.DisplayName
	case KindMeta:
		return "Meta: " + d.DisplayName
	}
	return d.DisplayName
}

// SettingsEntry is the entry for Factory's customModels schema.
//
// All reasoning metadata is explicit so Droid/Factory does not need to infer
// supported effort levels from built-in model defaults.
func (d ModelDefinition) SettingsEntry() map[string]any {
	entry := map[string]any{
		"model":           d.BaseModel,
		"id":              d.SimpleID(),
		"baseUrl":         d.BaseURL,
		"apiKey":          "dummy-not-used",
		"displayName":     "DroidProxy: " + d.settingsDisplayName(),
		"maxOutputTokens": d.MaxOutputTokens,
		"noImageSupport":  d.NoImageSupport,
		"provider":        d.Provider,
	}
	if d.MaxContextLimit != 0 {
		entry["maxContextLimit"] = d.MaxContextLimit
	}
	if len(d.Levels) == 0 {
		return entry
	}
	efforts := make([]string, len(d.Levels))
	for i, l := range d.Levels {
		efforts[i] = l.Value
	}
	entry["enableThinking"] = true
	entry["supportedReasoningEfforts"] = efforts
	entry["defaultReasoningEffort"] = d.DefaultLevelValue
	if len(d.Levels) == 1 {
		entry["reasoningEffort"] = d.Levels[0].Value
	} else {
		entry["reasoningEffort"] = d.DefaultLevelValue
	}
	return entry
}

// GrokFastModelID is the SuperGrok fast model served only by
// cli-chat-proxy.grok.com (GrokAuth.fastModelID in the macOS app).
const GrokFastModelID = "grok-4.7-build-fast"

var (
	levelMinimal = ThinkingLevel{"minimal", "Minimal"}
	levelLow     = ThinkingLevel{"low", "Low"}
	levelMedium  = ThinkingLevel{"medium", "Medium"}
	levelHigh    = ThinkingLevel{"high", "High"}
	levelXHigh   = ThinkingLevel{"xhigh", "xHigh"}
	levelMax     = ThinkingLevel{"max", "Max"}
)

func claudeAdvancedLevels() []ThinkingLevel {
	return []ThinkingLevel{levelLow, levelMedium, levelHigh, levelXHigh, levelMax}
}

func codexLevels() []ThinkingLevel {
	return []ThinkingLevel{levelLow, levelMedium, levelHigh, levelXHigh}
}

func gpt6Levels() []ThinkingLevel {
	return []ThinkingLevel{levelLow, levelMedium, levelHigh, levelXHigh, levelMax}
}

// museLevels is every reasoning effort api.meta.ai accepts for Muse Wonder
// (verified live for both variants: minimal through max return 200; none and
// ultra return 400, so they are not advertised).
func museLevels() []ThinkingLevel {
	return []ThinkingLevel{levelMinimal, levelLow, levelMedium, levelHigh, levelXHigh, levelMax}
}

// MuseModel builds a Muse Wonder model or its cheaper/faster "contributor"
// companion. Completions go through CLIProxyAPI's openai-compatibility
// passthrough; Responses are TLS-forwarded by ThinkingProxy to
// https://api.meta.ai/v1 so encrypted reasoning persists across turns.
func MuseModel(baseModel, idSlug, displayName string) ModelDefinition {
	return ModelDefinition{
		BaseModel:         baseModel,
		IDSlug:            idSlug,
		DisplayName:       displayName,
		MaxOutputTokens:   256_000,
		MaxContextLimit:   1_048_576,
		Provider:          "openai",
		ProviderKey:       "meta",
		BaseURL:           "http://localhost:8317/v1",
		Kind:              KindMeta,
		Levels:            museLevels(),
		DefaultLevelValue: "max",
	}
}

// antigravityModel defaults: 65536 output tokens, no context limit, [high],
// default "high".
func antigravityModel(baseModel, idSlug, displayName string, maxOutputTokens, maxContextLimit int, levels []ThinkingLevel, defaultLevel string) ModelDefinition {
	if maxOutputTokens == 0 {
		maxOutputTokens = 65536
	}
	if levels == nil {
		levels = []ThinkingLevel{levelHigh}
	}
	if defaultLevel == "" {
		defaultLevel = "high"
	}
	return ModelDefinition{
		BaseModel:         baseModel,
		IDSlug:            idSlug,
		DisplayName:       displayName,
		MaxOutputTokens:   maxOutputTokens,
		MaxContextLimit:   maxContextLimit,
		Provider:          "openai",
		ProviderKey:       "antigravity",
		BaseURL:           "http://localhost:8317/v1",
		Kind:              KindAntigravity,
		Levels:            levels,
		DefaultLevelValue: defaultLevel,
	}
}

var (
	metaKeyMu      sync.RWMutex
	metaKeyChecker = func() bool { return false }
)

// SetMetaKeyChecker installs the function that reports whether Meta Muse has
// an enabled account with an unexpired, non-empty Model API key
// (MetaMuseCredentialStore.hasUsableAPIKey). Until it is set, Muse models are
// never listed. It is injected rather than imported so the catalog does not
// depend on the Meta credential store.
func SetMetaKeyChecker(f func() bool) {
	if f == nil {
		f = func() bool { return false }
	}
	metaKeyMu.Lock()
	metaKeyChecker = f
	metaKeyMu.Unlock()
}

func hasUsableMetaAPIKey() bool {
	metaKeyMu.RLock()
	f := metaKeyChecker
	metaKeyMu.RUnlock()
	return f()
}

// Definitions returns every model DroidProxy currently exposes, in the order
// they are written into Factory's customModels.
func Definitions() []ModelDefinition {
	list := []ModelDefinition{
		{
			BaseModel: "claude-fable-5-1", IDSlug: "fable-5-1", DisplayName: "Fable 5.1",
			MaxOutputTokens: 128000, Provider: "anthropic", ProviderKey: "claude",
			BaseURL: "http://localhost:8317", Kind: KindClaudeAdaptive,
			Levels: claudeAdvancedLevels(), DefaultLevelValue: "xhigh",
		},
		{
			BaseModel: "claude-opus-5-5", IDSlug: "opus-5-5", DisplayName: "Opus 5.5",
			MaxOutputTokens: 128000, Provider: "anthropic", ProviderKey: "claude",
			BaseURL: "http://localhost:8317", Kind: KindClaudeAdaptive,
			Levels: claudeAdvancedLevels(), DefaultLevelValue: "xhigh",
		},
		{
			BaseModel: "claude-sonnet-5-5", IDSlug: "sonnet-5-5", DisplayName: "Sonnet 5.5",
			MaxOutputTokens: 128000, Provider: "anthropic", ProviderKey: "claude",
			BaseURL: "http://localhost:8317", Kind: KindClaudeAdaptive,
			Levels: claudeAdvancedLevels(), DefaultLevelValue: "xhigh",
		},
		{
			BaseModel: "claude-haiku-5-5", IDSlug: "haiku-5-5", DisplayName: "Haiku 5.5",
			MaxOutputTokens: 128000, MaxContextLimit: 1_000_000, Provider: "anthropic", ProviderKey: "claude",
			BaseURL: "http://localhost:8317", Kind: KindClaudeAdaptive,
			Levels: claudeAdvancedLevels(), DefaultLevelValue: "xhigh",
		},

		// Context windows match CLIProxyAPI's Codex model registry.
		{
			BaseModel: "gpt-6-astra", IDSlug: "gpt-6-astra", DisplayName: "GPT 6 Astra",
			MaxOutputTokens: 128000, MaxContextLimit: 272_000, Provider: "openai", ProviderKey: "codex",
			BaseURL: "http://localhost:8317/v1", Kind: KindCodex,
			Levels: gpt6Levels(), DefaultLevelValue: "xhigh",
		},
		{
			BaseModel: "gpt-6.1-sol", IDSlug: "gpt-6.1-sol", DisplayName: "GPT 6.1 Sol",
			MaxOutputTokens: 128000, MaxContextLimit: 272_000, Provider: "openai", ProviderKey: "codex",
			BaseURL: "http://localhost:8317/v1", Kind: KindCodex,
			Levels: gpt6Levels(), DefaultLevelValue: "xhigh",
		},
		{
			BaseModel: "gpt-6.1-terra", IDSlug: "gpt-6.1-terra", DisplayName: "GPT 6.1 Terra",
			MaxOutputTokens: 128000, MaxContextLimit: 272_000, Provider: "openai", ProviderKey: "codex",
			BaseURL: "http://localhost:8317/v1", Kind: KindCodex,
			Levels: gpt6Levels(), DefaultLevelValue: "xhigh",
		},
		{
			BaseModel: "gpt-6-luna", IDSlug: "gpt-6-luna", DisplayName: "GPT 6 Luna",
			MaxOutputTokens: 128000, MaxContextLimit: 272_000, Provider: "openai", ProviderKey: "codex",
			BaseURL: "http://localhost:8317/v1", Kind: KindCodex,
			Levels: gpt6Levels(), DefaultLevelValue: "xhigh",
		},

		// Antigravity subscription models go through the antigravity executor
		// via OpenAI-compatible chat-completions: provider="openai" plus a
		// baseURL ending in /v1 makes Droid send POST /v1/chat/completions.
		antigravityModel("gemini-3.8-flash-high", "gemini-3.8-flash-high", "Gemini 3.8 Flash (High)", 0, 0, nil, ""),
		// Only the -high id is in the CLIProxyAPI registry. reasoning_effort still
		// selects low, medium, or high thinking on it (verified live 2026-10-29).
		antigravityModel("gemini-4-argon-high", "gemini-4-argon-high", "Gemini 4 Argon", 0, 0, []ThinkingLevel{levelLow, levelMedium, levelHigh}, ""),
		// Claude 5.5 is only served to directly billed (non-trial) Google AI
		// Pro/Ultra accounts. CLIProxyAPI registers an Antigravity model only when
		// the account's fetchAvailableModels returns it, so on other accounts the
		// 5.5 entries fail with `unknown provider for model`; those accounts use
		// the 4.6 entries below.
		antigravityModel("ag-c55s-high", "ag-c55s-high", "Claude Sonnet 5.5 (High)", 128000, 1_000_000, nil, ""),
		antigravityModel("ag-c55o-high", "ag-c55o-high", "Claude Opus 5.5 (High)", 128000, 1_000_000, nil, ""),
		antigravityModel("ag-c46s-thinking", "ag-c46s-thinking", "Claude Sonnet 4.6 (Thinking)", 64000, 0, nil, ""),
		antigravityModel("ag-c46o-thinking", "ag-c46o-thinking", "Claude Opus 4.6 (Thinking)", 64000, 0, nil, ""),

		// CLIProxyAPI recognizes these catalog IDs and removes the `kimi-`
		// prefix, sending Kimi Code's native model ID upstream.
		{
			BaseModel: "kimi-k3.1-pro", IDSlug: "kimi-k3.1-pro", DisplayName: "Kimi K3.1 Pro",
			MaxOutputTokens: 65536, Provider: "openai", ProviderKey: "kimi",
			BaseURL: "http://localhost:8317/v1", Kind: KindKimi,
			Levels: []ThinkingLevel{levelMax}, DefaultLevelValue: "max",
		},
		{
			BaseModel: "kimi-k3.1-pro-highspeed", IDSlug: "kimi-k3.1-pro-highspeed", DisplayName: "Kimi K3.1 Pro Highspeed",
			MaxOutputTokens: 65536, Provider: "openai", ProviderKey: "kimi",
			BaseURL: "http://localhost:8317/v1", Kind: KindKimi,
			Levels: []ThinkingLevel{levelMax}, DefaultLevelValue: "max",
		},
		{
			BaseModel: "kimi-k3", IDSlug: "kimi-k3", DisplayName: "Kimi K3",
			MaxOutputTokens: 65536, Provider: "openai", ProviderKey: "kimi",
			BaseURL: "http://localhost:8317/v1", Kind: KindKimi,
			Levels: []ThinkingLevel{levelMax}, DefaultLevelValue: "max",
		},
		{
			BaseModel: "kimi-k2.6", IDSlug: "kimi-k2.6", DisplayName: "Kimi K2.6",
			MaxOutputTokens: 65536, Provider: "openai", ProviderKey: "kimi",
			BaseURL: "http://localhost:8317/v1", Kind: KindKimi,
			Levels: []ThinkingLevel{levelHigh}, DefaultLevelValue: "high",
		},

		// Junie (JetBrains AI) models: ThinkingProxy strips the `junie-` prefix
		// and forwards to the JetBrains Grazie backend. The prefix keeps these
		// distinct from the OAuth Claude/Codex entries.
		{
			BaseModel: "junie-claude-opus-5-5", IDSlug: "junie-claude-opus-5-5", DisplayName: "Junie Opus 5.5",
			MaxOutputTokens: 128000, Provider: "anthropic", ProviderKey: "junie",
			BaseURL: "http://localhost:8317", Kind: KindJunie,
			Levels: claudeAdvancedLevels(), DefaultLevelValue: "xhigh",
		},
		{
			BaseModel: "junie-gpt-6.1-sol", IDSlug: "junie-gpt-6.1-sol", DisplayName: "Junie Sol 6.1",
			MaxOutputTokens: 128000, MaxContextLimit: 272_000, Provider: "openai", ProviderKey: "junie",
			BaseURL: "http://localhost:8317/v1", Kind: KindJunie,
			Levels: gpt6Levels(), DefaultLevelValue: "xhigh",
		},

		// Grok OAuth (SuperGrok / X Premium+) via the Responses API;
		// ThinkingProxy attaches the bearer. Both go to cli-chat-proxy (Grok
		// Build billing); api.x.ai would bill as X API. The fast variant is the
		// same model on faster infrastructure (2x price). Context window from
		// docs.x.ai: grok-4.7=500k.
		{
			BaseModel: "grok-4.7", IDSlug: "grok-4.7", DisplayName: "Grok 4.7",
			MaxOutputTokens: 128000, MaxContextLimit: 500_000, Provider: "openai", ProviderKey: "grok",
			BaseURL: "http://localhost:8317/v1", Kind: KindGrok,
			Levels: codexLevels(), DefaultLevelValue: "xhigh",
		},
		{
			BaseModel: GrokFastModelID, IDSlug: GrokFastModelID, DisplayName: "Grok 4.7 Fast",
			MaxOutputTokens: 128000, MaxContextLimit: 500_000, Provider: "openai", ProviderKey: "grok",
			BaseURL: "http://localhost:8317/v1", Kind: KindGrok,
			Levels: codexLevels(), DefaultLevelValue: "xhigh",
		},
	}

	// Muse Wonder models are only exposed with an enabled, usable key, matching
	// backend configuration eligibility. Contributor Mode selects the
	// contributor variant of each Wonder model; the catalog never includes both
	// variants of the same model at once.
	if hasUsableMetaAPIKey() {
		if prefs.MetaContributorMode() {
			list = append(list,
				MuseModel("muse-wonder-1.0-contributor", "muse-wonder-1.0-contributor", "Muse Wonder 1.0 Contributor"),
				MuseModel("muse-wonder-full-1.0-contributor", "muse-wonder-full-1.0-contributor", "Muse Wonder Full 1.0 Contributor"),
			)
		} else {
			list = append(list,
				MuseModel("muse-wonder-1.0", "muse-wonder-1.0", "Muse Wonder 1.0"),
				MuseModel("muse-wonder-full-1.0", "muse-wonder-full-1.0", "Muse Wonder Full 1.0"),
			)
		}
	}
	return list
}

// EnabledDefinitions is the single seam where provider and per-model
// enablement resolve: a definition is in when its provider passes
// providerIsEnabled (nil includes every provider) and the model itself is
// enabled in prefs.
func EnabledDefinitions(providerIsEnabled func(providerKey string) bool) []ModelDefinition {
	var out []ModelDefinition
	for _, d := range Definitions() {
		if providerIsEnabled != nil && !providerIsEnabled(d.ProviderKey) {
			continue
		}
		if !prefs.IsModelEnabled(d.IDSlug) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// FindDefinition returns the definition with this ID slug, if one exists.
func FindDefinition(slug string) (ModelDefinition, bool) {
	for _, d := range Definitions() {
		if d.IDSlug == slug {
			return d, true
		}
	}
	return ModelDefinition{}, false
}

// SettingsModels returns the Factory entries for the enabled definitions.
func SettingsModels(providerIsEnabled func(providerKey string) bool) []map[string]any {
	var out []map[string]any
	for _, d := range EnabledDefinitions(providerIsEnabled) {
		out = append(out, d.SettingsEntry())
	}
	return out
}

// AllSettingsIDs is the set of SimpleIDs for the current definitions.
func AllSettingsIDs() map[string]bool {
	ids := map[string]bool{}
	for _, d := range Definitions() {
		ids[d.SimpleID()] = true
	}
	return ids
}
