const $ = (s) => document.querySelector(s);
const state = {
  // items is the library; archive is the archive, fetched only while it is in view or an
  // archived artifact is open, so a long archive costs nothing the rest of the time.
  items: [],
  archive: null,
  archiveView: false,
  artifact: null,
  version: 0,
  view: "preview",
  editing: false,
  dirty: false,
  base: 0,
  // The source being worked on: the main source and supporting files by path. The editor
  // shows one at a time: file is a path, or "" for the main source.
  main: "",
  files: {},
  file: "",
};
const filePathPattern = /^[A-Za-z0-9_][A-Za-z0-9._-]*(\/[A-Za-z0-9_][A-Za-z0-9._-]*)*$/;
function syncSource() {
  if (state.file) state.files[state.file] = $("#source").value;
  else state.main = $("#source").value;
}
function workingSource() {
  syncSource();
  return { content: state.main, files: { ...state.files } };
}
// An update's files: everything being worked on, and null for each file it removed.
function fileChanges(previous = {}) {
  const out = { ...state.files };
  for (const p of Object.keys(previous)) if (!(p in out)) out[p] = null;
  return out;
}
function mainName() {
  const kind = $("#edit-kind").value || state.artifact?.revisions.at(-1).kind;
  return { html: "index.html", react: "App.tsx", markdown: "index.md", slides: "slides.md", svg: "image.svg", mermaid: "diagram.mmd" }[kind] || "main";
}
function renderFiles() {
  const paths = Object.keys(state.files).sort(),
    select = $("#file-select");
  select.replaceChildren(
    ...["", ...paths].map((p) => {
      const o = document.createElement("option");
      o.value = p;
      o.textContent = p || `${mainName()} (page)`;
      return o;
    }),
  );
  select.value = state.file;
  $("#files").hidden = !paths.length && !state.editing;
  $("#add-file").hidden = $("#remove-file").hidden = !state.editing;
  $("#remove-file").disabled = !state.file;
}
function showFile(path) {
  syncSource();
  state.file = path;
  $("#source").value = path ? state.files[path] : state.main;
  renderFiles();
}
let toastTimer,
  previewTimer,
  previewSequence = 0;
function setPreview(url) {
  const el = $("#preview");
  // A fresh frame prevents overlapping draft/saved navigations from retaining
  // a provisional sandbox document, and gives every render a clean JS runtime.
  const next = el.cloneNode(false);
  next.dataset.ready = "false";
  next.src = url;
  el.replaceWith(next);
}
function frameFor(w) {
  const main = $("#preview");
  if (w === main.contentWindow) return { id: state.artifact?.id, src: main.getAttribute("src") };
  const thumb = [...document.querySelectorAll(".gallery-thumb iframe")].find(
    (f) => f.contentWindow === w,
  );
  return thumb && { id: thumb.dataset.id, src: thumb.getAttribute("src"), thumbnail: true };
}
bridgeArtifactFrames(frameFor);
let commentAnchor = "";
hostArtifactFrames(frameFor, {
  // A page's comments.openComposer opens this page's comment box with what it pointed at.
  compose: (quote, anchor) => {
    setQuote(quote || "");
    commentAnchor = anchor || "";
    commentsOpen(true);
    $("#comment-form textarea").focus();
  },
  // artifact.publish: views move to the new version; a files publish keeps its frame.
  published: async (id, version, keepFrame) => {
    if (state.artifact?.id !== id || state.dirty) return;
    if (!keepFrame) return load(id);
    state.artifact = await api(`/api/artifacts/${encodeURIComponent(id)}`);
    state.version = state.artifact.revisions.length;
    render({ keepFrame: true });
  },
  commented: () => loadComments(),
});
forwardDeckKeys(
  () => $("#preview"),
  () =>
    !$("#artifact").hidden &&
    state.view !== "source" &&
    state.view !== "changes" &&
    (state.editing ? $("#edit-kind").value : state.artifact?.revisions[state.version - 1]?.kind) === "slides",
);
window.addEventListener("message", (event) => {
  const el = $("#preview");
  if (
    event.source === el.contentWindow &&
    event.data?.type === "shartifacts-preview-ready" &&
    event.data.url === el.getAttribute("src")
  )
    el.dataset.ready = "true";
});
function clearDraft() {
  try {
    localStorage.removeItem(draftKey(state.artifact.id));
  } catch {}
}
async function previewDraft() {
  const seq = ++previewSequence;
  try {
    const d = await api("/api/previews", {
      method: "POST",
      body: JSON.stringify({
        title: $("#edit-title").value,
        kind: $("#edit-kind").value,
        ...workingSource(),
      }),
    });
    if (seq === previewSequence && state.editing) setPreview(d.url);
  } catch (e) {
    if (seq === previewSequence) toast("Preview: " + e.message);
  }
}
// A toast may offer one action, such as undoing what it reports.
function toast(message, action) {
  $("#toast").textContent = message;
  if (action) {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = action.label;
    b.onclick = () => {
      $("#toast").hidden = true;
      action.run();
    };
    $("#toast").append(" ", b);
  }
  $("#toast").hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => ($("#toast").hidden = true), 4500);
}
// Counts this page's writes, at their start and end, so the poll can tell when a list it
// fetched overlapped a save and may predate it.
let localWrites = 0;
async function api(path, options = {}) {
  const write = !!options.method && options.method !== "GET";
  if (write) localWrites++;
  const r = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", "X-Shartifacts-Viewer": viewerId(), ...options.headers },
  }).finally(() => write && localWrites++);
  if (!r.ok) {
    let msg;
    try {
      msg = (await r.json()).error;
    } catch {
      msg = await r.text().catch(() => r.statusText);
    }
    throw new Error(msg || `Request failed (${r.status})`);
  }
  return r.json();
}
function ago(date) {
  return new Date(date).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}
const icon = {
  html: "</>",
  react: "{ }",
  markdown: "M↓",
  slides: "▭",
  svg: "◇",
  mermaid: "⑂",
  code: "#_",
};
// Pinned artifacts lead; otherwise the server's most-recently-updated order.
const byPin = (a, b) => (b.pinned ? 1 : 0) - (a.pinned ? 1 : 0);
function gallery() {
  const cards = $("#gallery-cards");
  cards.replaceChildren();
  // Thumbnails are live artifacts; keep none running while the home page is hidden.
  if ($("#welcome").hidden) return;
  $("#gallery").hidden = !state.items.length;
  for (const a of [...state.items].sort(byPin).slice(0, 12)) {
    // A live, non-interactive render of the latest version, loaded as it scrolls in.
    const thumb = document.createElement("div");
    thumb.className = "gallery-thumb";
    const frame = document.createElement("iframe");
    frame.loading = "lazy";
    frame.tabIndex = -1;
    frame.setAttribute("aria-hidden", "true");
    frame.setAttribute("sandbox", $("#preview").getAttribute("sandbox"));
    frame.referrerPolicy = "no-referrer";
    frame.dataset.id = a.id;
    frame.src = `/preview/${a.id}?version=${a.version}`;
    thumb.append(frame);
    const card = document.createElement("div");
    card.className = "gallery-card";
    const open = document.createElement("button");
    open.className = "gallery-open";
    const kind = document.createElement("span");
    kind.className = "eyebrow";
    kind.textContent = `${a.kind.toUpperCase()}${a.pinned ? " · PINNED" : ""}`;
    const title = document.createElement("strong");
    title.textContent = (artifactIcon(a.icon) ? artifactIcon(a.icon) + " " : "") + a.title;
    open.append(kind, title);
    if (a.description) {
      const p = document.createElement("span");
      p.className = "gallery-description";
      p.textContent = a.description;
      open.append(p);
    }
    const meta = document.createElement("small");
    meta.textContent = `v${a.version} · ${ago(a.updated_at)}`;
    open.append(meta);
    card.append(thumb, open);
    card.onclick = () => navigate(a.id);
    cards.append(card);
  }
}
// Library search: typed words, tag chips and the filter menus together. With none of
// them the library lists everything; otherwise the server ranks and excerpts matches.
const searchTags = new Set();
let searchTimer,
  searchSequence = 0;
function searching() {
  return Boolean(
    $("#search").value.trim() || searchTags.size || $("#filter").value || $("#when").value || $("#has").value,
  );
}
// The sidebar lists the library or, once asked, the archive: the artifacts put away so
// the library stays short. Either can be searched, and selected in bulk.
function shelf() {
  return state.archiveView ? state.archive || [] : state.items;
}
const byArchived = (a, b) => String(b.archived_at).localeCompare(String(a.archived_at));
function archiveOpen(open) {
  state.archiveView = open;
  selected.clear();
  const b = $("#toggle-archive");
  b.setAttribute("aria-pressed", String(open));
  if (open) b.textContent = "← Back to the library";
  else b.innerHTML = '<span aria-hidden="true">▤</span> Archive';
  return refresh({ force: true });
}
// Select mode turns the list into checkboxes for archiving, restoring or deleting many.
const selected = new Set();
let selecting = false,
  lastPicked = "";
function selectMode(on) {
  selecting = on;
  selected.clear();
  $("#toggle-select").setAttribute("aria-pressed", String(on));
  $("#toggle-select").textContent = on ? "Done" : "Select";
  list();
}
function bulkBar(visible) {
  const n = selected.size;
  $("#bulk-bar").hidden = !selecting;
  $("#bulk-count").textContent = n ? `${n} selected` : "None selected";
  $("#bulk-all").textContent = n && visible.every((id) => selected.has(id)) ? "Select none" : "Select all";
  $("#bulk-archive").hidden = state.archiveView;
  $("#bulk-restore").hidden = $("#bulk-delete").hidden = !state.archiveView;
  for (const b of ["#bulk-archive", "#bulk-restore", "#bulk-delete"]) $(b).disabled = !n;
}
function pick(id, range) {
  const ids = [...$("#library").querySelectorAll(".library-item")].map((b) => b.dataset.id);
  const on = !selected.has(id);
  // Shift-click selects (or clears) everything between the last pick and this one.
  const from = ids.indexOf(lastPicked),
    to = ids.indexOf(id);
  for (const i of range && from >= 0 ? ids.slice(Math.min(from, to), Math.max(from, to) + 1) : [id])
    on ? selected.add(i) : selected.delete(i);
  lastPicked = id;
  for (const b of $("#library").querySelectorAll(".library-item"))
    b.setAttribute("aria-pressed", String(selected.has(b.dataset.id)));
  bulkBar(ids);
}
function segments(parent, parts, fallback) {
  if (!parts?.length) {
    parent.textContent = fallback;
    return;
  }
  for (const part of parts) {
    if (part.match) {
      const m = document.createElement("mark");
      m.textContent = part.text;
      parent.append(m);
    } else parent.append(part.text);
  }
}
function tagChips() {
  const counts = new Map();
  for (const a of shelf()) for (const t of a.tags || []) counts.set(t, (counts.get(t) || 0) + 1);
  const top = [...counts].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).slice(0, 10).map(([t]) => t);
  for (const t of searchTags) if (!top.includes(t)) top.unshift(t);
  const chips = $("#tag-chips");
  chips.replaceChildren(
    ...top.map((t) => {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = `#${t}`;
      b.setAttribute("aria-pressed", String(searchTags.has(t)));
      b.onclick = () => {
        if (searchTags.has(t)) searchTags.delete(t);
        else searchTags.add(t);
        list();
      };
      return b;
    }),
  );
  chips.hidden = !top.length;
  activeFilters();
}
// The filters fold away so the library keeps the room; what is filtering it stays in view as
// chips that remove themselves, and the Filters button counts them.
const filterMenus = ["#filter", "#when", "#has"];
function filtersOpen(open) {
  $("#filters").hidden = !open;
  $("#toggle-filters").setAttribute("aria-expanded", String(open));
  localStorage.setItem("shartifacts.filters", open ? "open" : "closed");
  activeFilters();
}
function activeFilters() {
  const chips = [];
  const chip = (label, remove) => {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = `${label} ×`;
    b.setAttribute("aria-label", `Remove filter ${label}`);
    b.onclick = () => {
      remove();
      list();
    };
    chips.push(b);
  };
  for (const t of searchTags) chip(`#${t}`, () => searchTags.delete(t));
  for (const el of filterMenus.map($))
    if (el.value) chip(el.selectedOptions[0].textContent, () => (el.value = ""));
  const count = $("#filter-count");
  count.textContent = chips.length;
  count.hidden = !chips.length;
  const row = $("#active-filters");
  if (chips.length > 1) {
    const clear = document.createElement("button");
    clear.type = "button";
    clear.className = "clear-filters";
    clear.textContent = "Clear all";
    clear.onclick = () => {
      searchTags.clear();
      for (const el of filterMenus.map($)) el.value = "";
      list();
    };
    chips.push(clear);
  }
  row.replaceChildren(...chips);
  row.hidden = !chips.length || !$("#filters").hidden;
}
// Filter the library to one tag, from a tag shown anywhere else.
function findTag(tag) {
  searchTags.clear();
  searchTags.add(tag);
  $("#search").value = "";
  list();
  if (narrowScreen.matches) libraryOpen(true);
}
function libraryItem(a, hit) {
  const b = document.createElement("button");
  b.className =
    "library-item" +
    (a.id === state.artifact?.id ? " active" : "") +
    (a.pinned ? " pinned" : "") +
    (selecting ? " selecting" : "");
  b.dataset.id = a.id;
  if (selecting) b.setAttribute("aria-pressed", String(selected.has(a.id)));
  b.title = a.description ? `${a.title} — ${a.description}` : a.title;
  const i = document.createElement("span"),
    emoji = artifactIcon(a.icon);
  i.className = "type-icon" + (emoji ? " emoji" : "");
  i.textContent = emoji || icon[a.kind];
  const t = document.createElement("span");
  t.className = "library-text";
  const strong = document.createElement("strong");
  segments(strong, hit?.title_segments, a.title);
  const small = document.createElement("small");
  small.textContent = `${a.pinned && !a.archived_at ? "PINNED · " : ""}${a.kind.toUpperCase()} · v${a.version}`;
  if (a.archived_at)
    small.textContent += ` · archived ${new Date(a.archived_at).toLocaleDateString(undefined, { month: "short", day: "numeric" })}`;
  else if (hit) small.textContent += ` · ${ago(a.updated_at)}`;
  t.append(strong, small);
  const excerpt = hit?.snippet?.length ? hit.snippet : null;
  if (hit && (excerpt || a.description)) {
    const p = document.createElement("span");
    p.className = "library-snippet";
    segments(p, excerpt, a.description);
    t.append(p);
  }
  b.append(i, t);
  if (selecting) {
    const box = document.createElement("span");
    box.className = "select-box";
    box.setAttribute("aria-hidden", "true");
    b.prepend(box);
  }
  b.onclick = (e) => (selecting ? pick(a.id, e.shiftKey) : navigate(a.id));
  return b;
}
function showLibrary(entries, { total, suggestion, active }) {
  // A selection is only ever of what is in view, so a bulk action does what it shows.
  const visible = entries.map(([a]) => a.id);
  for (const id of selected) if (!visible.includes(id)) selected.delete(id);
  $("#library").replaceChildren(...entries.map(([a, hit]) => libraryItem(a, hit)));
  $("#library").classList.toggle("archive-shelf", state.archiveView);
  $("#library-label").textContent = active ? "RESULTS" : state.archiveView ? "ARCHIVE" : "THE LIBRARY";
  $("#count").textContent = active ? total : shelf().length;
  $("#did-you-mean").hidden = !suggestion;
  $("#suggestion").textContent = suggestion || "";
  bulkBar(visible);
  if (!entries.length) {
    const p = document.createElement("p");
    p.className = "empty-library";
    p.textContent = state.archiveView && !shelf().length
      ? "Nothing archived. Archive what you no longer need and the library stays short; it keeps every version, and agents can tidy up too."
      : !shelf().length
        ? "An empty shelf. Plenty of possibility."
        : "Nothing matches. Try fewer words, or filters like tag:finance, kind:react, has:comments or updated:month.";
    $("#library").append(p);
  }
}
// Resolves once the library shows the newest results: false when newer ones replaced them.
function list({ immediate = false } = {}) {
  // Gallery thumbnails are live pages; rebuild them only when the library changes.
  const shown = JSON.stringify([state.items, state.artifact?.id]);
  if (shown !== galleried) gallery();
  galleried = shown;
  listed = listKey();
  tagChips();
  clearTimeout(searchTimer);
  if (!searching()) {
    searchSequence++;
    showLibrary(
      [...shelf()].sort(state.archiveView ? byArchived : byPin).map((a) => [a]),
      { active: false },
    );
    return Promise.resolve(true);
  }
  // Typing settles briefly before each search; an older response never replaces a newer one.
  const sequence = ++searchSequence;
  return new Promise((resolve) => {
    searchTimer = setTimeout(async () => {
      const params = new URLSearchParams({ q: $("#search").value, limit: "100" });
      if (state.archiveView) params.set("archived", "only");
      for (const t of searchTags) params.append("tag", t);
      for (const [key, el] of [["kind", "#filter"], ["updated", "#when"], ["has", "#has"]])
        if ($(el).value) params.set(key, $(el).value);
      try {
        const res = await api(`/api/search?${params}`);
        if (sequence !== searchSequence) return resolve(false);
        showLibrary(
          res.results.map((r) => [r, r]),
          { total: res.total, suggestion: res.suggestion, active: true },
        );
        resolve(true);
      } catch (e) {
        if (sequence === searchSequence) toast(e.message);
        resolve(false);
      }
    }, immediate ? 0 : 120);
  });
}
let listed = "",
  galleried = "";
function listKey() {
  return JSON.stringify([state.items, state.archive, state.archiveView, state.artifact?.id]);
}
async function refresh({ force = false } = {}) {
  const items = await api("/api/artifacts");
  // The archive is fetched only when it is in view, or for an open artifact that is (or
  // has just gone) out of the library, so the poll can tell archived from deleted.
  const open = state.artifact;
  const archived =
    state.archiveView || open?.archived_at || (open && !items.some((a) => a.id === open.id))
      ? await api("/api/artifacts?archived=only")
      : null;
  state.items = items;
  state.archive = archived;
  // The poll calls this every few seconds; rebuilding unchanged lists would drop focus.
  if (force || listKey() !== listed) list();
}
function draftKey(id) {
  return `shartifacts.draft.${id}`;
}
function remember() {
  if (!state.artifact) return;
  try {
    if (state.dirty)
      localStorage.setItem(
        draftKey(state.artifact.id),
        JSON.stringify({
          title: $("#edit-title").value,
          kind: $("#edit-kind").value,
          ...workingSource(),
          note: $("#edit-note").value,
          description: $("#edit-description").value,
          icon: $("#edit-icon").value,
          tags: $("#edit-tags").value,
          base: state.base,
        }),
      );
  } catch {
    toast(
      "Browser draft storage is unavailable. Keep this tab open until you save.",
    );
  }
}
// Keep the same frame and draft alive when changing presentation.
let fullscreenFocus;
function leaveImmersive() {
  document.body.classList.remove("immersive");
  $("#exit-fullscreen").hidden = true;
  fullscreenFocus?.focus();
}
$("#fullscreen").onclick = async () => {
  fullscreenFocus = document.activeElement;
  document.body.classList.add("immersive");
  $("#exit-fullscreen").hidden = false;
  $("#exit-fullscreen").focus();
  // LAN HTTP and some mobile browsers lack this API. CSS still fills the viewport.
  try {
    await $("#panes").requestFullscreen?.();
  } catch {
    /* Keep the viewport fallback when native fullscreen is unavailable. */
  }
};
$("#exit-fullscreen").onclick = async () => {
  if (document.fullscreenElement) await document.exitFullscreen();
  leaveImmersive();
};
document.addEventListener("fullscreenchange", () => {
  if (!document.fullscreenElement && document.body.classList.contains("immersive"))
    leaveImmersive();
});
function libraryOpen(open) {
  document.body.classList.toggle("library-open", open);
  $("#toggle-library").setAttribute("aria-expanded", String(open));
  $("#library-backdrop").hidden = !open;
  $(".workspace").inert = open;
  (open ? $("#close-library") : $("#toggle-library")).focus();
}
$("#toggle-library").onclick = () => libraryOpen(true);
$("#close-library").onclick = $("#library-backdrop").onclick = () =>
  libraryOpen(false);
// An open artifact fills the workspace; its details or its comments open on demand.
function detailsOpen(open) {
  if (open) commentsOpen(false);
  document.body.classList.toggle("details-open", open);
  $("#toggle-details").setAttribute("aria-expanded", String(open));
}
function commentsOpen(open) {
  if (open) detailsOpen(false);
  document.body.classList.toggle("comments-open", open);
  $("#comments-link").setAttribute("aria-expanded", String(open));
}
$("#toggle-details").onclick = () =>
  detailsOpen(!document.body.classList.contains("details-open"));
$("#comments-link").onclick = () => {
  const open = !document.body.classList.contains("comments-open");
  commentsOpen(open);
  if (open) $("#comment-form textarea").focus();
};
$("#close-comments").onclick = () => {
  commentsOpen(false);
  $("#comments-link").focus();
};
const narrowScreen = matchMedia("(max-width: 900px)");
narrowScreen.addEventListener("change", () => {
  if (!narrowScreen.matches && document.body.classList.contains("library-open"))
    libraryOpen(false);
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") {
    if (document.body.classList.contains("library-open")) libraryOpen(false);
    else if (document.body.classList.contains("comments-open") && !document.querySelector("dialog[open]"))
      $("#close-comments").click();
    if (document.body.classList.contains("immersive") && !document.fullscreenElement)
      leaveImmersive();
  }
  if (
    e.key === "Tab" &&
    document.body.classList.contains("library-open") &&
    !document.querySelector("dialog[open]")
  ) {
    const controls = [
      ...$("#sidebar").querySelectorAll("a,button,input,select"),
    ].filter((el) => el.getClientRects().length);
    const first = controls[0], last = controls.at(-1);
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }
});
// Line diff (Myers): a list of [op, line] with op " ", "-" or "+", or null when the
// edit distance is too large to trace in memory.
function diffLines(a, b) {
  let start = 0,
    endA = a.length,
    endB = b.length;
  while (start < endA && start < endB && a[start] === b[start]) start++;
  while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) endA--, endB--;
  const A = a.slice(start, endA),
    B = b.slice(start, endB),
    N = A.length,
    M = B.length,
    off = N + M,
    v = new Int32Array(2 * (N + M) + 2),
    trace = [];
  let found = N === 0 && M === 0;
  for (let d = 0; !found && d <= N + M; d++) {
    if (d > 2000) return null;
    trace.push(v.slice(off - d, off + d + 1));
    for (let k = -d; k <= d; k += 2) {
      let x = k === -d || (k !== d && v[off + k - 1] < v[off + k + 1]) ? v[off + k + 1] : v[off + k - 1] + 1;
      let y = x - k;
      while (x < N && y < M && A[x] === B[y]) x++, y++;
      v[off + k] = x;
      if (x >= N && y >= M) {
        found = true;
        break;
      }
    }
  }
  const middle = [];
  let x = N,
    y = M;
  for (let d = trace.length - 1; d >= 0; d--) {
    const w = trace[d],
      k = x - y,
      at = (j) => w[j + d];
    const prevK = k === -d || (k !== d && at(k - 1) < at(k + 1)) ? k + 1 : k - 1;
    const prevX = d === 0 ? 0 : at(prevK),
      prevY = d === 0 ? 0 : prevX - prevK;
    while (x > prevX && y > prevY) middle.push([" ", A[--x]]), y--;
    if (d > 0) middle.push(x === prevX ? ["+", B[--y]] : ["-", A[--x]]);
  }
  return [
    ...a.slice(0, start).map((l) => [" ", l]),
    ...middle.reverse(),
    ...a.slice(endA).map((l) => [" ", l]),
  ];
}
// Changes: the selected version (or the unsaved draft) against an earlier version; by
// default the one before it (the latest saved one for a draft).
let compareWith = null;
function renderChanges() {
  const a = state.artifact;
  if (!a || state.view !== "changes") return;
  const draft = state.editing && state.dirty,
    version = draft ? a.revisions.length : state.version,
    select = $("#compare-base");
  const bases = draft ? a.revisions.map((r) => r.version) : a.revisions.map((r) => r.version).filter((n) => n < version);
  select.replaceChildren(
    ...[...bases, 0].reverse().map((n) => {
      const o = document.createElement("option");
      o.value = n;
      o.textContent = n ? `v${n}` : "nothing (new)";
      return o;
    }),
  );
  select.value = String(bases.includes(compareWith) || compareWith === 0 ? compareWith : bases.at(-1) || 0);
  const base = Number(select.value);
  const before = base ? a.revisions[base - 1] : { title: "", kind: "", content: "" };
  const after = draft
    ? { title: $("#edit-title").value, kind: $("#edit-kind").value, ...workingSource() }
    : a.revisions[version - 1];
  const out = $("#diff");
  out.replaceChildren();
  const note = (text) => out.append(node("div", "diff-note", text));
  if (base && before.title !== after.title) note(`Title: “${before.title}” → “${after.title}”`);
  if (base && before.kind !== after.kind) note(`Type: ${before.kind} → ${after.kind}`);
  // The page, then each file either version has.
  const paths = [...new Set([...Object.keys(before.files || {}), ...Object.keys(after.files || {})])].sort();
  let added = 0,
    removed = 0;
  for (const p of ["", ...paths]) {
    const from = p ? before.files?.[p] : before.content,
      to = p ? after.files?.[p] : after.content;
    if ((p || paths.length) && from === to) continue;
    if (paths.length) out.append(node("div", "diff-file", p || mainName()));
    const counts = renderDiff(out, from, to, note);
    added += counts[0];
    removed += counts[1];
  }
  if (!out.querySelector(".diff-line, .diff-file")) note("No source changes.");
  $("#changes-title").textContent = `${draft ? "UNSAVED DRAFT" : "V" + version} CHANGES · +${added} −${removed}`;
}
// Appends the line diff of one file (undefined: absent in that version); returns [+, −].
function renderDiff(out, from, to, note) {
  if (to === undefined) return note("Removed."), [0, from.split("\n").length];
  const lines = diffLines(from ? from.split("\n") : [], to.split("\n"));
  if (!lines) return note("Too many changes to compare line by line; rewritten."), [0, 0];
  if (!lines.some(([op]) => op !== " ")) return note("No changes."), [0, 0];
  let ln = [0, 0],
    added = 0,
    removed = 0;
  const rows = lines.map(([op, text]) => {
    if (op !== "+") ln[0]++;
    if (op !== "-") ln[1]++;
    added += op === "+";
    removed += op === "-";
    return { op, text, a: op === "+" ? "" : ln[0], b: op === "-" ? "" : ln[1] };
  });
  // Show changed lines with three lines of context; collapse the rest.
  const keep = rows.map(() => false);
  rows.forEach((r, i) => {
    if (r.op !== " ") for (let j = Math.max(0, i - 3); j <= Math.min(rows.length - 1, i + 3); j++) keep[j] = true;
  });
  for (let i = 0; i < rows.length; ) {
    if (!keep[i]) {
      let j = i;
      while (j < rows.length && !keep[j]) j++;
      out.append(node("div", "diff-gap", `⋯ ${j - i} unchanged line${j - i === 1 ? "" : "s"}`));
      i = j;
      continue;
    }
    const r = rows[i++],
      row = node("div", "diff-line" + (r.op === "+" ? " ins" : r.op === "-" ? " del" : ""));
    row.append(node("span", "", String(r.a)), node("span", "", String(r.b)), node("span", "", r.op), node("span", "", r.text));
    out.append(row);
  }
  return [added, removed];
}
$("#file-select").onchange = () => showFile($("#file-select").value);
$("#add-file").onclick = () => {
  const p = prompt("New file path, such as style.css, data.json or components/Chart.tsx");
  if (p === null) return;
  const path = p.trim();
  if (!filePathPattern.test(path)) return toast("Use a relative path of letters, digits, . _ - and /");
  syncSource();
  if (!(path in state.files)) state.files[path] = "";
  showFile(path);
  changed();
  $("#source").focus();
};
$("#remove-file").onclick = () => {
  if (!state.file || !confirm(`Remove ${state.file} from this artifact?`)) return;
  delete state.files[state.file];
  state.file = "";
  $("#source").value = state.main;
  renderFiles();
  changed();
};
$("#compare-base").onchange = () => {
  compareWith = Number($("#compare-base").value);
  renderChanges();
};
function setView(view) {
  state.view = view;
  $("#panes").dataset.view = view;
  renderChanges();
  document.querySelectorAll("[data-view]").forEach((b) => {
    if (b.tagName === "BUTTON")
      b.classList.toggle("active", b.dataset.view === view);
  });
}
function setEditing(value) {
  if (!value) {
    clearTimeout(previewTimer);
    previewSequence++;
  }
  state.editing = value;
  document.body.classList.toggle("editing", value);
  $("#editor-meta").hidden = !value;
  $("#save").hidden = !value;
  $("#discard").hidden = !value;
  $("#edit").hidden = value;
  $("#source").readOnly = !value;
  renderFiles();
}
// Tags as typed: separated by commas or spaces, with or without #.
function tagList(text) {
  return [...new Set(text.split(/[,\s]+/).map((t) => t.replace(/^#+/, "").toLowerCase()).filter(Boolean))];
}
function renderTags(tags) {
  $("#artifact-tags").replaceChildren(
    ...tags.map((t) => {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = `#${t}`;
      b.title = `Everything tagged ${t}`;
      b.onclick = () => findTag(t);
      return b;
    }),
  );
  $("#artifact-tags").hidden = !tags.length;
}
// Artifacts that share this one's vocabulary, and tags its words suggest.
let relatedFor = "";
async function related(a, { force = false } = {}) {
  const key = `${a.id}:${a.revisions.length}:${(a.tags || []).join()}`;
  if (key === relatedFor && !force) return;
  relatedFor = key;
  let res;
  try {
    res = await api(`/api/artifacts/${a.id}/related`);
  } catch {
    return;
  }
  if (relatedFor !== key) return;
  $("#related-list").replaceChildren(
    ...res.related.slice(0, 4).map((r) => {
      const b = document.createElement("button");
      b.type = "button";
      b.className = "related-item";
      const title = document.createElement("strong");
      title.textContent = (artifactIcon(r.icon) ? artifactIcon(r.icon) + " " : "") + r.title;
      b.append(title);
      if (r.shared?.length) {
        const why = document.createElement("small");
        why.textContent = `Shares ${r.shared.join(", ")}`;
        b.append(why);
      }
      b.onclick = () => navigate(r.id);
      return b;
    }),
  );
  $("#related").hidden = !res.related.length;
  state.suggestedTags = res.suggested_tags;
  renderTagSuggestions();
}
function renderTagSuggestions() {
  const have = new Set(tagList($("#edit-tags").value)),
    offer = (state.suggestedTags || []).filter((t) => !have.has(t)).slice(0, 5);
  const box = $("#tag-suggestions");
  box.replaceChildren();
  if (offer.length) {
    const label = document.createElement("span");
    label.textContent = "Suggested";
    box.append(label);
  }
  for (const t of offer) {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = `+ ${t}`;
    b.setAttribute("aria-label", `Add tag ${t}`);
    b.onclick = () => {
      const input = $("#edit-tags");
      input.value = [...tagList(input.value), t].join(", ");
      input.dispatchEvent(new Event("input"));
      renderTagSuggestions();
    };
    box.append(b);
  }
  box.hidden = !offer.length;
}
function render({ keepFrame = false } = {}) {
  const a = state.artifact,
    r = a.revisions[state.version - 1],
    latest = a.revisions.length;
  $("#welcome").hidden = true;
  $("#artifact").hidden = false;
  $("#artifact-name").textContent = r.title;
  $("#artifact-description").textContent = a.description || "";
  $("#artifact-description").hidden = !a.description;
  $("#pin").textContent = a.pinned ? "Unpin from library top" : "Pin to top of library";
  renderArchived();
  $("#type-label").textContent = `${r.kind.toUpperCase()} / ARTIFACT`;
  $("#crumb").textContent = `Workspace / ${r.title}`;
  $("#saved-at").textContent =
    `Saved ${ago(r.created_at)} · ${latest} version${latest === 1 ? "" : "s"}`;
  $("#revision-note").textContent =
    r.note || "Every saved version stays in your library.";
  $("#versions").replaceChildren();
  for (const v of [...a.revisions].reverse()) {
    const o = document.createElement("option");
    o.value = v.version;
    o.textContent = `v${v.version}${v.version === latest ? " · latest" : ""}`;
    o.selected = v.version === state.version;
    $("#versions").append(o);
  }
  state.main = r.content;
  state.files = { ...(r.files || {}) };
  state.file = "";
  $("#source").value = r.content;
  $("#edit-title").value = r.title;
  $("#edit-kind").value = r.kind;
  $("#edit-note").value = "";
  $("#edit-description").value = a.description || "";
  $("#edit-icon").value = a.icon || "";
  $("#edit-tags").value = (a.tags || []).join(", ");
  renderTags(a.tags || []);
  related(a);
  setTabIcon(a.icon);
  // A page with files is served from its version's directory, beside them.
  if (!keepFrame)
    setPreview(
      r.files ? `/preview/${a.id}/${r.version}/` : `/preview/${a.id}?version=${r.version}`,
    );
  compareWith = null;
  queueMicrotask(renderChanges);
  $("#download").href = `/api/artifacts/${a.id}/download?version=${r.version}`;
  $("#open-view").href =
    r.version === latest ? `/view/${a.id}` : `/view/${a.id}?version=${r.version}`;
  $("#restore").hidden = state.version === latest;
  $("#edit").hidden = state.version !== latest;
  $("#draft-status").textContent = "Saved";
  $("#remote-change").hidden = true;
  state.dirty = false;
  state.base = latest;
  setEditing(false);
  $("#edit").hidden = state.version !== latest;
  list();
}
// Comment threads on the open artifact. Agents read and answer them over MCP.
let comments = [],
  commentQuote = "";
function node(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}
function commentNode(c) {
  const el = node("div", "comment");
  const meta = node("div", "comment-meta");
  meta.append(
    node("strong", "", c.author),
    ` · v${c.version} · ${ago(c.created_at)}${c.to_claude ? " · for your agent" : ""}${c.anchor ? " · on " + c.anchor.split(" > ").pop() : ""}`,
  );
  el.append(meta);
  if (c.quote) el.append(node("blockquote", "comment-quote", c.quote));
  el.append(node("p", "comment-body", c.body));
  return el;
}
function renderComments() {
  const all = $("#show-resolved").checked,
    threads = $("#threads");
  threads.replaceChildren();
  const roots = comments.filter((c) => !c.parent && (all || !c.resolved));
  for (const root of roots) {
    const thread = node("article", "thread" + (root.resolved ? " resolved" : ""));
    for (const c of [root, ...comments.filter((r) => r.parent === root.id)])
      thread.append(commentNode(c));
    const actions = node("div", "thread-actions");
    const reply = node("button", "", "Reply");
    reply.type = "button";
    reply.onclick = () => {
      if (thread.querySelector(".reply-form")) return;
      const form = node("form", "reply-form");
      const text = node("textarea");
      text.rows = 2;
      text.required = true;
      text.setAttribute("aria-label", "Reply");
      const send = node("button", "primary", "Reply");
      form.append(text, send);
      form.onsubmit = async (e) => {
        e.preventDefault();
        send.disabled = true;
        try {
          await addComment({ body: text.value, parent: root.id });
        } catch (err) {
          toast(err.message);
          send.disabled = false;
        }
      };
      thread.append(form);
      text.focus();
    };
    const resolve = node("button", "", root.resolved ? "Reopen" : "Resolve");
    resolve.type = "button";
    resolve.onclick = async () => {
      try {
        await api(`/api/artifacts/${state.artifact.id}/comments/${root.id}`, {
          method: "PUT",
          body: JSON.stringify({ resolved: !root.resolved }),
        });
        await loadComments();
      } catch (e) {
        toast(e.message);
      }
    };
    actions.append(reply, resolve);
    thread.append(actions);
    threads.append(thread);
  }
  if (!roots.length)
    threads.append(
      node("p", "fineprint", comments.length ? "No open comments." : "No comments yet."),
    );
  const open = comments.filter((c) => !c.parent && !c.resolved).length;
  $("#comments-link").textContent = open ? `Comments · ${open}` : "Comments";
}
async function loadComments() {
  const id = state.artifact?.id;
  if (!id) return;
  try {
    const list = await api(`/api/artifacts/${encodeURIComponent(id)}/comments`);
    if (state.artifact?.id !== id) return;
    comments = list;
    renderComments();
  } catch {
    /* Keep the threads already shown when the server is unreachable. */
  }
}
async function addComment(input) {
  await api(`/api/artifacts/${state.artifact.id}/comments`, {
    method: "POST",
    body: JSON.stringify({ author: viewerName || undefined, ...input }),
  });
  await loadComments();
  await refresh();
}
function setQuote(text) {
  commentQuote = text.slice(0, 2000);
  $("#comment-quote").textContent = commentQuote;
  $("#comment-quote").hidden = $("#clear-quote").hidden = !commentQuote;
}
$("#source").addEventListener("select", (e) => {
  const t = e.target;
  if (t.selectionEnd > t.selectionStart)
    setQuote(t.value.slice(t.selectionStart, t.selectionEnd));
});
$("#clear-quote").onclick = () => {
  setQuote("");
  commentAnchor = "";
};
// Who is using this browser: artifacts see them through the user capability and keep their
// private data under their id. There are no accounts; people simply say who they are.
let viewerName = "";
async function renderViewer() {
  try {
    const { people } = await api("/api/people");
    const me = people.find((p) => p.id === viewerId());
    viewerName = me?.name || "";
    $("#viewer").textContent = me ? me.name : "Who are you?";
    $("#viewer").style.setProperty("--viewer", me?.color || "transparent");
    return people;
  } catch {
    return [];
  }
}
function setViewer(id) {
  try {
    if (id) localStorage.setItem("shartifacts.viewer", id);
    else localStorage.removeItem("shartifacts.viewer");
  } catch {
    toast("This browser blocks local storage, so it cannot remember who you are.");
  }
  $("#viewer-dialog").close();
  renderViewer();
  // Artifacts read who is viewing when they start, so restart the one on screen.
  if (state.artifact && !state.dirty) render();
}
$("#viewer").onclick = async () => {
  const people = await renderViewer();
  const list = $("#people");
  list.replaceChildren(
    ...people.map((p) => {
      const b = node("button", "person" + (p.id === viewerId() ? " current" : ""), p.name);
      b.type = "button";
      b.style.setProperty("--viewer", p.color);
      b.onclick = () => setViewer(p.id);
      return b;
    }),
  );
  if (!people.length) list.append(node("p", "fineprint", "Nobody yet. Add yourself below."));
  $("#viewer-dialog").showModal();
};
$("#person-form").onsubmit = async (e) => {
  e.preventDefault();
  try {
    const p = await api("/api/people", { method: "POST", body: JSON.stringify({ name: e.target.elements.name.value }) });
    e.target.reset();
    setViewer(p.id);
  } catch (err) {
    toast(err.message);
  }
};
$("#forget-viewer").onclick = () => setViewer("");
renderViewer();
$("#show-resolved").onchange = renderComments;
$("#comment-form").onsubmit = async (e) => {
  e.preventDefault();
  const form = e.target,
    b = form.querySelector("[type=submit]");
  b.disabled = true;
  try {
    await addComment({
      body: form.elements.body.value,
      quote: commentQuote || undefined,
      anchor: commentAnchor || undefined,
    });
    form.reset();
    setQuote("");
    commentAnchor = "";
  } catch (err) {
    toast(err.message);
  } finally {
    b.disabled = false;
  }
};
async function load(id) {
  try {
    const a = await api(`/api/artifacts/${encodeURIComponent(id)}`);
    if (state.artifact?.id !== a.id) {
      detailsOpen(false);
      commentsOpen(false);
      comments = [];
      setQuote("");
      renderComments();
    }
    state.artifact = a;
    loadComments();
    const requested = Number(
      new URL(location.href).searchParams.get("version"),
    );
    state.version =
      requested >= 1 && requested <= a.revisions.length
        ? requested
        : a.revisions.length;
    render();
    let draft;
    try {
      draft = JSON.parse(localStorage.getItem(draftKey(id)));
    } catch {}
    if (
      draft &&
      typeof draft.content === "string" &&
      typeof draft.title === "string" &&
      Number.isInteger(draft.base)
    ) {
      setEditing(true);
      setView("split");
      state.main = draft.content;
      state.files = draft.files && typeof draft.files === "object" ? draft.files : {};
      state.file = "";
      $("#source").value = draft.content;
      $("#edit-title").value = draft.title;
      $("#edit-kind").value = draft.kind;
      $("#edit-note").value = draft.note;
      if (typeof draft.description === "string")
        $("#edit-description").value = draft.description;
      if (typeof draft.icon === "string") $("#edit-icon").value = draft.icon;
      if (typeof draft.tags === "string") $("#edit-tags").value = draft.tags;
      state.base = draft.base;
      state.dirty = true;
      renderFiles();
      $("#draft-status").textContent = "Recovered draft";
      $("#remote-change").hidden = draft.base === a.revisions.length;
      previewDraft();
      toast("Your unsaved draft was recovered.");
    }
  } catch (e) {
    toast(e.message);
    if (!state.artifact) {
      $("#welcome").hidden = false;
      $("#artifact").hidden = true;
      list();
    }
  }
}
async function navigate(id) {
  remember();
  if (document.body.classList.contains("library-open")) libraryOpen(false);
  history.pushState({}, "", `/a/${id}`);
  await load(id);
}
function changed() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(previewDraft, 650);
  state.dirty = true;
  $("#draft-status").textContent = "Unsaved · draft kept in this browser";
  remember();
  renderChanges();
}
async function save() {
  if (!state.editing) return;
  clearTimeout(previewTimer);
  previewSequence++;
  const b = $("#save");
  b.disabled = true;
  try {
    const a = await api(`/api/artifacts/${state.artifact.id}`, {
      method: "PUT",
      body: JSON.stringify({
        title: $("#edit-title").value,
        kind: $("#edit-kind").value,
        content: workingSource().content,
        files: fileChanges(state.artifact.revisions[state.base - 1]?.files),
        note: $("#edit-note").value,
        description: $("#edit-description").value,
        icon: $("#edit-icon").value,
        tags: tagList($("#edit-tags").value),
        base_version: state.base,
      }),
    });
    state.dirty = false;
    clearDraft();
    state.artifact = a;
    state.version = a.revisions.length;
    render();
    await refresh();
    toast(`Version ${state.version} saved`);
  } catch (e) {
    toast(e.message);
    if (e.message.includes("changed")) $("#remote-change").hidden = false;
  } finally {
    b.disabled = false;
  }
}
async function create(input) {
  const a = await api("/api/artifacts", {
    method: "POST",
    body: JSON.stringify(input),
  });
  await refresh();
  await navigate(a.id);
  return a;
}
async function copy(text) {
  try {
    if (navigator.clipboard && window.isSecureContext)
      await navigator.clipboard.writeText(text);
    else {
      const t = document.createElement("textarea");
      t.value = text;
      t.style.position = "fixed";
      t.style.opacity = "0";
      const host = document.querySelector("dialog[open]") || document.body;
      host.append(t);
      t.focus();
      t.select();
      const ok = document.execCommand("copy");
      t.remove();
      if (!ok) throw new Error("Clipboard unavailable");
    }
    toast("Copied");
  } catch {
    toast(
      "Copy unavailable in this browser. Select and copy the address manually.",
    );
  }
}
function openCreate() {
  const form = $("#create-form");
  form.querySelector(".form-error").textContent = "";
  $("#create-dialog").showModal();
}
$("#new").onclick = openCreate;
$("#start").onclick = openCreate;
document
  .querySelectorAll("[data-close]")
  .forEach((b) => (b.onclick = () => b.closest("dialog").close()));
$("#search").oninput = list;
for (const el of filterMenus) $(el).onchange = list;
$("#toggle-filters").onclick = () => filtersOpen($("#filters").hidden);
filtersOpen(localStorage.getItem("shartifacts.filters") === "open");
// The sidebar's less frequent destinations share one menu at its foot, so the library keeps
// the height. It closes on a choice, a click elsewhere or Escape.
function sidebarMenuOpen(open, { focus = false } = {}) {
  $("#sidebar-menu").hidden = !open;
  $("#toggle-sidebar-menu").setAttribute("aria-expanded", String(open));
  if (open && focus) $("#sidebar-menu").querySelector("a,button").focus();
}
$("#toggle-sidebar-menu").onclick = (e) =>
  sidebarMenuOpen($("#sidebar-menu").hidden, { focus: e.detail === 0 });
$("#sidebar-menu").addEventListener("click", (e) => {
  // A theme choice keeps the menu open, so the next one can be tried in place.
  if (e.target.closest("a,button") && !e.target.closest("[data-palette-choice]")) sidebarMenuOpen(false);
});
document.addEventListener("click", (e) => {
  if (!e.target.closest("#sidebar-menu, #toggle-sidebar-menu")) sidebarMenuOpen(false);
});
$("#sidebar").addEventListener("keydown", (e) => {
  if (e.key === "Escape" && selecting && $("#sidebar-menu").hidden && !e.target.closest("#search")) {
    e.stopPropagation();
    selectMode(false);
    $("#toggle-select").focus();
    return;
  }
  if (e.key !== "Escape" || $("#sidebar-menu").hidden) return;
  e.stopPropagation();
  sidebarMenuOpen(false);
  $("#toggle-sidebar-menu").focus();
});
$("#suggestion").onclick = () => {
  $("#search").value = $("#suggestion").textContent;
  list();
  $("#search").focus();
};
$("#search").addEventListener("keydown", (e) => {
  if (e.key === "Escape" && $("#search").value) {
    e.stopPropagation();
    $("#search").value = "";
    list();
  } else if (e.key === "ArrowDown") {
    e.preventDefault();
    $("#library .library-item")?.focus();
  } else if (e.key === "Enter") {
    e.preventDefault();
    list({ immediate: true }).then((shown) => {
      const first = $("#library .library-item");
      if (shown !== false && first) navigate(first.dataset.id);
    });
  }
});
$("#library").addEventListener("keydown", (e) => {
  if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
  const items = [...$("#library").querySelectorAll(".library-item")],
    at = items.indexOf(document.activeElement);
  if (at < 0) return;
  e.preventDefault();
  if (e.key === "ArrowUp" && at === 0) $("#search").focus();
  else items[Math.min(items.length - 1, Math.max(0, at + (e.key === "ArrowDown" ? 1 : -1)))].focus();
});
document
  .querySelectorAll("button[data-view]")
  .forEach((b) => (b.onclick = () => setView(b.dataset.view)));
$("#edit").onclick = () => {
  compareWith = null;
  setEditing(true);
  setView("split");
  $("#source").focus();
};
$("#save").onclick = save;
$("#discard").onclick = () => {
  if (state.dirty && !confirm("Discard your unsaved draft?")) return;
  clearDraft();
  render();
};
for (const id of ["source", "edit-title", "edit-kind", "edit-note", "edit-description", "edit-icon", "edit-tags"])
  $("#" + id).addEventListener("input", changed);
$("#source").addEventListener("keydown", (e) => {
  if (e.key === "Tab" && state.editing) {
    e.preventDefault();
    const t = e.target,
      start = t.selectionStart;
    t.setRangeText("  ", start, t.selectionEnd, "end");
    changed();
  }
});
$("#versions").onchange = () => {
  if (state.dirty) {
    $("#versions").value = state.version;
    toast("Save or discard your draft before changing versions.");
    return;
  }
  remember();
  state.version = Number($("#versions").value);
  render();
};
$("#restore").onclick = async () => {
  const r = state.artifact.revisions[state.version - 1];
  try {
    const a = await api(`/api/artifacts/${state.artifact.id}`, {
      method: "PUT",
      body: JSON.stringify({
        ...r,
        version: undefined,
        created_at: undefined,
        files: {
          ...Object.fromEntries(
            Object.keys(state.artifact.revisions.at(-1).files || {}).map((p) => [p, null]),
          ),
          ...r.files,
        },
        base_version: state.artifact.revisions.length,
        note: `Restored from v${r.version}`,
      }),
    });
    state.artifact = a;
    state.version = a.revisions.length;
    render();
    await refresh();
    toast("Previous version restored as a new version");
  } catch (e) {
    toast(e.message);
  }
};
$("#reload").onclick = async () => {
  if (
    state.dirty &&
    !confirm("Discard your local draft and load the latest saved version?")
  )
    return;
  state.dirty = false;
  clearDraft();
  await load(state.artifact.id);
};
$("#share").onclick = () =>
  copy(`${location.origin}/a/${state.artifact.id}?version=${state.version}`);
$("#download").onclick = () => {};
$("#more").onclick = () => ($("#more-menu").hidden = !$("#more-menu").hidden);
document.addEventListener("click", (e) => {
  if (!e.target.closest("#more-menu") && !e.target.closest("#more"))
    $("#more-menu").hidden = true;
});
$("#copy-source").onclick = () => {
  $("#more-menu").hidden = true;
  // The file the source pane shows: the page, or the supporting file picked.
  syncSource();
  copy(state.file ? state.files[state.file] : state.main);
};
$("#fork").onclick = async () => {
  const r = state.artifact.revisions[state.version - 1];
  try {
    await create({
      title: r.title + " (copy)",
      kind: r.kind,
      content: r.content,
      files: r.files,
      description: state.artifact.description || undefined,
      icon: state.artifact.icon || undefined,
      tags: state.artifact.tags,
      note: `Copied from ${state.artifact.id} v${r.version}`,
    });
    $("#more-menu").hidden = true;
    toast("Artifact duplicated");
  } catch (e) {
    toast(e.message);
  }
};
// Read-only links for people outside the network, on the operator's share origin.
async function renderShares() {
  const id = state.artifact.id;
  const [{ share_origin }, { shares }] = await Promise.all([api("/api/config"), api(`/api/artifacts/${id}/shares`)]);
  $("#share-off").hidden = !!share_origin;
  $("#share-latest").disabled = $("#share-this").disabled = !share_origin;
  $("#share-this").textContent = `Link to this version (v${state.version})`;
  const list = $("#share-list");
  list.replaceChildren(
    ...shares.map((sh) => {
      const row = node("div", "share-row");
      const copyBtn = node("button", "", "Copy");
      copyBtn.type = "button";
      copyBtn.onclick = () => copy(sh.url);
      const revoke = node("button", "danger", "Revoke");
      revoke.type = "button";
      revoke.onclick = async () => {
        if (!confirm("Revoke this link? Anyone using it loses access at once.")) return;
        await api(`/api/shares/${sh.token}`, { method: "DELETE" });
        toast("Link revoked");
        renderShares();
      };
      row.append(node("code", "", sh.url || "(sharing is off)"), node("span", "fineprint", sh.version ? `v${sh.version}` : "latest"), copyBtn, revoke);
      return row;
    }),
  );
  if (!shares.length) list.append(node("p", "fineprint", "No links yet."));
}
async function createShare(version) {
  try {
    const sh = await api(`/api/artifacts/${state.artifact.id}/shares`, { method: "POST", body: JSON.stringify({ version }) });
    await renderShares();
    copy(sh.url);
  } catch (e) {
    toast(e.message);
  }
}
$("#share-out").onclick = async () => {
  $("#more-menu").hidden = true;
  try {
    await renderShares();
    $("#share-dialog").showModal();
  } catch (e) {
    toast(e.message);
  }
};
$("#share-latest").onclick = () => createShare(0);
$("#share-this").onclick = () => createShare(state.version);
$("#pin").onclick = async () => {
  try {
    const a = await api(`/api/artifacts/${state.artifact.id}/pin`, {
      method: "PUT",
      body: JSON.stringify({ pinned: !state.artifact.pinned }),
    });
    state.artifact.pinned = a.pinned;
    $("#pin").textContent = a.pinned ? "Unpin from library top" : "Pin to top of library";
    $("#more-menu").hidden = true;
    await refresh();
    toast(a.pinned ? "Pinned to the top of your library" : "Unpinned");
  } catch (e) {
    toast(e.message);
  }
};
// closeArtifact returns to the home page once the open artifact is deleted.
function closeArtifact() {
  try {
    localStorage.removeItem(draftKey(state.artifact.id));
  } catch {}
  state.artifact = null;
  state.dirty = false;
  setEditing(false);
  history.pushState({}, "", "/artifacts");
  $("#artifact").hidden = true;
  $("#welcome").hidden = false;
  $("#more-menu").hidden = true;
  $("#crumb").textContent = "Workspace / Your artifacts";
  setTabIcon("");
}
$("#delete").onclick = async () => {
  if (
    !confirm(
      "Delete this artifact and all saved versions? This cannot be undone.",
    )
  )
    return;
  try {
    await api(
      `/api/artifacts/${state.artifact.id}?base_version=${state.artifact.revisions.length}`,
      { method: "DELETE" },
    );
    closeArtifact();
    await refresh();
    toast("Artifact deleted");
  } catch (e) {
    toast(e.message);
  }
};
$("#delete-archived").onclick = $("#delete").onclick;
// An archived artifact still opens, with a notice saying so; the more menu archives or
// restores it.
function renderArchived() {
  const at = state.artifact?.archived_at;
  $("#archived-notice").hidden = !at;
  if (at) $("#archived-at").textContent = `Archived ${ago(at)}.`;
  $("#archive").textContent = at ? "Restore to library" : "Archive artifact";
}
const plural = (n, one) => `${n} ${one}${n === 1 ? "" : "s"}`;
// archive archives, restores or permanently deletes artifacts (delete takes only archived
// ones) and reports what happened, offering to undo an archive or restore.
async function archive(op, ids) {
  const res = await api("/api/artifacts/bulk", { method: "POST", body: JSON.stringify({ op, ids }) });
  const open = state.artifact;
  if (open && res.done.includes(open.id)) {
    if (op === "delete") closeArtifact();
    else open.archived_at = op === "archive" ? new Date().toISOString() : undefined;
    if (op !== "delete") renderArchived();
  }
  for (const id of res.done) selected.delete(id);
  await refresh({ force: true });
  const n = res.done.length;
  const said = { archive: "Archived", restore: "Restored", delete: "Deleted" }[op];
  let message = `${said} ${plural(n, "artifact")}`;
  if (res.failed.length) message += `; ${plural(res.failed.length, "artifact")} could not be: ${res.failed[0].error}`;
  const undo = { archive: "restore", restore: "archive" }[op];
  toast(message, undo && n ? { label: "Undo", run: () => archive(undo, res.done).catch((e) => toast(e.message)) } : undefined);
  return res;
}
$("#archive").onclick = async () => {
  $("#more-menu").hidden = true;
  try {
    await archive(state.artifact.archived_at ? "restore" : "archive", [state.artifact.id]);
  } catch (e) {
    toast(e.message);
  }
};
$("#unarchive").onclick = () => archive("restore", [state.artifact.id]).catch((e) => toast(e.message));
$("#toggle-select").onclick = () => selectMode(!selecting);
$("#toggle-archive").onclick = () => archiveOpen(!state.archiveView).catch((e) => toast(e.message));
$("#bulk-all").onclick = () => {
  const ids = [...$("#library").querySelectorAll(".library-item")].map((b) => b.dataset.id);
  const all = ids.length && ids.every((id) => selected.has(id));
  selected.clear();
  if (!all) for (const id of ids) selected.add(id);
  list();
};
for (const [button, op] of [["#bulk-archive", "archive"], ["#bulk-restore", "restore"], ["#bulk-delete", "delete"]])
  $(button).onclick = async () => {
    const ids = [...selected];
    if (
      op === "delete" &&
      !confirm(`Permanently delete ${plural(ids.length, "archived artifact")} with every version, their data and comments? This cannot be undone.`)
    )
      return;
    try {
      await archive(op, ids);
    } catch (e) {
      toast(e.message);
    }
  };
$("#create-form").onsubmit = async (e) => {
  e.preventDefault();
  const form = e.target,
    b = form.querySelector("[type=submit]");
  b.disabled = true;
  try {
    const data = Object.fromEntries(new FormData(form));
    await create(data);
    $("#create-dialog").close();
    form.reset();
    toast("Artifact created");
  } catch (err) {
    form.querySelector(".form-error").textContent = err.message;
  } finally {
    b.disabled = false;
  }
};
$("#import-file").onchange = async (e) => {
  const file = e.target.files[0];
  if (!file) return;
  if (file.size > 2 * 1024 * 1024) {
    $("#create-form .form-error").textContent =
      "Files must be 2 MiB or smaller.";
    return;
  }
  try {
    const ext = file.name.split(".").pop().toLowerCase(),
      form = $("#create-form");
    form.elements.title.value = file.name.replace(/\.[^.]+$/, "");
    form.elements.kind.value =
      {
        html: "html",
        htm: "html",
        jsx: "react",
        tsx: "react",
        md: "markdown",
        markdown: "markdown",
        svg: "svg",
        mmd: "mermaid",
      }[ext] || "code";
    form.elements.content.value = await file.text();
  } catch (err) {
    $("#create-form .form-error").textContent = err.message;
  }
};
function mediaEmbed(item) {
  if (/^(text\/(csv|plain|markdown)|application\/)/.test(item.mime_type))
    return `await window.fs.readFile(${JSON.stringify(item.name || item.url)}${/^text|json/.test(item.mime_type) ? ', { encoding: "utf8" }' : ""})`;
  if (item.mime_type.startsWith("video/"))
    return `<video controls playsinline preload="metadata" src="${item.url}"></video>`;
  if (item.mime_type.startsWith("audio/"))
    return `<audio controls preload="metadata" src="${item.url}"></audio>`;
  if (item.mime_type === "text/vtt")
    return `<track kind="captions" src="${item.url}" srclang="en" label="English" default>`;
  return `<img src="${item.url}" alt="">`;
}
// mediaUnused shows only the files no artifact or app names, for clearing out.
let mediaUnused = false;
async function refreshMedia() {
  const { media } = await api(mediaUnused ? "/api/media?usage=1" : "/api/media");
  const list = $("#media-list");
  list.replaceChildren();
  $("#media-unused").textContent = mediaUnused ? "Show all files" : "Find unused files";
  $("#media-unused").hidden = !media.length && !mediaUnused;
  if (!media.length) {
    list.textContent = "Your media library is ready for its first file.";
    return;
  }
  const shown = mediaUnused ? media.filter((m) => !m.used_by.length) : media;
  if (mediaUnused) {
    const size = shown.reduce((n, m) => n + m.size, 0);
    list.append(
      Object.assign(document.createElement("p"), {
        textContent: shown.length
          ? `${shown.length} of ${media.length} files (${(size / 1048576).toFixed(1)} MiB) are not named by any artifact, its saved data or an app. A page that lets people pick files as it runs could still use one.`
          : "Every file is named by an artifact or an app.",
      }),
    );
  }
  for (const item of shown) {
    const row = document.createElement("div");
    row.className = "media-item";
    const info = document.createElement("span");
    info.textContent = `${item.name ? item.name + " · " : ""}${item.mime_type} · ${(item.size / 1048576).toFixed(2)} MiB`;
    const code = document.createElement("code");
    code.textContent = item.url;
    const actions = document.createElement("div");
    actions.className = "media-actions";
    const button = document.createElement("button");
    button.textContent = "Copy embed";
    button.onclick = () => copy(mediaEmbed(item));
    const del = document.createElement("button");
    del.className = "quiet danger";
    del.textContent = "Delete";
    del.onclick = () => deleteMedia(item, del);
    actions.append(button, del);
    row.append(info, code, actions);
    list.append(row);
  }
}
// deleteMedia asks first, naming what still uses the file: deleting cannot be undone, and
// pages that embed it show it as missing.
async function deleteMedia(item, button) {
  const label = item.name || item.id;
  button.disabled = true;
  try {
    const { media } = await api("/api/media?usage=1");
    const users = media.find((m) => m.id === item.id)?.used_by || [];
    const names = users.slice(0, 3).map((u) => `${u.kind} “${u.title}”`);
    if (users.length > 3) names.push(`${users.length - 3} more`);
    const question = users.length
      ? `“${label}” is used by ${names.join(", ")}. Delete it anyway? They will show it as missing. This cannot be undone.`
      : `Delete “${label}”? No artifact or app names it. This cannot be undone.`;
    if (!confirm(question)) return;
    await api(`/api/media/${encodeURIComponent(item.id)}`, { method: "DELETE" });
    toast(`Deleted ${label}`);
    await refreshMedia();
  } catch (e) {
    $("#media-status").textContent = e.message;
  } finally {
    button.disabled = false;
  }
}
$("#media-unused").onclick = async () => {
  mediaUnused = !mediaUnused;
  try {
    await refreshMedia();
  } catch (e) {
    $("#media-status").textContent = e.message;
  }
};
$("#media").onclick = async () => {
  $("#media-dialog").showModal();
  try {
    await refreshMedia();
  } catch (e) {
    $("#media-status").textContent = e.message;
  }
};
$("#media-file").onchange = async (e) => {
  const file = e.target.files[0];
  if (!file) return;
  const status = $("#media-status");
  if (!file.size || file.size > 256 * 1048576) {
    status.textContent = "Choose a nonempty file of 256 MiB or smaller.";
    return;
  }
  const input = e.target;
  input.disabled = true;
  status.textContent = `Uploading ${file.name}…`;
  const ext = file.name.split(".").pop().toLowerCase();
  const mime =
    {
      mp4: "video/mp4",
      webm: "video/webm",
      mov: "video/quicktime",
      mp3: "audio/mpeg",
      m4a: "audio/mp4",
      wav: "audio/wav",
      flac: "audio/flac",
      vtt: "text/vtt",
      csv: "text/csv",
      json: "application/json",
      txt: "text/plain",
      md: "text/markdown",
      pdf: "application/pdf",
      xlsx: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    }[ext] || file.type;
  try {
    const item = await api(`/api/media?name=${encodeURIComponent(file.name)}`, {
      method: "POST",
      headers: { "Content-Type": mime },
      body: file,
    });
    await refreshMedia();
    status.textContent = `Uploaded ${file.name}. Copy its embed code below.`;
  } catch (err) {
    status.textContent = `Upload failed: ${err.message}. Choose the file again to retry.`;
  } finally {
    input.disabled = false;
    input.value = "";
  }
};
$("#example").onclick = async () => {
  const b = $("#example");
  b.disabled = true;
  try {
    const response = await fetch("/design-system.css");
    if (!response.ok) throw new Error("Could not load the design system");
    const designCSS = await response.text();
    await create({
      title: "A moment of focus",
      kind: "html",
      note: "An interactive starter. Make it your own.",
      content:
        `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><style>${designCSS}body{min-height:100vh;display:grid;place-items:center}main{text-align:center;padding:var(--space-xl) var(--space-lg)}small{font:12px var(--font-body);letter-spacing:1.5px}h1{font-size:40px;margin:24px 0 12px}p{font-size:16px;color:var(--muted)}#clock{font:clamp(65px,15vw,100px)/1.2 var(--font-display);margin:32px 0;font-variant-numeric:tabular-nums;letter-spacing:-3px}button{margin:4px}.flower{font-size:45px;color:var(--ink);margin-bottom:32px}</style></head><body><main><div class="flower">✳</div><small>ONE THING AT A TIME</small><h1>A moment of focus.</h1><p>A small space to do your best work.</p><div id="clock">25:00</div><button id="toggle" class="primary">Start focusing</button><button id="reset">Reset</button></main><script>let left=1500,running=false;const clock=document.getElementById('clock'),toggle=document.getElementById('toggle');function paint(){clock.textContent=String(Math.floor(left/60)).padStart(2,'0')+':'+String(left%60).padStart(2,'0');toggle.textContent=running?'Pause':'Start focusing'}toggle.onclick=()=>{running=!running;paint()};document.getElementById('reset').onclick=()=>{left=1500;running=false;paint()};setInterval(()=>{if(running&&left>0){left--;if(left===0)running=false;paint()}},1000);<` +
        `/script></body></html>`,
    });
  } catch (e) {
    toast(e.message);
  } finally {
    b.disabled = false;
  }
};
window.addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === "s") {
    e.preventDefault();
    save();
  }
  if (
    e.key === "n" &&
    !e.metaKey &&
    !e.ctrlKey &&
    !["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement.tagName) &&
    !document.querySelector("dialog[open]")
  )
    openCreate();
  // / or Cmd/Ctrl+K finds something, from anywhere outside a text field.
  if (
    ((e.key === "/" && !e.metaKey && !e.ctrlKey) || ((e.metaKey || e.ctrlKey) && e.key === "k")) &&
    !["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement.tagName) &&
    !document.querySelector("dialog[open]")
  ) {
    e.preventDefault();
    if (narrowScreen.matches) libraryOpen(true);
    $("#search").focus();
    $("#search").select();
  }
});
window.addEventListener("beforeunload", remember);
window.addEventListener("popstate", () => {
  remember();
  const id = location.pathname.split("/")[2];
  if (id) load(id);
  else {
    state.artifact = null;
    state.dirty = false;
    setEditing(false);
    detailsOpen(false);
    commentsOpen(false);
    $("#artifact").hidden = true;
    $("#welcome").hidden = false;
    $("#crumb").textContent = "Workspace / Your artifacts";
    setTabIcon("");
    list();
  }
});
setInterval(async () => {
  try {
    const writes = localWrites;
    await refresh();
    const a = state.artifact;
    // A list fetched while this page was saving may be older than what it now shows.
    if (!a || writes !== localWrites) return;
    const summary = state.items.find((v) => v.id === a.id) || state.archive?.find((v) => v.id === a.id);
    if (!summary) {
      $("#remote-change").hidden = false;
      return;
    }
    // Archived or restored elsewhere, by an agent or another tab.
    if ((summary.archived_at || "") !== (a.archived_at || "")) {
      a.archived_at = summary.archived_at;
      renderArchived();
      list();
    }
    const replies = comments.length,
      open = comments.filter((c) => !c.parent && !c.resolved).length;
    if ((summary.comments || 0) !== replies || (summary.open_comments || 0) !== open)
      loadComments();
    if (summary.version > a.revisions.length) {
      if (state.editing || state.version !== a.revisions.length)
        $("#remote-change").hidden = false;
      else await load(a.id);
    }
  } catch {
    /* Keep the current source and draft visible when the server disconnects. */
  }
}, 3000);
(async () => {
  try {
    await refresh();
    const id = location.pathname.split("/")[2];
    if (id) await load(id);
  } catch (e) {
    toast(`Cannot reach shartifacts: ${e.message}`);
  }
})();
