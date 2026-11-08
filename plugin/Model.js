// Pure helpers for the DroidProxy panel. The daemon snapshot is local but
// still treated as data: strings are clipped, icons and colors are allowlisted.

function clip(value, max) {
  var s = String(value == null ? "" : value).replace(/[\r\n\t]+/g, " ").replace(/ +/g, " ").trim()
  if (s.length <= max) return s
  return s.slice(0, max)
}

function safeIcon(name) {
  var s = String(name || "")
  return /^icon-[a-z0-9-]+\.(png|svg)$/.test(s) ? s : ""
}

function safeColor(value) {
  var s = String(value || "")
  return /^#[0-9A-Fa-f]{6}$/.test(s) ? s : ""
}

function tintIcon(name) {
  return name.endsWith(".svg") && name.indexOf("junie") === -1
}

function setting(state, key, fallback) {
  var settings = state && state.settings
  if (!settings || settings[key] === undefined || settings[key] === null) return fallback
  return settings[key]
}

function providerList(state) {
  var raw = state && Array.isArray(state.providers) ? state.providers : []
  var out = []
  var limit = Math.min(raw.length, 16)
  for (var i = 0; i < limit; i++) {
    var provider = raw[i] || {}
    var id = clip(provider.id, 40)
    if (!/^[a-z0-9-]+$/.test(id)) continue
    var rawAccounts = Array.isArray(provider.accounts) ? provider.accounts : []
    var accounts = []
    var accountLimit = Math.min(rawAccounts.length, 64)
    for (var a = 0; a < accountLimit; a++) {
      var account = rawAccounts[a] || {}
      // IDs are compared exactly by the daemon; 320 sits above any
      // filename the filesystem allows, so this never truncates one.
      var accountId = clip(account.id || account.email || "", 320)
      if (accountId === "") continue
      accounts.push({
        id: accountId,
        displayName: clip(account.displayName || account.email || accountId, 120),
        email: clip(account.email, 120),
        expired: account.expired === true,
        disabled: account.disabled === true
      })
    }
    var rawModels = Array.isArray(provider.models) ? provider.models : []
    var models = []
    var modelLimit = Math.min(rawModels.length, 32)
    for (var m = 0; m < modelLimit; m++) {
      var model = rawModels[m] || {}
      var slug = clip(model.slug || "", 80)
      if (!/^[a-z0-9][a-z0-9._-]*$/.test(slug)) continue
      models.push({
        slug: slug,
        displayName: clip(model.displayName || slug, 80),
        enabled: model.enabled === true,
        installed: model.installed === true
      })
    }
    out.push({
      id: id,
      name: clip(provider.name || id, 60),
      icon: safeIcon(provider.icon),
      color: safeColor(provider.color),
      enabled: provider.enabled === true,
      primerApplies: provider.primerApplies !== false,
      authenticating: provider.authenticating === true,
      help: clip(provider.help, 240),
      accounts: accounts,
      models: models
    })
  }
  return out
}

function remainingOf(window) {
  if (!window) return -1
  if (window.remainingPercent === undefined || window.remainingPercent === null) return -1
  var n = Number(window.remainingPercent)
  if (!isFinite(n)) return -1
  return Math.max(0, Math.min(100, n))
}

function usageAccounts(state) {
  var usage = state && state.usage
  var raw = usage && Array.isArray(usage.accounts) ? usage.accounts : []
  var out = []
  var limit = Math.min(raw.length, 64)
  for (var i = 0; i < limit; i++) {
    var account = raw[i] || {}
    var rawWindows = Array.isArray(account.windows) ? account.windows : []
    var windows = []
    // The Claude parser emits up to seven buckets; keep them all.
    var windowLimit = Math.min(rawWindows.length, 8)
    for (var w = 0; w < windowLimit; w++) {
      var item = rawWindows[w] || {}
      windows.push({
        title: clip(item.title || "", 40),
        remaining: remainingOf(item),
        resetText: clip(item.resetText, 80)
      })
    }
    var id = clip(account.id || account.email || "", 320)
    var credits = account.resetCredits || null
    out.push({
      provider: clip(account.provider, 40),
      providerName: clip(account.providerName || account.provider || "", 40),
      email: clip(account.email, 120),
      id: id,
      error: clip(account.error, 160),
      loading: account.loading === true,
      windows: windows,
      resetCount: credits && isFinite(Number(credits.availableCount)) ? Math.max(0, Math.floor(Number(credits.availableCount))) : 0,
      resetExpires: credits ? clip(credits.nextExpiresAt || "", 40) : ""
    })
  }
  return out
}

var fastModeCatalog = [
  { key: "gpt6AstraFastMode", label: "GPT 6 Astra" },
  { key: "gpt61SolFastMode", label: "GPT 6.1 Sol" },
  { key: "gpt61TerraFastMode", label: "GPT 6.1 Terra" },
  { key: "gpt6LunaFastMode", label: "GPT 6 Luna" }
]

function fastModes(state) {
  var settings = state && state.settings
  if (!settings) return []
  var out = []
  for (var i = 0; i < fastModeCatalog.length; i++) {
    var mode = fastModeCatalog[i]
    if (typeof settings[mode.key] === "boolean") out.push(mode)
  }
  return out
}

function numberOr(value, fallback) {
  var n = Number(value)
  if (!isFinite(n)) return fallback
  return n
}

function primer(state, id) {
  var settings = state && state.settings
  if (!settings || !/^[a-z0-9-]+$/.test(id)) return null
  var provider = providerById(state, id)
  if (provider && provider.primerApplies === false) return null
  var enabledKey = "windowPrimerEnabled." + id
  var minutesKey = "windowPrimerMinutes." + id
  if (typeof settings[enabledKey] !== "boolean") return null
  return {
    enabled: settings[enabledKey],
    minutes: numberOr(settings[minutesKey], 420),
    enabledKey: enabledKey,
    minutesKey: minutesKey
  }
}

function shiftMinutes(minutes, delta) {
  var m = (Math.round(numberOr(minutes, 0)) + delta) % 1440
  if (m < 0) m += 1440
  return m
}

function clockLabel(minutes) {
  var m = shiftMinutes(minutes, 0)
  var h = Math.floor(m / 60)
  var min = m % 60
  function pad(n) { return (n < 10 ? "0" : "") + n }
  return pad(h) + ":" + pad(min)
}

function safeHttp(value) {
  var s = clip(value, 300)
  return /^https:\/\/[^\s]+$/.test(s) ? s : ""
}

function signIn(state) {
  var grok = state && state.grok
  var meta = state && state.meta
  if (grok && grok.userCode) {
    return {
      provider: "grok",
      code: clip(grok.userCode, 40),
      url: safeHttp(grok.verificationUrl),
      error: ""
    }
  }
  if (meta && meta.deviceCode) {
    return {
      provider: "meta",
      code: clip(meta.deviceCode, 80),
      url: safeHttp(meta.verificationUrl),
      error: clip(meta.lastError, 160)
    }
  }
  return null
}

function serverError(state) {
  var server = state && state.server
  return server ? clip(server.lastError, 200) : ""
}

function providerById(state, id) {
  var list = providerList(state)
  for (var i = 0; i < list.length; i++) if (list[i].id === id) return list[i]
  return null
}

// Index into usageAccounts(state) for one provider account, by account id
// first and then by provider + email, or -1 when the account has no usage.
function usageIndexForAccount(usage, providerId, account) {
  if (!account) return -1
  for (var i = 0; i < usage.length; i++)
    if (usage[i].provider === providerId && usage[i].id === account.id) return i
  for (var j = 0; j < usage.length; j++)
    if (usage[j].provider === providerId && account.email && usage[j].email === account.email) return j
  return -1
}

function cursorEntries(state, ui) {
  var advanced = !!(ui && ui.advanced)
  var entries = []
  function add(entry) { entries.push(entry) }

  add({ id: "server", kind: "server" })
  if (state && state.server && state.server.url) add({ id: "copy-url", kind: "copy-url" })
  var pending = signIn(state)
  if (pending) {
    add({ id: "signin-copy", kind: "signin-copy" })
    if (pending.url) add({ id: "signin-open", kind: "signin-open", url: pending.url })
    add({ id: "signin-cancel", kind: "signin-cancel", provider: pending.provider })
  }

  var usage = usageAccounts(state)
  if (usage.length > 0) add({ id: "refresh", kind: "refresh" })

  var providers = providerList(state)
  for (var p = 0; p < providers.length; p++) {
    var provider = providers[p]
    add({ id: "provider:" + provider.id, kind: "provider", provider: provider.id })
    if (!provider.enabled) continue
    for (var a = 0; a < provider.accounts.length; a++) {
      var account = provider.accounts[a]
      add({
        id: "account:" + provider.id + ":" + account.id,
        kind: "account",
        provider: provider.id,
        accountId: account.id
      })
      var u = usageIndexForAccount(usage, provider.id, account)
      if (u >= 0) {
        add({ id: "usage:" + u, kind: "usage", index: u })
        if (usage[u].resetCount > 0) add({ id: "reset:" + u, kind: "reset", index: u })
      }
    }
    if (provider.id === "junie") add({ id: "junie", kind: "junie" })
    else add({ id: "connect:" + provider.id, kind: "connect", provider: provider.id })
    for (var mo = 0; mo < provider.models.length; mo++) {
      add({
        id: "model:" + provider.models[mo].slug,
        kind: "model",
        provider: provider.id,
        slug: provider.models[mo].slug
      })
    }
    if (primer(state, provider.id)) add({ id: "primer:" + provider.id, kind: "primer", provider: provider.id })
    if (provider.id === "codex") {
      var modes = fastModes(state)
      for (var m = 0; m < modes.length; m++) add({ id: "fast:" + modes[m].key, kind: "fast", key: modes[m].key })
    }
    if (provider.id === "meta" && typeof setting(state, "metaContributorMode", null) === "boolean")
      add({ id: "contributor", kind: "setting", key: "metaContributorMode" })
  }

  add({ id: "factory", kind: "factory" })
  add({ id: "advanced", kind: "advanced" })
  if (advanced) {
    add({ id: "launch", kind: "setting", key: "launchAtLogin" })
    add({ id: "failover", kind: "setting", key: "sequentialAccountFailover" })
    add({ id: "verbose", kind: "setting", key: "verboseLogging" })
    add({ id: "remote", kind: "setting", key: "allowRemote" })
    add({ id: "secret", kind: "secret" })
    add({ id: "beta", kind: "setting", key: "beta" })
    if (setting(state, "beta", false) === true) add({ id: "bind", kind: "bind" })
    add({ id: "auth-folder", kind: "open", method: "open.authFolder" })
    add({ id: "logs-folder", kind: "open", method: "open.logsFolder" })
    add({ id: "update-check", kind: "update-check" })
    if (state && state.update && state.update.state === "available")
      add({ id: "update-install", kind: "update-install" })
    add({ id: "update-auto-check", kind: "setting", key: "autoCheckUpdates" })
    add({ id: "update-auto-install", kind: "setting", key: "autoInstallUpdates" })
  }
  return entries
}

function entryIndex(entries, id) {
  for (var i = 0; i < entries.length; i++) if (entries[i].id === id) return i
  return -1
}

// The cursor id after stepping dy entries from cursorId, clamped to
// the list; an unknown cursor restarts at the first entry.
function steppedCursorId(entries, cursorId, dy) {
  if (!entries || entries.length === 0) return cursorId
  var index = entryIndex(entries, cursorId)
  if (index < 0) index = 0
  else index = Math.max(0, Math.min(entries.length - 1, index + dy))
  return entries[index].id
}
