// The compose shape, in containers: shartifacts with -runner-url, and the runner sidecar
// mounting only the apps directory. Deploys the notes template through it, then a probe app
// whose backend tries to escape, to check what the sandbox actually stops on this kernel.
import { spawnSync } from "node:child_process";
import assert from "node:assert/strict";

const name = `shartifacts-apps-${process.pid}`,
  runnerName = `${name}-runner`,
  net = `${name}-net`,
  port = 28000 + Math.floor(Math.random() * 10000),
  base = `http://127.0.0.1:${port}`;
// A named volume, as on a server's local disk: Landlock loses its rules on FUSE mounts such
// as Docker Desktop's shared folders, and the runner then reports backends as unconfined.
const volume = `${name}-data`;
function docker(args, ok = true) {
  const p = spawnSync("docker", args, { encoding: "utf8" });
  if (ok) assert.equal(p.status, 0, p.stderr);
  return p.stdout.trim();
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function api(route, body, method = body ? "POST" : "GET") {
  const r = await fetch(base + route, { method, headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
  const text = await r.text();
  assert.ok(r.ok, `${route}: ${r.status} ${text}`);
  return JSON.parse(text);
}
const hardening = ["--cap-drop", "ALL", "--security-opt", "no-new-privileges:true"];

const probe = `package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func main() {
	http.HandleFunc("/api/probe", func(w http.ResponseWriter, r *http.Request) {
		res := map[string]string{}
		try := func(name string, f func() error) {
			if err := f(); err != nil {
				res[name] = "denied: " + err.Error()
			} else {
				res[name] = "allowed"
			}
		}
		try("read_runner_token", func() error { _, err := os.ReadFile("/apps/.runner-token"); return err })
		try("list_other_apps", func() error { _, err := os.ReadDir("/apps"); return err })
		try("write_outside_data", func() error { return os.WriteFile("/apps/escape.txt", []byte("x"), 0o600) })
		try("write_own_data", func() error { return os.WriteFile(filepath.Join(os.Getenv("DATA_DIR"), "ok.txt"), []byte("x"), 0o600) })
		try("see_artifacts", func() error { _, err := os.ReadDir("/data"); return err })
		try("run_shell", func() error { return exec.Command("/bin/sh", "-c", "true").Run() })
		try("connect_shartifacts", func() error {
			c, err := net.DialTimeout("tcp", os.Getenv("PROBE_TARGET"), 2*time.Second)
			if err == nil {
				c.Close()
			}
			return err
		})
		json.NewEncoder(w).Encode(res)
	})
	http.ListenAndServe(net.JoinHostPort(os.Getenv("HOST"), os.Getenv("PORT")), nil)
}
`;

try {
  docker(["build", "-q", "-t", "shartifacts:verify", "."]);
  docker(["build", "-q", "--target", "runner", "-t", "shartifacts-runner:verify", "."]);
  docker(["network", "create", net]);
  docker(["volume", "create", volume]);
  docker(["run", "-d", "--name", name, "--network", net, "-p", `127.0.0.1:${port}:8080`, "--mount", `type=volume,src=${volume},dst=/data`, ...hardening, "shartifacts:verify", "-data", "/data", "-runner-url", `http://${runnerName}:8090`]);
  for (let i = 0; ; i++) {
    try {
      if ((await fetch(base + "/healthz")).ok) break;
    } catch {}
    if (i > 80) throw new Error("shartifacts did not start");
    await sleep(250);
  }
  docker(["run", "-d", "--name", runnerName, "--network", net, "--mount", `type=volume,src=${volume},dst=/apps,volume-subpath=apps`, "--memory", "1g", "--pids-limit", "512", ...hardening, "shartifacts-runner:verify"]);
  let runtime;
  for (let i = 0; ; i++) {
    runtime = (await api("/api/apps")).runtime;
    if (!runtime.error) break;
    if (i > 80) throw new Error("runner unreachable: " + runtime.error);
    await sleep(250);
  }
  assert.equal(runtime.remote, true);
  assert.match(runtime.go, /^go1\./);
  assert.equal(docker(["exec", runnerName, "id", "-u"]), "10001");
  console.log(`runner: ${runtime.go} ${runtime.os}/${runtime.arch}; sandbox: ${runtime.sandbox.detail}`);

  // The template, built by the sidecar's toolchain and served through it.
  const notes = await api("/api/apps", { name: "Notes", template: "notes" });
  assert.equal(notes.deployment.status, "ready", JSON.stringify(notes.deployment));
  const live = `/run/${notes.app.id}/live/`;
  const added = await fetch(base + live + "api/notes", { method: "POST", headers: { "Content-Type": "application/json" }, body: '{"text":"in a container"}' });
  assert.equal(added.status, 201);
  // The runner restarting loses nothing: the next request starts the backend again.
  docker(["restart", runnerName]);
  for (let i = 0; ; i++) {
    const r = await fetch(base + live + "api/notes").catch(() => null);
    if (r && r.ok) {
      assert.match(await r.text(), /in a container/);
      break;
    }
    if (i > 80) throw new Error("backend did not come back after a runner restart");
    await sleep(250);
  }

  // The probe: what can a backend reach?
  const app = await api("/api/apps", { name: "Probe", files: { "server/main.go": probe } });
  assert.equal(app.deployment.status, "ready", JSON.stringify(app.deployment));
  const id = app.app.id;
  await api(`/api/apps/${id}/env`, { vars: { PROBE_TARGET: `${name}:8080` } }, "PUT");
  const confined = await api(`/run/${id}/live/api/probe`);
  console.log("network off:", confined);
  assert.equal(confined.write_own_data, "allowed");
  assert.match(confined.see_artifacts, /denied/, "the runner must not see /data");
  assert.equal(runtime.sandbox.mechanism, "landlock", runtime.sandbox.detail);
  if (runtime.sandbox.mechanism === "landlock") {
    for (const k of ["read_runner_token", "list_other_apps", "write_outside_data", "run_shell"]) assert.match(confined[k], /denied/, k);
    if (runtime.sandbox.network) assert.match(confined.connect_shartifacts, /denied/);
  }
  // With network enabled, the same backend may connect out.
  const v2 = await api(`/api/apps/${id}/deployments`, { base_version: 1, merge: true, files: { "shartifacts.json": '{"backend":{"network":true}}' } });
  assert.equal(v2.deployment.status, "ready", JSON.stringify(v2.deployment));
  assert.equal(v2.deployment.network, true);
  const open = await api(`/run/${id}/live/api/probe`);
  console.log("network on: connect_shartifacts =", open.connect_shartifacts);
  assert.equal(open.connect_shartifacts, "allowed");

  console.log(
    `PASS: compose shape (shartifacts + runner sidecar, no root, no capabilities): Go backends build in the sidecar and serve through shartifacts, survive a runner restart; the sidecar cannot see artifacts; sandbox ${runtime.sandbox.mechanism}${runtime.sandbox.mechanism === "landlock" ? ` ABI ${runtime.sandbox.abi} blocks reading the runner token, other apps, writes outside DATA_DIR, running a shell${runtime.sandbox.network ? " and TCP connections unless the app enables network" : ""}` : ""}.`,
  );
} finally {
  spawnSync("docker", ["rm", "-f", name, runnerName], { stdio: "ignore" });
  spawnSync("docker", ["network", "rm", net], { stdio: "ignore" });
  spawnSync("docker", ["volume", "rm", volume], { stdio: "ignore" });
}
