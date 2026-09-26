"use strict";

/* DroidProxy settings web app. Renders the daemon state snapshot, sends
 * actions to /api/call/<method>, and stays live over /api/events (SSE) with
 * a polling fallback. No dependencies, no build step. */

const ui = {
  state: null,
  remoteOpen: false,
  fastModeOpen: true,
  expandedProviders: {},
  sseOk: false,
};

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === null || v === undefined) continue;
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k.startsWith("on") && typeof v === "function")
        node.addEventListener(k.slice(2), v);
      else if (k === "html") node.innerHTML = v;
      else node.setAttribute(k, v);
    }
  }
  for (const child of children.flat(9)) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child.nodeType ? child : document.createTextNode(child));
  }
  return node;
}

function setting(key, fallback) {
  const s = ui.state && ui.state.settings;
  const v = s ? s[key] : undefined;
  return v === undefined || v === null ? fallback : v;
}

function providerColor(id, fallback) {
  const providers = (ui.state && ui.state.providers) || [];
  const p = providers.find((x) => x.id === id);
  if (p && p.color) return p.color;
  return fallback || "#8e8e93";
}

function providerIcon(id) {
  const providers = (ui.state && ui.state.providers) || [];
  const p = providers.find((x) => x.id === id);
  return (p && p.icon) || "";
}

/* ---------------- icons ---------------- */

function iconEl(iconFile, color, px) {
  if (!iconFile) return el("span");
  if (iconFile.endsWith(".svg")) {
    const s = el("span", { class: "svgicon" });
    s.style.width = px + "px";
    s.style.height = px + "px";
    s.style.background = color || "currentColor";
    s.style.webkitMaskImage = `url(/icons/${iconFile})`;
    s.style.webkitMaskSize = "contain";
    s.style.webkitMaskRepeat = "no-repeat";
    s.style.maskImage = `url(/icons/${iconFile})`;
    s.style.maskSize = "contain";
    s.style.maskRepeat = "no-repeat";
    return s;
  }
  const img = el("img", { src: `/icons/${iconFile}`, alt: "" });
  img.style.width = px + "px";
  img.style.height = px + "px";
  return img;
}

/* ---------------- api ---------------- */

async function fetchState() {
  try {
    const res = await fetch("/api/state", { cache: "no-store" });
    if (!res.ok) throw new Error("bad status");
    ui.state = await res.json();
    setOffline(false);
    render();
  } catch {
    setOffline(true);
  }
}

async function call(method, params) {
  try {
    const res = await fetch("/api/call/" + method, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(params || {}),
    });
    const result = await res.json();
    if ((result.message && result.message !== "") || (result.error && result.error !== ""))
      showResult(result);
    fetchState();
    return result;
  } catch {
    setOffline(true);
    return { ok: false };
  }
}

function setSetting(key, value) {
  call("settings.set", { key, value });
}

function connectSSE() {
  let src;
  try {
    src = new EventSource("/api/events");
  } catch {
    return;
  }
  src.onmessage = (ev) => {
    let msg;
    try {
      msg = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (msg.type === "state" && msg.state) {
      ui.state = msg.state;
      ui.sseOk = true;
      setOffline(false);
      render();
    } else if (msg.type === "message") {
      showResult({ message: msg.body, error: msg.level === "error" ? msg.body : "", title: msg.title });
    }
  };
  src.onerror = () => {
    ui.sseOk = false;
    if (!ui.state) setOffline(true);
  };
  setInterval(() => {
    if (!ui.sseOk) fetchState();
  }, 5000);
}

function setOffline(offline) {
  document.getElementById("offlineBanner").classList.toggle("visible", offline);
}

/* ---------------- shared widgets ---------------- */

function toggle(checked, color, onToggle, label) {
  const b = el("button", {
    class: "switch",
    role: "switch",
    "aria-checked": checked ? "true" : "false",
    title: label || "",
    onclick: () => onToggle(!checked),
  });
  if (color) b.style.setProperty("--switch-on", color);
  return b;
}

function collapseHead(title, open, onToggle) {
  return el(
    "div",
    { class: "collapse-head" + (open ? " open" : ""), onclick: onToggle },
    el("span", { text: title }),
    el("span", { class: "chev", text: "▶" })
  );
}

/* ---------------- header ---------------- */

function renderHeader() {
  const st = ui.state;
  const chip = document.getElementById("versionChip");
  if (st && st.app && st.app.version) {
    chip.hidden = false;
    chip.textContent = "v" + st.app.version;
  } else {
    chip.hidden = true;
  }
  const pill = document.getElementById("serverPill");
  const text = document.getElementById("serverPillText");
  const running = st && st.server.running;
  const starting = st && st.server.starting;
  pill.className = "status-pill " + (running ? "running" : starting ? "starting" : "stopped");
  text.textContent = running ? "Running" : starting ? "Starting…" : "Stopped";
  pill.title = running ? "Click to stop the server" : "Click to start the server";
  pill.onclick = () => call("server.toggle");

  const footer = document.getElementById("footerLine");
  footer.innerHTML = "";
  if (st && st.app) {
    footer.append(
      document.createTextNode(`DroidProxy v${st.app.version} was made possible thanks to `),
      el("a", { href: st.app.cliProxyApiUrl, target: "_blank", rel: "noreferrer", text: "CLIProxyAPI" }),
      document.createTextNode(" | License: MIT")
    );
  }
}

/* ---------------- usage gauges ---------------- */

function ringSVG(size, stroke, tracks) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", `0 0 ${size} ${size}`);
  let r = size / 2 - stroke / 2 - 1;
  for (const t of tracks) {
    const c = 2 * Math.PI * r;
    const track = document.createElementNS("http://www.w3.org/2000/svg", "circle");
    track.setAttribute("cx", size / 2);
    track.setAttribute("cy", size / 2);
    track.setAttribute("r", r);
    track.setAttribute("class", "track");
    track.setAttribute("stroke-width", stroke);
    svg.append(track);
    if (t.percent !== null && t.percent !== undefined) {
      const v = document.createElementNS("http://www.w3.org/2000/svg", "circle");
      v.setAttribute("cx", size / 2);
      v.setAttribute("cy", size / 2);
      v.setAttribute("r", r);
      v.setAttribute("class", "value");
      v.setAttribute("stroke", t.color);
      v.setAttribute("stroke-width", stroke);
      v.setAttribute("stroke-dasharray", c.toFixed(2));
      v.setAttribute(
        "stroke-dashoffset",
        (c * (1 - Math.max(0, Math.min(100, t.percent)) / 100)).toFixed(2)
      );
      svg.append(v);
    }
    r -= stroke + 2.5;
  }
  return svg;
}

function usageHelpText(w) {
  const usage =
    w.hasRemaining && w.remainingPercent !== undefined
      ? `${Math.round(w.remainingPercent)}% left`
      : "Usage unavailable";
  if (!w.resetText) return `${w.title}: ${usage}`;
  return `${w.title}: ${usage}\nResets ${w.resetText}`;
}

function showGaugeTip(text, x, y) {
  const tip = document.getElementById("gaugeTip");
  tip.textContent = text;
  tip.classList.add("visible");
  const pad = 14;
  const rect = tip.getBoundingClientRect();
  tip.style.left =
    Math.min(window.innerWidth - rect.width - pad, Math.max(pad, x - rect.width / 2)) + "px";
  tip.style.top = Math.min(window.innerHeight - rect.height - pad, y + 18) + "px";
}

function hideGaugeTip() {
  document.getElementById("gaugeTip").classList.remove("visible");
}

function gauge(windows, providerId, iconFile) {
  const color = providerColor(providerId);
  const g = el("div", { class: "gauge" });
  const inner = windows[0];
  const outer = windows.length > 1 ? windows[1] : null;
  const innerPct = inner.hasRemaining ? inner.remainingPercent : null;
  const outerPct = outer && outer.hasRemaining ? outer.remainingPercent : null;
  g.append(
    ringSVG(64, 4, [
      { percent: outer ? outerPct : null, color },
      { percent: innerPct, color },
    ])
  );
  const logo = el("div", { class: "gauge-logo" });
  logo.append(iconEl(iconFile, color, 22));
  g.append(logo);
  const help = [usageHelpText(inner)].concat(outer ? [usageHelpText(outer)] : []).join("\n");
  g.addEventListener("mousemove", (e) => showGaugeTip(help, e.clientX, e.clientY));
  g.addEventListener("mouseleave", hideGaugeTip);
  return g;
}

function compactGauge(window, providerId, iconFile) {
  const color = providerColor(providerId);
  const row = el("div", { class: "gauge-stack" });
  const g = el("div", { class: "gauge compact" });
  g.append(
    ringSVG(34, 3, [{ percent: window.hasRemaining ? window.remainingPercent : null, color }])
  );
  const logo = el("div", { class: "gauge-logo" });
  logo.append(iconEl(iconFile, color, 12));
  g.append(logo);
  const help = usageHelpText(window);
  g.addEventListener("mousemove", (e) => showGaugeTip(help, e.clientX, e.clientY));
  g.addEventListener("mouseleave", hideGaugeTip);
  row.append(g);
  row.append(el("span", { class: "gauge-email", text: window.title, title: help }));
  return row;
}
function usageCard() {
  const usage = ui.state.usage;
  if (!usage || !usage.visible) return null;
  const refreshing = usage.refreshing;

  const head = el(
    "div",
    { class: "usage-head" },
    el("p", { class: "section-title", text: "OAUTH QUOTA USAGE" }),
    el("button", {
      class: "btn ghost",
      text: refreshing ? "…" : "↻ Refresh",
      title: "Refresh usage quotas",
      disabled: refreshing ? "true" : null,
      onclick: () => call("usage.refresh"),
    })
  );

  const flow = el("div", { class: "gauge-flow" });
  const accounts = usage.accounts || [];
  if (accounts.length === 0) {
    flow.append(
      el("p", {
        class: "hint",
        text: "Connect Codex, Claude, Grok, or Meta Muse OAuth accounts to show quota windows.",
      })
    );
  }
  const byProvider = {};
  for (const a of accounts) byProvider[a.provider] = (byProvider[a.provider] || 0) + 1;

  for (const a of accounts) {
    const group = el("div", { class: "gauge-group" });
    if ((byProvider[a.provider] || 0) > 1) {
      group.append(el("div", { class: "gauge-email", text: a.email, title: a.email }));
    }
    if (a.loading) {
      group.append(el("div", { class: "spinner" }));
    } else if (a.error) {
      group.append(
        el("div", { class: "gauge-error", title: a.error }, el("span", { text: "⚠" }), el("span", { text: a.error }))
      );
    } else if ((a.windows || []).length > 2) {
      const stack = el("div", { class: "gauge-group" });
      for (const w of a.windows) stack.append(compactGauge(w, a.provider, providerIcon(a.provider)));
      group.append(stack);
    } else if ((a.windows || []).length > 0) {
      group.append(gauge(a.windows, a.provider, providerIcon(a.provider)));
    }
    if (!a.loading && !a.error && a.updatedAt && a.provider === "meta") {
      group.append(el("div", { class: "gauge-email", text: "as of " + relTime(a.updatedAt) }));
    }
    group.title = `${a.providerName || a.provider} · ${a.email}`;
    flow.append(group);
  }
  return el("section", { class: "card" }, head, flow);
}

function relTime(iso) {
  const then = new Date(iso).getTime();
  if (isNaN(then)) return iso;
  const secs = Math.max(0, Math.round((Date.now() - then) / 1000));
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

/* ---------------- server card ---------------- */

function serverCard() {
  const s = ui.state.server;
  const running = s.running;
  const rows = [];
  rows.push(
    el(
      "div",
      { class: "row" },
      el(
        "div",
        { class: "row-label" },
        running ? `Running on port ${s.proxyPort}` : "Server is stopped",
        !running && s.lastError ? el("span", { class: "sub", text: s.lastError }) : null
      ),
      el(
        "div",
        { class: "btn-row" },
        running
          ? el("button", { class: "btn", text: "Copy URL", onclick: () => call("server.copyUrl") })
          : null,
        running
          ? el("button", { class: "btn", text: "Dashboard", onclick: () => call("open.dashboard") })
          : null,
        el("button", {
          class: "btn",
          text: running ? "Stop" : "Start",
          onclick: () => call("server.toggle"),
        })
      )
    )
  );
  if (running) {
    rows.push(
      el(
        "div",
        { class: "row" },
        el("div", { class: "row-label" }, el("span", { class: "mono", text: s.url })),
        el("button", { class: "btn ghost", text: "Copy", onclick: () => call("server.copyUrl") })
      )
    );
  }
  return el("section", { class: "card" }, el("p", { class: "section-title", text: "SERVER" }), ...rows);
}

/* ---------------- general card ---------------- */

function textFieldRow(label, value, { password, placeholder, onCommit }) {
  const input = el("input", {
    class: "field",
    type: password ? "password" : "text",
    value: value || "",
    placeholder: placeholder || "",
  });
  input.style.width = "220px";
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") onCommit(input.value);
  });
  input.addEventListener("change", () => onCommit(input.value));
  return el("div", { class: "row" }, el("div", { class: "row-label", text: label }), input);
}

function generalCard() {
  const applied = ui.state.factory && ui.state.factory.modelsInstalled;
  const card = el(
    "section",
    { class: "card" },
    el("p", { class: "section-title", text: "GENERAL" }),
    el(
      "div",
      { class: "row" },
      el("div", { class: "row-label", text: "Launch at login" }),
      toggle(setting("launchAtLogin", false), null, (v) => setSetting("launchAtLogin", v))
    ),
    el(
      "div",
      { class: "row" },
      el("div", { class: "row-label", text: "Auth files" }),
      el("button", { class: "btn", text: "Open Folder", onclick: () => call("open.authFolder") })
    ),
    el(
      "div",
      { class: "row" },
      el(
        "div",
        { class: "row-label" },
        "Factory custom models",
        el("span", {
          class: "sub",
          text: "Writes DroidProxy model aliases into ~/.factory/settings.json (timestamped backup first). Reasoning effort is selected from Droid CLI.",
        })
      ),
      el(
        "div",
        { class: "btn-row" },
        applied ? el("span", { class: "hint", text: "✓ Applied" }) : null,
        el("button", {
          class: "btn",
          text: applied ? "Re-apply" : "Apply",
          onclick: () => call("factory.apply"),
        })
      )
    )
  );

  // Remote management collapsible.
  const remoteHead = collapseHead(
    `Remote Management · ${setting("allowRemote", false) ? "On" : "Off"}`,
    ui.remoteOpen,
    () => {
      ui.remoteOpen = !ui.remoteOpen;
      render();
    }
  );
  const remoteBody = el(
    "div",
    { class: "collapse-body" + (ui.remoteOpen ? " open" : "") },
    el(
      "div",
      { class: "row" },
      el("div", { class: "row-label", text: "Allow remote access" }),
      toggle(setting("allowRemote", false), null, (v) => setSetting("allowRemote", v))
    ),
    textFieldRow("Secret key", setting("secretKey", ""), {
      password: true,
      placeholder: "Enter secret key",
      onCommit: (v) => setSetting("secretKey", v),
    }),
    setting("beta", false)
      ? textFieldRow("Bind address", setting("bindAddress", "127.0.0.1"), {
          placeholder: "127.0.0.1",
          onCommit: (v) => setSetting("bindAddress", v),
        })
      : null,
    setting("beta", false)
      ? el("p", {
          class: "hint",
          text: "Default is 127.0.0.1. Set to 0.0.0.0 to allow access from other devices on your network. Requires server restart.",
        })
      : null,
    setting("allowRemote", false) && !setting("secretKey", "")
      ? el("p", { class: "hint warn", text: "⚠ Set a secret key to secure remote access" })
      : null
  );
  card.append(el("div", { class: "row" }, el("div", null, remoteHead, remoteBody)));

  card.append(
    el(
      "div",
      { class: "row" },
      el("div", { class: "row-label", text: "Beta features" }),
      toggle(setting("beta", false), null, (v) => setSetting("beta", v))
    ),
    el(
      "div",
      { class: "row" },
      el("div", { class: "row-label", text: "Verbose logging" }),
      toggle(setting("verboseLogging", false), null, (v) => setSetting("verboseLogging", v))
    ),
    el(
      "div",
      { class: "row" },
      el("div", { class: "row-label", text: "Request logs" }),
      el("button", { class: "btn", text: "Open Logs", onclick: () => call("open.logsFolder") })
    ),
    el(
      "div",
      { class: "row" },
      el(
        "div",
        { class: "row-label" },
        "Sequential account failover",
        el("span", {
          class: "sub",
          text: "Ride one account until its quota runs out, then move to the next. Leave off to spread requests evenly.",
        })
      ),
      toggle(setting("sequentialAccountFailover", false), null, (v) =>
        setSetting("sequentialAccountFailover", v)
      )
    )
  );
  return card;
}
/* ---------------- providers ---------------- */

function enabledCount(providerId) {
  const providers = ui.state.providers || [];
  const p = providers.find((x) => x.id === providerId);
  if (!p) return 0;
  return (p.accounts || []).filter((a) => !a.disabled).length;
}

function deviceBox(kind) {
  const box = el("div", { class: "device-box" });
  if (kind === "grok") {
    const g = ui.state.grok || {};
    box.append(
      el("p", { text: "Enter this code at the verification link:" }),
      el("div", { class: "device-code", text: g.userCode || "…" }),
      el(
        "div",
        { class: "btn-row" },
        el("button", {
          class: "btn",
          text: "Copy",
          disabled: g.userCode ? null : "true",
          onclick: () => call("clipboard.copy", { text: g.userCode }),
        }),
        el("button", {
          class: "btn",
          text: "Open Grok",
          disabled: g.verificationUrl ? null : "true",
          onclick: () => call("open.url", { url: g.verificationUrl }),
        }),
        el("button", {
          class: "btn ghost",
          text: "Cancel sign-in",
          onclick: () => call("provider.cancelAuth", { provider: "grok" }),
        })
      )
    );
  } else {
    const m = ui.state.meta || {};
    box.append(
      el("p", { text: "Complete Meta sign-in with this device code:" }),
      el("div", { class: "device-code", text: m.deviceCode || "…" }),
      el(
        "div",
        { class: "btn-row" },
        el("button", {
          class: "btn",
          text: "Copy",
          disabled: m.deviceCode ? null : "true",
          onclick: () => call("clipboard.copy", { text: m.deviceCode }),
        }),
        el("button", {
          class: "btn",
          text: "Open Meta",
          disabled: m.verificationUrl ? null : "true",
          onclick: () => call("open.url", { url: m.verificationUrl }),
        }),
        el("button", {
          class: "btn ghost",
          text: "Cancel sign-in",
          onclick: () => call("provider.cancelAuth", { provider: "meta" }),
        })
      )
    );
  }
  return box;
}

const FAST_MODES = [
  { key: "gpt6AstraFastMode", title: "GPT 6 Astra" },
  { key: "gpt6SolFastMode", title: "GPT 6 Sol" },
  { key: "gpt6LunaFastMode", title: "GPT 6 Luna" },
];

function accountRow(p, a) {
  const showToggle = (p.accounts || []).length > 1;
  const canDisable = a.disabled || enabledCount(p.id) > 1;
  const row = el("div", {
    class: "account" + (a.disabled ? " disabled" : "") + (a.expired && !a.disabled ? " expired" : ""),
  });
  row.style.setProperty("--provider", p.color || "#f27b2f");
  row.append(el("span", { class: "dot" }));
  row.append(el("span", { class: "name", text: a.displayName, title: a.displayName }));
  if (a.expired && !a.disabled) row.append(el("span", { class: "tag", text: "(expired)" }));
  if (a.disabled) row.append(el("span", { class: "tag", text: "(disabled)" }));
  if (showToggle) {
    row.append(
      el("button", {
        class: "link-btn",
        text: a.disabled ? "Enable" : "Disable",
        title: !canDisable ? "At least one account must remain enabled" : "",
        disabled: canDisable ? null : "true",
        onclick: () => call("account.toggleDisabled", { provider: p.id, accountId: a.id }),
      })
    );
  }
  row.append(
    el("button", {
      class: "link-btn red",
      text: "⊖ Remove",
      onclick: () => confirmRemove(p, a),
    })
  );
  return row;
}

function providerSection(p) {
  const sec = el("div", { class: "provider" + (p.enabled ? "" : " disabled") });
  sec.style.setProperty("--provider", p.color || "#f27b2f");

  const head = el("div", { class: "provider-head" });
  head.append(
    toggle(p.enabled, p.color, (v) => call("provider.setEnabled", { provider: p.id, enabled: v }), p.name)
  );
  const iconWrap = el("span", { class: "provider-icon" });
  iconWrap.append(iconEl(p.icon, p.color, 20));
  head.append(iconWrap);
  head.append(el("span", { class: "provider-name", text: p.name, title: p.help || p.name }));
  if (p.authenticating) {
    head.append(el("span", { class: "hint", text: "signing in…" }));
  } else if (p.enabled) {
    head.append(
      el("button", {
        class: "btn",
        text: "Add Account",
        onclick: () => {
          if (p.kind === "junie") junieDialog();
          else call("provider.connect", { provider: p.id });
        },
      })
    );
  }
  sec.append(head);
  if (p.help) sec.append(el("p", { class: "provider-help", text: p.help }));

  if (!p.enabled) return sec;
  const detail = el("div", { class: "provider-detail" });

  if (p.kind === "grok" && p.authenticating) detail.append(deviceBox("grok"));
  if (p.kind === "meta" && p.authenticating) detail.append(deviceBox("meta"));
  if (p.kind === "meta" && ui.state.meta && ui.state.meta.lastError) {
    detail.append(el("p", { class: "hint warn", text: ui.state.meta.lastError }));
  }
  if (p.kind === "meta") {
    detail.append(
      el(
        "div",
        { class: "fast-row" },
        el("span", {
          text: "Muse Spark · Contributor mode",
          title: "Applies Muse Spark 1.3 Contributor instead of Muse Spark 1.3 when Factory custom models are applied. Only one is ever active.",
        }),
        toggle(setting("metaContributorMode", false), null, (v) => setSetting("metaContributorMode", v))
      )
    );
  }
  if (p.id === "codex") {
    const fm = el("div", { class: "fast-mode" });
    fm.append(
      collapseHead("Fast Mode", ui.fastModeOpen, () => {
        ui.fastModeOpen = !ui.fastModeOpen;
        render();
      })
    );
    if (ui.fastModeOpen) {
      for (const f of FAST_MODES) {
        fm.append(
          el(
            "div",
            { class: "fast-row" },
            el("span", {
              text: f.title,
              title: `Injects service_tier=priority for ${f.title} Responses API requests`,
            }),
            toggle(setting(f.key, false), null, (v) => setSetting(f.key, v))
          )
        );
      }
    }
    detail.append(fm);
  }

  const accounts = p.accounts || [];
  if (accounts.length === 0) {
    detail.append(el("div", { class: "no-accounts", text: "No connected accounts" }));
  } else {
    const expanded = ui.expandedProviders[p.id] === true;
    const summary = el(
      "div",
      {
        class: "account-summary",
        onclick: () => {
          ui.expandedProviders[p.id] = !expanded;
          render();
        },
      },
      el("span", {
        class: "count",
        text: `${accounts.length} connected account${accounts.length === 1 ? "" : "s"}`,
      })
    );
    if (enabledCount(p.id) > 1) {
      summary.append(
        el("span", {
          class: "route",
          text: setting("sequentialAccountFailover", false)
            ? "· Sequential auto-failover"
            : "· Round-robin w/ auto-failover",
        })
      );
    }
    summary.append(el("span", { class: "chev", text: expanded ? "▾" : "▸" }));
    detail.append(summary);
    if (expanded) {
      for (const a of accounts) detail.append(accountRow(p, a));
    }
  }
  sec.append(detail);
  return sec;
}

function providersCard() {
  const card = el(
    "section",
    { class: "card" },
    el("p", { class: "section-title", text: "SERVICES" })
  );
  for (const p of ui.state.providers || []) card.append(providerSection(p));
  return card;
}

/* ---------------- updates ---------------- */

function updateStatusText(u) {
  switch (u.state) {
    case "checking":
      return "Checking for updates…";
    case "upToDate":
      return `Up to date (v${u.currentVersion})`;
    case "available":
      return `Update available: v${u.latestVersion}`;
    case "downloading":
      return `Downloading update… ${Math.round((u.progress || 0) * 100)}%`;
    case "installing":
      return "Installing update…";
    case "error":
      return `Update check failed: ${u.error || ""}`;
    default:
      return `Current version: v${u.currentVersion}`;
  }
}

function updatesCard() {
  const u = ui.state.update;
  const card = el(
    "section",
    { class: "card" },
    el("p", { class: "section-title", text: "UPDATES" }),
    el(
      "div",
      { class: "row" },
      el("div", { class: "row-label", text: updateStatusText(u) }),
      el(
        "div",
        { class: "btn-row" },
        u.state === "idle" || u.state === "upToDate" || u.state === "error"
          ? el("button", { class: "btn", text: "Check now", onclick: () => call("update.check") })
          : null,
        u.state === "available"
          ? el("button", {
              class: "btn primary",
              text: u.latestVersion ? `Install v${u.latestVersion}` : "Install update",
              onclick: () => call("update.install"),
            })
          : null
      )
    )
  );
  if (u.state === "downloading") {
    const bar = el("div", { class: "progress" });
    const fill = el("div");
    fill.style.width = `${Math.round((u.progress || 0) * 100)}%`;
    bar.append(fill);
    card.append(bar);
  }
  if (u.state === "available" && u.notes) card.append(el("div", { class: "notes", text: u.notes }));
  if (u.state === "error" && u.error) card.append(el("p", { class: "hint warn", text: u.error }));
  card.append(
    el(
      "div",
      { class: "row" },
      el("div", { class: "row-label", text: "Automatically check for updates" }),
      toggle(setting("autoCheckUpdates", true), null, (v) => setSetting("autoCheckUpdates", v))
    ),
    el(
      "div",
      { class: "row" },
      el("div", { class: "row-label", text: "Automatically install updates" }),
      toggle(setting("autoInstallUpdates", false), null, (v) => setSetting("autoInstallUpdates", v))
    )
  );
  return card;
}

/* ---------------- dialogs ---------------- */

function openDialog({ title, message, isError, input, buttons }) {
  document.getElementById("dialogTitle").textContent = title || "DroidProxy";
  const msg = document.getElementById("dialogMessage");
  msg.textContent = message || "";
  msg.className = isError ? "error" : "";
  const field = document.getElementById("dialogInput");
  if (input) {
    field.hidden = false;
    field.type = input.password ? "password" : "text";
    field.placeholder = input.placeholder || "";
    field.value = "";
    setTimeout(() => field.focus(), 50);
  } else {
    field.hidden = true;
  }
  const actions = document.getElementById("dialogActions");
  actions.innerHTML = "";
  for (const b of buttons) {
    const btn = el("button", {
      class: "btn" + (b.primary ? " primary" : ""),
      text: b.label,
      onclick: () => {
        closeDialog();
        if (b.onClick) b.onClick(field.value);
      },
    });
    actions.append(btn);
  }
  field.onkeydown = (e) => {
    if (e.key === "Enter") {
      const primary = buttons.find((b) => b.primary) || buttons[buttons.length - 1];
      closeDialog();
      if (primary && primary.onClick) primary.onClick(field.value);
    }
  };
  document.getElementById("overlay").classList.add("visible");
}

function closeDialog() {
  document.getElementById("overlay").classList.remove("visible");
}

function showResult(result) {
  const message =
    result.error && result.error !== "" ? result.error : result.message || "";
  if (!message) return;
  openDialog({
    title: result.title && result.title !== "" ? result.title : "DroidProxy",
    message,
    isError: !!(result.error && result.error !== ""),
    buttons: [{ label: "OK", primary: true }],
  });
}

function confirmRemove(p, a) {
  openDialog({
    title: "Remove account",
    message: `Remove ${a.displayName} from ${p.name}?`,
    buttons: [
      { label: "Cancel" },
      {
        label: "Remove",
        primary: true,
        onClick: () => call("account.remove", { provider: p.id, accountId: a.id }),
      },
    ],
  });
}

function junieDialog() {
  openDialog({
    title: "Add Junie API Key",
    message: "Enter your JetBrains Junie API key. It will be saved under ~/.cli-proxy-api/junie.json.",
    input: { password: true, placeholder: "Enter Junie Key" },
    buttons: [
      { label: "Cancel" },
      {
        label: "Save",
        primary: true,
        onClick: (value) => {
          if (value) call("junie.saveKey", { apiKey: value });
        },
      },
    ],
  });
}

function quitDialog() {
  openDialog({
    title: "Quit DroidProxy",
    message: "Stop the servers and quit the daemon?",
    buttons: [
      { label: "Cancel" },
      { label: "Quit", primary: true, onClick: () => call("app.quit") },
    ],
  });
}

/* ---------------- render ---------------- */

function render() {
  renderHeader();
  if (!ui.state) return;
  const app = document.getElementById("app");
  app.innerHTML = "";
  const active = document.activeElement;
  const activeKey =
    active && active.classList && active.classList.contains("field") ? active.placeholder : null;
  app.append(
    serverCard(),
    usageCard(),
    generalCard(),
    providersCard(),
    updatesCard()
  );
  if (activeKey) {
    const again = Array.from(app.querySelectorAll(".field")).find(
      (f) => f.placeholder === activeKey
    );
    if (again) {
      again.focus();
      again.setSelectionRange(again.value.length, again.value.length);
    }
  }
}

document.getElementById("overlay").addEventListener("click", (e) => {
  if (e.target.id === "overlay") closeDialog();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeDialog();
});
document.getElementById("issueBtn").addEventListener("click", () => {
  const url =
    (ui.state && ui.state.app && ui.state.app.issuesUrl) ||
    "https://github.com/nikships/droidproxy-omarchy/issues";
  call("open.url", { url });
});
document.getElementById("quitBtn").addEventListener("click", quitDialog);

fetchState();
connectSSE();
   
