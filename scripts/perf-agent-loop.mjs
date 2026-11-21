// The whole loop with a real agent: a person gives feedback, Claude Code (headless, connected
// to an isolated shartifacts over MCP) makes the change, and a browser on the person's side
// watches for it to appear. It records when each tool call started and finished, so the time
// the model spends thinking separates from the time shartifacts spends.
//
//   node scripts/perf-agent-loop.mjs [scenario…]   artifact-chat artifact-comment app-frontend app-fullstack
//   PERF_RUNS=3 PERF_MODEL=sonnet node scripts/perf-agent-loop.mjs app-frontend
//
// It needs the claude CLI signed in. It runs with project settings only from an empty
// directory, so the user's hooks and MCP servers stay out of it, and with no built-in tools.
import { chromium } from "@playwright/test";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { spawn, spawnSync } from "node:child_process";
import { mkdtemp, mkdir, writeFile, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import assert from "node:assert/strict";

const scenarios = process.argv.slice(2).length ? process.argv.slice(2) : ["artifact-chat", "artifact-comment", "app-frontend", "app-fullstack"];
const RUNS = Number(process.env.PERF_RUNS || 3);
const MODEL = process.env.PERF_MODEL || "";
const now = () => performance.now();
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const fmt = (ms) => (ms == null ? "—" : ms >= 1000 ? (ms / 1000).toFixed(1) + "s" : ms.toFixed(0) + "ms");

const dir = await mkdtemp(path.join(tmpdir(), "shartifacts-agentloop-"));
const binary = path.join(dir, "shartifacts");
assert.equal(spawnSync("go", ["build", "-o", binary, "."], { stdio: "inherit" }).status, 0);
const port = 18000 + Math.floor(Math.random() * 10000);
const base = `http://127.0.0.1:${port}`;
const server = spawn(binary, ["-listen", `127.0.0.1:${port}`, "-data", path.join(dir, "data"), "-share-listen", "off"], { stdio: ["ignore", "ignore", "pipe"] });
server.stderr.on("data", () => {});
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
const client = new Client({ name: "seed", version: "1" });
await client.connect(new StreamableHTTPClientTransport(new URL(base + "/mcp")));
const tool = async (name, args = {}) => {
  const res = await client.callTool({ name, arguments: args });
  assert.ok(!res.isError, `${name}: ${JSON.stringify(res.content).slice(0, 1000)}`);
  return res.structuredContent ?? res.content[0].text;
};
const browser = await chromium.launch({
  ...(process.env.CHROME_PATH
    ? { executablePath: process.env.CHROME_PATH }
    : process.platform === "darwin"
      ? { executablePath: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" }
      : {}),
});
const cwd = path.join(dir, "agent");
await mkdir(cwd);
// Warm the Go build cache once, as a server that has deployed before would have it.
await tool("deploy_app", { name: "warm-up", template: "notes" });

const packing = `<!doctype html><html><head><meta charset="utf-8"><title>Packing list</title><style>body{font:16px system-ui;max-width:40rem;margin:2rem auto}li{margin:.4rem 0}</style></head><body>
<h1>Packing list</h1>
<ul>
<li><label><input type="checkbox"> Passport</label></li>
<li><label><input type="checkbox"> Phone charger</label></li>
<li><label><input type="checkbox"> Toothbrush</label></li>
<li><label><input type="checkbox"> Rain jacket</label></li>
</ul>
<script>document.querySelectorAll('input').forEach(i=>i.onchange=()=>i.parentElement.style.opacity=i.checked?.4:1)</script>
</body></html>`;

// Each scenario seeds a fresh artifact or app, opens it the way the person would, and says
// what the agent is told and what the person should then see.
const setups = {
  async "artifact-chat"() {
    const { id } = await tool("create_artifact", { title: "Packing list", kind: "html", content: packing });
    return {
      url: `${base}/a/${id}`,
      ready: "Packing list",
      expect: ["Weekend away", "Sunscreen"],
      prompt: `In shartifacts, artifact ${id} is a packing list I have open. Feedback: rename the heading to "Weekend away" and add a "Sunscreen" item to the list. Change that artifact in place, then reply "done".`,
    };
  },
  async "artifact-comment"() {
    const { id } = await tool("create_artifact", { title: "Packing list", kind: "html", content: packing });
    return {
      url: `${base}/a/${id}`,
      ready: "Packing list",
      expect: ["Weekend away", "Sunscreen"],
      comment: { id, body: 'Please rename the heading to "Weekend away" and add a "Sunscreen" item.' },
      prompt: `You are iterating on shartifacts artifact ${id} with a person who leaves comments on it. Wait for their feedback with watch_artifacts (ids ["${id}"], kinds ["comments"], wait_seconds 50; call it again if nothing arrives). When a comment arrives, read it with artifact_comments, make the change to the artifact, resolve the comment, then reply "done".`,
    };
  },
  async "app-frontend"() {
    const a = await tool("deploy_app", { name: "Notes", template: "notes" });
    return {
      url: `${base}/apps/${a.id}`,
      ready: "React in front",
      app: a.id,
      expect: ["Family notes", "Everything the house needs to remember."],
      prompt: `In shartifacts, app ${a.id} (Notes) is open in front of me. Feedback: change the heading from "Notes" to "Family notes", and the line under it to "Everything the house needs to remember." Ship it live, then reply "done".`,
    };
  },
  async "app-fullstack"() {
    const a = await tool("deploy_app", { name: "Notes", template: "notes" });
    for (const text of ["Milk", "Bins out Tuesday"]) await tool("manage_app", { id: a.id, op: "call", method: "POST", path: "/api/notes", body: JSON.stringify({ text }) });
    return {
      url: `${base}/apps/${a.id}`,
      ready: "React in front",
      app: a.id,
      expect: ["Total notes: 2"],
      prompt: `In shartifacts, app ${a.id} (Notes) is open in front of me. Feedback: under the heading, show "Total notes: N", where N comes from a new backend endpoint GET /api/stats that returns {"count": N}. Ship it live, then reply "done".`,
    };
  },
};

async function frameHas(page, texts) {
  for (const f of page.frames()) {
    try {
      if (await f.evaluate((ts) => !!document.body && ts.every((t) => document.body.innerText.includes(t)), texts)) return true;
    } catch {}
  }
  return false;
}

// runAgent starts headless Claude Code and timestamps each stream-json event as it arrives.
function runAgent(prompt) {
  const mcp = JSON.stringify({ mcpServers: { shartifacts: { type: "http", url: `${base}/mcp` } } });
  const args = ["-p", prompt, "--setting-sources", "project", "--tools", "", "--strict-mcp-config", "--mcp-config", mcp, "--allowedTools", "mcp__shartifacts", "--no-session-persistence", "--output-format", "stream-json", "--verbose", ...(MODEL ? ["--model", MODEL] : [])];
  const t0 = now();
  const proc = spawn("claude", args, { cwd, stdio: ["ignore", "pipe", "pipe"] });
  const events = [];
  let buf = "";
  proc.stdout.on("data", (b) => {
    buf += b;
    let i;
    while ((i = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, i);
      buf = buf.slice(i + 1);
      if (!line.trim()) continue;
      try {
        events.push({ t: now() - t0, ...JSON.parse(line) });
      } catch {}
    }
  });
  let stderr = "";
  proc.stderr.on("data", (b) => (stderr += b));
  const done = new Promise((resolve) => proc.on("exit", (code) => resolve({ code, t: now() - t0, stderr })));
  return { t0, events, done, proc };
}

// timeline turns the stream into tool calls with start and end times.
function timeline(events) {
  const calls = [];
  const byId = new Map();
  for (const e of events) {
    for (const c of e.message?.content || []) {
      if (e.type === "assistant" && c.type === "tool_use") {
        const call = { name: c.name.replace("mcp__shartifacts__", ""), start: e.t, input: c.input };
        calls.push(call);
        byId.set(c.id, call);
      }
      if (e.type === "user" && c.type === "tool_result" && byId.has(c.tool_use_id)) {
        const call = byId.get(c.tool_use_id);
        call.end = e.t;
        call.error = !!c.is_error;
      }
    }
  }
  return calls;
}

const all = [];
for (const name of scenarios) {
  for (let run = 1; run <= RUNS; run++) {
    const s = await setups[name]();
    const page = await browser.newPage({ viewport: { width: 1280, height: 860 } });
    await page.goto(s.url);
    for (let i = 0; !(await frameHas(page, [s.ready])); i++) {
      if (i > 400) throw new Error("page never loaded");
      await sleep(25);
    }
    const agent = runAgent(s.prompt);
    let feedbackAt = null, noticeAt = null, visibleAt = null;
    const deadline = now() + 300000;
    let exited = null;
    agent.done.then((r) => (exited = r));
    while (now() < deadline) {
      const rel = now() - agent.t0;
      // The person comments once the agent is listening for it.
      if (s.comment && feedbackAt == null && timeline(agent.events).some((c) => c.name === "watch_artifacts")) {
        await sleep(1000);
        const r = await fetch(`${base}/api/artifacts/${s.comment.id}/comments`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ body: s.comment.body }) });
        assert.ok(r.ok);
        feedbackAt = now() - agent.t0;
      }
      // An attentive person clicks the hub's "Load it" as soon as it shows.
      if (s.app) {
        const notice = page.locator(`#panel-${s.app} .notice`);
        if (await notice.isVisible().catch(() => false)) {
          noticeAt ??= rel;
          await page.locator(`#panel-${s.app} .load-live`).click().catch(() => {});
        }
      }
      if (await frameHas(page, s.expect)) {
        visibleAt = now() - agent.t0;
        break;
      }
      if (exited && now() - agent.t0 > exited.t + 15000) break;
      await sleep(25);
    }
    const r = await agent.done;
    await page.close();
    const calls = timeline(agent.events);
    const result = agent.events.find((e) => e.type === "result") || {};
    const init = agent.events.find((e) => e.type === "system" && e.subtype === "init");
    const toolMs = calls.reduce((n, c) => n + ((c.end ?? c.start) - c.start), 0);
    const watchMs = calls.filter((c) => c.name === "watch_artifacts").reduce((n, c) => n + ((c.end ?? c.start) - c.start), 0);
    // The last write the person can see: the final update or deploy before it appeared.
    const writes = calls.filter((c) => ["update_artifact", "deploy_app", "create_artifact"].includes(c.name) && c.end != null && (visibleAt == null || c.end <= visibleAt + 50));
    const lastWrite = writes[writes.length - 1];
    const rec = {
      scenario: name,
      run,
      model: init?.model || MODEL || "default",
      ok: visibleAt != null,
      startup: init?.t,
      feedbackAt,
      visibleAt,
      feedbackToVisible: visibleAt != null ? visibleAt - (feedbackAt ?? 0) : null,
      exit: r.t,
      tools: calls.map((c) => ({ name: c.name, start: Math.round(c.start), ms: c.end != null ? Math.round(c.end - c.start) : null, error: c.error })),
      toolMs: toolMs - watchMs,
      watchMs,
      lastWriteEnd: lastWrite?.end,
      writeToVisible: lastWrite && visibleAt != null ? visibleAt - lastWrite.end : null,
      noticeAt,
      turns: result.num_turns,
      apiMs: result.duration_api_ms,
      costUSD: result.total_cost_usd,
      result: String(result.result || "").slice(0, 200),
      stderr: r.code ? r.stderr.slice(0, 500) : undefined,
    };
    rec.modelMs = rec.visibleAt != null ? rec.visibleAt - (rec.startup ?? 0) - rec.toolMs - watchMs - (rec.writeToVisible ?? 0) : null;
    all.push(rec);
    console.log(
      `${name} #${run} [${rec.model}] ${rec.ok ? "visible" : "NOT VISIBLE"} ` +
        `feedback→visible ${fmt(rec.feedbackToVisible)} | startup ${fmt(rec.startup)}, model ${fmt(rec.modelMs)}, tools ${fmt(rec.toolMs)}, write→visible ${fmt(rec.writeToVisible)} | ` +
        `${calls.map((c) => `${c.name}${c.error ? "!" : ""}(${fmt(c.end != null ? c.end - c.start : null)})`).join(" → ")} | turns ${rec.turns}, $${rec.costUSD?.toFixed(3)} | agent exit ${fmt(rec.exit)}`,
    );
  }
}

await mkdir("test-results/perf", { recursive: true });
const out = `test-results/perf/agent-loop${MODEL ? "-" + MODEL : ""}.json`;
let previous = [];
try {
  previous = JSON.parse(await readFile(out, "utf8"));
} catch {}
await writeFile(out, JSON.stringify([...previous, ...all], null, 2));
console.log(`raw: ${out}`);
await browser.close();
await client.close();
process.exit(0);
