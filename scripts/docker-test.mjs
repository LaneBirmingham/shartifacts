import { spawnSync } from "node:child_process";
import assert from "node:assert/strict";
const tag = "shartifacts:verify",
  name = `shartifacts-test-${process.pid}`,
  volume = `${name}-data`,
  port = 28000 + Math.floor(Math.random() * 10000),
  base = `http://127.0.0.1:${port}`;
function docker(args) {
  const p = spawnSync("docker", args, { encoding: "utf8" });
  assert.equal(p.status, 0, p.stderr);
  return p.stdout.trim();
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function wait() {
  for (let i = 0; i < 60; i++) {
    try {
      if ((await fetch(base + "/healthz")).ok) return;
    } catch {}
    await sleep(250);
  }
  throw new Error("Container did not become healthy");
}
async function api(route, body) {
  const r = await fetch(base + route, {
    method: body ? "POST" : "GET",
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  });
  assert.ok(r.ok, await r.clone().text());
  return r.json();
}
function run() {
  docker([
    "run",
    "-d",
    "--name",
    name,
    "-p",
    `127.0.0.1:${port}:8080`,
    "-v",
    `${volume}:/data`,
    "--cap-drop",
    "ALL",
    "--security-opt",
    "no-new-privileges:true",
    tag,
  ]);
}
try {
  docker(["volume", "create", volume]);
  run();
  await wait();
  assert.equal(docker(["exec", name, "id", "-u"]), "10001");
  const a = await api("/api/artifacts", {
    title: "Container counter",
    kind: "react",
    content: `import React,{useState} from 'react'; export default function App(){const [n,set]=useState(0);return <button onClick={()=>set(n+1)}>{n}</button>}`,
  });
  const preview = await fetch(`${base}/preview/${a.id}`);
  const rendered = await preview.text();
  assert.ok(rendered.includes("ArtifactModule.default"));
  assert.ok(!rendered.includes('id="error">'));
  assert.ok(
    preview.headers
      .get("content-security-policy")
      .includes("sandbox allow-scripts"),
  );
  const vendor = await fetch(base + "/vendor/react.js");
  assert.ok(vendor.ok);
  assert.ok((await vendor.text()).length > 100000);
  const health = docker([
    "exec",
    name,
    "wget",
    "-q",
    "-O",
    "-",
    "http://127.0.0.1:8080/healthz",
  ]);
  assert.equal(JSON.parse(health).status, "ok");
  const mediaBytes = Buffer.from("persistent media content");
  const upload = await fetch(base + "/api/media", {
    method: "POST",
    headers: { "Content-Type": "video/mp4" },
    body: mediaBytes,
  });
  assert.equal(upload.status, 201);
  const media = await upload.json();
  docker(["stop", name]);
  docker(["rm", name]);
  run();
  await wait();
  const saved = await api("/api/artifacts/" + a.id);
  assert.equal(saved.revisions[0].title, "Container counter");
  assert.deepEqual(
    Buffer.from(await (await fetch(base + media.url)).arrayBuffer()),
    mediaBytes,
  );
  console.log(
    "PASS: Docker starts without root/capabilities, serves bundled assets, compiles React on Linux, and persists artifacts and media across container recreation.",
  );
} finally {
  spawnSync("docker", ["rm", "-f", name], { stdio: "ignore" });
  spawnSync("docker", ["volume", "rm", volume], { stdio: "ignore" });
}
