// A full-page view of one artifact: /view/{id} follows the latest version (refreshing when
// it changes); /view/{id}?version=N stays on that version.
const id = decodeURIComponent(location.pathname.split("/")[2] || "");
const pinned = Number(new URL(location.href).searchParams.get("version")) || 0;
const frame = document.getElementById("preview");
let shown = 0,
  kind = "";
forwardDeckKeys(
  () => frame,
  () => kind === "slides",
);
const frameFor = (w) =>
  w === frame.contentWindow ? { id, src: frame.getAttribute("src") } : null;
bridgeArtifactFrames(frameFor);
hostArtifactFrames(frameFor, {
  // A files publish keeps this frame running on the version it made.
  published: (_, version, keepFrame) => {
    if (keepFrame) shown = version;
    refresh();
  },
});
async function refresh() {
  const r = await fetch(`/api/artifacts/${encodeURIComponent(id)}`);
  if (!r.ok) {
    document.getElementById("view-title").textContent =
      r.status === 404 ? "Artifact not found" : "Cannot reach shartifacts";
    return;
  }
  const a = await r.json();
  const latest = a.revisions.length;
  const version = pinned >= 1 && pinned <= latest ? pinned : latest;
  const rev = a.revisions[version - 1];
  kind = rev.kind;
  document.title = rev.title;
  setTabIcon(a.icon);
  document.getElementById("view-title").textContent = rev.title;
  document.getElementById("view-version").textContent =
    `v${version}${version === latest ? "" : ` of ${latest}`}`;
  document.getElementById("view-edit").href =
    `/a/${encodeURIComponent(id)}?version=${version}`;
  if (version !== shown) {
    shown = version;
    frame.src = rev.files
      ? `/preview/${encodeURIComponent(id)}/${version}/`
      : `/preview/${encodeURIComponent(id)}?version=${version}`;
  }
}
refresh();
if (!pinned) setInterval(() => refresh().catch(() => {}), 3000);
