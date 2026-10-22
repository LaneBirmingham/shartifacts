// A read-only share link: /s/{token}. Shows the shared version (or the latest, for a link
// that follows it) with a read-only runtime: data and storage to read, nothing to write.
const token = decodeURIComponent(location.pathname.split("/")[2] || "");
const frame = document.getElementById("preview");
let shown = 0;
const frameFor = (w) =>
  w === frame.contentWindow
    ? { id: token, src: frame.getAttribute("src"), base: `/s/${encodeURIComponent(token)}/api`, thumbnail: true, share: true }
    : null;
bridgeArtifactFrames(frameFor);
hostArtifactFrames(frameFor, {});
forwardDeckKeys(
  () => frame,
  () => kind === "slides",
);
let kind = "";
async function refresh() {
  const r = await fetch(`/s/${encodeURIComponent(token)}/meta`);
  if (!r.ok) {
    document.getElementById("view-title").textContent = "This link was revoked";
    frame.removeAttribute("src");
    return false;
  }
  const m = await r.json();
  kind = m.kind;
  document.title = m.title;
  setTabIcon(m.icon);
  document.getElementById("view-title").textContent = m.title;
  document.getElementById("view-version").textContent = m.follows_latest ? "" : `v${m.version}`;
  if (m.version !== shown) {
    shown = m.version;
    frame.src = `/s/${encodeURIComponent(token)}/preview${m.files ? "/" : ""}`;
  }
  return m.follows_latest;
}
refresh().then((follows) => {
  if (follows) setInterval(() => refresh().catch(() => {}), 10000);
});
