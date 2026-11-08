# DroidProxy Setup

## 1. Launch & Authenticate

1. Install DroidProxy (see the [README](README.md#install)). The installer starts it and adds the DroidProxy icon to the Omarchy bar.
2. Click the DroidProxy icon in the bar to open Settings in your browser. You can also run `droidproxy open`, or search for "DroidProxy" in the Omarchy app launcher.
3. Click **Add Account** next to Claude Code, Codex, Antigravity (Gemini), Kimi, Meta Muse (device-code sign-in), Junie (API key), or **Grok** (device-code browser login). Browser sign-ins open in your default browser.

From a terminal you can do the same with `droidproxy login <provider>` (for example `droidproxy login claude`).

For **Grok 4.7** and **Grok 4.7 Fast** (SuperGrok / X Premium+ OAuth):

1. Connect **Grok** in Settings (device-code browser login)
2. Click **Apply** / **Re-apply** under Factory models (registers `custom:droidproxy:grok-4.7` and `custom:droidproxy:grok-4.7-build-fast`)
3. In Droid, `/model` → **DroidProxy: Grok 4.7** or **DroidProxy: Grok 4.7 Fast**

Grok 4.7 and Grok 4.7 Fast (`grok-4.7-build-fast`, the same model on faster infrastructure) are both forwarded to `cli-chat-proxy.grok.com` with `x-grok-model-override`, so usage counts as Grok Build rather than X API. The public xAI API does not serve the fast id.

Apply also removes retired ids, including the old Cursor models (`cursor-composer-2.5`, `cursor-grok-4.6`, `cursor-grok-4.5`, `cursor-small`, `cursor-grok-4.6-fast`) and `grok-4.5` / `grok-4.6`, from `~/.factory/settings.json`.

> Note: some SuperGrok tiers return HTTP 403 on the OAuth API surface even after a successful login. Fallback is an `XAI_API_KEY` via Factory BYOK.

### Grok Imagine (image generation)

Chat and image generation share the same Grok OAuth session, but **Droid will not call the image API by itself**. Install the skill from this repo:

```bash
mkdir -p ~/.factory/skills
cp -R skills/grok-imagine ~/.factory/skills/
```

See [`skills/grok-imagine/SKILL.md`](skills/grok-imagine/SKILL.md). With DroidProxy running and Grok connected, the skill posts to `http://localhost:8317/v1/images/generations` (`grok-imagine-image-2.0`); the proxy injects your subscription bearer and forwards to `api.x.ai`.

### GPT Image (Codex OAuth)

Same pattern as Grok, but through Codex. ChatGPT Plus/Pro OAuth is required; Free accounts are skipped (`auth_not_found`).

```bash
mkdir -p ~/.factory/skills
cp -R skills/gpt-image ~/.factory/skills/
```

See [`skills/gpt-image/SKILL.md`](skills/gpt-image/SKILL.md). With DroidProxy running and Codex connected, the skill posts to `http://localhost:8317/v1/images/generations` (`gpt-image-2.6-flare` or `gpt-image-2.6-sunburst`; an older id such as 2.5 when the backend rejects 2.6). CLIProxyAPI injects your Codex bearer. Selecting a DroidProxy GPT chat model does not generate images.

## 2. Configure Factory

Open `~/.factory/settings.json` and add the following to the `customModels` array:

```json
"customModels": [
    {
      "model": "claude-fable-5-1",
      "id": "custom:droidproxy:fable-5-1",
      "index": 0,
      "baseUrl": "http://localhost:8317",
      "apiKey": "dummy-not-used",
      "displayName": "DroidProxy: Fable 5.1",
      "maxOutputTokens": 128000,
      "noImageSupport": false,
      "provider": "anthropic"
    },
    {
      "model": "claude-opus-5-5",
      "id": "custom:droidproxy:opus-5-5",
      "index": 1,
      "baseUrl": "http://localhost:8317",
      "apiKey": "dummy-not-used",
      "displayName": "DroidProxy: Opus 5.5",
      "maxOutputTokens": 128000,
      "noImageSupport": false,
      "provider": "anthropic"
    },
    {
      "model": "claude-sonnet-5-5",
      "id": "custom:droidproxy:sonnet-5-5",
      "index": 2,
      "baseUrl": "http://localhost:8317",
      "apiKey": "dummy-not-used",
      "displayName": "DroidProxy: Sonnet 5.5",
      "maxOutputTokens": 128000,
      "noImageSupport": false,
      "provider": "anthropic"
    },
    {
      "model": "gpt-6.1-sol",
      "id": "custom:droidproxy:gpt-6.1-sol",
      "index": 3,
      "baseUrl": "http://localhost:8317/v1",
      "apiKey": "dummy-not-used",
      "displayName": "DroidProxy: GPT 6.1 Sol",
      "maxOutputTokens": 128000,
      "noImageSupport": false,
      "provider": "openai"
    },
    {
      "model": "gpt-6.1-terra",
      "id": "custom:droidproxy:gpt-6.1-terra",
      "index": 4,
      "baseUrl": "http://localhost:8317/v1",
      "apiKey": "dummy-not-used",
      "displayName": "DroidProxy: GPT 6.1 Terra",
      "maxOutputTokens": 128000,
      "noImageSupport": false,
      "provider": "openai"
    },
    {
      "model": "gpt-6-luna",
      "id": "custom:droidproxy:gpt-6-luna",
      "index": 5,
      "baseUrl": "http://localhost:8317/v1",
      "apiKey": "dummy-not-used",
      "displayName": "DroidProxy: GPT 6 Luna",
      "maxOutputTokens": 128000,
      "noImageSupport": false,
      "provider": "openai"
    },
    {
      "model": "gemini-4-argon-high",
      "id": "custom:droidproxy:gemini-4-argon-high",
      "index": 6,
      "baseUrl": "http://localhost:8317/v1",
      "apiKey": "dummy-not-used",
      "displayName": "DroidProxy: Antigravity: Gemini 4 Argon",
      "maxOutputTokens": 65536,
      "noImageSupport": false,
      "provider": "openai"
    },
    {
      "model": "gemini-3.8-flash-high",
      "id": "custom:droidproxy:gemini-3.8-flash-high",
      "index": 7,
      "baseUrl": "http://localhost:8317/v1",
      "apiKey": "dummy-not-used",
      "displayName": "DroidProxy: Antigravity: Gemini 3.8 Flash (High)",
      "maxOutputTokens": 65536,
      "noImageSupport": false,
      "provider": "openai"
    },
    {
      "model": "kimi-k3",
      "id": "custom:droidproxy:kimi-k3",
      "index": 8,
      "baseUrl": "http://localhost:8317/v1",
      "apiKey": "dummy-not-used",
      "displayName": "DroidProxy: Kimi K3",
      "maxOutputTokens": 65536,
      "noImageSupport": false,
      "provider": "openai",
      "enableThinking": true,
      "supportedReasoningEfforts": ["max"],
      "defaultReasoningEffort": "max",
      "reasoningEffort": "max"
    }
]
```

Use the standard Claude, Codex, Antigravity, and Kimi model aliases in the `model` field. Claude entries use `http://localhost:8317` with `provider: "anthropic"`; GPT/Codex, Antigravity, and Kimi entries use `provider: "openai"` with `http://localhost:8317/v1`. Reasoning effort is chosen per session from Droid CLI's model selector. DroidProxy preserves that selection, and CLIProxyAPI translates it to the provider-native request format.

## 3. Choose Reasoning Effort

Reasoning effort is selected per session in Droid CLI's model picker. DroidProxy registers each model with its native reasoning levels, preserves the selected value, and lets CLIProxyAPI translate it to the upstream provider format. Supported levels per model:

- Fable 5.1: `low`, `medium`, `high`, `xhigh`, or `max`
- Opus 5.5: `low`, `medium`, `high`, `xhigh`, or `max`
- Sonnet 5.5, Haiku 5.5: `low`, `medium`, `high`, `xhigh`, or `max`
- GPT 6 Astra, GPT 6.1 Sol, GPT 6.1 Terra, GPT 6 Luna: `low`, `medium`, `high`, `xhigh`, or `max`
- Gemini 4 Argon (Antigravity): `low`, `medium`, or `high`
- Gemini 3.8 Flash (Antigravity): `high`
- Kimi K3: `max` (the only currently supported effort)
- Kimi K2.6: `high`

## 4. Enable Thinking Output

1. Start Factory
2. Run `/settings`
3. Set **Show thinking in main view: On**
