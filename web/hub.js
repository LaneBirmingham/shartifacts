"use strict";
// The apps hub: a launcher of apps and a tab per open app. Each tab keeps its app's frame
// alive while hidden, so switching tabs never reloads an app.

const $ = (s, el = document) => el.querySelector(s);
const node = (tag, cls, text) => {
  const el = document.createElement(tag);
  if (cls) el.className = cls;
  if (text !== undefined) el.textContent = text;
  return el;
};
const saved = {
  get(key, fallback) {
    try {
      const v = localStorage.getItem(key);
      return v === null ? fallback : JSON.parse(v);
    } catch {
      return fallback;
    }
  },
  set(key, value) {
    try {
      localStorage.setItem(key, JSON.stringify(value));
    } catch {}
  },
};
// bytes writes a size the way people read one: 1.4 GiB.
const bytes = (n) => {
  const units = ["bytes", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return i ? `${n.toFixed(n < 10 ? 1 : 0)} ${units[i]}` : `${n} bytes`;
};
const viewerId = () => {
  try {
    return localStorage.getItem("shartifacts.viewer") || "";
  } catch {
    return "";
  }
};

async function api(path, options = {}) {
  const headers = { "X-Shartifacts-Viewer": viewerId(), ...options.headers };
  if (typeof options.body === "string") headers["Content-Type"] = "application/json";
  const res = await fetch(path, { ...options, headers });
  const text = await res.text();
  let body = null;
  try {
    body = text ? JSON.parse(text) : null;
  } catch {
    body = text;
  }
  if (!res.ok) throw new Error((body && body.error) || res.statusText);
  return body;
}

let toastTimer;
function toast(msg) {
  const t = $("#toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), Math.max(3200, msg.length * 60));
}

function timeAgo(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  if (s < 86400 * 30) return `${Math.round(s / 86400)} d ago`;
  return new Date(iso).toLocaleDateString();
}

const icon = (name) => (typeof artifactIcon === "function" && artifactIcon(name)) || "▦";
const backendLabel = { go: "Go backend", binary: "Binary backend" };

// An app launched from its own icon on a phone's home screen (start_url adds ?installed,
// apps_install.go) shows alone: no tab bar or toolbar around it, and it leaves the hub's
// saved tabs as they were.
const installedApp = (() => {
  const m = location.pathname.match(/^\/apps\/([0-9a-f]{24})$/);
  return m && new URLSearchParams(location.search).has("installed") ? m[1] : "";
})();
if (installedApp) document.documentElement.classList.add("installed");

const state = {
  apps: [],
  runtime: null,
  shareOrigin: "",
  appOrigin: "",
  tabs: saved.get("shartifacts.hub.tabs", []),
  active: "home",
  views: new Map(),
};

// ---- data -----------------------------------------------------------------------

async function refresh() {
  let body;
  try {
    body = await api("/api/apps");
  } catch (e) {
    // Keep what is on screen and say so; the next poll tries again.
    $("#load-error").textContent = `Can't load your apps (${e.message}). Retrying…`;
    $("#load-error").hidden = false;
    return;
  }
  $("#load-error").hidden = true;
  state.apps = body.apps;
  state.runtime = body.runtime;
  state.shareOrigin = body.share_origin || "";
  state.appOrigin = body.app_origin || "";
  const known = new Set(state.apps.map((a) => a.id));
  for (const id of [...state.tabs]) if (!known.has(id)) closeTab(id);
  renderRuntime();
  renderHome();
  renderTabs();
  for (const view of state.views.values()) syncView(view);
  refreshPorts();
}

// ---- ports ----------------------------------------------------------------------

let portsShown = null;

// refreshPorts redraws the port registry only when it changed, so a poll never moves the
// page under someone about to release a claim.
async function refreshPorts() {
  let body;
  try {
    body = await api("/api/ports");
  } catch {
    return;
  }
  const text = JSON.stringify(body.claims);
  if (text === portsShown) return;
  portsShown = text;
  renderPorts(body.claims);
}

function renderPorts(claims) {
  const hosts = new Map();
  for (const c of claims) {
    if (!hosts.has(c.host)) hosts.set(c.host, []);
    hosts.get(c.host).push(c);
  }
  $("#port-count").textContent = claims.length
    ? `${claims.length} port${claims.length === 1 ? "" : "s"} on ${hosts.size} machine${hosts.size === 1 ? "" : "s"}`
    : "";
  $("#ports-empty").hidden = claims.length > 0;
  const list = $("#port-hosts");
  list.replaceChildren();
  for (const [host, rows] of hosts) {
    const card = node("section", "port-host");
    card.append(node("h3", "port-host-name", host));
    const table = node("table", "port-table");
    for (const c of rows) {
      const tr = node("tr");
      const port = node("td", "port-number", String(c.port));
      if (c.protocol !== "tcp") port.append(node("span", "port-proto", `/${c.protocol}`));
      const service = node("td", "port-service");
      service.append(node("strong", "", c.service));
      if (c.note) service.append(node("small", "", c.note));
      const who = node("td", "port-owner", c.owner || "");
      who.append(node("small", "", timeAgo(c.updated_at)));
      who.title = `Claimed ${new Date(c.claimed_at).toLocaleString()}`;
      const release = node("button", "quiet port-release", "×");
      release.type = "button";
      release.title = `Release ${c.port}/${c.protocol} on ${host}`;
      release.setAttribute("aria-label", release.title);
      release.onclick = async () => {
        if (!confirm(`Release ${c.port}/${c.protocol} on ${host} (${c.service})? Do this once the service no longer listens there.`)) return;
        try {
          await api(`/api/ports/${encodeURIComponent(host)}/${c.port}?protocol=${c.protocol}`, { method: "DELETE" });
          toast(`Released ${c.port}/${c.protocol} on ${host}`);
        } catch (e) {
          toast(e.message);
        }
        refreshPorts();
      };
      const action = node("td", "port-action");
      action.append(release);
      tr.append(port, service, who, action);
      table.append(tr);
    }
    card.append(table);
    list.append(card);
  }
}

const summary = (id) => state.apps.find((a) => a.id === id);

function renderRuntime() {
  const rt = state.runtime;
  if (!rt) return;
  const where = rt.remote ? "in the runner sidecar" : "beside shartifacts";
  $("#rt-backends").textContent = rt.error
    ? `Runner unreachable: ${rt.error}`
    : rt.go
      ? `Go (${rt.go.replace(/^go/, "")}) or prebuilt binaries, ${rt.os}/${rt.arch}, ${where}`
      : `Prebuilt binaries only (no Go toolchain), ${rt.os}/${rt.arch}, ${where}`;
  $("#rt-backends").classList.toggle("warn", !!rt.error || !rt.go);
  const sb = rt.sandbox || {};
  $("#rt-sandbox").textContent =
    sb.mechanism === "landlock"
      ? `Landlock ABI ${sb.abi}: files${sb.network ? " and network" : ""} confined`
      : rt.remote
        ? "Container only: backends cannot see artifacts, but are not confined per app"
        : "None: backends run unconfined as the shartifacts user";
  $("#rt-sandbox").title = sb.detail || "";
  $("#rt-sandbox").classList.toggle("warn", sb.mechanism !== "landlock");
  $("#rt-share").textContent = state.shareOrigin ? state.shareOrigin.replace(/^https?:\/\//, "") : "Off (see docs/sharing.md)";
}

function statusOf(a) {
  if (a.status === "building") return { cls: "building", text: `deploying v${a.latest}` };
  if (a.status === "failed" && a.latest > a.live) return { cls: "failed", text: `v${a.latest} failed` };
  if (!a.live) return { cls: "", text: "no live version" };
  return { cls: "ok", text: `v${a.live} live` };
}

function renderHome() {
  const grid = $("#app-grid");
  grid.replaceChildren();
  $("#empty").hidden = state.apps.length > 0;
  $("#app-count").textContent = state.apps.length ? `${state.apps.length} app${state.apps.length === 1 ? "" : "s"}` : "";
  for (const a of state.apps) {
    const card = node("button", "app-card");
    card.type = "button";
    const top = node("div", "card-top");
    top.append(node("span", "card-icon", icon(a.icon)), node("span", "card-name", a.name));
    const meta = node("div", "card-meta");
    const st = statusOf(a);
    meta.append(node("span", `pill ${st.cls}`, st.text));
    meta.append(node("span", "pill", a.live ? backendLabel[a.backend] || "Frontend only" : "—"));
    if (a.pinned) meta.append(node("span", "pill", "Pinned"));
    if (a.shares) meta.append(node("span", "pill", `${a.shares} link${a.shares === 1 ? "" : "s"}`));
    meta.append(node("span", "pill", timeAgo(a.updated_at)));
    card.append(top, node("p", "card-desc", a.description || ""), meta);
    card.onclick = () => openTab(a.id);
    grid.append(card);
  }
}

// ---- tabs -----------------------------------------------------------------------

function renderTabs() {
  const bar = $("#tabs");
  [...bar.querySelectorAll(".tab.app-tab")].forEach((t) => t.remove());
  for (const id of state.tabs) {
    const a = summary(id);
    const tab = node("div", "tab app-tab");
    tab.setAttribute("role", "tab");
    tab.tabIndex = 0;
    tab.dataset.tab = id;
    tab.id = `tab-${id}`;
    tab.setAttribute("aria-selected", String(state.active === id));
    tab.setAttribute("aria-controls", `panel-${id}`);
    tab.draggable = true;
    const st = a ? statusOf(a) : { cls: "none" };
    const dot = node("span", `tab-dot ${st.cls === "ok" ? "" : st.cls || "none"}`);
    dot.title = st.text || "";
    const close = node("button", "tab-close", "×");
    close.type = "button";
    close.setAttribute("aria-label", `Close ${a ? a.name : "app"}`);
    close.onclick = (e) => {
      e.stopPropagation();
      closeTab(id);
    };
    tab.append(node("span", "tab-icon", icon(a && a.icon)), node("span", "tab-name", a ? a.name : "…"), dot, close);
    tab.title = a ? a.name : "";
    tab.onclick = () => activate(id);
    tab.onauxclick = (e) => e.button === 1 && closeTab(id);
    tab.onkeydown = (e) => {
      if (e.key === "Enter" || e.key === " ") activate(id);
      if (e.key === "Delete") closeTab(id);
    };
    tab.ondragstart = (e) => {
      e.dataTransfer.setData("text/plain", id);
      tab.classList.add("dragging");
    };
    tab.ondragend = () => tab.classList.remove("dragging");
    tab.ondragover = (e) => {
      e.preventDefault();
      tab.classList.add("drop-before");
    };
    tab.ondragleave = () => tab.classList.remove("drop-before");
    tab.ondrop = (e) => {
      e.preventDefault();
      tab.classList.remove("drop-before");
      const moved = e.dataTransfer.getData("text/plain");
      if (!moved || moved === id) return;
      state.tabs = state.tabs.filter((t) => t !== moved);
      state.tabs.splice(state.tabs.indexOf(id), 0, moved);
      persist();
      renderTabs();
    };
    bar.append(tab);
  }
  $("#tab-home").setAttribute("aria-selected", String(state.active === "home"));
}

function persist() {
  saved.set("shartifacts.hub.tabs", state.tabs);
}

function openTab(id) {
  if (!installedApp && !state.tabs.includes(id)) {
    state.tabs.push(id);
    persist();
  }
  ensureView(id);
  activate(id);
}

function closeTab(id) {
  state.tabs = state.tabs.filter((t) => t !== id);
  persist();
  const view = state.views.get(id);
  if (view && focused === view) leaveFocus({ animate: false, push: false });
  if (view) {
    clearInterval(view.logTimer);
    view.panel.remove();
    state.views.delete(id);
  }
  if (state.active === id) activate(state.tabs[state.tabs.length - 1] || "home");
  else renderTabs();
}

function activate(id, push = true) {
  if (focused && focused.id !== id) leaveFocus({ animate: false, push: false });
  state.active = id;
  for (const p of document.querySelectorAll(".panel")) p.classList.toggle("active", p.id === (id === "home" ? "home" : `panel-${id}`));
  renderTabs();
  const tab = id === "home" ? $("#tab-home") : document.getElementById(`tab-${id}`);
  if (tab) tab.scrollIntoView({ block: "nearest", inline: "nearest" });
  const a = id !== "home" && summary(id);
  document.title = a ? (installedApp ? a.name : `${a.name} — shartifacts`) : "shartifacts — apps";
  setTabIcon(a ? a.icon : "");
  offerInstall(id);
  const path = id === "home" ? "/" : `/apps/${id}`;
  if (push && !installedApp && location.pathname !== path) history.pushState({}, "", path);
  if (id !== "home") {
    const view = state.views.get(id);
    if (view && view.detail && !view.started) loadFrame(view);
    if (view && view.drawer === "logs") loadLogs(view);
  }
}

function setTabIcon(name) {
  const link = document.querySelector('link[rel="icon"]');
  const emoji = name && typeof artifactIcon === "function" ? artifactIcon(name) : "";
  link.href = emoji
    ? `data:image/svg+xml,${encodeURIComponent(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><text y=".9em" font-size="88">${emoji}</text></svg>`)}`
    : "/favicon.svg";
}

// ---- installing an app on its own ------------------------------------------------

// offerInstall links the manifest of whatever is showing, so a browser's "Install app"
// installs the app in the active tab under its own name and icon, or the workspace from
// the home tab. The app's manifest needs the session cookie, which a manifest request
// sends only when asked to. It is linked once the app's launcher icon is drawn, so the
// browser's first look at it already finds the icon.
async function offerInstall(id) {
  const link = $('link[rel="manifest"]');
  const touch = $('link[rel="apple-touch-icon"]');
  if (id === "home") {
    link.removeAttribute("crossorigin");
    link.href = "/manifest.webmanifest";
    touch.href = "/icon-192.png";
    return;
  }
  const view = state.views.get(id);
  if (!view || !view.detail) return; // loadDetail calls again
  await drawLauncherIcon(view.detail).catch(() => {});
  if (state.active !== id) return;
  const href = `/apps/${id}/manifest.webmanifest`;
  if (link.getAttribute("href") !== href) {
    link.crossOrigin = "use-credentials";
    link.href = href;
  }
  touch.href = `/apps/${id}/icon.png`;
}

// The install button. A browser offers an install (beforeinstallprompt) for the manifest
// the page loaded with, and the server links an app's own on a full load of /apps/{id}.
// Switching tabs afterwards only swaps the link, which Chrome may ignore when the
// workspace is already installed. So the button prompts when this page loaded as the app,
// and otherwise reloads the page as the app (?install) and offers the prompt there, where
// the browser's first look is at the app's manifest.
let installEvent = null;
const loadedAs = location.pathname.match(/^\/apps\/([0-9a-f]{24})$/)?.[1] || "";
window.addEventListener("beforeinstallprompt", (e) => {
  e.preventDefault();
  installEvent = e;
  if (loadedAs && new URLSearchParams(location.search).has("install")) toast("Ready: tap Install to add this app");
});
window.addEventListener("appinstalled", () => {
  installEvent = null;
  toast("Installed. Open it from your home screen.");
});

async function installApp(id) {
  if (installEvent && loadedAs === id) {
    const ev = installEvent;
    installEvent = null; // a browser allows each offer once
    await ev.prompt();
    return;
  }
  if (loadedAs !== id || !new URLSearchParams(location.search).has("install")) {
    location.assign(`/apps/${id}?install`);
    return;
  }
  toast("Your browser isn't offering this. Open it in Chrome itself, not the installed shartifacts app, then ⋮ → Install app (iPhone: Share → Add to Home Screen). If it says already installed, remove the old home-screen entry.");
}

// A page reloaded for installing waits briefly for the browser's offer, and says so if none comes.
if (loadedAs && new URLSearchParams(location.search).has("install")) {
  setTimeout(() => !installEvent && toast("No install offer yet. Tap Install to see why."), 4000);
}

// launcherKey names what an app's launcher tile shows: its emoji, or its first letter.
const launcherKey = (d) => {
  const emoji = typeof artifactIcon === "function" ? artifactIcon(d.icon) : "";
  return emoji ? `emoji:${emoji}` : `letter:${([...d.name.trim()][0] || "?").toUpperCase()}`;
};
const drawing = new Map();

// drawLauncherIcon draws the tile a phone shows for an app installed on its own, when the
// one the server keeps was drawn from something else. The browser draws it because it has
// the emoji font the server lacks. The picture fills the middle 55%, inside the circle
// launchers crop maskable icons to, so one tile serves as both kinds.
function drawLauncherIcon(d) {
  const key = launcherKey(d);
  if (d.launcher_icon === key) return Promise.resolve();
  if (drawing.get(d.id)?.key === key) return drawing.get(d.id).done;
  const done = (async () => {
    const size = 512;
    const c = document.createElement("canvas");
    c.width = c.height = size;
    const g = c.getContext("2d");
    g.fillStyle = "#f9f8f6";
    g.fillRect(0, 0, size, size);
    const [kind, text] = [key.slice(0, key.indexOf(":")), key.slice(key.indexOf(":") + 1)];
    g.font = kind === "emoji" ? `280px "Apple Color Emoji", "Segoe UI Emoji", "Noto Color Emoji", sans-serif` : `300px Georgia, "Times New Roman", serif`;
    g.fillStyle = "#403b38";
    g.textAlign = "left";
    g.textBaseline = "alphabetic";
    // Centre the glyph's ink, not its line box, which sits emoji and letters off-centre.
    const m = g.measureText(text);
    g.fillText(text, size / 2 - (m.actualBoundingBoxRight - m.actualBoundingBoxLeft) / 2, size / 2 + (m.actualBoundingBoxAscent - m.actualBoundingBoxDescent) / 2);
    const blob = await new Promise((resolve) => c.toBlob(resolve, "image/png"));
    if (!blob) return;
    await api(`/api/apps/${d.id}/launcher-icon?for=${encodeURIComponent(key)}`, { method: "PUT", body: blob, headers: { "Content-Type": "image/png" } });
    d.launcher_icon = key;
  })().finally(() => drawing.get(d.id)?.done === done && drawing.delete(d.id));
  drawing.set(d.id, { key, done });
  return done;
}

$("#tab-home").onclick = () => activate("home");

// ---- an app's panel -------------------------------------------------------------

function frameURL(id, version) {
  const viewer = viewerId();
  return `/run/${id}/${version}/` + (viewer ? `?viewer=${encodeURIComponent(viewer)}` : "");
}

function ensureView(id) {
  if (state.views.has(id)) return state.views.get(id);
  const panel = $("#app-panel").content.firstElementChild.cloneNode(true);
  panel.id = `panel-${id}`;
  panel.setAttribute("aria-labelledby", `tab-${id}`);
  $("#panels").append(panel);
  const view = { id, panel, frame: $("iframe", panel), version: "live", loadedLive: null, drawer: null, detail: null, errors: 0, logTimer: null };
  state.views.set(id, view);
  $(".reload", panel).onclick = () => loadFrame(view);
  $(".version", panel).onchange = (e) => {
    view.version = e.target.value;
    loadFrame(view);
  };
  $(".load-live", panel).onclick = () => {
    view.version = "live";
    loadFrame(view);
  };
  $(".share", panel).onclick = () => openShare(view);
  $(".focus-app", panel).onclick = () => enterFocus(view);
  $(".install-app", panel).onclick = () => installApp(view.id);
  $(".close-drawer", panel).onclick = () => setDrawer(view, null);
  for (const b of panel.querySelectorAll("[data-drawer]")) b.onclick = () => setDrawer(view, view.drawer === b.dataset.drawer ? null : b.dataset.drawer);
  // A tab's frame loads when the tab is first shown, so restoring many tabs starts no
  // backends until they are looked at.
  loadDetail(view).then(() => state.active === id && !view.started && loadFrame(view));
  return view;
}

async function loadDetail(view, logs = false) {
  try {
    view.detail = await api(`/api/apps/${view.id}${logs ? "?logs" : ""}`);
  } catch (e) {
    toast(e.message);
    return;
  }
  renderView(view);
  if (state.active === view.id) offerInstall(view.id);
}

function renderView(view) {
  const d = view.detail;
  if (!d) return;
  const p = view.panel;
  $(".app-icon", p).textContent = icon(d.icon);
  $(".app-name", p).textContent = d.name;
  const sel = $(".version", p);
  sel.replaceChildren();
  const live = node("option", "", d.live ? `Live · v${d.live}` : "Live · none yet");
  live.value = "live";
  sel.append(live);
  for (const dep of d.deployments) {
    const usable = dep.status === "ready" && !dep.pruned;
    const o = node("option", "", `v${dep.version}${dep.version === d.live ? " (live)" : ""}${usable ? "" : ` · ${dep.pruned ? "pruned" : dep.status}`}${dep.note ? ` — ${dep.note.slice(0, 40)}` : ""}`);
    o.value = String(dep.version);
    o.disabled = !usable;
    sel.append(o);
  }
  sel.value = String(view.version);
  const st = summary(view.id) ? statusOf(summary(view.id)) : { cls: "", text: "" };
  const pill = $(".status", p);
  pill.className = `pill status ${st.cls}`;
  pill.textContent = st.text;
  $(".open", p).href = frameURL(view.id, view.version);
  if (view.drawer) renderDrawer(view);
  renderFrameState(view);
}

// renderFrameState covers the frame when there is nothing to show yet.
function renderFrameState(view) {
  const d = view.detail;
  const empty = $(".frame-empty", view.panel);
  if (!d || view.version !== "live" || d.live) {
    empty.hidden = true;
    return;
  }
  const latest = d.deployments[0];
  empty.replaceChildren();
  const box = node("div");
  if (!latest) box.append(node("p", "", "No versions yet. Deploy one with deploy_app or an archive from the Versions panel."));
  else if (latest.status === "building") box.append(node("p", "", `Building version ${latest.version}… This tab shows it as soon as it is live.`));
  else {
    box.append(node("p", "danger", `Version ${latest.version} failed to deploy, so nothing is live yet.`));
    if (latest.error) box.append(node("pre", "console", latest.error));
  }
  empty.append(box);
  empty.hidden = false;
}

// App pages have an opaque origin, so the browser gives them no storage of their own. The
// hub keeps each app's localStorage for them, in IndexedDB so apps never crowd the
// workspace's own localStorage: it names the frame with what the app saved, which the page's
// runtime reads before the app's scripts run, and saves what the page posts back
// (shartifacts-app-storage). sessionStorage lives as long as the tab's view. Browsers take a
// frame's name only when the frame is created, so each load gets a new frame.
const STORAGE_NAME = "shartifacts-storage:";
const appStorage = (() => {
  let db;
  const open = () =>
    (db ||= new Promise((resolve, reject) => {
      const req = indexedDB.open("shartifacts-apps", 1);
      req.onupgradeneeded = () => req.result.createObjectStore("storage");
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => reject(req.error);
    }));
  const run = (mode, op) =>
    open().then(
      (d) =>
        new Promise((resolve, reject) => {
          const tx = d.transaction("storage", mode);
          const req = op(tx.objectStore("storage"));
          tx.oncomplete = () => resolve(req.result);
          tx.onerror = tx.onabort = () => reject(tx.error);
        }),
    );
  return {
    get: (id) => run("readonly", (s) => s.get(id)).catch(() => undefined),
    set: (id, local) => run("readwrite", (s) => s.put(local, id)).catch(() => {}),
    delete: (id) => run("readwrite", (s) => s.delete(id)).catch(() => {}),
  };
})();

async function loadFrame(view) {
  view.started = true;
  const d = view.detail;
  view.loadedLive = d ? d.live : null;
  $(".notice", view.panel).hidden = true;
  $(".version", view.panel).value = String(view.version);
  renderFrameState(view);
  view.local ||= (await appStorage.get(view.id)) || {};
  const frame = view.frame.cloneNode(false);
  frame.name = STORAGE_NAME + JSON.stringify({ app: view.id, local: view.local, session: view.session || {} });
  frame.src = frameURL(view.id, view.version);
  view.frame.replaceWith(frame);
  view.frame = frame;
  $(".open", view.panel).href = frame.src;
}

// syncView follows changes the poll found: a new live version, a finished build.
async function syncView(view) {
  const a = summary(view.id);
  if (!a || !view.detail) return;
  const changed = a.live !== view.detail.live || a.latest !== view.detail.latest || a.status !== (view.detail.deployments[0] || {}).status || a.updated_at !== view.detail.updated_at;
  if (!changed) return;
  await loadDetail(view, view.drawer === "versions");
  if (view.version !== "live" || a.live === view.loadedLive) return;
  if (!view.started) return;
  if (!view.loadedLive) {
    loadFrame(view); // nothing was showing; nothing to lose
    return;
  }
  const notice = $(".notice", view.panel);
  $("span", notice).textContent = `Version ${a.live} is live now.`;
  notice.hidden = false;
}

// ---- drawers --------------------------------------------------------------------

function setDrawer(view, name) {
  view.drawer = name;
  clearInterval(view.logTimer);
  view.logTimer = null;
  for (const b of view.panel.querySelectorAll("[data-drawer]")) b.setAttribute("aria-pressed", String(b.dataset.drawer === name));
  $(".drawer", view.panel).hidden = !name;
  if (!name) return;
  if (name === "versions") loadDetail(view, true);
  else renderDrawer(view);
  if (name === "logs") {
    view.errors = 0;
    updateBadge(view);
    loadLogs(view);
    view.logTimer = setInterval(() => !document.hidden && state.active === view.id && loadLogs(view), 3000);
  }
}

function renderDrawer(view) {
  const title = { versions: "Versions", logs: "Logs", settings: "Settings" }[view.drawer];
  $(".drawer-title", view.panel).textContent = title;
  const body = $(".drawer-body", view.panel);
  if (view.drawer === "versions") renderVersions(view, body);
  if (view.drawer === "settings") renderSettings(view, body);
  if (view.drawer === "logs" && !body.dataset.logs) body.replaceChildren(node("p", "fineprint", "Loading…"));
}

function archiveType(name) {
  if (/\.zip$/i.test(name)) return "application/zip";
  if (/\.(tgz|gz)$/i.test(name)) return "application/gzip";
  if (/\.tar$/i.test(name)) return "application/x-tar";
  return "";
}

function renderVersions(view, body) {
  delete body.dataset.logs;
  const d = view.detail;
  body.replaceChildren();
  const actions = node("div", "drawer-actions");
  const upload = node("label", "button");
  upload.textContent = "Deploy an archive…";
  const input = node("input");
  input.type = "file";
  input.accept = ".zip,.tar,.tgz,.gz";
  input.hidden = true;
  input.onchange = async () => {
    const file = input.files[0];
    if (!file) return;
    const type = archiveType(file.name);
    if (!type) return toast("Choose a .zip, .tar or .tar.gz");
    toast(`Deploying ${file.name}…`);
    try {
      const res = await api(`/api/apps/${view.id}/deployments?note=${encodeURIComponent("Uploaded " + file.name)}`, { method: "POST", body: file, headers: { "Content-Type": type } });
      toast(res.deployment.status === "failed" ? `Version ${res.deployment.version} failed to build` : `Version ${res.deployment.version} is ${res.deployment.status}`);
    } catch (e) {
      toast(e.message);
    }
    refresh();
    loadDetail(view, true);
  };
  upload.append(input);
  const download = node("a", "button", "Download source");
  download.href = `/api/apps/${view.id}/download`;
  actions.append(upload, download);
  body.append(actions);
  if (!d.deployments.length) body.append(node("p", "fineprint", "No versions yet."));
  else {
    // Older versions lose their files, so say how far rollback reaches before someone needs it.
    const reach = d.oldest_rollback ? ` The oldest you can roll back to is v${d.oldest_rollback}.` : "";
    body.append(node("p", "fineprint", `Files are kept for the newest ${d.keep_versions} versions that built, the live one and versions links pin.${reach}`));
  }
  const list = node("div", "version-list");
  for (const dep of d.deployments) {
    const card = node("div", "version" + (dep.version === d.live ? " is-live" : ""));
    const head = node("div", "version-head");
    head.append(node("strong", "", `v${dep.version}`));
    if (dep.version === d.live) head.append(node("span", "pill ok", "live"));
    if (dep.status !== "ready") head.append(node("span", `pill ${dep.status === "building" ? "building" : "failed"}`, dep.status));
    if (dep.pruned) head.append(node("span", "pill", "pruned"));
    if (view.version === String(dep.version)) head.append(node("span", "pill", "showing"));
    card.append(head);
    if (dep.note) card.append(node("p", "version-note", dep.note));
    const parts = [timeAgo(dep.created_at), `${dep.files} files`, dep.backend ? backendLabel[dep.backend] : dep.frontend ? "frontend only" : ""];
    if (dep.network) parts.push("network on");
    card.append(node("div", "version-meta", parts.filter(Boolean).join(" · ")));
    for (const w of dep.warnings || []) card.append(node("p", "version-warn", w));
    if (dep.error) card.append(node("pre", "version-error", dep.error));
    if (dep.log) {
      const det = node("details", "log");
      det.append(node("summary", "", "Build log"), node("pre", "console", dep.log));
      card.append(det);
    }
    const buttons = node("div", "version-buttons");
    if (dep.status === "ready" && !dep.pruned) {
      const preview = node("button", "", view.version === String(dep.version) ? "Showing" : "Preview");
      preview.type = "button";
      preview.disabled = view.version === String(dep.version);
      preview.onclick = () => {
        view.version = String(dep.version);
        loadFrame(view);
        renderVersions(view, body);
      };
      buttons.append(preview);
      if (dep.version !== d.live) {
        const promote = node("button", "primary", dep.version < d.live ? "Roll back to this" : "Make live");
        promote.type = "button";
        promote.onclick = async () => {
          try {
            await api(`/api/apps/${view.id}/live`, { method: "PUT", body: JSON.stringify({ version: dep.version }) });
            toast(`Version ${dep.version} is live`);
            view.version = "live";
            await refresh();
            await loadDetail(view, true);
            loadFrame(view);
          } catch (e) {
            toast(e.message);
          }
        };
        buttons.append(promote);
      }
      const src = node("a", "button", "Source");
      src.href = `/api/apps/${view.id}/download?version=${dep.version}`;
      buttons.append(src);
    }
    if (buttons.childElementCount) card.append(buttons);
    list.append(card);
  }
  body.append(list);
}

async function loadLogs(view) {
  if (view.drawer !== "logs") return;
  const version = view.version === "live" ? (view.detail && view.detail.live) || 0 : view.version;
  let out;
  try {
    out = await api(`/api/apps/${view.id}/logs?version=${version}&tail=300`);
  } catch (e) {
    $(".drawer-body", view.panel).replaceChildren(node("p", "fineprint", e.message));
    return;
  }
  if (view.drawer !== "logs") return;
  const body = $(".drawer-body", view.panel);
  const stick = !body.dataset.logs || body.scrollTop + body.clientHeight >= body.scrollHeight - 30;
  body.dataset.logs = "1";
  body.replaceChildren();
  const actions = node("div", "drawer-actions");
  const restart = node("button", "", "Restart backend");
  restart.type = "button";
  restart.onclick = async () => {
    try {
      await api(`/api/apps/${view.id}/restart`, { method: "POST", body: "{}" });
      toast("Stopped; the next request starts it again");
      loadLogs(view);
    } catch (e) {
      toast(e.message);
    }
  };
  if (out.backend || out.backend_error) actions.append(restart);
  body.append(node("h4", "", `Version ${out.version}`), actions);
  if (out.backend) {
    const b = out.backend;
    const kv = node("dl", "kv");
    const add = (k, v) => v !== undefined && v !== null && v !== "" && kv.append(node("dt", "", k), node("dd", "", String(v)));
    add("Backend", b.state);
    add("PID", b.pid);
    add("Started", b.started && timeAgo(b.started));
    add("Requests", b.requests);
    add("Last exit", b.exit);
    body.append(kv);
    const con = node("pre", "console");
    for (const l of b.lines) {
      const line = node("span", l.stream, `${new Date(l.at).toLocaleTimeString()}  ${l.text}\n`);
      con.append(line);
    }
    if (!b.lines.length) con.textContent = "No output yet. The backend starts on its first request.";
    body.append(node("h4", "", "Backend output"), con);
  } else if (out.backend_error) {
    body.append(node("p", "danger", out.backend_error));
  } else {
    body.append(node("p", "fineprint", "This version has no backend."));
  }
  body.append(node("h4", "", "Browser errors"));
  if (out.frontend_errors.length) {
    const con = node("pre", "console");
    con.textContent = out.frontend_errors.map((e) => `${new Date(e.at).toLocaleTimeString()}  ${e.message}`).join("\n");
    body.append(con);
  } else body.append(node("p", "fineprint", "None reported from this version's pages."));
  if (out.build_log || out.build_error) {
    const det = node("details", "log");
    det.append(node("summary", "", "Build log"), node("pre", "console", [out.build_error, out.build_log].filter(Boolean).join("\n\n")));
    body.append(det);
  }
  if (stick) body.scrollTop = body.scrollHeight;
}

function renderSettings(view, body) {
  delete body.dataset.logs;
  const d = view.detail;
  body.replaceChildren();
  const form = node("form", "settings");
  const field = (label, name, value, max) => {
    const l = node("label", "", label);
    const i = node("input");
    i.name = name;
    i.value = value || "";
    i.maxLength = max;
    l.append(i);
    return l;
  };
  const pin = node("label", "check");
  const pinBox = node("input");
  pinBox.type = "checkbox";
  pinBox.name = "pinned";
  pinBox.checked = !!d.pinned;
  pin.append(pinBox, document.createTextNode("Pin to the top of the launcher"));
  const save = node("button", "primary", "Save");
  save.type = "submit";
  form.append(field("Name", "name", d.name, 100), field("Description", "description", d.description, 300), field("Icon", "icon", d.icon, 32));
  if (state.appOrigin) {
    // Public links' subdomain; empty keeps the generated one. Old hostnames redirect.
    const sub = field("Link subdomain", "label", d.label, 40);
    sub.querySelector("input").placeholder = "generated";
    sub.append(node("small", "", `Public links: ${d.link_origin}/p/…`));
    form.append(sub);
  }
  form.append(pin, save);
  form.onsubmit = async (e) => {
    e.preventDefault();
    const f = new FormData(form);
    const label = state.appOrigin ? { label: f.get("label").trim().toLowerCase() } : {};
    try {
      view.detail = await api(`/api/apps/${view.id}`, {
        method: "PATCH",
        body: JSON.stringify({ name: f.get("name"), description: f.get("description"), icon: f.get("icon"), pinned: pinBox.checked, ...label }),
      });
      toast("Saved");
      refresh();
      renderView(view);
    } catch (err) {
      toast(err.message);
    }
  };
  body.append(form);

  body.append(node("h4", "", "Backend variables & secrets"));
  body.append(node("p", "fineprint", "Given to the backend as environment variables. Values are never shown again; changing them restarts the backend."));
  const list = node("div", "env-list");
  for (const key of d.env) {
    const row = node("div", "env-row");
    const rm = node("button", "quiet", "Remove");
    rm.type = "button";
    rm.onclick = () => setEnv(view, { [key]: null });
    row.append(node("span", "", key), rm);
    list.append(row);
  }
  if (!d.env.length) list.append(node("p", "fineprint", "None set."));
  const envForm = node("form", "env-form");
  const k = node("input");
  k.placeholder = "API_KEY";
  k.pattern = "[A-Z_][A-Z0-9_]*";
  k.required = true;
  k.setAttribute("aria-label", "Variable name");
  const v = node("input");
  v.placeholder = "value";
  v.type = "password";
  v.setAttribute("aria-label", "Value");
  const add = node("button", "", "Set");
  add.type = "submit";
  envForm.append(k, v, add);
  envForm.onsubmit = (e) => {
    e.preventDefault();
    setEnv(view, { [k.value.trim()]: v.value });
  };
  body.append(list, envForm);

  body.append(node("h4", "", "Storage"));
  const usage = node("p", "fineprint", "Measuring…");
  body.append(usage);
  api(`/api/apps/${view.id}/usage`).then(
    (u) => {
      const quota = u.data_quota ? ` of its ${bytes(u.data_quota)} quota` : "";
      usage.replaceChildren(
        `Backend data: ${bytes(u.data_bytes)} in ${u.data_files} files${quota}.`,
        node("br"),
        `Versions: ${u.versions} keep their files, ${bytes(u.versions_bytes)} on disk; files a version did not change are shared with the one before.`,
      );
    },
    (e) => (usage.textContent = e.message),
  );

  body.append(node("h4", "danger", "Danger zone"));
  const del = node("button", "danger", "Delete app, versions & data");
  del.type = "button";
  del.onclick = async () => {
    if (!confirm(`Delete “${d.name}”, all ${d.latest} versions, its backend data and its links? This cannot be undone.`)) return;
    try {
      await api(`/api/apps/${view.id}`, { method: "DELETE" });
      appStorage.delete(view.id);
      toast(`Deleted ${d.name}`);
      closeTab(view.id);
      refresh();
    } catch (err) {
      toast(err.message);
    }
  };
  body.append(del);
}

async function setEnv(view, vars) {
  try {
    view.detail = await api(`/api/apps/${view.id}/env`, { method: "PUT", body: JSON.stringify({ vars }) });
    toast("Variables saved; the backend restarts on its next request");
    renderView(view);
  } catch (e) {
    toast(e.message);
  }
}

function updateBadge(view) {
  const badge = $("[data-drawer=logs] .badge", view.panel);
  badge.hidden = !view.errors;
  badge.textContent = String(view.errors);
}

// Pages report errors (and readiness) and save their storage through postMessage; only our
// own frames count, each for its own app.
const isMap = (v) => !!v && typeof v === "object" && !Array.isArray(v);
addEventListener("message", (e) => {
  const d = e.data;
  if (!d || typeof d !== "object") return;
  for (const view of state.views.values()) {
    if (view.frame.contentWindow !== e.source) continue;
    if (d.type === "shartifacts-app-error" && typeof d.message === "string") {
      view.errors++;
      updateBadge(view);
      api(`/api/apps/${view.id}/errors`, { method: "POST", body: JSON.stringify({ version: Number(d.version) || 0, message: d.message }) }).catch(() => {});
    }
    if (d.type === "shartifacts-app-key" && d.key === "Escape" && focused === view) appEscape();
    if (d.type === "shartifacts-app-storage" && d.app === view.id && isMap(d.local)) {
      view.local = d.local;
      view.session = isMap(d.session) ? d.session : {};
      appStorage.set(view.id, d.local);
    }
  }
});

// ---- sharing --------------------------------------------------------------------

let shareView = null;
async function openShare(view) {
  shareView = view;
  await loadDetail(view);
  renderShares();
  $("#share-version").textContent = view.version === "live" ? `Link to v${view.detail.live}` : `Link to v${view.version}`;
  $("#share-dialog").showModal();
}

function renderShares() {
  const off = !state.shareOrigin;
  $("#share-off").hidden = !off;
  $("#share-live").disabled = $("#share-version").disabled = off || !shareView.detail.live;
  const list = $("#share-list");
  list.replaceChildren();
  for (const sh of shareView.detail.shares) {
    const row = node("div", "share-row");
    const copyRow = node("div", "copy-row");
    const input = node("input");
    input.readOnly = true;
    input.value = sh.url || "(share origin is off)";
    const copy = node("button", "", "Copy");
    copy.type = "button";
    copy.onclick = async () => ((await shartifactsCopy(input.value)) ? toast("Copied") : input.select());
    // Shown large, so someone nearby can scan the link off this screen.
    const qr = node("button", "", "QR code");
    qr.type = "button";
    qr.disabled = !sh.url;
    qr.onclick = () => shartifactsShowQR(sh.url);
    const revoke = node("button", "quiet danger", "Revoke");
    revoke.type = "button";
    revoke.onclick = async () => {
      try {
        await api(`/api/app-shares/${sh.token}`, { method: "DELETE" });
        await loadDetail(shareView);
        renderShares();
        refresh();
      } catch (e) {
        toast(e.message);
      }
    };
    copyRow.append(input, copy, qr, revoke);
    row.append(copyRow, node("small", "", `${sh.version ? `Version ${sh.version}` : "Follows live"}${sh.read_only ? " · read-only" : ""} · created ${timeAgo(sh.created_at)}`));
    list.append(row);
  }
  if (!shareView.detail.shares.length) list.append(node("p", "fineprint", "No links yet."));
}

async function createShare(version) {
  try {
    const sh = await api(`/api/apps/${shareView.id}/shares`, { method: "POST", body: JSON.stringify({ version, read_only: $("#share-readonly").checked }) });
    toast((await shartifactsCopy(sh.url)) ? "Link created and copied" : "Link created");
    await loadDetail(shareView);
    renderShares();
    refresh();
  } catch (e) {
    toast(e.message);
  }
}
$("#share-live").onclick = () => createShare(0);
$("#share-version").onclick = () => createShare(shareView.version === "live" ? shareView.detail.live : Number(shareView.version));

// ---- new app, guide, connect ----------------------------------------------------

for (const b of document.querySelectorAll("[data-close]")) b.onclick = () => b.closest("dialog").close();

function openNew(template) {
  const form = $("#new-form");
  form.reset();
  $(".form-error", form).textContent = "";
  $("#archive-name").textContent = "";
  $("#archive-label").hidden = true;
  if (template) form.elements.name.value = "Notes";
  $("#new-dialog").showModal();
}
$("#new-app").onclick = () => openNew(false);
$("#empty-template").onclick = () => openNew(true);
$("#new-form").addEventListener("change", (e) => {
  if (e.target.name === "source") $("#archive-label").hidden = e.target.value !== "archive";
  if (e.target.id === "archive") $("#archive-name").textContent = e.target.files[0] ? e.target.files[0].name : "";
});
$("#new-form").onsubmit = async (e) => {
  e.preventDefault();
  const form = e.target;
  const f = new FormData(form);
  const err = $(".form-error", form);
  err.textContent = "";
  const button = $("#create-app");
  button.disabled = true;
  button.textContent = "Building…";
  try {
    let res;
    if (f.get("source") === "archive") {
      const file = $("#archive").files[0];
      if (!file) throw new Error("Choose an archive first");
      const type = archiveType(file.name);
      if (!type) throw new Error("Choose a .zip, .tar or .tar.gz");
      const q = new URLSearchParams({ name: f.get("name"), description: f.get("description"), icon: f.get("icon"), note: "Uploaded " + file.name });
      res = await api(`/api/apps?${q}`, { method: "POST", body: file, headers: { "Content-Type": type } });
    } else {
      res = await api("/api/apps", {
        method: "POST",
        body: JSON.stringify({ name: f.get("name"), description: f.get("description") || "A React frontend talking to a Go backend.", icon: f.get("icon") || "notes", template: "notes", note: "Started from the notes template" }),
      });
    }
    $("#new-dialog").close();
    if (res.deployment && res.deployment.status === "failed") toast("Created, but the first version failed to build: see Versions");
    await refresh();
    openTab(res.app.id);
    if (res.deployment && res.deployment.status === "failed") setDrawer(state.views.get(res.app.id), "versions");
  } catch (ex) {
    err.textContent = ex.message;
  } finally {
    button.disabled = false;
    button.textContent = "Create & deploy ↗";
  }
};

$("#show-guide").onclick = async () => {
  $("#guide-dialog").showModal();
  if ($("#guide-text").textContent) return;
  try {
    const res = await fetch("/api/apps/guide");
    $("#guide-text").textContent = await res.text();
  } catch (e) {
    $("#guide-text").textContent = e.message;
  }
};


// ---- focus ----------------------------------------------------------------------

// Focus fills the window with one app. It lifts the app's own frame rather than loading the
// app again, so nothing the app holds is lost. The frame takes its new size at once and then
// grows from where it was by transform alone (FLIP): the app lays out once, and only the
// compositor works while it moves. /apps/{id}?focus opens an app this way, and Back leaves.
const reduceMotion = matchMedia("(prefers-reduced-motion: reduce)");
let focused = null;
let focusPushed = false;
let focusAnim = null;
let tuckTimer = 0;
let lastEscape = 0;

function flipFrom(el, from, done) {
  const to = el.getBoundingClientRect();
  if (reduceMotion.matches || !from.width || !from.height || !to.width || !to.height) return done();
  const start = `translate(${from.left - to.left}px, ${from.top - to.top}px) scale(${from.width / to.width}, ${from.height / to.height})`;
  el.style.willChange = "transform";
  const anim = el.animate(
    [
      { transformOrigin: "0 0", transform: start },
      { transformOrigin: "0 0", transform: "none" },
    ],
    { duration: 320, easing: "cubic-bezier(0.2, 0.8, 0.2, 1)" },
  );
  focusAnim = anim;
  // A cancelled run (a second tap mid-flight) settles too, so classes never go stale.
  anim.onfinish = anim.oncancel = () => {
    el.style.willChange = "";
    if (focusAnim === anim) focusAnim = null;
    done();
  };
}

// The chrome under a focused frame stays in the page but out of reach of Tab and screen
// readers.
function chromeInert(view, on) {
  $(".hub-bar").inert = on;
  for (const sel of [".app-toolbar", ".notice", ".drawer"]) $(sel, view.panel).inert = on;
}

function enterFocus(view, push = true) {
  if (focused === view) return;
  if (focused) leaveFocus({ animate: false, push: false });
  const wrap = $(".frame-wrap", view.panel);
  // Measured before anything changes, transforms included, so a reversal starts from
  // where the frame is on screen.
  const from = wrap.getBoundingClientRect();
  focusAnim?.cancel();
  focused = view;
  wrap.classList.remove("unfocusing");
  wrap.classList.add("focused");
  chromeInert(view, true);
  flipFrom(wrap, from, () => {});
  const a = summary(view.id);
  const bar = $("#focus-bar");
  $(".focus-icon", bar).textContent = icon(a && a.icon);
  $(".focus-name", bar).textContent = a ? a.name : "";
  bar.classList.remove("hinting");
  bar.hidden = false;
  showFocusBar();
  if (push) {
    history.pushState({ focus: view.id }, "", `/apps/${view.id}?focus`);
    focusPushed = true;
  }
  view.frame.focus();
}

function leaveFocus({ animate = true, push = true } = {}) {
  const view = focused;
  if (!view) return;
  const wrap = $(".frame-wrap", view.panel);
  const from = wrap.getBoundingClientRect();
  focusAnim?.cancel();
  focused = null;
  wrap.classList.remove("focused");
  chromeInert(view, false);
  $("#focus-bar").hidden = true;
  clearTimeout(tuckTimer);
  if (animate) {
    wrap.classList.add("unfocusing");
    flipFrom(wrap, from, () => wrap.classList.remove("unfocusing"));
  }
  if (push && new URLSearchParams(location.search).has("focus")) {
    // Undo our own history entry, so Back after leaving does not re-enter focus. A page
    // opened at ?focus has no entry of ours to undo, so it drops the query instead.
    if (focusPushed) history.back();
    else history.replaceState({}, "", `/apps/${view.id}`);
  }
  focusPushed = false;
  $(".focus-app", view.panel).focus({ preventScroll: true });
}

// The pill shows on entering and whenever it is reached for, then tucks into a handle so it
// covers as little of the app as it can.
function showFocusBar(hold = 2400) {
  const bar = $("#focus-bar");
  bar.classList.remove("tucked");
  clearTimeout(tuckTimer);
  if (hold) tuckTimer = setTimeout(() => !bar.matches(":hover, :focus-within") && bar.classList.add("tucked"), hold);
}
{
  const bar = $("#focus-bar");
  bar.onpointerenter = () => showFocusBar(0);
  bar.onpointerleave = () => showFocusBar(900);
  bar.addEventListener("focusin", () => showFocusBar(0));
  bar.addEventListener("focusout", () => showFocusBar(900));
  $(".focus-handle", bar).onclick = () => showFocusBar();
  $(".focus-exit", bar).onclick = () => leaveFocus();
}

// Escape inside an app is the app's own first (closing its menus and dialogs), so leaving
// from there takes a second press. The first shows how.
function appEscape() {
  const now = Date.now();
  if (now - lastEscape < 600) {
    lastEscape = 0;
    leaveFocus();
    return;
  }
  lastEscape = now;
  $("#focus-bar").classList.add("hinting");
  showFocusBar();
}

// ---- routing & keys -------------------------------------------------------------

function route(push) {
  const m = location.pathname.match(/^\/apps\/([0-9a-f]{24})/);
  if (m) {
    openTab(m[1]);
    const view = state.views.get(m[1]);
    if (new URLSearchParams(location.search).has("focus")) enterFocus(view, false);
    else leaveFocus({ push: false });
  } else activate("home", push);
}
addEventListener("popstate", () => route(false));
addEventListener("keydown", (e) => {
  if (focused && e.key === "Escape" && !document.querySelector("dialog[open]")) {
    e.preventDefault();
    leaveFocus();
    return;
  }
  if (!e.altKey || e.metaKey || e.ctrlKey) return;
  const n = Number(e.key);
  if (n >= 1 && n <= 9) {
    e.preventDefault();
    activate(n === 1 ? "home" : state.tabs[n - 2] || state.active);
  }
});

(async () => {
  await refresh();
  if (!installedApp) for (const id of state.tabs) ensureView(id);
  route(false);
  setInterval(() => !document.hidden && refresh(), 4000);
  document.addEventListener("visibilitychange", () => !document.hidden && refresh());
})();
