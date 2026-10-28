import { chromium, expect } from "@playwright/test";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { spawn, spawnSync } from "node:child_process";
import { mkdtemp, rm, mkdir, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";
const dir = await mkdtemp(path.join(tmpdir(), "shartifacts-test-"));
const binary = path.join(dir, "shartifacts");
const build = spawnSync("go", ["build", "-o", binary, "."], {
  stdio: "inherit",
});
assert.equal(build.status, 0);
const port = 18000 + Math.floor(Math.random() * 10000),
  base = `http://127.0.0.1:${port}`;
let server, browser, client, page;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function start() {
  server = spawn(
    binary,
    ["-listen", `127.0.0.1:${port}`, "-data", path.join(dir, "data")],
    { stdio: ["ignore", "ignore", "pipe"] },
  );
  server.stderr.on("data", (b) => process.stderr.write(b));
  for (let i = 0; i < 100; i++) {
    if (server.exitCode !== null) throw new Error("Server exited");
    try {
      if ((await fetch(base + "/healthz")).ok) return;
    } catch {}
    await sleep(100);
  }
  throw new Error("Server did not start");
}
async function stop() {
  const proc = server;
  if (!proc || proc.exitCode !== null) return;
  await new Promise((resolve) => {
    proc.once("exit", resolve);
    proc.kill("SIGTERM");
  });
}
async function api(route, body, method = body ? "POST" : "GET") {
  const r = await fetch(base + route, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  });
  assert.ok(r.ok, await r.clone().text());
  return r.json();
}
// A freshly loaded preview is an out-of-process frame; Chrome routes input into it only
// once its hit-test data catches up, slightly after load. Until then a click lands on the
// parent's <iframe> element. Wait until a pointer move is actually received inside.
async function frameInteractive() {
  await expect(page.locator("#preview")).toHaveAttribute("data-ready", "true");
  const body = page.frameLocator("#preview").locator("body");
  await body.evaluate(() => {
    window.__routed = false;
    addEventListener("pointermove", () => (window.__routed = true), { once: true });
  });
  const box = await page.locator("#preview").boundingBox();
  let step = 0;
  await expect
    .poll(async () => {
      await page.mouse.move(box.x + 4 + (step++ % 2), box.y + box.height - 4);
      return body.evaluate(() => window.__routed);
    })
    .toBe(true);
}
// An open artifact fills the workspace; its title, actions, views and versions open on demand.
async function details() {
  if ((await page.locator("#toggle-details").getAttribute("aria-expanded")) !== "true")
    await page.locator("#toggle-details").click();
}
// Media, agents and sign-in live in the menu at the foot of the sidebar.
async function sidebarMenu(name) {
  if ((await page.locator("#toggle-sidebar-menu").getAttribute("aria-expanded")) !== "true")
    await page.locator("#toggle-sidebar-menu").click();
  await page.locator("#sidebar-menu").getByRole("button", { name }).click();
  await expect(page.locator("#sidebar-menu")).toBeHidden();
}
async function immersive(native = true) {
  const view = await page.locator("#panes").getAttribute("data-view");
  const source = await page.locator("#preview").getAttribute("src");
  await page.locator("#fullscreen").click();
  await expect(page.locator("body")).toHaveClass(/immersive/);
  await expect.poll(() => page.locator("#preview").evaluate(el => {
    const r = el.getBoundingClientRect();
    return r.x === 0 && r.y === 0 && Math.abs(r.width - innerWidth) < 1 && Math.abs(r.height - innerHeight) < 1;
  })).toBe(true);
  await expect(page.locator("#exit-fullscreen")).toBeVisible();
  const kind = (await page.locator("#type-label").textContent()).split(" ")[0].toLowerCase();
  await page.screenshot({ path: `test-results/fullscreen-${kind}-${page.viewportSize().width}-${native ? "native" : "fallback"}.png` });
  await expect.poll(() => page.evaluate(() => document.fullscreenElement?.id ?? null))
    .toBe(native ? "panes" : null);
  await page.locator("#exit-fullscreen").click();
  await expect(page.locator("body")).not.toHaveClass(/immersive/);
  await expect(page.locator("#panes")).toHaveAttribute("data-view", view);
  await expect(page.locator("#preview")).toHaveAttribute("src", source);
  await expect(page.locator("#fullscreen")).toBeFocused();
}
try {
  await start();
  browser = await chromium.launch({
    headless: true,
    ...(process.env.CHROME_PATH
      ? { executablePath: process.env.CHROME_PATH }
      : process.platform === "darwin"
        ? {
            executablePath:
              "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
          }
        : {}),
  });
  page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(base + "/artifacts");
  await expect(
    page.getByRole("heading", { name: "A home for things you make." }),
  ).toBeVisible();
  // Installed from the menu, the workspace should open as its own app rather than in a
  // browser window with a toolbar over it: that needs a manifest with the fields and icons
  // Chrome asks for, served as JSON, and icons that really load.
  const manifest = await page.evaluate(async () => {
    const href = document.querySelector('link[rel="manifest"]')?.getAttribute("href");
    if (!href) return null;
    const res = await fetch(href);
    const m = await res.json();
    const icons = [];
    for (const i of m.icons)
      icons.push({ ...i, type: (await fetch(i.src)).headers.get("content-type") });
    return { ...m, contentType: res.headers.get("content-type"), icons };
  });
  assert.equal(manifest?.display, "standalone", "the manifest asks for a window of its own");
  assert.ok(manifest.start_url, "the manifest says where the app opens");
  assert.match(manifest.contentType, /application\/manifest\+json/, "the manifest's type");
  for (const [size, purpose] of [["192x192", "any"], ["512x512", "any"], ["512x512", "maskable"]]) {
    const icon = manifest.icons.find((i) => i.sizes === size && i.purpose === purpose);
    assert.ok(icon, `a ${size} ${purpose} icon`);
    assert.equal(icon.type, "image/png", `the ${size} ${purpose} icon's type`);
  }
  await mkdir("test-results", { recursive: true });
  await page.screenshot({
    path: "test-results/welcome-desktop.png",
    animations: "disabled",
    fullPage: true,
  });
  await page
    .getByRole("button", { name: "Try an interactive example" })
    .click();
  await expect(page.locator("#artifact-name")).toHaveText("A moment of focus");
  const frame = page.frameLocator("#preview");
  await expect(page.locator("#preview")).toHaveAttribute("data-ready", "true");
  await frameInteractive();
  await frame.getByRole("button", { name: "Start focusing" }).click();
  await expect(
    frame.getByRole("button", { name: "Pause", exact: true }),
  ).toBeVisible();
  const id = page.url().split("/a/")[1];
  await details();
  await page.getByRole("button", { name: "Edit source", exact: true }).click();
  const updated = `<h1 id="test">Draft preview</h1><button onclick="this.textContent='Clicked'">Try me</button>`;
  await page
    .getByRole("textbox", { name: "Artifact source", exact: true })
    .fill(updated);
  await expect(frame.locator("#test")).toHaveText("Draft preview");
  const beforeSave = await api("/api/artifacts/" + id);
  assert.equal(beforeSave.revisions.length, 1);
  assert.ok(!beforeSave.revisions[0].content.includes("Draft preview"));
  await page.reload();
  await expect(
    page.getByRole("textbox", { name: "Artifact source", exact: true }),
  ).toHaveValue(updated);
  await expect(page.locator("#draft-status")).toHaveText("Recovered draft");
  await page.keyboard.press(process.platform === "darwin" ? "Meta+s" : "Control+s");
  await expect(page.locator("#versions")).toHaveValue("2");
  await expect(page.locator("#preview")).toHaveAttribute("src", `/preview/${id}?version=2`);
  await expect(page.locator("#preview")).toHaveAttribute("data-ready", "true");
  await expect(frame.locator("#test")).toHaveText("Draft preview");
  await frameInteractive();
  await frame.getByRole("button", { name: "Try me", exact: true }).click();
  await expect(
    frame.getByRole("button", { name: "Clicked", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("combobox", { name: "Artifact version", exact: true })
    .selectOption("1");
  await expect(
    frame.getByRole("heading", { name: "A moment of focus." }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Restore", exact: true }).click();
  await expect(page.locator("#versions")).toHaveValue("3");
  const download = await fetch(
    base + `/api/artifacts/${id}/download?version=2`,
  );
  assert.equal(await download.text(), updated);
  // A separate, real MCP client exercises discovery, source reads, creation, update, and conflicts.
  client = new Client({ name: "shartifacts-browser-test", version: "1.0.0" });
  await client.connect(
    new StreamableHTTPClientTransport(new URL(base + "/mcp")),
  );
  const tools = await client.listTools();
  assert.deepEqual(tools.tools.map((t) => t.name).sort(), [
    "artifact_comments",
    "artifact_db",
    "artifact_storage",
    "begin_media_upload",
    "create_artifact",
    "delete_artifact",
    "deploy_app",
    "get_app",
    "get_artifact",
    "get_design_system",
    "list_artifacts",
    "list_media",
    "manage_app",
    "ports",
    "search_artifacts",
    "update_artifact",
    "upload_media_chunk",
    "watch_artifacts",
  ]);
  // A shell-capable agent discovers the real host through MCP and uploads bytes directly.
  const uploadInfo = (
    await client.callTool({ name: "list_media", arguments: {} })
  ).structuredContent;
  assert.equal(uploadInfo.upload_url, base + "/api/media");
  assert.ok(uploadInfo.curl_example.includes(base + "/api/media"));
  const directUpload = spawnSync(
    "curl",
    [
      "--fail-with-body",
      "--silent",
      "--show-error",
      "-H",
      "Content-Type: video/webm",
      "--data-binary",
      "@scripts/fixtures/sample.webm",
      uploadInfo.upload_url,
    ],
    { encoding: "utf8" },
  );
  assert.equal(directUpload.status, 0, directUpload.stderr);
  const directVideo = JSON.parse(directUpload.stdout);
  assert.deepEqual(
    Buffer.from(await (await fetch(base + directVideo.url)).arrayBuffer()),
    await readFile("scripts/fixtures/sample.webm"),
  );
  const designText = (
    await client.callTool({ name: "get_design_system", arguments: {} })
  ).content[0].text;
  const design = {
    html_starter: designText.slice(designText.indexOf("<!doctype html>")),
  };
  assert.ok(
    design.html_starter.includes(await readFile("web/design-system.css", "utf8")),
  );
  const styledResult = await client.callTool({
    name: "create_artifact",
    arguments: {
      title: "Shared design test",
      kind: "html",
      content: design.html_starter.replace(
        "</section>",
        `<video id="direct-video" controls muted playsinline src="${directVideo.url}"></video></section>`,
      ),
    },
  });
  const styledArtifact = styledResult.structuredContent;
  const styledRead = await client.callTool({
    name: "get_artifact",
    arguments: { id: styledArtifact.id },
  });
  assert.ok(
    styledRead.structuredContent.revisions
      .at(-1)
      .content.includes(directVideo.url),
  );
  await page.goto(base + "/a/" + styledArtifact.id);
  assert.deepEqual(
    await page.locator("body").evaluate((el) => ({
      canvas: getComputedStyle(el).backgroundColor,
      primary: getComputedStyle(document.querySelector("#new")).backgroundColor,
    })),
    { canvas: "rgb(249, 248, 246)", primary: "rgb(181, 82, 59)" },
  );
  assert.equal(
    await page
      .locator("html")
      .evaluate((el) => getComputedStyle(el).backgroundColor),
    "rgb(249, 248, 246)",
  );
  await expect(frame.locator("h1")).toHaveCSS("font-weight", "400");
  await expect(frame.locator(".card")).toHaveCSS(
    "background-color",
    "rgb(235, 230, 224)",
  );
  await expect(frame.locator(".primary")).toHaveCSS(
    "background-color",
    "rgb(181, 82, 59)",
  );
  // Theme: another palette restyles the workspace, survives a reload and leaves the
  // artifact on the default one.
  await page.locator("#toggle-sidebar-menu").click();
  await page.locator('[data-palette-choice="harbour"]').click();
  await expect(page.locator("#sidebar-menu")).toBeVisible();
  await expect(page.locator("#new")).toHaveCSS("background-color", "rgb(44, 93, 143)");
  await expect(frame.locator(".primary")).toHaveCSS("background-color", "rgb(181, 82, 59)");
  await page.goto(base + "/a/" + styledArtifact.id);
  await expect(page.locator("#new")).toHaveCSS("background-color", "rgb(44, 93, 143)");
  await page.locator("#toggle-sidebar-menu").click();
  await expect(page.locator('[data-palette-choice="harbour"]')).toHaveAttribute("aria-pressed", "true");
  await page.locator('[data-palette-choice="terracotta"]').click();
  await expect(page.locator("#new")).toHaveCSS("background-color", "rgb(181, 82, 59)");
  await page.keyboard.press("Escape");
  await expect
    .poll(() => frame.locator("#direct-video").evaluate((v) => v.readyState))
    .toBeGreaterThanOrEqual(2);
  await frame.locator("#direct-video").evaluate((v) => v.play());
  await expect
    .poll(() => frame.locator("#direct-video").evaluate((v) => v.currentTime))
    .toBeGreaterThan(0.1);
  await details();
  await page.getByRole("button", { name: "Source", exact: true }).click();
  await expect(page.locator(".source-pane")).toHaveCSS(
    "background-color",
    "rgb(28, 26, 25)",
  );
  await page.screenshot({
    path: "test-results/design-source-desktop.png",
    fullPage: true,
  });
  await page.getByRole("button", { name: "Preview", exact: true }).click();
  await page.screenshot({
    path: "test-results/design-preview-desktop.png",
    fullPage: true,
  });
  // Upload real VP8 video through MCP in several chunks, then prove decode/play/seek.
  const videoBytes = await readFile(
    new URL("./fixtures/sample.webm", import.meta.url),
  );
  const begun = await client.callTool({
    name: "begin_media_upload",
    arguments: { mime_type: "video/webm" },
  });
  assert.ok(!begun.isError, JSON.stringify(begun));
  const uploadID = begun.structuredContent.upload_id;
  let video;
  for (let offset = 0; offset < videoBytes.length; offset += 1000) {
    const chunk = videoBytes.subarray(offset, offset + 1000);
    const result = await client.callTool({
      name: "upload_media_chunk",
      arguments: {
        upload_id: uploadID,
        offset,
        data_base64: chunk.toString("base64"),
        final: offset + chunk.length === videoBytes.length,
      },
    });
    assert.ok(!result.isError, JSON.stringify(result));
    if (result.structuredContent.url) video = result.structuredContent;
    else assert.equal(result.structuredContent.offset, offset + chunk.length);
  }
  assert.ok(video.url);
  const listing = await client.callTool({ name: "list_media", arguments: {} });
  assert.ok(listing.structuredContent.media.some((m) => m.url === video.url));
  const vtt = await fetch(base + "/api/media", {
    method: "POST",
    headers: { "Content-Type": "text/vtt" },
    body: "WEBVTT\n\n00:00.000 --> 00:02.000\nVideo caption\n",
  }).then((r) => r.json());
  const videoSource = `<h1>Uploaded video</h1><video id="movie" style="width:640px" controls muted playsinline crossorigin="anonymous" preload="auto" src="${video.url}"><track kind="captions" src="${vtt.url}" srclang="en" default></video>`;
  const mediaArtifact = await api("/api/artifacts", {
    title: "Video playback",
    kind: "html",
    content: videoSource,
  });
  await page.goto(base + "/a/" + mediaArtifact.id);
  // Record the actual visible iframe area, not its offscreen CSS height.
  for (const size of [{ width: 697, height: 837 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(size);
    await expect(page.locator("#preview")).toHaveAttribute("data-ready", "true");
    const metrics = await page.locator("#preview").evaluate((el) => {
      const r = el.getBoundingClientRect();
      return { width: innerWidth, height: innerHeight,
        scrollWidth: document.documentElement.scrollWidth,
        clientWidth: document.documentElement.clientWidth,
        previewTop: r.top, previewWidth: r.width, previewHeight: r.height,
        visibleHeight: Math.max(0, Math.min(r.bottom, innerHeight) - Math.max(r.top, 0)) };
    });
    console.log("MOBILE " + JSON.stringify(metrics));
    assert.ok(metrics.scrollWidth <= metrics.clientWidth, "mobile overflow");
    assert.ok(metrics.visibleHeight / metrics.height > 0.9, "preview must occupy >90% of viewport");
    await immersive();
    await page.screenshot({ path: `test-results/mobile-after-${size.width}.png` });
  }
  await page.setViewportSize({ width: 1440, height: 1000 });
  const movie = frame.locator("#movie");
  await expect
    .poll(() => movie.evaluate((v) => v.readyState))
    .toBeGreaterThanOrEqual(2);
  await expect
    .poll(() => movie.evaluate((v) => v.textTracks[0]?.cues?.length))
    .toBe(1);
  assert.ok(await movie.evaluate((v) => v.duration > 1));
  await movie.evaluate((v) => v.play());
  await expect
    .poll(() => movie.evaluate((v) => v.currentTime))
    .toBeGreaterThan(0.1);
  await movie.evaluate((v) => {
    v.pause();
    v.currentTime = 1;
  });
  await expect
    .poll(() => movie.evaluate((v) => v.currentTime))
    .toBeGreaterThanOrEqual(1);
  await expect(page.locator("#preview")).toHaveAttribute(
    "sandbox",
    "allow-scripts allow-forms allow-modals allow-downloads allow-popups allow-popups-to-escape-sandbox allow-pointer-lock",
  );
  await expect(page.locator("#preview")).toHaveAttribute("allow", "fullscreen *; clipboard-write *; camera *; microphone *; geolocation *; display-capture *");
  assert.equal(await movie.evaluate(v => document.fullscreenEnabled), true);
  // Click the browser's own media fullscreen button, not a replacement JS button.
  const mediaBox = await movie.boundingBox();
  await movie.hover();
  await page.mouse.click(mediaBox.x + mediaBox.width - 64, mediaBox.y + mediaBox.height - 48);
  await expect.poll(() => movie.evaluate(v => document.fullscreenElement === v)).toBe(true);
  assert.equal(await page.evaluate(() => document.fullscreenElement?.id), "preview");
  await page.screenshot({ path: "test-results/native-video-fullscreen.png" });
  await page.evaluate(() => document.exitFullscreen());
  await expect.poll(() => movie.evaluate(v => document.fullscreenElement === null)).toBe(true);
  await immersive();
  const range = await fetch(base + video.url, {
    headers: { Range: "bytes=0-31" },
  });
  assert.equal(range.status, 206);
  assert.deepEqual(
    Buffer.from(await range.arrayBuffer()),
    videoBytes.subarray(0, 32),
  );
  await page.screenshot({
    path: "test-results/media-playback.png",
    fullPage: true,
  });
  // The browser uploader accepts a real image and exposes reusable embed markup.
  await sidebarMenu("＋ Media library");
  await page.locator("#media-file").setInputFiles({
    name: "pixel.png",
    mimeType: "image/png",
    buffer: Buffer.from(
      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aL1sAAAAASUVORK5CYII=",
      "base64",
    ),
  });
  await expect(page.locator("#media-status")).toContainText(
    "Uploaded pixel.png",
  );
  await expect(page.locator("#media-list")).toContainText("image/png");
  await page.locator("#media-dialog [data-close]").click();
  const image = (await api("/api/media")).media.find(
    (m) => m.mime_type === "image/png",
  );
  const imageArtifact = await api("/api/artifacts", {
    title: "Uploaded image",
    kind: "html",
    content: `<img id="picture" src="${image.url}">`,
  });
  await page.goto(base + "/a/" + imageArtifact.id);
  await expect
    .poll(() => frame.locator("#picture").evaluate((img) => img.naturalWidth))
    .toBe(1);
  // The library finds files nothing names and deletes one after asking; the image the
  // artifact embeds is not offered.
  const stray = await (
    await fetch(base + "/api/media?name=stray.csv", { method: "POST", headers: { "Content-Type": "text/csv" }, body: "a,b\n1,2\n" })
  ).json();
  await sidebarMenu("＋ Media library");
  await page.locator("#media-unused").click();
  await expect(page.locator("#media-unused")).toHaveText("Show all files");
  await expect(page.locator("#media-list")).toContainText("stray.csv");
  await expect(page.locator("#media-list")).not.toContainText("pixel.png");
  let question = "";
  page.once("dialog", (d) => ((question = d.message()), d.accept()));
  await page.locator(".media-item", { hasText: "stray.csv" }).getByRole("button", { name: "Delete" }).click();
  await expect(page.locator("#media-list")).not.toContainText("stray.csv");
  assert.match(question, /No artifact or app names it/);
  assert.equal((await api("/api/media")).media.some((m) => m.id === stray.id), false);
  // Deleting a file in use names who uses it; dismissing keeps it.
  await page.locator("#media-unused").click();
  page.once("dialog", (d) => ((question = d.message()), d.dismiss()));
  await page.locator(".media-item", { hasText: "pixel.png" }).getByRole("button", { name: "Delete" }).click();
  await expect.poll(() => question).toMatch(/used by artifact “Uploaded image”/);
  assert.equal((await api("/api/media")).media.some((m) => m.id === image.id), true);
  await page.locator("#media-dialog [data-close]").click();
  // Audio and React use the same persisted media route, including in unsaved drafts.
  const wav = Buffer.alloc(44 + 16000);
  wav.write("RIFF");
  wav.writeUInt32LE(wav.length - 8, 4);
  wav.write("WAVEfmt ", 8);
  wav.writeUInt32LE(16, 16);
  wav.writeUInt16LE(1, 20);
  wav.writeUInt16LE(1, 22);
  wav.writeUInt32LE(8000, 24);
  wav.writeUInt32LE(16000, 28);
  wav.writeUInt16LE(2, 32);
  wav.writeUInt16LE(16, 34);
  wav.write("data", 36);
  wav.writeUInt32LE(16000, 40);
  const audio = await fetch(base + "/api/media", {
    method: "POST",
    headers: { "Content-Type": "audio/wav" },
    body: wav,
  }).then((r) => r.json());
  const mediaReact = await api("/api/artifacts", {
    title: "React media",
    kind: "react",
    content: `export default function App(){return <><video id="react-movie" controls muted playsInline src="${video.url}"/><audio id="sound" controls src="${audio.url}"/></>}`,
  });
  await page.goto(base + "/a/" + mediaReact.id);
  await expect
    .poll(() => frame.locator("#react-movie").evaluate((v) => v.readyState))
    .toBeGreaterThanOrEqual(2);
  await expect
    .poll(() => frame.locator("#sound").evaluate((v) => v.readyState))
    .toBeGreaterThanOrEqual(2);
  await frame.locator("#sound").evaluate((a) => a.play());
  await expect
    .poll(() => frame.locator("#sound").evaluate((a) => a.currentTime))
    .toBeGreaterThan(0);
  await details();
  await page.locator("#edit").click();
  await page
    .locator("#source")
    .fill(
      `export default function App(){return <video id="draft-movie" controls muted playsInline src="${video.url}"/>}`,
    );
  await expect
    .poll(() => frame.locator("#draft-movie").evaluate((v) => v.readyState))
    .toBeGreaterThanOrEqual(2);
  await page.locator("#discard").click();
  const reactSource = `import React, {useState} from 'react'; export default function Counter(){const [n,setN]=useState<number>(0);return <button onClick={()=>setN(n+1)}>Count {n}</button>}`;
  const made = await client.callTool({
    name: "create_artifact",
    arguments: { title: "Agent counter", kind: "react", content: reactSource },
  });
  assert.ok(!made.isError, JSON.stringify(made));
  const counter = made.structuredContent;
  assert.ok(counter.id);
  await page.goto(base + "/a/" + counter.id);
  await expect(frame.getByRole("button", { name: "Count 0" })).toBeVisible();
  await frameInteractive();
  await frame.getByRole("button", { name: "Count 0" }).click();
  await expect(frame.getByRole("button", { name: "Count 1" })).toBeVisible();
  for (const width of [390, 697, 1440]) {
    await page.setViewportSize({ width, height: 844 });
    await immersive();
    await expect(frame.getByRole("button", { name: "Count 1" })).toBeVisible();
  }
  const read = await client.callTool({
    name: "get_artifact",
    arguments: { id: counter.id },
  });
  assert.equal(read.structuredContent.revisions[0].content, reactSource);
  const change = {
    id: counter.id,
    title: "Agent counter updated",
    kind: "react",
    content: reactSource.replace("Count {n}", "Total {n}"),
    base_version: 1,
  };
  const changed = await client.callTool({
    name: "update_artifact",
    arguments: change,
  });
  assert.ok(!changed.isError);
  await expect(page.locator("#artifact-name")).toHaveText(
    "Agent counter updated",
    { timeout: 10000 },
  );
  await expect(frame.getByRole("button", { name: "Total 0" })).toBeVisible();
  const conflict = await client.callTool({
    name: "update_artifact",
    arguments: change,
  });
  assert.equal(conflict.isError, true);
  // A dirty browser editor must survive an agent update without overwriting either version.
  await details();
  await page.getByRole("button", { name: "Edit source", exact: true }).click();
  await page
    .getByRole("textbox", { name: "Artifact source", exact: true })
    .fill(reactSource.replace("Count {n}", "Browser {n}"));
  await client.callTool({
    name: "update_artifact",
    arguments: { ...change, title: "Agent v3", base_version: 2 },
  });
  await expect(page.locator("#remote-change")).toBeVisible({ timeout: 10000 });
  await page.getByRole("button", { name: "Save version", exact: true }).click();
  await expect(page.locator("#toast")).toContainText("artifact changed");
  await expect(
    page.getByRole("textbox", { name: "Artifact source", exact: true }),
  ).toHaveValue(reactSource.replace("Count {n}", "Browser {n}"));
  page.once("dialog", (d) => d.accept());
  await page
    .getByRole("button", { name: "Discard draft", exact: true })
    .click();
  const samples = [
    {
      title: "Markdown test",
      kind: "markdown",
      content:
        "# Local document\n\n**Bold text**\n\n```mermaid\ngraph LR\n A[Create] --> B[Keep]\n```",
      selector: "h1",
      text: "Local document",
    },
    {
      title: "Diagram test",
      kind: "mermaid",
      content: "graph LR\n A[Hello] --> B[Network]",
      selector: "svg",
      text: null,
    },
    {
      title: "SVG test",
      kind: "svg",
      content:
        '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><circle cx="50" cy="50" r="30" fill="green"/></svg>',
      selector: "circle",
      text: null,
    },
    {
      title: "Code test",
      kind: "code",
      content: "<script>window.hacked=true</script>",
      selector: "pre",
      text: "<script>window.hacked=true</script>",
    },
  ];
  for (const sample of samples) {
    const { selector, text, ...input } = sample;
    const a = await api("/api/artifacts", input);
    await page.goto(base + "/a/" + a.id);
    await expect(frame.locator(selector).first()).toBeVisible();
    if (text) await expect(frame.locator(selector).first()).toHaveText(text);
    for (const width of [390, 697, 1440]) {
      await page.setViewportSize({ width, height: 844 });
      await immersive();
    }
    if (sample.kind === "markdown")
      await expect(frame.locator("svg")).toBeVisible();
    assert.equal(
      await page
        .locator("#preview")
        .evaluate((el) => el.contentDocument === null),
      true,
      "iframe exposed its document to parent",
    );
  }
  // Source/Split return intact, even when the browser denies or lacks native fullscreen.
  await details();
  for (const view of ["source", "split"]) {
    await page.locator(`button[data-view="${view}"]`).click();
    await immersive();
  }
  await page.evaluate(() => { document.querySelector("#panes").requestFullscreen = undefined; });
  await immersive(false);
  await page.evaluate(() => { document.querySelector("#panes").requestFullscreen = () => Promise.reject(new Error("Denied")); });
  await immersive(false);
  await page.locator("#fullscreen").click();
  await page.keyboard.press("Escape");
  await expect(page.locator("body")).not.toHaveClass(/immersive/);
  await page.evaluate(() => { delete document.querySelector("#panes").requestFullscreen; });
  await page.locator("#fullscreen").click();
  await expect.poll(() => page.evaluate(() => document.fullscreenElement?.id)).toBe("panes");
  await page.evaluate(() => document.exitFullscreen());
  await expect(page.locator("body")).not.toHaveClass(/immersive/);
  await page.locator('button[data-view="preview"]').click();
  // Breakpoint boundaries, cover widths, fold width, narrow phones and desktop.
  const longTitle = await api("/api/artifacts", { title: "W".repeat(200), kind: "html", content: "<h1>Long title fixture</h1>" });
  await page.goto(base + "/a/" + longTitle.id);
  for (const width of [320, 390, 430, 640, 697, 812, 900, 901, 1024, 1440]) {
    await page.setViewportSize({ width, height: 844 });
    const checkControls = async () => {
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth), true, `overflow at ${width}`);
      const clipped = await page.locator(".workspace button, .workspace select, .workspace a.button").evaluateAll(els => els.filter(el => {
        const r = el.getBoundingClientRect();
        return r.width && r.height && (r.left < 0 || r.right > document.documentElement.clientWidth);
      }).map(el => el.id || el.textContent));
      assert.deepEqual(clipped, [], `clipped controls at ${width}`);
    };
    await checkControls();
    assert.ok(await page.locator("#preview").evaluate(el => el.getBoundingClientRect().height / innerHeight > 0.85), `preview fills the workspace at ${width}`);
    await expect(page.locator("#artifact-name")).toBeHidden();
    await page.locator("#toggle-details").click();
    await checkControls();
    await page.locator("#toggle-details").click();
    await page.locator("#comments-link").click();
    await expect(page.getByRole("textbox", { name: "New comment" })).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.locator("#comments")).toBeHidden();
    if (width <= 900) {
      await page.locator("#toggle-library").click();
      await expect(page.locator("#search")).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(page.locator("#toggle-library")).toBeFocused();
    }
  }
  await page.setViewportSize({ width: 1440, height: 1000 });
  // A pinned share link stays on its selected historical revision through fullscreen.
  await page.goto(base + `/a/${id}?version=2`);
  await expect(frame.locator("#test")).toHaveText("Draft preview");
  await immersive();
  await expect(page.locator("#versions")).toHaveValue("2");
  assert.ok(page.url().endsWith("?version=2"));
  // Preview JavaScript cannot read the parent or call the artifact API.
  const malicious = await api("/api/artifacts", {
    title: "Sandbox test",
    kind: "html",
    content:
      `<div id="result">checking</div><script>let parentBlocked=false;try{parent.document.body.dataset.compromised='yes'}catch{parentBlocked=true}fetch('${base}/api/artifacts',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({title:'BAD',kind:'code',content:'bad'})}).then(()=>document.getElementById('result').textContent='bad').catch(()=>document.getElementById('result').textContent=parentBlocked?'blocked':'bad');<` +
      `/script>`,
  });
  await page.goto(base + "/a/" + malicious.id);
  await expect(frame.locator("#result")).toHaveText("blocked");
  assert.equal(
    await page.locator("body").getAttribute("data-compromised"),
    null,
  );
  assert.ok(!(await api("/api/artifacts")).some((a) => a.title === "BAD"));
  // Import form, search, duplicate, delete, and mobile layout.
  await page.getByRole("button", { name: "＋ New artifact N" }).click();
  await page.locator("#import-file").setInputFiles({
    name: "imported.md",
    mimeType: "text/markdown",
    buffer: Buffer.from("# Imported file"),
  });
  await page.locator("#create-form button[type=submit]").click();
  await expect(page.locator("#artifact-name")).toHaveText("imported");
  await expect(
    frame.getByRole("heading", { name: "Imported file" }),
  ).toBeVisible();
  await details();
  await page.getByRole("button", { name: "More artifact actions" }).click();
  await page
    .getByRole("button", { name: "Duplicate artifact", exact: true })
    .click();
  await expect(page.locator("#artifact-name")).toHaveText("imported (copy)");
  const search = page.getByRole("searchbox", { name: "Search artifacts" });
  await search.fill("imported");
  await expect(page.locator("#library .library-item")).toHaveCount(2);
  await search.fill("");
  // Search reads what artifacts say, forgives typos and offers the correction, filters
  // by tag and contents, and Enter opens the best match.
  const copyURL = page.url();
  await api("/api/artifacts", { title: "Household budget", kind: "html", content: "<h1>Spending</h1><p>Mortgage repayments and groceries this month.</p><table><tr><td>Rent</td></tr></table>", tags: ["Finance", "home"] });
  await api("/api/artifacts", { title: "Refinance calculator", kind: "html", content: "<p>Compare two mortgage loans and their repayments.</p>", tags: ["finance"] });
  await page.reload();
  await search.fill("grocries");
  await expect(page.locator("#library .library-item")).toHaveCount(1);
  await expect(page.locator("#library .library-item mark")).toHaveText(["groceries"]);
  await expect(page.locator("#suggestion")).toHaveText("groceries");
  await page.locator("#suggestion").click();
  await expect(search).toHaveValue("groceries");
  await search.fill("");
  // Tags and filter menus stay folded away until asked for, so the library keeps the room.
  await expect(page.locator("#filters")).toBeHidden();
  await page.locator("#toggle-filters").click();
  await page.locator("#tag-chips button", { hasText: "#finance" }).click();
  await expect(page.locator("#library-label")).toHaveText("RESULTS");
  await expect(page.locator("#filter-count")).toHaveText("1");
  await expect(page.locator("#library .library-item")).toHaveCount(2);
  await page.locator("#has").selectOption("table");
  await expect(page.locator("#library .library-item")).toHaveCount(1);
  await expect(page.locator("#library .library-item")).toContainText("Household budget");
  await page.locator("#has").selectOption("");
  await page.locator("#tag-chips button", { hasText: "#finance" }).click();
  await expect(page.locator("#library-label")).toHaveText("THE LIBRARY");
  await expect(page.locator("#filter-count")).toBeHidden();
  await page.locator("#toggle-filters").click();
  await expect(page.locator("#filters")).toBeHidden();
  await page.evaluate(() => document.activeElement.blur());
  await page.keyboard.press("/");
  await expect(search).toBeFocused();
  await search.fill("refinanse");
  await search.press("Enter");
  await expect(page.locator("#artifact-name")).toHaveText("Refinance calculator");
  await search.fill("");
  // Details show tags and related artifacts; editing suggests tags from the content.
  await details();
  await expect(page.locator("#artifact-tags")).toHaveText("#finance");
  await expect(page.locator("#related-list .related-item").first()).toContainText("Household budget");
  await page.getByRole("button", { name: "Edit source", exact: true }).click();
  const offered = page.locator("#tag-suggestions button").first();
  const added = (await offered.textContent()).replace(/^\+ /, "");
  await offered.click();
  await expect(page.locator("#edit-tags")).toHaveValue(`finance, ${added}`);
  await page.getByRole("button", { name: "Save version", exact: true }).click();
  await expect(page.locator("#artifact-tags button")).toHaveText(["#finance", `#${added}`]);
  await page.locator("#artifact-tags button").first().click();
  await expect(page.locator("#library .library-item")).toHaveCount(2);
  // With the filters folded away, what filters the library shows as chips that remove it.
  await page.locator("#active-filters").getByRole("button", { name: "Remove filter #finance" }).click();
  await expect(page.locator("#active-filters")).toBeHidden();
  await expect(page.locator("#library-label")).toHaveText("THE LIBRARY");
  // Select mode archives several at once; the archive lists, searches, restores and
  // permanently deletes them, and an archived artifact opens with a notice saying so.
  const scratch = [];
  for (const title of ["Scratch one", "Scratch two"])
    scratch.push((await api("/api/artifacts", { title, kind: "markdown", content: `# ${title}` })).id);
  await page.reload();
  const libraryTitles = page.locator("#library .library-item strong");
  await expect(libraryTitles.filter({ hasText: "Scratch" })).toHaveCount(2);
  await page.locator("#toggle-select").click();
  await expect(page.locator("#bulk-archive")).toBeDisabled();
  await page.locator("#library .library-item", { hasText: "Scratch two" }).click();
  await page.locator("#library .library-item", { hasText: "Scratch one" }).click();
  await expect(page.locator("#bulk-count")).toHaveText("2 selected");
  await page.locator("#bulk-archive").click();
  await expect(page.locator("#toast")).toContainText("Archived 2 artifacts");
  await expect(libraryTitles.filter({ hasText: "Scratch" })).toHaveCount(0);
  assert.ok(!(await api("/api/artifacts")).some((a) => a.title.startsWith("Scratch")));
  await page.locator("#toggle-select").click();
  await page.locator("#toggle-archive").click();
  await expect(page.locator("#library-label")).toHaveText("ARCHIVE");
  await expect(libraryTitles).toHaveText(["Scratch one", "Scratch two"]);
  await search.fill(`"scratch two"`);
  await expect(libraryTitles).toHaveText(["Scratch two"]);
  await search.fill("");
  await page.locator("#library .library-item", { hasText: "Scratch one" }).click();
  await expect(page.locator("#archived-notice")).toBeVisible();
  await page.getByRole("button", { name: "Restore to library" }).click();
  await expect(page.locator("#archived-notice")).toBeHidden();
  await expect(libraryTitles).toHaveText(["Scratch two"]);
  await page.locator("#toggle-select").click();
  await page.locator("#bulk-all").click();
  page.once("dialog", (d) => d.accept());
  await page.locator("#bulk-delete").click();
  await expect(page.locator("#toast")).toContainText("Deleted 1 artifact");
  await expect(page.locator("#library .empty-library")).toContainText("Nothing archived");
  assert.equal((await fetch(`${base}/api/artifacts/${scratch[1]}`)).status, 404);
  await page.locator("#toggle-select").click();
  await page.locator("#toggle-archive").click();
  await expect(page.locator("#library-label")).toHaveText("THE LIBRARY");
  await expect(libraryTitles.filter({ hasText: "Scratch one" })).toHaveCount(1);
  // Bulk delete refuses what is still in the library.
  assert.equal((await api("/api/artifacts/bulk", { op: "delete", ids: [scratch[0]] })).failed[0].id, scratch[0]);
  await api(`/api/artifacts/${scratch[0]}?base_version=1`, undefined, "DELETE");
  // On a short tablet screen the library still starts right under the search.
  await page.setViewportSize({ width: 1024, height: 640 });
  await expect(page.locator("#library .library-item").first()).toBeInViewport();
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.goto(copyURL);
  for (const size of [
    { width: 390, height: 844 },
    { width: 390, height: 500 },
  ]) {
    await page.setViewportSize(size);
    if (await page.locator("#toggle-details").getAttribute("aria-expanded") === "false")
      await page.locator("#toggle-details").click();
    await page
      .getByRole("button", { name: "Edit source", exact: true })
      .click();
    await expect(
      page.getByRole("button", { name: "Save version", exact: true }),
    ).toBeVisible();
    assert.equal(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
      true,
      "mobile horizontal overflow",
    );
    await expect(
      frame.getByRole("heading", { name: "Imported file" }),
    ).toBeVisible();
    await page.screenshot({
      path: `test-results/editor-${size.width}x${size.height}.png`,
      fullPage: true,
    });
    await page
      .getByRole("button", { name: "Discard draft", exact: true })
      .click();
  }
  await page.locator("#toggle-library").click();
  await sidebarMenu("＋ Media library");
  await expect(page.locator("#media-list")).toContainText("video/webm");
  assert.equal(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
    true,
    "mobile media library overflow",
  );
  await page.screenshot({
    path: "test-results/media-library-mobile.png",
    fullPage: true,
  });
  await page.locator("#media-dialog [data-close]").click();
  await page.locator("#close-library").click();
  await details();
  await page.getByRole("button", { name: "More artifact actions" }).click();
  page.once("dialog", (d) => d.accept());
  await page.getByRole("button", { name: "Delete artifact & history" }).click();
  await expect(
    page.getByRole("heading", { name: "A home for things you make." }),
  ).toBeVisible();
  await page.locator("#toggle-library").click();
  await sidebarMenu("⌁ Connect an agent");
  await expect(page.locator("#connect-dialog input[readonly]")).toHaveValue(base + "/mcp");
  await expect(page.locator("#connect-dialog")).toContainText(`claude mcp add --transport http --scope user shartifacts ${base}/mcp`);
  await page.locator("#connect-dialog [data-close]").click();
  await expect(page.locator("#connect-dialog")).not.toBeVisible();
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.goto(base + "/a/" + id);
  await expect(
    frame.getByRole("heading", { name: "A moment of focus." }),
  ).toBeVisible();
  await page.screenshot({
    path: "test-results/workspace-desktop.png",
    fullPage: true,
  });
  // Claude artifact parity: bundled React libraries, shadcn/ui, CDN scripts in HTML,
  // window.storage, window.fs.readFile, window.claude (off), runtime error reports, comments, gallery thumbnails, Changes view, slides with math, multi-file artifacts, forms, dialogs, downloads, links and the full-page view.
  const libs = await api("/api/artifacts", {
    title: "Libraries",
    kind: "react",
    content: `import React, {useEffect, useRef, useState} from 'react';
import {LineChart, Line} from 'recharts';
import {Camera} from 'lucide-react';
import * as d3 from 'd3';
import _ from 'lodash';
import * as math from 'mathjs';
import * as THREE from 'three';
import Chart from 'chart.js/auto';
import {Card, CardContent, CardHeader, CardTitle} from '@/components/ui/card';
import {Button} from '@/components/ui/button';
import {Tabs, TabsContent, TabsList, TabsTrigger} from '@/components/ui/tabs';
export default function App(){
  const canvas = useRef(); const [n, setN] = useState(0);
  useEffect(() => { new Chart(canvas.current, {type: 'bar', data: {labels: ['a'], datasets: [{data: [1]}]}}); }, []);
  return <Card className="m-6 max-w-md"><CardHeader><CardTitle>Libraries</CardTitle></CardHeader><CardContent className="space-y-4">
    <Camera />
    <LineChart width={200} height={80} data={[{y: 1}, {y: 3}]}><Line dataKey="y" /></LineChart>
    <canvas ref={canvas} width="200" height="80" />
    <output id="libs">{[d3.sum([1, 2]), _.chunk([1, 2, 3, 4], 2).length, math.evaluate('2^5'), new THREE.Vector3(3, 4, 0).length()].join('|')}</output>
    <Button id="inc" onClick={() => setN(n + 1)}>Clicked {n}</Button>
    <Tabs defaultValue="a"><TabsList><TabsTrigger value="a">A</TabsTrigger><TabsTrigger value="b">B</TabsTrigger></TabsList><TabsContent value="a">Panel A</TabsContent><TabsContent value="b">Panel B</TabsContent></Tabs>
  </CardContent></Card>;
}`,
  });
  await page.goto(base + "/a/" + libs.id);
  await expect(frame.locator("#libs")).toHaveText("3|2|32|5");
  await expect(frame.locator("svg.lucide-camera")).toHaveCount(1);
  await expect(frame.locator(".recharts-line path")).toHaveCount(1);
  await frameInteractive();
  await frame.locator("#inc").click();
  await expect(frame.locator("#inc")).toHaveText("Clicked 1");
  await frame.getByRole("tab", { name: "B" }).click();
  await expect(frame.getByRole("tabpanel")).toHaveText("Panel B");
  // Tailwind generated the shadcn theme: the primary button is dark slate.
  await expect
    .poll(() => frame.locator("#inc").evaluate((e) => getComputedStyle(e).backgroundColor))
    .toBe("rgb(15, 23, 42)");
  await page.screenshot({ path: "test-results/react-libraries.png" });
  const cdn = await api("/api/artifacts", {
    title: "CDN scripts",
    kind: "html",
    content: `<script src="https://cdnjs.cloudflare.com/ajax/libs/Chart.js/4.4.1/chart.umd.min.js" integrity="sha512-x" crossorigin="anonymous"></script><script src="https://cdn.tailwindcss.com"></script><script>tailwind.config={theme:{extend:{colors:{brand:'#ff0066'}}}}</script><div id="box" class="p-4 bg-brand">x</div><output id="globals"></output><script>document.getElementById('globals').textContent=typeof Chart</script>`,
  });
  await page.goto(base + "/a/" + cdn.id);
  await expect(frame.locator("#globals")).toHaveText("function");
  await expect
    .poll(() => frame.locator("#box").evaluate((e) => getComputedStyle(e).backgroundColor))
    .toBe("rgb(255, 0, 102)");
  const tool = await api("/api/artifacts", {
    title: "Storage tool",
    kind: "html",
    content: `<output id="count">…</output><form id="f"><input name="n" value="5"><button id="add">Add</button></form>
<button id="ask">Ask</button><output id="answer"></output>
<a id="dl" download="data.csv" href="data:text/csv,a,b">Download</a> <a id="ext" href="https://example.com/">Example</a>
<script>const out=document.getElementById('count');
async function show(){const r=await window.storage.get('count');out.textContent=r?r.value:'0'}
document.getElementById('f').onsubmit=async e=>{e.preventDefault();const r=await window.storage.get('count');const n=(r?Number(r.value):0)+Number(e.target.n.value);await window.storage.set('count',String(n));await window.storage.set('total',String(n),true);show()};
document.getElementById('ask').onclick=()=>{document.getElementById('answer').textContent=confirm('Sure?')?'yes':'no'};show();<` + `/script>`,
  });
  await page.goto(base + "/a/" + tool.id);
  await expect(frame.locator("#count")).toHaveText("0");
  await frameInteractive();
  await frame.locator("#add").click();
  await expect(frame.locator("#count")).toHaveText("5");
  await frame.locator("#add").click();
  await expect(frame.locator("#count")).toHaveText("10");
  page.once("dialog", (d) => d.accept());
  await frame.locator("#ask").click();
  await expect(frame.locator("#answer")).toHaveText("yes");
  const [csv] = await Promise.all([page.waitForEvent("download"), frame.locator("#dl").click()]);
  assert.equal(csv.suggestedFilename(), "data.csv");
  const [tab] = await Promise.all([page.context().waitForEvent("page"), frame.locator("#ext").click()]);
  assert.equal(tab.url(), "https://example.com/");
  await tab.close();
  const shared = await api(`/api/artifacts/${tool.id}/storage`, { op: "get", key: "total", shared: true });
  assert.deepEqual(shared, { key: "total", value: "10", shared: true });
  // Storage belongs to the artifact: another artifact's frame sees none of it.
  await page.goto(base + "/a/" + cdn.id);
  assert.equal(
    await frame.locator("body").evaluate(() => window.storage.get("count")),
    null,
  );
  // Errors browsers hit in a saved version reach agents through get_artifact.
  const broken = await client.callTool({
    name: "create_artifact",
    arguments: { title: "Broken", kind: "html", content: `<img src="https://example.com/cat.png"><script>missingFunction()<` + `/script>` },
  });
  assert.match(broken.structuredContent.warnings[0], /blocked by the sandbox: https:\/\/example.com\/cat.png/);
  await page.goto(base + "/a/" + broken.structuredContent.id);
  await expect(frame.locator("#error")).toContainText("missingFunction");
  await expect
    .poll(async () => {
      const r = await client.callTool({ name: "get_artifact", arguments: { id: broken.structuredContent.id } });
      return (r.structuredContent.runtime_errors || []).map((e) => e.message).sort().join("\n");
    })
    .toMatch(/missingFunction is not defined[\s\S]*blocked by the sandbox: https:\/\/example.com\/cat.png/);
  // window.fs.readFile reads Media library data files by their upload name.
  const csvUpload = await fetch(base + "/api/media?name=sales.csv", {
    method: "POST",
    headers: { "Content-Type": "text/csv" },
    body: "region,total\nnorth,5\nsouth,7\n",
  });
  assert.equal(csvUpload.status, 201);
  const reader = await api("/api/artifacts", {
    title: "Sales",
    kind: "react",
    content: `import {useEffect, useState} from 'react'; import Papa from 'papaparse';
export default function Sales(){ const [total, setTotal] = useState('…');
  useEffect(() => { window.fs.readFile('sales.csv', {encoding: 'utf8'}).then(text => { const rows = Papa.parse(text, {header: true, skipEmptyLines: true}).data; setTotal(String(rows.reduce((n, r) => n + Number(r.total), 0))); }); window.fs.readFile('missing.csv').catch(e => document.title = e.message); }, []);
  return <output id="total">{total}</output>; }`,
  });
  await page.goto(base + "/a/" + reader.id);
  await expect(frame.locator("#total")).toHaveText("12");
  // window.claude is off without an API key, and says how to enable it.
  assert.match(
    await frame.locator("body").evaluate(() => window.claude.complete("hi").catch((e) => e.message)),
    /ANTHROPIC_API_KEY/,
  );
  // The full-page view shares the same storage and follows new versions.
  const viewer = await browser.newPage({ viewport: { width: 1024, height: 768 } });
  viewer.on("pageerror", (e) => errors.push(e.message));
  await viewer.goto(base + "/view/" + tool.id);
  await expect(viewer.frameLocator("#preview").locator("#count")).toHaveText("10");
  await expect(viewer.locator("#view-title")).toHaveText("Storage tool");
  await page.goto(base + "/a/" + tool.id);
  await expect(page.locator("#open-view")).toHaveAttribute("href", "/view/" + tool.id);
  await api(`/api/artifacts/${tool.id}`, { title: "Storage tool", kind: "html", edits: [{ old_str: "<output id=\"count\">", new_str: "<h1 id=\"v2\">v2</h1><output id=\"count\">" }], base_version: 1 }, "PUT");
  await expect(viewer.frameLocator("#preview").locator("#v2")).toHaveText("v2");
  await expect(viewer.frameLocator("#preview").locator("#count")).toHaveText("10");
  await viewer.screenshot({ path: "test-results/view-page.png" });
  await viewer.close();
  // Comments: a quoted note, an agent's reply over MCP, then resolving the thread.
  await page.goto(base + "/a/" + tool.id);
  await page.locator("#comments-link").click();
  await page.getByRole("textbox", { name: "New comment" }).fill("Show a running total.");
  await page.getByRole("button", { name: "Comment", exact: true }).click();
  await expect(page.locator("#comments-link")).toHaveText("Comments · 1");
  const threads = await client.callTool({ name: "artifact_comments", arguments: { id: tool.id, op: "list" } });
  const thread = threads.structuredContent.threads[0];
  assert.equal(thread.body, "Show a running total.");
  await client.callTool({ name: "artifact_comments", arguments: { id: tool.id, op: "add", parent: thread.id, body: "Added in v3." } });
  await expect(page.locator(".thread .comment").nth(1)).toContainText("Added in v3.");
  await page.getByRole("button", { name: "Resolve", exact: true }).click();
  await expect(page.locator("#comments-link")).toHaveText("Comments");
  await page.locator("#close-comments").click();
  // Comments on the preview itself: comment mode picks what is clicked instead of using
  // it, the thread keeps what it points at for agents, and its numbered pin opens it.
  await frameInteractive();
  await page.locator("#comment-mode").click();
  await expect(page.locator("#comment-hint")).toBeVisible();
  await frame.locator("#add").click();
  await expect(page.locator("#pointed-label")).toHaveText("On button#add · “Add”");
  await page.getByRole("textbox", { name: "Comment on this part" }).fill("Label it Add to total.");
  await page.getByRole("button", { name: "Send to agent" }).click();
  const pins = frame.locator("shartifacts-overlay .pin");
  await expect(pins).toHaveText(["1"]);
  await expect(page.locator("#comments-link")).toHaveText("Comments · 1");
  await expect(frame.locator("#count")).toHaveText("10");
  const pointed = (await client.callTool({ name: "artifact_comments", arguments: { id: tool.id, op: "list" } })).structuredContent.threads[0];
  assert.deepEqual(
    [pointed.anchor, pointed.element, pointed.quote, pointed.to_claude, pointed.version],
    ["#add", '<button id="add">', "Add", true, 2],
  );
  await page.keyboard.press("Escape");
  await expect(page.locator("#comment-hint")).toBeHidden();
  await pins.first().click();
  await expect(page.locator("#comments")).toBeVisible();
  await expect(page.locator(".thread.flash")).toContainText("Label it Add to total.");
  await expect(page.locator(".thread-target").first()).toContainText("On button#add");
  await expect(page.locator(".thread.unplaced")).toHaveCount(0);
  await page.locator("#close-comments").click();
  // Outside comment mode the page works as usual, and selected text offers a chip that quotes it.
  await frame.locator("#v2").evaluate((h) => getSelection().selectAllChildren(h));
  await frame.locator("shartifacts-overlay .chip").click();
  await expect(page.locator("#pointed-quote")).toHaveText("v2");
  await page.getByRole("textbox", { name: "Comment on this part" }).fill("Call it version two.");
  await page.keyboard.press(process.platform === "darwin" ? "Meta+Enter" : "Control+Enter");
  await expect(pins).toHaveText(["1", "2"]);
  for (const t of (await client.callTool({ name: "artifact_comments", arguments: { id: tool.id, op: "list" } })).structuredContent.threads)
    await client.callTool({ name: "artifact_comments", arguments: { id: tool.id, op: "resolve", comment_id: t.id } });
  await expect(pins).toHaveCount(0);
  // Pinning lifts an artifact to the top of the library and the home gallery.
  await details();
  await page.getByRole("button", { name: "More artifact actions" }).click();
  await page.getByRole("button", { name: "Pin to top of library" }).click();
  await expect(page.locator("#library .library-item").first()).toContainText("PINNED");
  await page.goto(base + "/artifacts");
  await expect(page.locator(".gallery-card").first()).toContainText("Storage tool");
  // Cards carry live thumbnails that read (but never write) the artifact's storage.
  await expect(
    page.frameLocator(`.gallery-thumb iframe[data-id="${tool.id}"]`).locator("#count"),
  ).toHaveText("10");
  // Changes: the edit an agent made to the storage tool, line by line.
  await page.goto(base + "/a/" + tool.id);
  await details();
  await page.getByRole("button", { name: "Changes", exact: true }).click();
  await expect(page.locator("#changes-title")).toHaveText("V2 CHANGES · +1 −1");
  await expect(page.locator(".diff-line.ins")).toContainText('<h1 id="v2">v2</h1>');
  await page.getByRole("button", { name: "Preview", exact: true }).click();
  // Slides with math: a deck navigated by keyboard.
  const deck = await api("/api/artifacts", {
    title: "Deck",
    kind: "slides",
    content: "# Quarterly update\n\nQ4\n\n---\n\n## Uptime\n\nAvailability is $1 - \\frac{d}{t}$.\n\nNote: mention the migration.\n\n---\n\n## Done",
  });
  await page.goto(base + "/a/" + deck.id);
  await expect(frame.locator("#count")).toHaveText("1 / 3");
  await expect(frame.locator(".slide.active.title h1")).toHaveText("Quarterly update");
  // Arrow keys pressed in the workspace reach the deck.
  await page.locator("body").press("ArrowRight");
  await expect(frame.locator(".slide.active h2")).toHaveText("Uptime");
  await expect(frame.locator(".slide.active .katex")).toHaveCount(1);
  await expect(frame.locator(".slide.active .notes")).toBeHidden();
  await page.screenshot({ path: "test-results/slides.png" });
  // Multi-file: a page with CSS, ES modules, fetched JSON and a second page.
  const site = (await client.callTool({
    name: "create_artifact",
    arguments: {
      title: "Team site",
      kind: "html",
      content: `<link rel="stylesheet" href="css/site.css"><h1 id="t">Home</h1><p id="team">…</p><a id="about" href="about.html">About</a><script type="module" src="js/app.js"></script>`,
      files: {
        "css/site.css": "h1{color:rgb(204, 120, 92)}",
        "js/app.js": "import {load} from './data.js'; load().then(d => document.getElementById('team').textContent = d.join(', '))",
        "js/data.js": "export const load = () => fetch('team.json').then(r => r.json())",
        "team.json": '["Sam","Ada"]',
        "about.html": '<h1 id="t">About us</h1>',
      },
    },
  })).structuredContent;
  assert.equal(site.warnings, undefined);
  await page.goto(base + "/a/" + site.id);
  await expect(page.locator("#preview")).toHaveAttribute("src", `/preview/${site.id}/1/`);
  await expect(frame.locator("#team")).toHaveText("Sam, Ada");
  await expect(frame.locator("#t")).toHaveCSS("color", "rgb(204, 120, 92)");
  await frameInteractive();
  await frame.locator("#about").click();
  await expect(frame.locator("#t")).toHaveText("About us");
  await details();
  await page.getByRole("button", { name: "Source", exact: true }).click();
  await expect(page.locator("#file-select option")).toHaveText(["index.html (page)", "about.html", "css/site.css", "js/app.js", "js/data.js", "team.json"]);
  await page.locator("#file-select").selectOption("team.json");
  await expect(page.getByRole("textbox", { name: "Artifact source", exact: true })).toHaveValue('["Sam","Ada"]');
  const zipped = await fetch(base + `/api/artifacts/${site.id}/download`);
  assert.equal(zipped.headers.get("content-type"), "application/zip");
  await page.getByRole("button", { name: "Preview", exact: true }).click();
  await page.screenshot({ path: "test-results/gallery.png", fullPage: true });
  // Restart from exactly the same directory and verify revision history survives.
  await client.close();
  client = null;
  await stop();
  await start();
  const durable = await api("/api/artifacts/" + id);
  assert.equal(durable.revisions.length, 3);
  assert.equal(durable.revisions[1].content, updated);
  assert.equal(durable.revisions[2].note, "Restored from v1");
  assert.deepEqual(
    await api(`/api/artifacts/${tool.id}/storage`, { op: "get", key: "count" }),
    { key: "count", value: "10", shared: false },
    "artifact storage survives restart",
  );
  assert.deepEqual(
    Buffer.from(await (await fetch(base + video.url)).arrayBuffer()),
    videoBytes,
    "uploaded media survives restart exactly",
  );
  await page.goto(base + "/a/" + mediaArtifact.id);
  await expect
    .poll(() => frame.locator("#movie").evaluate((v) => v.readyState))
    .toBeGreaterThanOrEqual(2);
  // Sign-in: set a password in the workspace, connect an agent with a token, sign in from
  // another browser, revoke the token and sign out.
  await page.goto(base + "/artifacts");
  await sidebarMenu("⚿ Set a password");
  await page.locator("#signin-dialog input[name=password]").fill("correct horse");
  await page.locator("#signin-dialog input[name=again]").fill("correct horse");
  await page.getByRole("button", { name: "Turn on sign-in" }).click();
  await expect(page.locator("#signin-title")).toHaveText("Sign-in is on.");
  await page.locator("#signin-dialog [data-close]").click();
  await expect(page.locator("#signin")).toHaveText("⚿ Sign-in & tokens");
  assert.equal((await fetch(base + "/api/artifacts")).status, 401, "the API without sign-in");
  // This browser stays signed in, and previews still load their media, which they request
  // cross-site without the cookie.
  await page.goto(base + "/a/" + mediaArtifact.id);
  await expect
    .poll(() => frame.locator("#movie").evaluate((v) => v.readyState))
    .toBeGreaterThanOrEqual(2);
  // Connect an agent makes a token and puts it in the commands.
  await sidebarMenu("⌁ Connect an agent");
  await page.locator("#connect-dialog").getByRole("button", { name: "Make a token" }).click();
  await expect(page.locator("#connect-dialog pre").first()).toContainText("--header \"Authorization: Bearer shx_");
  const token = (await page.locator("#connect-dialog pre").first().textContent()).match(/Bearer (shx_[\w-]+)/)[1];
  await page.locator("#connect-dialog [data-close]").click();
  const agent = new Client({ name: "token-test", version: "1" });
  await agent.connect(new StreamableHTTPClientTransport(new URL(base + "/mcp"), { requestInit: { headers: { Authorization: `Bearer ${token}` } } }));
  assert.ok((await agent.callTool({ name: "list_artifacts", arguments: {} })).structuredContent.artifacts.length > 0);
  // The agent's shell uploads with the URL list_media hands it, without the token.
  const uploadURL = (await agent.callTool({ name: "list_media", arguments: {} })).structuredContent.upload_url;
  assert.match(uploadURL, /\/api\/media\/[^/]+$/);
  assert.equal((await fetch(uploadURL + "?name=hello.txt", { method: "POST", headers: { "Content-Type": "text/plain" }, body: "hello" })).status, 201);
  // Another browser must sign in, and then lands where it was going.
  const other = await browser.newPage({ viewport: { width: 390, height: 844 } });
  other.on("pageerror", (e) => errors.push(e.message));
  await other.goto(base + "/a/" + mediaArtifact.id);
  await expect(other).toHaveURL(/\/login\?next=/);
  await other.locator("#password").fill("wrong horse");
  await other.locator("#password").press("Enter");
  await expect(other.locator("#login-error")).toHaveText("That isn't the password.");
  await other.screenshot({ path: "test-results/login-mobile.png" });
  await other.locator("#password").fill("correct horse");
  await other.locator("#password").press("Enter");
  await expect(other).toHaveURL(base + "/a/" + mediaArtifact.id);
  await expect(other.frameLocator("#preview").locator("#movie")).toBeAttached();
  await other.close();
  // Revoking the token refuses the agent from its next request.
  await page.goto(base + "/");
  await page.locator("[data-signin]").click();
  const listed = page.locator("#signin-dialog .token-list li", { hasText: "Claude Code" });
  await expect(listed).toContainText("last used");
  await page.screenshot({ path: "test-results/signin.png" });
  page.once("dialog", (d) => d.accept());
  await listed.getByRole("button", { name: "Revoke" }).click();
  await expect(listed).toHaveCount(0);
  await assert.rejects(agent.callTool({ name: "list_artifacts", arguments: {} }));
  await agent.close().catch(() => {});
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page).toHaveURL(/\/login/);
  assert.equal((await fetch(base + "/api/artifacts")).status, 401);
  // The "Broken" artifact errors on purpose, including in its home-page thumbnail.
  assert.deepEqual(errors.filter((e) => !e.includes("missingFunction")), [], "browser runtime errors");
  console.log(
    "PASS: MCP upload URL discovery, direct curl upload and embed, shared design CSS and rendered styling, MCP chunked media upload, browser image upload, video decode/playback/seek/captions, byte ranges, media persistence, all six artifact types, live drafts, recovery, history, restore, downloads, MCP lifecycle, concurrent editing, sandbox, imports, library, search (content, typos, corrections, tags, filters, keyboard), tags, related artifacts, duplication, deletion, archiving (bulk select, the archive, restore, delete forever), mobile drawer/details/geometry, all-kind immersive mode, native video fullscreen, fullscreen fallback, pinned links, keyboard save, bundled React libraries, shadcn/ui, Tailwind, CDN scripts, window.storage, window.fs.readFile, window.claude (off), runtime error reports, comments, gallery thumbnails, Changes view, slides with math, multi-file artifacts, forms, dialogs, downloads, external links, full-page view, restart persistence, commenting on the preview (comment mode, pins, quoted selections), installing as an app (manifest, icons), and sign-in (password, login page, previews and media without the cookie, agent tokens over MCP, upload grants, revoking, signing out).",
  );
} catch (e) {
  if (page) {
    console.error(
      "Frame URLs:",
      page.frames().map((f) => f.url()),
    );
    console.error(
      "Preview src:",
      await page.locator("#preview").getAttribute("src"),
    );
    console.error(
      "Frame text:",
      await page
        .frameLocator("#preview")
        .locator("body")
        .innerText()
        .catch(() => "(no body)"),
    );
    await page.screenshot({ path: "test-results/failure.png", fullPage: true });
  }
  throw e;
} finally {
  if (client) await client.close();
  if (browser) await browser.close();
  await stop();
  await rm(dir, { recursive: true, force: true });
}
