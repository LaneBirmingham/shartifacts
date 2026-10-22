// The capability runtime (claude.use) end to end, in Chrome: two people on one artifact,
// then Claude sampling against a fake Messages API and connectors against this server's
// own MCP endpoint. Run with `npm run test:runtime`.
import { chromium } from "@playwright/test";
import { spawn, spawnSync } from "node:child_process";
import { createServer } from "node:http";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";

const dir = await mkdtemp(path.join(tmpdir(), "shartifacts-runtime-"));
const binary = path.join(dir, "shartifacts");
assert.equal(spawnSync("go", ["build", "-o", binary, "."], { stdio: "inherit" }).status, 0);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const servers = [];
async function start(port, args = [], env = {}) {
  const proc = spawn(binary, ["-listen", `127.0.0.1:${port}`, "-data", path.join(dir, "data-" + port), ...args], { stdio: ["ignore", "ignore", "inherit"], env: { ...process.env, ...env } });
  servers.push(proc);
  for (let i = 0; i < 100; i++) {
    try {
      if ((await fetch(`http://127.0.0.1:${port}/healthz`)).ok) return `http://127.0.0.1:${port}`;
    } catch {}
    await sleep(100);
  }
  throw new Error("server did not start");
}
const post = async (base, p, b, h = {}) => {
  const r = await fetch(base + p, { method: "POST", headers: { "Content-Type": "application/json", ...h }, body: JSON.stringify(b) });
  assert.ok(r.ok, await r.clone().text());
  return r.json();
};
const browser = await chromium.launch({
  headless: true,
  ...(process.env.CHROME_PATH ? { executablePath: process.env.CHROME_PATH } : process.platform === "darwin" ? { executablePath: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" } : {}),
});
// A fake Messages API: streams when asked, calls the page's first tool once, then answers.
const upstream = [];
const fake = createServer((req, res) => {
  let b = "";
  req.on("data", (c) => (b += c));
  req.on("end", () => {
    const body = JSON.parse(b);
    upstream.push(body);
    const last = body.messages.at(-1);
    const result = last.content.find?.((c) => c.type === "tool_result");
    const text = last.content.find?.((c) => c.type === "text")?.text || "";
    let blocks, stop = "end_turn";
    if (body.tools?.length && !result) [blocks, stop] = [[{ type: "text", text: "Let me check." }, { type: "tool_use", id: "toolu_1", name: body.tools[0].name, input: { x: 1 } }], "tool_use"];
    else if (result) blocks = [{ type: "text", text: "Result: done" }];
    else if (text.includes("REFUSE")) [blocks, stop] = [[], "refusal"];
    else blocks = [{ type: "text", text: body.system?.[0]?.text?.includes("JSON") ? 'Here: {"tags":["a","b"]}' : "echo: " + text }];
    const msg = { id: "msg_1", type: "message", role: "assistant", model: body.model, content: blocks, stop_reason: stop, stop_sequence: null, usage: { input_tokens: 1, output_tokens: 1 } };
    if (!body.stream) {
      res.setHeader("Content-Type", "application/json");
      return res.end(JSON.stringify(msg));
    }
    res.setHeader("Content-Type", "text/event-stream");
    const ev = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`);
    ev("message_start", { message: { ...msg, content: [], stop_reason: null } });
    blocks.forEach((blk, i) => {
      if (blk.type === "text") {
        ev("content_block_start", { index: i, content_block: { type: "text", text: "" } });
        for (const part of blk.text.match(/.{1,5}/g)) ev("content_block_delta", { index: i, delta: { type: "text_delta", text: part } });
      } else {
        ev("content_block_start", { index: i, content_block: { type: "tool_use", id: blk.id, name: blk.name, input: {} } });
        ev("content_block_delta", { index: i, delta: { type: "input_json_delta", partial_json: JSON.stringify(blk.input) } });
      }
      ev("content_block_stop", { index: i });
    });
    ev("message_delta", { delta: { stop_reason: stop, stop_sequence: null }, usage: { output_tokens: 1 } });
    ev("message_stop", {});
    res.end();
  });
});
await new Promise((r) => fake.listen(0, "127.0.0.1", r));
try {
  // ---- Two people on one artifact ----------------------------------------------------
  const base = await start(18400 + Math.floor(Math.random() * 1000));
  const sam = await post(base, "/api/people", { name: "Sam" }),
    ada = await post(base, "/api/people", { name: "Ada" });
  const page = `<!doctype html><html><body><div id="who"></div><div id="tasks"></div><div id="peers"></div><div id="msg"></div><img id="pic"><div id="out">…</div>
<script>
(async () => {
  const names = ["user","db","room","assets","permissions","files","mcp","artifact","comments","downloads","sample","nope"];
  const caps = Object.fromEntries(await Promise.all(names.map(async n => [n, await claude.use(n)])));
  window.caps = caps; window.marker = Math.random();
  const me = await caps.user.me();
  document.getElementById('who').textContent = me.name + '|' + (await caps.user.isOwner()) + '|' + (caps.nope === null) + '|' + (caps.sample === null);
  caps.db.collection('tasks').orderBy('n').onSnapshot(s => document.getElementById('tasks').textContent = s.docs.map(d => d.data().title).join(','));
  caps.room.onPeers(c => document.getElementById('peers').textContent = c.peers.length + ':' + c.peers.map(p => p.presence.name || '?').sort().join(','));
  caps.room.presence({ name: me.name });
  caps.room.on('ping', m => document.getElementById('msg').textContent = (m.isMe ? 'me' : 'other') + JSON.stringify(m.data));
  document.getElementById('out').textContent = 'ready';
})().catch(e => document.getElementById('out').textContent = 'ERR ' + JSON.stringify(e));
<\/script></body></html>`;
  const a = await post(base, "/api/artifacts", { title: "Caps", kind: "html", content: page }, { "X-Shartifacts-Viewer": sam.id });
  const open = async (person) => {
    const ctx = await browser.newContext({ acceptDownloads: true });
    await ctx.addInitScript((id) => {
      try {
        localStorage.setItem("shartifacts.viewer", id);
      } catch {}
    }, person.id);
    const p = await ctx.newPage();
    const errors = [];
    p.on("pageerror", (e) => errors.push(e.message));
    await p.goto(`${base}/a/${a.id}`);
    await p.frameLocator("#preview").locator("#out").filter({ hasText: "ready" }).waitFor();
    return { p, fl: p.frameLocator("#preview"), f: () => p.frames().find((f) => f.url().includes("/preview/")), errors };
  };
  const A = await open(sam),
    B = await open(ada);
  assert.equal(await A.fl.locator("#who").textContent(), "Sam|true|true|true");
  assert.equal(await B.fl.locator("#who").textContent(), "Ada|false|true|true");
  assert.equal(await A.p.locator("#viewer").textContent(), "Sam");
  await A.f().evaluate(() => caps.db.collection("tasks").doc("t1").set({ n: 1, title: "alpha" }));
  await B.fl.locator("#tasks").filter({ hasText: "alpha" }).waitFor({ timeout: 5000 });
  await B.f().evaluate(() => caps.db.collection("tasks").add({ n: 2, title: "beta" }));
  await A.fl.locator("#tasks").filter({ hasText: "alpha,beta" }).waitFor({ timeout: 5000 });
  await A.fl.locator("#peers").filter({ hasText: "2:Ada,Sam" }).waitFor({ timeout: 5000 });
  await B.f().evaluate(() => caps.room.emit("ping", { x: 1 }));
  await A.fl.locator("#msg").filter({ hasText: 'other{"x":1}' }).waitFor({ timeout: 5000 });
  assert.equal(await B.f().evaluate(async () => { const id = await caps.user.id(); await caps.db.doc(`data/users/${id}/prefs`).set({ theme: "dark" }); return (await caps.db.doc(`data/users/${id}/prefs`).get()).data().theme; }), "dark");
  assert.equal(await A.f().evaluate(async (id) => (await caps.db.doc(`data/users/${id}/prefs`).get()).exists, ada.id), false, "private data is hidden from the owner");
  assert.deepEqual(
    await A.f().evaluate(async () => {
      const bytes = Uint8Array.from(atob("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aL1sAAAAASUVORK5CYII="), (c) => c.charCodeAt(0));
      const r = await caps.assets.upload(new Blob([bytes], { type: "image/png" }));
      const img = document.getElementById("pic");
      img.src = r.url;
      await img.decode();
      return [img.naturalWidth, await caps.assets.upload(new Blob(["<p>"], { type: "text/html" })).catch((e) => e.code), (await caps.assets.list()).usage.files];
    }),
    [1, "unsupported_type", 1],
  );
  assert.deepEqual(
    await A.f().evaluate(async () => {
      const r = await caps.mcp.callTool("artifacts_data", "query", { collection: "tasks" });
      const h = await caps.mcp.server("artifacts_data");
      return [r.payload.docs.length, (await h.get({ path: "tasks/t1" })).data.title, await caps.permissions.state("db")];
    }),
    [2, "alpha", "granted"],
  );
  await A.f().evaluate(async () => caps.comments.create({ anchor: await caps.comments.anchorFor(document.getElementById("who")), text: "From the page" }));
  // The thread list lives in the comments drawer, closed by default: present is enough.
  await A.p.locator(".comment-body").filter({ hasText: "From the page" }).waitFor({ state: "attached" });
  A.p.once("dialog", (d) => d.accept());
  const [file] = await Promise.all([A.p.waitForEvent("download"), A.f().evaluate(() => caps.downloads.save({ filename: "tasks.csv", data: "n\n1" }))]);
  assert.equal(file.suggestedFilename(), "tasks.csv");
  const marker = await A.f().evaluate(() => window.marker);
  assert.deepEqual(await A.f().evaluate(() => caps.artifact.publish({ "notes.md": "# Notes" })), { version: "2" });
  // Ada's view still runs version 1 until it follows the publish, so her save conflicts.
  assert.equal(await B.f().evaluate(() => caps.artifact.publish("<!doctype html><h1>stale</h1>").catch((e) => e.code)), "conflict");
  await sleep(3500);
  assert.equal(await A.f().evaluate(() => window.marker), marker, "a files publish keeps its frame running");
  assert.equal(await A.f().evaluate(() => caps.artifact.edit([]).catch((e) => e.code)), "invalid_content");
  const solo = await browser.newPage();
  await solo.goto(`${base}/preview/${a.id}?version=1`);
  assert.equal(await solo.evaluate(() => claude.use("db")), null, "a standalone preview has no runtime host");
  assert.deepEqual([...A.errors, ...B.errors], []);

  // ---- Claude sampling and connectors ----------------------------------------------
  const port = 19400 + Math.floor(Math.random() * 500);
  await writeFile(path.join(dir, "connectors.json"), JSON.stringify([{ name: "Library", url: `http://127.0.0.1:${port}/mcp` }]));
  const claudeBase = await start(port, ["-connectors", path.join(dir, "connectors.json")], { ANTHROPIC_API_KEY: "fake", ANTHROPIC_BASE_URL: `http://127.0.0.1:${fake.address().port}` });
  const s = await post(claudeBase, "/api/artifacts", { title: "S", kind: "html", content: "<!doctype html><output id=o>…</output><script>claude.use('sample').then(s => { window.sample = s; document.getElementById('o').textContent = 'ready' })<\/script>" });
  const sp = await browser.newPage();
  await sp.goto(`${claudeBase}/a/${s.id}`);
  await sp.frameLocator("#preview").locator("#o").filter({ hasText: "ready" }).waitFor();
  const f = sp.frames().find((x) => x.url().includes("/preview/"));
  assert.equal((await f.evaluate(() => sample("Hello there"))).text, "echo: Hello there");
  assert.deepEqual(
    await f.evaluate(async () => {
      const ups = [];
      const r = await sample("Stream me", { onText: (u) => ups.push(u), cache: false });
      return [r.text, ups.length > 1, ups.every((u, i) => u.text === (ups[i - 1]?.text || "") + u.delta)];
    }),
    ["echo: Stream me", true, true],
  );
  const calls = upstream.length;
  await f.evaluate(() => sample("Hello there"));
  assert.equal(upstream.length, calls, "a repeated call replays the cached answer");
  assert.deepEqual(
    await f.evaluate(async () => {
      const ran = [];
      const r = await sample("Use the tool", { onText: () => {}, tools: [{ name: "get_total", description: "Returns the total.", execute: async (input) => (ran.push(input), { total: 42 }) }] });
      return [r.text, ran];
    }),
    ["Let me check.\n\nResult: done", [{ x: 1 }]],
  );
  assert.deepEqual(upstream.at(-1).messages.map((m) => m.content.map((c) => c.type)), [["text"], ["text", "tool_use"], ["tool_result"]]);
  assert.deepEqual(await f.evaluate(() => sample.json("Tags please")), { tags: ["a", "b"] });
  assert.equal(await f.evaluate(() => sample("REFUSE", { cache: false }).catch((e) => e.code)), "refused");
  assert.deepEqual(
    await f.evaluate(async () => {
      const mcp = await claude.use("mcp");
      const lib = (await mcp.listTools()).servers.find((x) => x.server === "Library");
      const r = await mcp.callTool("Library", "list_artifacts", {});
      return [lib.authStatus, r.payload.artifacts.length, await mcp.callTool("Nope", "x", {}).catch((e) => e.code), await mcp.callTool("Library", "get_artifact", { id: "missing" }).catch((e) => e.code)];
    }),
    ["connected", 1, "server_not_connected", "tool_error"],
  );
  // ---- Read-only share links on their own hostname ---------------------------------
  // One server, two names: 127.0.0.1 is the workspace, localhost the share origin.
  const sharePort = 19900 + Math.floor(Math.random() * 90);
  const workspace = await start(sharePort, ["-share-origin", `http://localhost:${sharePort}`, "-share-listen", "off"]);
  const shareOrigin = `http://localhost:${sharePort}`;
  const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==", "base64");
  const pic = await (await fetch(`${workspace}/api/media`, { method: "POST", headers: { "Content-Type": "image/png" }, body: png })).json();
  const board = await post(workspace, "/api/artifacts", {
    title: "Board",
    kind: "html",
    content: `<!doctype html><h1 id="v">one</h1><p id="task">…</p><p id="count">…</p><p id="mine">…</p><p id="write">…</p><img id="pic" src="/media/${pic.id}"><img id="asset"><script>
(async () => {
  const db = await claude.use("db");
  db.doc("tasks/t1").onSnapshot((s) => {
    document.getElementById("task").textContent = s.exists ? s.data().title : "none";
    if (s.exists && s.data().img) document.getElementById("asset").src = s.data().img;
  });
  const c = await window.storage.get("count", true);
  document.getElementById("count").textContent = c ? c.value : "none";
  const mine = await window.storage.get("diary");
  document.getElementById("mine").textContent = mine ? mine.value : "none";
  document.getElementById("write").textContent = await db.doc("probe/write").set({ by: "page" }).then(() => "wrote", (e) => e.code);
})();
<\/script>`,
  });
  const asset = await (await fetch(`${workspace}/api/artifacts/${board.id}/assets`, { method: "POST", headers: { "Content-Type": "image/png" }, body: png })).json();
  await post(workspace, `/api/artifacts/${board.id}/db`, { op: "set", path: "tasks/t1", data: { title: "public task", img: asset.url } });
  await post(workspace, `/api/artifacts/${board.id}/storage`, { op: "set", key: "count", value: "7", shared: true });
  await post(workspace, `/api/artifacts/${board.id}/storage`, { op: "set", key: "diary", value: "owner only" });
  const loaded = async (frame, sel) => {
    for (let i = 0; i < 100; i++) {
      if (await frame.locator(sel).evaluate((el) => el.complete && el.naturalWidth > 0)) return true;
      await new Promise((r) => setTimeout(r, 100));
    }
    return false;
  };
  const ws = await browser.newPage();
  await ws.goto(`${workspace}/a/${board.id}`);
  // The workspace loads the same objects at their own paths, as before.
  const own = ws.frameLocator("#preview");
  await own.locator("#task").filter({ hasText: "public task" }).waitFor();
  assert.ok(await loaded(own, "#pic"), "workspace media");
  assert.ok(await loaded(own, "#asset"), "workspace asset");
  assert.equal(await own.locator("#mine").textContent(), "owner only");
  await ws.locator("#toggle-details").click(); // the artifact's actions live in its details
  await ws.getByRole("button", { name: "More artifact actions" }).click();
  await ws.getByRole("button", { name: "Share read-only link…" }).click();
  await ws.getByRole("button", { name: "Link that follows new versions" }).click();
  await ws.locator(".share-row code").first().waitFor();
  const link = await ws.locator(".share-row code").first().textContent();
  assert.ok(link.startsWith(shareOrigin + "/s/"), link);
  const visitor = await (await browser.newContext()).newPage();
  const visitorErrors = [];
  visitor.on("pageerror", (e) => visitorErrors.push(e.message));
  await visitor.goto(link);
  const shared = visitor.frameLocator("#preview");
  await shared.locator("#task").filter({ hasText: "public task" }).waitFor();
  assert.equal(await shared.locator("#count").textContent(), "7");
  // Media and assets load through the link's token; the owner's personal storage stays theirs.
  assert.ok(await loaded(shared, "#pic"), "shared media");
  assert.ok(await loaded(shared, "#asset"), "shared asset");
  assert.ok((await shared.locator("#asset").getAttribute("src")).startsWith(`/s/${link.split("/s/")[1]}/_blob/`));
  assert.equal(await shared.locator("#mine").textContent(), "none");
  for (const path of [`/media/${pic.id}`, asset.url]) assert.equal((await fetch(shareOrigin + path)).status, 404, path);
  await shared.locator("#write").filter({ hasText: "invalid_argument" }).waitFor();
  assert.equal(await visitor.locator("#view-title").textContent(), "Board");
  // Data changes reach the visitor live; new versions within the poll.
  await post(workspace, `/api/artifacts/${board.id}/db`, { op: "set", path: "tasks/t1", data: { title: "updated task" } });
  await shared.locator("#task").filter({ hasText: "updated task" }).waitFor({ timeout: 5000 });
  await fetch(`${workspace}/api/artifacts/${board.id}`, { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ edits: [{ old_str: ">one<", new_str: ">two<" }], base_version: 1 }) });
  await shared.locator("#v").filter({ hasText: "two" }).waitFor({ timeout: 15000 });
  // The share hostname serves nothing else.
  for (const path of ["/", "/api/artifacts", `/a/${board.id}`, `/view/${board.id}`, "/mcp"]) assert.equal((await fetch(shareOrigin + path)).status, 404, path);
  const { shares } = await (await fetch(`${workspace}/api/artifacts/${board.id}/shares`)).json();
  await fetch(`${workspace}/api/shares/${shares[0].token}`, { method: "DELETE" });
  await visitor.reload();
  assert.equal((await fetch(link)).status, 404);
  assert.deepEqual(visitorErrors, []);

  console.log("PASS: claude.use runtime: identity, realtime db across two people, private per-person data, room presence and messages, assets, permissions, artifacts_data over mcp, page comments, downloads, self-publishing (files keep the frame, stale html conflicts), Claude sampling (streaming, cache, page tools, json, refusals), connectors over MCP, and read-only share links (own hostname serving nothing else, media and assets only through the token, live data, new versions, no writes, no personal storage, revocation).");
} finally {
  await browser.close();
  servers.forEach((p) => p.kill());
  fake.close();
  await rm(dir, { recursive: true, force: true });
}
