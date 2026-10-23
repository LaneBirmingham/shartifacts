// End-to-end test of the apps hub: an agent builds and deploys over MCP, a person uses the
// app in its tab (its sandboxed page calling its Go backend), rolls back, follows a new
// deploy, reads logs, and shares a public link that works on the app's own hostname, where
// the page keeps real storage and cookies; in the hub, the page's localStorage is kept for it.
import { chromium, expect } from "@playwright/test";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { spawn, spawnSync } from "node:child_process";
import { mkdtemp, mkdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";

const dir = await mkdtemp(path.join(tmpdir(), "shartifacts-apps-"));
const binary = path.join(dir, "shartifacts");
assert.equal(spawnSync("go", ["build", "-o", binary, "."], { stdio: "inherit" }).status, 0);
const port = 18000 + Math.floor(Math.random() * 10000);
const base = `http://127.0.0.1:${port}`;
// The share hostname and the apps' own hostnames are other names for the same server, as a
// reverse proxy would deliver them (Chrome resolves every *.localhost to this machine).
const shareOrigin = `http://localhost:${port}`;
const appOrigin = `http://*.localhost:${port}`;
const server = spawn(binary, ["-listen", `127.0.0.1:${port}`, "-data", path.join(dir, "data"), "-share-origin", shareOrigin, "-app-origin", appOrigin, "-share-listen", "off"], { stdio: ["ignore", "ignore", "pipe"] });
server.stderr.on("data", (b) => process.stderr.write(b));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
for (let i = 0; ; i++) {
  try {
    if ((await fetch(base + "/healthz")).ok) break;
  } catch {}
  if (i > 100) throw new Error("server did not start");
  await sleep(100);
}

let browser, client;
const tool = async (name, args = {}) => {
  const res = await client.callTool({ name, arguments: args });
  assert.ok(!res.isError, `${name}: ${JSON.stringify(res.content)}`);
  return res.structuredContent ?? res.content[0].text;
};
try {
  client = new Client({ name: "apps-test", version: "1" });
  await client.connect(new StreamableHTTPClientTransport(new URL(base + "/mcp")));
  const tools = (await client.listTools()).tools.map((t) => t.name);
  for (const t of ["get_app", "deploy_app", "manage_app"]) assert.ok(tools.includes(t), `tool ${t}`);
  for (const t of ["app_guide", "create_app", "promote_app", "call_app"]) assert.ok(!tools.includes(t), `retired tool ${t}`);
  const guide = await tool("get_app", { guide: true });
  assert.match(guide, /Backend contract/);
  assert.match(guide, /127\.0\.0\.1:\$PORT/);

  // The agent: create from the template, then change the heading with an edit.
  const created = await tool("deploy_app", { name: "Team notes", template: "notes", icon: "notes", description: "Notes the whole house can see." });
  assert.equal(created.status, "ready", JSON.stringify(created));
  const id = created.id;
  const v2 = await tool("deploy_app", { id, base_version: 1, merge: true, note: "Rename the heading", edits: [{ file: "src/App.tsx", old_str: "<h1>Notes", new_str: "<h1>Team notes" }] });
  assert.equal(v2.status, "ready", JSON.stringify(v2));
  assert.equal(v2.live, 2);
  const health = await tool("manage_app", { id, op: "call", path: "/api/health" });
  const listed = await tool("get_app");
  assert.ok(listed.apps.some((a) => a.id === id && a.live === 2));
  assert.equal(listed.runtime.go.startsWith("go"), true);
  assert.equal(health.status, 200);
  assert.match(health.body, /"version":"2"/);

  browser = await chromium.launch({
    ...(process.env.CHROME_PATH
      ? { executablePath: process.env.CHROME_PATH }
      : process.platform === "darwin"
        ? { executablePath: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" }
        : {}),
  });
  const page = await browser.newPage({ viewport: { width: 1440, height: 920 } });
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await mkdir("test-results", { recursive: true });

  // The new main page: the launcher.
  await page.goto(base + "/");
  await expect(page.getByRole("heading", { name: "Apps that run." })).toBeVisible();
  await expect(page.locator("#rt-backends")).toContainText("Go");
  await page.screenshot({ path: "test-results/apps-home.png" });

  // Open the app in a tab; its page talks to its backend through the sandbox.
  await page.locator(".app-card", { hasText: "Team notes" }).click();
  await expect(page).toHaveURL(`${base}/apps/${id}`);
  await expect(page.getByRole("tab", { name: /Team notes/ })).toHaveAttribute("aria-selected", "true");
  const frame = page.frameLocator(`#panel-${id} iframe`);
  await expect(frame.getByRole("heading", { name: /Team notes/ })).toBeVisible();
  await frame.getByLabel("New note").fill("Buy milk");
  await frame.getByRole("button", { name: "Add note" }).click();
  await expect(frame.getByText("Buy milk")).toBeVisible();
  await page.screenshot({ path: "test-results/apps-tab.png" });

  // The sandboxed page has no storage of its own; the hub keeps its localStorage.
  await frame.locator("body").evaluate(() => localStorage.setItem("hub-kept", "yes"));
  const keptInHub = () =>
    page.evaluate(
      (id) =>
        new Promise((resolve) => {
          const req = indexedDB.open("shartifacts-apps", 1);
          req.onsuccess = () => {
            const get = req.result.transaction("storage").objectStore("storage").get(id);
            get.onsuccess = () => resolve(get.result ? get.result["hub-kept"] : null);
            get.transaction.oncomplete = () => req.result.close();
          };
          req.onerror = () => resolve(null);
        }),
      id,
    );
  await expect.poll(keptInHub).toBe("yes");

  // Tabs survive a reload, and so does the data, the page's localStorage included.
  await page.reload();
  await expect(page.getByRole("tab", { name: /Team notes/ })).toHaveAttribute("aria-selected", "true");
  await expect(frame.getByText("Buy milk")).toBeVisible();
  assert.equal(await frame.locator("body").evaluate(() => localStorage.getItem("hub-kept")), "yes");
  // A reload inside the frame keeps localStorage and sessionStorage too.
  await frame.locator("body").evaluate(() => {
    window.beforeReload = true;
    sessionStorage.setItem("tab", "1");
    localStorage.setItem("hub-kept", "again");
    setTimeout(() => location.reload(), 50);
  });
  await expect.poll(() => frame.locator("body").evaluate(() => !window.beforeReload).catch(() => false)).toBe(true);
  await expect(frame.getByText("Buy milk")).toBeVisible();
  assert.deepEqual(await frame.locator("body").evaluate(() => [localStorage.getItem("hub-kept"), sessionStorage.getItem("tab")]), ["again", "1"]);
  await expect.poll(keptInHub).toBe("again");

  // Switching to Home and back keeps the app's frame alive.
  await page.getByRole("tab", { name: "Apps" }).click();
  await expect(page).toHaveURL(`${base}/`);
  await page.getByRole("tab", { name: /Team notes/ }).click();
  await expect(frame.getByText("Buy milk")).toBeVisible();

  // Versions: roll back to version 1, whose heading is plain "Notes".
  const panel = page.locator(`#panel-${id}`);
  await panel.getByRole("button", { name: "Versions" }).click();
  await expect(panel.locator(".version.is-live")).toContainText("v2");
  await expect(panel.locator(".drawer-body")).toContainText("Files are kept for the newest 20 versions that built, the live one and versions links pin. The oldest you can roll back to is v1.");
  await panel.locator(".version", { hasText: "v1" }).getByRole("button", { name: "Roll back to this" }).click();
  await expect(frame.getByRole("heading", { name: /^Notes/ })).toBeVisible();
  await expect(frame.getByText("Buy milk")).toBeVisible(); // data is shared by versions
  await page.screenshot({ path: "test-results/apps-versions.png" });

  // An agent deploys while the tab is open: the tab offers the new version.
  const v3 = await tool("deploy_app", { id, base_version: 2, merge: true, note: "Say hello", edits: [{ file: "src/App.tsx", old_str: "<h1>Team notes", new_str: "<h1>Hello notes" }] });
  assert.equal(v3.status, "ready", JSON.stringify(v3));
  await expect(panel.locator(".notice")).toContainText("Version 3 is live now.", { timeout: 15000 });
  await panel.getByRole("button", { name: "Load it" }).click();
  await expect(frame.getByRole("heading", { name: /Hello notes/ })).toBeVisible();

  // Logs: the backend's own output.
  await panel.getByRole("button", { name: /^Logs/ }).click();
  await expect(panel.locator(".console").first()).toContainText("notes listening");
  await page.screenshot({ path: "test-results/apps-logs.png" });
  // The backend did not change since version 1, so neither version rebuilt it.
  assert.match((await tool("get_app", { id, logs: true, version: 3 })).logs.build_log, /reused that build/);
  const withLogs = await tool("get_app", { id, logs: true, files: ["src/App.tsx"] });
  assert.equal(withLogs.logs.backend.state, "running");
  assert.match(withLogs.contents["src/App.tsx"], /Hello notes/);

  // Settings: a secret reaches the backend's environment and is never shown again.
  await panel.getByRole("button", { name: "Settings" }).click();
  await panel.getByLabel("Variable name").fill("API_TOKEN");
  await panel.getByLabel("Value").fill("s3cret");
  await panel.getByRole("button", { name: "Set", exact: true }).click();
  await expect(panel.locator(".env-row")).toContainText("API_TOKEN");
  // Storage: the disk the versions and the backend's data take.
  await expect(panel.getByText(/^Backend data: .+ in \d+ files\./)).toBeVisible();
  await expect(panel.getByText(/Versions: 3 keep their files/)).toBeVisible();
  const got = await tool("get_app", { id });
  assert.deepEqual(got.env, ["API_TOKEN"]);
  assert.ok(!JSON.stringify(got).includes("s3cret"));

  // Share: a link that follows live, on the app's own hostname.
  await panel.getByRole("button", { name: "Share" }).click();
  await page.getByRole("button", { name: "Link that follows live" }).click();
  const shareURL = await page.locator("#share-list input").first().inputValue();
  assert.match(shareURL, new RegExp(`^http://[a-z2-7]{16}\\.localhost:${port}/p/`));
  // The link's QR code opens large over the share panel and closes without closing it.
  await page.locator("#share-list").getByRole("button", { name: "QR code" }).first().click();
  const qr = page.locator("dialog.qr-overlay");
  await expect(qr.getByRole("img", { name: `QR code for ${shareURL}` })).toBeVisible();
  await expect(qr.locator(".qr-url")).toHaveText(shareURL);
  assert.ok((await qr.boundingBox()).width > 600, "the QR overlay fills most of the screen");
  await page.keyboard.press("Escape");
  await expect(qr).toBeHidden();
  await expect(page.locator("#share-dialog")).toBeVisible();
  await page.locator("#share-dialog [data-close]").click();
  const visitor = await browser.newPage({ viewport: { width: 390, height: 844 } });
  visitor.on("pageerror", (e) => errors.push("visitor: " + e.message));
  await visitor.goto(shareURL);
  await expect(visitor.getByRole("heading", { name: /Hello notes/ })).toBeVisible();
  await expect(visitor.getByText("Buy milk")).toBeVisible();
  await visitor.getByLabel("New note").fill("From outside");
  await visitor.getByRole("button", { name: "Add note" }).click();
  await expect(visitor.getByText("From outside")).toBeVisible();
  await visitor.screenshot({ path: "test-results/apps-share-mobile.png" });
  // There the page keeps its origin: localStorage, cookies and IndexedDB survive a reload.
  await visitor.evaluate(
    () =>
      new Promise((resolve, reject) => {
        localStorage.setItem("seen", "yes");
        document.cookie = "visit=1; Path=/; SameSite=Lax";
        const req = indexedDB.open("kept", 1);
        req.onupgradeneeded = () => req.result.createObjectStore("s");
        req.onsuccess = () => (req.result.close(), resolve());
        req.onerror = () => reject(req.error);
      }),
  );
  await visitor.reload();
  await expect(visitor.getByText("From outside")).toBeVisible();
  assert.deepEqual(await visitor.evaluate(() => Promise.all([localStorage.getItem("seen"), document.cookie, indexedDB.databases().then((d) => d.map((x) => x.name))])), ["yes", "visit=1", ["kept"]]);
  // Old links on the share hostname move to the app's own.
  await visitor.goto(shareOrigin + new URL(shareURL).pathname);
  await expect(visitor).toHaveURL(shareURL);
  // Another app's link is another origin, with none of it.
  const other = await tool("deploy_app", { name: "Other", files: { "index.html": "<!doctype html><html><head><title>Other</title></head><body><h1>Other app</h1></body></html>" } });
  assert.equal(other.status, "ready", JSON.stringify(other));
  const otherLink = await tool("manage_app", { id: other.id, op: "share", version: 0 });
  assert.notEqual(new URL(otherLink.url).host, new URL(shareURL).host);
  await visitor.goto(otherLink.url);
  await expect(visitor.getByRole("heading", { name: "Other app" })).toBeVisible();
  assert.deepEqual(await visitor.evaluate(() => [localStorage.getItem("seen"), document.cookie]), [null, ""]);
  // The share hostname serves nothing else.
  assert.equal((await fetch(shareOrigin + "/api/apps")).status, 404);
  assert.equal((await fetch(shareOrigin + `/run/${id}/live/`)).status, 404);

  // A read-only link: the page hides writing, and the backend never sees a write.
  const ro = await tool("manage_app", { id, op: "share", version: 0, read_only: true });
  await visitor.goto(ro.url);
  await expect(visitor.getByText("From outside")).toBeVisible();
  assert.equal(await visitor.evaluate(() => localStorage.getItem("seen")), "yes"); // the same app's origin
  await expect(visitor.getByLabel("New note")).toHaveCount(0);
  const status = await visitor.evaluate(() => fetch("api/notes", { method: "POST", headers: { "Content-Type": "application/json" }, body: '{"text":"sneaky"}' }).then((r) => r.status));
  assert.equal(status, 403);

  // With sign-in on, the hub's frame still loads the app with a session (the frame's request
  // carries the SameSite=Lax cookie), while the backend refuses anyone not signed in. The
  // agent needs a token from now on.
  await page.evaluate(() => fetch("/api/auth/password", { method: "PUT", headers: { "Content-Type": "application/json" }, body: '{"password":"correct horse"}' }));
  assert.equal((await fetch(`${base}/run/${id}/live/api/notes`)).status, 401, "backend without sign-in");
  await page.goto(`${base}/apps/${id}`);
  await frame.getByLabel("New note").fill("Signed in");
  await frame.getByRole("button", { name: "Add note" }).click();
  await expect(frame.getByText("Signed in")).toBeVisible();
  await assert.rejects(client.listTools());
  await client.close().catch(() => {});
  const { token } = await page.evaluate(() => fetch("/api/auth/tokens", { method: "POST", headers: { "Content-Type": "application/json" }, body: '{"name":"apps test"}' }).then((r) => r.json()));
  client = new Client({ name: "apps-test", version: "1" });
  await client.connect(new StreamableHTTPClientTransport(new URL(base + "/mcp"), { requestInit: { headers: { Authorization: `Bearer ${token}` } } }));
  // Public links never ask.
  await visitor.goto(shareURL);
  await expect(visitor.getByText("Signed in")).toBeVisible();

  // Mobile hub.
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({ path: "test-results/apps-tab-mobile.png" });
  await page.getByRole("tab", { name: "Apps" }).click();
  await page.screenshot({ path: "test-results/apps-home-mobile.png", fullPage: true });
  await page.setViewportSize({ width: 1440, height: 920 });

  // A plain form posting to the backend carries the session too (the runtime tags its action);
  // the template's backend answers a form body with its own 400, not shartifacts' 403.
  await page.getByRole("tab", { name: /Team notes/ }).click();
  await frame.locator("body").evaluate(() => {
    const f = document.createElement("form");
    f.method = "post";
    f.action = "api/notes";
    f.innerHTML = '<input name="text" value="form">';
    document.body.append(f);
    f.requestSubmit();
  });
  await expect(frame.locator("body")).toContainText("send {");

  // The artifacts workspace lives on at /artifacts, a click away.
  await page.getByRole("link", { name: "Artifacts", exact: true }).click();
  await expect(page.getByRole("heading", { name: "A home for things you make." })).toBeVisible();
  // Apps sits in the menu at the foot of the sidebar.
  await page.locator("#toggle-sidebar-menu").click();
  await page.locator("#to-apps").click();
  await expect(page.getByRole("heading", { name: "Apps that run." })).toBeVisible();

  // Close the tab, delete with confirmation.
  await page.getByRole("button", { name: "Close Team notes" }).click();
  await expect(page.getByRole("tab", { name: /Team notes/ })).toHaveCount(0);
  const ask = await tool("manage_app", { id, op: "delete" });
  await tool("manage_app", { id, op: "delete", confirm: ask.confirm });
  assert.equal((await fetch(`${base}/run/${id}/live/`)).status, 404);

  assert.deepEqual(errors, [], "browser errors");
  console.log("PASS: apps hub — MCP get_app (guide, list, files, logs) / deploy_app (create, edit) / manage_app (call, share, delete), launcher, tabs (open, restore, switch, close), sandboxed frontend calling its Go backend (fetch and plain forms), rollback, live-deploy notice, logs drawer, secrets, public links (full and read-only) on the app's own hostname with persistent localStorage, cookies and IndexedDB kept apart from other apps, localStorage kept by the hub, mobile layouts, artifacts workspace at /artifacts, and sign-in (app sessions only for signed-in frames, backends closed to everyone else, agent tokens, public links unaffected).");
} finally {
  if (client) await client.close();
  if (browser) await browser.close();
  server.kill("SIGTERM");
  await new Promise((r) => server.once("exit", r));
  await rm(dir, { recursive: true, force: true });
}
