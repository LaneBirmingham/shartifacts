// Performance test of the agent iteration loop: how long it takes from an agent deciding on a
// change to a person seeing it. It times each stage separately (the MCP calls, the server's
// save or build, the browser noticing, the page rendering) for artifacts and apps, then how
// save latency grows with history and under concurrent agents.
//
//   node scripts/perf-test.mjs [section…]     sections: mcp browser apps history concurrency
//   PERF_N=10 node scripts/perf-test.mjs       iterations for the browser and app loops
//
// It builds its own binary and data directory, like the other suites, and writes the raw
// samples to test-results/perf/results.json.
import { chromium } from "@playwright/test";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { spawn, spawnSync } from "node:child_process";
import { mkdtemp, mkdir, writeFile, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";

const sections = new Set(process.argv.slice(2).length ? process.argv.slice(2) : ["mcp", "browser", "apps", "history", "concurrency"]);
const N = Number(process.env.PERF_N || 12);
const now = () => performance.now();
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const jitter = (max) => sleep(Math.random() * max);
const results = { started: new Date().toISOString(), host: process.platform + "/" + process.arch, n: N };

function stats(xs) {
  const s = [...xs].sort((a, b) => a - b);
  const q = (p) => s[Math.min(s.length - 1, Math.floor(p * s.length))];
  return { n: s.length, min: s[0], p50: q(0.5), p90: q(0.9), max: s[s.length - 1], mean: s.reduce((a, b) => a + b, 0) / s.length };
}
const fmt = (ms) => (ms >= 1000 ? (ms / 1000).toFixed(2) + "s" : ms.toFixed(0) + "ms");
function row(label, xs) {
  const st = stats(xs);
  console.log(`  ${label.padEnd(44)} p50 ${fmt(st.p50).padStart(7)}  p90 ${fmt(st.p90).padStart(7)}  min ${fmt(st.min).padStart(7)}  max ${fmt(st.max).padStart(7)}  (n=${st.n})`);
  return st;
}

// ---- server ---------------------------------------------------------------------------

const dir = await mkdtemp(path.join(tmpdir(), "shartifacts-perf-"));
const binary = path.join(dir, "shartifacts");
assert.equal(spawnSync("go", ["build", "-o", binary, "."], { stdio: "inherit" }).status, 0);
const port = 18000 + Math.floor(Math.random() * 10000);
const base = `http://127.0.0.1:${port}`;
const dataDir = path.join(dir, "data");
const server = spawn(binary, ["-listen", `127.0.0.1:${port}`, "-data", dataDir, "-share-listen", "off"], { stdio: ["ignore", "ignore", "pipe"] });
server.stderr.on("data", (b) => process.env.PERF_VERBOSE && process.stderr.write(b));
// Backends outlive a stopped server (main returns before Runtime.Close), so end them too,
// whether the run finishes or throws.
process.on("exit", () => {
  server.kill();
  spawnSync("pkill", ["-f", dir]);
});
for (let i = 0; ; i++) {
  try {
    if ((await fetch(base + "/healthz")).ok) break;
  } catch {}
  if (i > 100) throw new Error("server did not start");
  await sleep(100);
}

async function connect(name) {
  const c = new Client({ name, version: "1" });
  const t = now();
  await c.connect(new StreamableHTTPClientTransport(new URL(base + "/mcp?mode=tools")));
  return { client: c, connectMs: now() - t };
}
const { client, connectMs } = await connect("perf");
const tool = async (name, args = {}, c = client) => {
  const res = await c.callTool({ name, arguments: args });
  assert.ok(!res.isError, `${name}: ${JSON.stringify(res.content).slice(0, 2000)}`);
  return res.structuredContent ?? res.content[0].text;
};
const latest = (a) => a.revisions[a.revisions.length - 1].version;
const timedTool = async (name, args, c) => {
  const t = now();
  const r = await tool(name, args, c);
  return [now() - t, r];
};

// ---- fixtures -------------------------------------------------------------------------

const htmlSmall = `<!doctype html><html><head><style>body{font:16px system-ui;padding:2rem}li{margin:.3rem 0}</style></head><body>
<h1>Packing list</h1><p id="mark">MARK-0</p><ul>${Array.from({ length: 40 }, (_, i) => `<li><label><input type="checkbox"> Item ${i}</label></li>`).join("")}</ul>
<script>document.querySelectorAll('input').forEach(i=>i.onchange=()=>i.parentElement.style.opacity=i.checked?.4:1)</script></body></html>`;

const reactMedium = `import { useState, useMemo } from "react";
import { LineChart, Line, XAxis, YAxis, Tooltip, ResponsiveContainer } from "recharts";
import { Search } from "lucide-react";
const rows = ${JSON.stringify(Array.from({ length: 120 }, (_, i) => ({ day: i, kwh: Math.round(20 + 10 * Math.sin(i / 7) + (i % 5)), cost: +(i * 0.31).toFixed(2) })))};
export default function Usage() {
  const [q, setQ] = useState("");
  const shown = useMemo(() => rows.filter((r) => String(r.day).includes(q)), [q]);
  return (
    <div className="p-6 font-sans">
      <h1 className="text-2xl font-semibold">Energy usage</h1>
      <p id="mark">MARK-0</p>
      <label className="flex gap-2 items-center"><Search size={16} /><input value={q} onChange={(e) => setQ(e.target.value)} className="border px-2" /></label>
      <div style={{ height: 240 }}><ResponsiveContainer><LineChart data={shown}><XAxis dataKey="day" /><YAxis /><Tooltip /><Line dataKey="kwh" /></LineChart></ResponsiveContainer></div>
      <table><tbody>{shown.slice(0, 30).map((r) => <tr key={r.day}><td>{r.day}</td><td>{r.kwh}</td><td>{r.cost}</td></tr>)}</tbody></table>
    </div>
  );
}
`;

const markdown = `# Release notes\n\nMARK-0\n\n${Array.from({ length: 60 }, (_, i) => `- Change ${i}: fixed a thing in **module ${i % 7}** with \`code\``).join("\n")}\n\n| a | b |\n|---|---|\n| 1 | 2 |\n`;

// A large React artifact (≈ 60 KB) for the history test: a realistic dashboard with its data inline.
const reactLarge = reactMedium.replace(/const rows = .*;/, `const rows = ${JSON.stringify(Array.from({ length: 1200 }, (_, i) => ({ day: i, kwh: 20 + (i % 13), cost: +(i * 0.31).toFixed(2), site: "site-" + (i % 9) })))};`);

// ---- browser helpers --------------------------------------------------------------------

let browser;
async function launch() {
  browser ??= await chromium.launch({
    ...(process.env.CHROME_PATH
      ? { executablePath: process.env.CHROME_PATH }
      : process.platform === "darwin"
        ? { executablePath: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" }
        : {}),
  });
  return browser;
}

// waitForText scans every frame of the page, the sandboxed out-of-process ones included,
// until one shows text, and returns when it did.
async function waitForText(page, text, timeout = 30000) {
  const deadline = now() + timeout;
  while (now() < deadline) {
    for (const f of page.frames()) {
      try {
        if (await f.evaluate((t) => !!document.body && document.body.innerText.includes(t), text)) return now();
      } catch {}
    }
    await sleep(15);
  }
  throw new Error(`"${text}" never appeared`);
}

// requestLog records when the page first asked for a URL matching a pattern after a moment.
function requestLog(page) {
  const seen = [];
  page.on("request", (r) => seen.push({ t: now(), url: r.url() }));
  return (re, after) => seen.find((s) => s.t >= after && re.test(s.url))?.t;
}

// ---- 1. MCP primitives ---------------------------------------------------------------

if (sections.has("mcp")) {
  console.log("\n== MCP primitives (local loopback, one agent) ==");
  const out = (results.mcp = { connectMs });
  console.log(`  connect (initialize)                        ${fmt(connectMs)}`);
  {
    const t = now();
    await client.listTools();
    out.listToolsMs = now() - t;
    console.log(`  tools/list                                  ${fmt(out.listToolsMs)}`);
  }
  const kinds = { html: htmlSmall, react: reactMedium, markdown };
  const ids = {};
  for (const [kind, content] of Object.entries(kinds)) {
    const [ms, r] = await timedTool("create_artifact", { title: `perf ${kind}`, kind, content });
    ids[kind] = r.id;
    out[`create_${kind}`] = ms;
    console.log(`  create_artifact ${kind.padEnd(28)}${fmt(ms)}  (${(content.length / 1024).toFixed(1)} KB)`);
  }
  const reps = 30;
  for (const [kind, id] of Object.entries(ids)) {
    const reads = [], writes = [];
    let v = 1;
    for (let i = 1; i <= reps; i++) {
      const [rm, a] = await timedTool("get_artifact", { id });
      reads.push(rm);
      const [wm, u] = await timedTool("update_artifact", { id, base_version: latest(a), edits: [{ old_str: `MARK-${i - 1}`, new_str: `MARK-${i}` }] });
      writes.push(wm);
      v = u.version;
    }
    out[`get_${kind}`] = row(`get_artifact ${kind}`, reads);
    out[`update_${kind}`] = row(`update_artifact edits ${kind}`, writes);
  }
  {
    const xs = [];
    for (let i = 0; i < reps; i++) xs.push((await timedTool("list_artifacts", {}))[0]);
    out.list = row("list_artifacts", xs);
  }
  {
    const xs = [];
    for (let i = 0; i < reps; i++) xs.push((await timedTool("artifact_comments", { id: ids.html, op: "list" }))[0]);
    out.comments = row("artifact_comments list", xs);
  }
  {
    // A person comments; how long until an agent waiting in watch_artifacts hears of it.
    const xs = [];
    let { cursor } = await tool("watch_artifacts", { ids: [ids.html] });
    for (let i = 0; i < 20; i++) {
      const waiting = tool("watch_artifacts", { ids: [ids.html], after: cursor, kinds: ["comments"], wait_seconds: 20 });
      await sleep(150);
      const t0 = now();
      const r = await fetch(`${base}/api/artifacts/${ids.html}/comments`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ body: `Make the heading bigger (${i})` }) });
      assert.ok(r.ok);
      const w = await waiting;
      xs.push(now() - t0);
      assert.ok(w.events.length > 0, JSON.stringify(w));
      cursor = w.cursor;
    }
    out.watchWake = row("comment → watch_artifacts wakes", xs);
  }
  results.ids = ids;
}

// ---- 2. Artifact: update → visible in the person's browser -----------------------------

if (sections.has("browser")) {
  console.log("\n== Artifact update → visible in the browser ==");
  const b = await launch();
  const out = (results.browser = {});
  const surfaces = { workspace: (id) => `${base}/a/${id}`, view: (id) => `${base}/view/${id}` };
  const kinds = { html: htmlSmall, react: reactMedium, markdown };
  for (const [kind, content] of Object.entries(kinds)) {
    for (const [surface, url] of Object.entries(surfaces)) {
      const { id } = await tool("create_artifact", { title: `visible ${kind} ${surface}`, kind, content });
      const page = await b.newPage({ viewport: { width: 1280, height: 860 } });
      const requested = requestLog(page);
      await page.goto(url(id));
      await waitForText(page, "MARK-0");
      const s = { mcp: [], notice: [], render: [], total: [] };
      for (let i = 1; i <= N; i++) {
        // Land the update at a random point in the browser's poll cycle.
        await jitter(3000);
        const t0 = now();
        const a = await tool("get_artifact", { id });
        await tool("update_artifact", { id, base_version: latest(a), edits: [{ old_str: `MARK-${i - 1}`, new_str: `MARK-${i}` }] });
        const t1 = now();
        const tv = await waitForText(page, `MARK-${i}`);
        const tp = requested(/\/preview\//, t1) ?? tv;
        s.mcp.push(t1 - t0);
        s.notice.push(tp - t1);
        s.render.push(tv - tp);
        s.total.push(tv - t0);
      }
      console.log(` ${kind} in ${surface === "workspace" ? "/a/{id} (workspace)" : "/view/{id} (full page)"}:`);
      out[`${kind}_${surface}`] = {
        mcp: row("agent: get_artifact + update_artifact", s.mcp),
        notice: row("browser poll notices the new version", s.notice),
        render: row("preview loads and renders", s.render),
        total: row("TOTAL agent call → visible", s.total),
      };
      await page.close();
    }
  }
}

// ---- 3. Apps: deploy → visible in the hub tab ------------------------------------------

if (sections.has("apps")) {
  console.log("\n== Apps: deploy_app → live → visible in the hub ==");
  const out = (results.apps = {});
  {
    // The very first Go build on a fresh data directory has an empty build cache.
    const [ms, r] = await timedTool("deploy_app", { name: "Perf notes (cold)", template: "notes" });
    assert.equal(r.status, "ready", JSON.stringify(r));
    out.createCold = ms;
    const [ms2, r2] = await timedTool("deploy_app", { name: "Perf notes", template: "notes" });
    assert.equal(r2.status, "ready", JSON.stringify(r2));
    out.createWarm = ms2;
    out.appId = r2.id;
    console.log(`  create from template, cold Go cache         ${fmt(ms)}`);
    console.log(`  create from template, warm Go cache         ${fmt(ms2)}`);
  }
  const id = out.appId;
  const b = await launch();
  const page = await b.newPage({ viewport: { width: 1280, height: 860 } });
  await page.goto(`${base}/apps/${id}`);
  await waitForText(page, "React in front");
  const panel = page.locator(`#panel-${id}`);
  let version = (await tool("get_app", { id })).latest;
  let lede = "React in front, Go behind, data kept on the server.";
  let mark = 'os.Getenv("APP_VERSION")';

  // One loop iteration: deploy, then (when the frontend changed) wait for the hub's notice,
  // click it, and wait for the new page.
  async function iterate(kind, i) {
    const edits = [];
    const newLede = `LEDE-${kind}-${i}`;
    const newMark = `os.Getenv("APP_VERSION"), "mark": "${kind}-${i}"`;
    if (kind !== "backend") edits.push({ file: "src/App.tsx", old_str: lede, new_str: newLede });
    if (kind !== "frontend") edits.push({ file: "server/main.go", old_str: mark, new_str: newMark });
    await jitter(4000);
    const t0 = now();
    const [deployMs, d] = await timedTool("deploy_app", { id, base_version: version, merge: true, edits, note: `${kind} ${i}` });
    assert.equal(d.status, "ready", JSON.stringify(d).slice(0, 3000));
    version = d.version;
    const t1 = now();
    const sample = { deploy: deployMs };
    {
      const logs = JSON.stringify(await tool("get_app", { id, version, logs: true, tail: 40 }));
      sample.goBuild = (logs.match(/built in [0-9.]+s|reused that build/) || ["?"])[0];
    }
    if (kind !== "frontend") {
      mark = newMark;
      const [callMs, h] = await timedTool("manage_app", { id, op: "call", path: "/api/health" });
      assert.match(h.body, new RegExp(`${kind}-${i}`));
      sample.call = callMs;
    }
    if (kind !== "backend") {
      lede = newLede;
      await panel.locator(".notice", { hasText: `Version ${version} is live now.` }).waitFor({ state: "visible", timeout: 20000 });
      const t2 = now();
      await panel.locator(".load-live").click();
      const t3 = await waitForText(page, newLede);
      sample.notice = t2 - t1;
      sample.render = t3 - t2;
      sample.total = t3 - t0;
    }
    return sample;
  }

  for (const kind of ["frontend", "backend", "fullstack"]) {
    const samples = [];
    for (let i = 1; i <= N; i++) samples.push(await iterate(kind, i));
    console.log(` ${kind} change:`);
    const pick = (k) => samples.map((s) => s[k]).filter((x) => typeof x === "number");
    const o = (out[kind] = { deploy: row("deploy_app (stage, build, start, promote)", pick("deploy")) });
    o.goBuild = samples.map((s) => s.goBuild);
    console.log(`  Go backend build log: ${o.goBuild.join(", ")}`);
    if (kind !== "frontend") o.call = row("manage_app call /api/health", pick("call"));
    if (kind !== "backend") {
      o.notice = row("hub poll shows 'Version N is live now'", pick("notice"));
      o.render = row("person clicks Load it → new page rendered", pick("render"));
      o.total = row("TOTAL deploy call → visible (with click)", pick("total"));
    }
  }
  {
    // A deploy that changes nothing a build depends on: the fixed cost of a version.
    const xs = [];
    for (let i = 0; i < N; i++) {
      const [ms, d] = await timedTool("deploy_app", { id, base_version: version, merge: true, files: { "public/ping.txt": `ping ${i}` }, note: "static" });
      version = d.version;
      xs.push(ms);
    }
    out.staticOnly = row("deploy_app, one static file changed", xs);
  }
  {
    // promote=false then promote: the preview-first workflow.
    const xs = [], ps = [];
    for (let i = 0; i < 5; i++) {
      const [ms, d] = await timedTool("deploy_app", { id, base_version: version, merge: true, files: { "public/ping.txt": `staged ${i}` }, promote: false });
      version = d.version;
      xs.push(ms);
      const [pm] = await timedTool("manage_app", { id, op: "promote", version });
      ps.push(pm);
    }
    out.staged = row("deploy_app promote:false", xs);
    out.promote = row("manage_app promote (rollback/forward)", ps);
  }
  {
    const xs = [];
    for (let i = 0; i < 10; i++) xs.push((await timedTool("get_app", { id }))[0]);
    out.getApp = row("get_app {id}", xs);
  }
  await page.close();
}

// ---- 4. History: does a save get slower as an artifact gains versions? --------------------

if (sections.has("history")) {
  console.log("\n== Save latency vs history depth (≈60 KB React artifact) ==");
  const out = (results.history = { points: [] });
  const { id } = await tool("create_artifact", { title: "history", kind: "react", content: reactLarge });
  let v = 1;
  const total = Number(process.env.PERF_HISTORY || 300);
  const window = [];
  for (let i = 1; i <= total; i++) {
    const [ms, u] = await timedTool("update_artifact", { id, base_version: v, edits: [{ old_str: `MARK-${i - 1}`, new_str: `MARK-${i}` }] });
    v = u.version;
    window.push(ms);
    if (i === 1 || i % 50 === 0) {
      const t = now();
      const r = await fetch(`${base}/api/artifacts/${id}`);
      const body = await r.arrayBuffer();
      const getMs = now() - t;
      const file = (await stat(path.join(dataDir, "artifacts", `${id}.json`)).catch(() => stat(path.join(dataDir, `${id}.json`)))).size;
      const st = stats(window);
      out.points.push({ versions: v, saveP50: st.p50, saveP90: st.p90, fileBytes: file, apiGetBytes: body.byteLength, apiGetMs: getMs });
      console.log(`  v${String(v).padEnd(4)} save p50 ${fmt(st.p50).padStart(6)} p90 ${fmt(st.p90).padStart(6)}   file ${(file / 1e6).toFixed(1)} MB   GET /api/artifacts/{id} ${(body.byteLength / 1e6).toFixed(1)} MB in ${fmt(getMs)} (the /view page fetches this every 3s)`);
      window.length = 0;
    }
  }
}

// ---- 5. Concurrency: several agents at once ---------------------------------------------

if (sections.has("concurrency")) {
  console.log("\n== Concurrent agents ==");
  const out = (results.concurrency = {});
  for (const agents of [1, 4, 16]) {
    const clients = await Promise.all(Array.from({ length: agents }, (_, i) => connect(`agent-${i}`)));
    const ids = await Promise.all(clients.map(({ client: c }, i) => tool("create_artifact", { title: `conc ${agents}-${i}`, kind: "react", content: reactMedium }, c).then((r) => r.id)));
    const lat = [];
    const t = now();
    await Promise.all(
      clients.map(async ({ client: c }, k) => {
        let v = 1;
        for (let i = 1; i <= 20; i++) {
          const t0 = now();
          const a = await tool("get_artifact", { id: ids[k] }, c);
          const u = await tool("update_artifact", { id: ids[k], base_version: latest(a), edits: [{ old_str: `MARK-${i - 1}`, new_str: `MARK-${i}` }] }, c);
          lat.push(now() - t0);
          v = u.version;
        }
      }),
    );
    const wall = now() - t;
    out[`artifacts_${agents}`] = { ...row(`${agents} agents, get+update react each`, lat), throughput: (agents * 20) / (wall / 1000) };
    console.log(`  ${"".padEnd(44)} ${((agents * 20) / (wall / 1000)).toFixed(0)} updates/s overall`);
    await Promise.all(clients.map(({ client: c }) => c.close()));
  }
  {
    // Several apps deploying a frontend change at the same time.
    const apps = await Promise.all([0, 1, 2, 3].map((i) => tool("deploy_app", { name: `conc app ${i}`, template: "notes" })));
    for (const parallel of [1, 4]) {
      const lat = [];
      for (let round = 0; round < 3; round++) {
        await Promise.all(
          apps.slice(0, parallel).map(async (a, k) => {
            const cur = await tool("get_app", { id: a.id });
            const t0 = now();
            const d = await tool("deploy_app", { id: a.id, base_version: cur.latest, merge: true, files: { "src/extra.css": `.x${round}{color:red}` }, edits: round === 0 ? [{ file: "src/main.tsx", old_str: 'import "./styles.css";', new_str: 'import "./styles.css";\nimport "./extra.css";' }] : [] });
            assert.equal(d.status, "ready", JSON.stringify(d).slice(0, 2000));
            lat.push(now() - t0);
          }),
        );
      }
      out[`apps_${parallel}`] = row(`${parallel} app(s) deploying frontend at once`, lat);
    }
  }
}

await mkdir("test-results/perf", { recursive: true });
await writeFile("test-results/perf/results.json", JSON.stringify(results, null, 2));
console.log("\nraw samples: test-results/perf/results.json");
await browser?.close();
await client.close();
process.exit(0);
