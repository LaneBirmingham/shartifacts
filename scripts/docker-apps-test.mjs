// The compose shape, in containers: shartifacts with -runner-url, and the runner sidecar
// mounting only the apps directory. Deploys the notes templates (Go and Rust) through it,
// then probe apps whose backends (and, for Rust, build scripts) try to escape, to check what
// the sandbox actually stops on this kernel.
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

// deployed waits for a version a deploy left building (Rust builds can outlast the wait).
async function deployed(id, version) {
  for (let i = 0; ; i++) {
    const dep = (await api(`/api/apps/${id}`)).deployments.find((d) => d.version === version);
    if (dep && dep.status !== "building") return dep;
    if (i > 600) throw new Error(`version ${version} of ${id} still building`);
    await sleep(1000);
  }
}

// The Rust probe tries the same escapes twice: from its build script, which cargo runs while
// building and which reports through an environment variable baked into the binary, and from
// the running backend. It uses only the standard library, so it builds offline in seconds.
const rustAttempt = `fn attempt(r: std::io::Result<()>) -> String {
    match r {
        Ok(()) => "allowed".into(),
        Err(e) => format!("denied: {e}"),
    }
}
fn json(pairs: &[(&str, String)]) -> String {
    let fields: Vec<String> = pairs.iter().map(|(k, v)| format!("{k:?}:{v:?}")).collect();
    format!("{{{}}}", fields.join(","))
}
`;
const rustProbe = {
  "server/Cargo.toml": '[package]\nname = "probe"\nversion = "0.1.0"\nedition = "2024"\n',
  "server/build.rs": `use std::fs;
${rustAttempt}
fn main() {
    let res = json(&[
        ("read_runner_token", attempt(fs::read("/apps/.runner-token").map(|_| ()))),
        ("list_other_apps", attempt(fs::read_dir("/apps").map(|_| ()))),
        ("write_source", attempt(fs::write("src/escape.txt", "x"))),
        ("read_source", attempt(fs::read("src/main.rs").map(|_| ()))),
        ("write_own_cache", attempt(fs::write(format!("{}/probe.txt", std::env::var("CARGO_HOME").unwrap()), "x"))),
        ("read_etc", attempt(fs::read("/etc/passwd").map(|_| ()))),
        ("list_other_caches", attempt(fs::read_dir(format!("{}/../..", std::env::var("CARGO_HOME").unwrap())).map(|_| ()))),
        ("write_cache_root", attempt(fs::write(format!("{}/../../../escape.txt", std::env::var("CARGO_HOME").unwrap()), "x"))),
        ("shell_reads_token", attempt(std::process::Command::new("/bin/sh").arg("-c").arg("cat /apps/.runner-token").output().and_then(|o| if o.status.success() { Ok(()) } else { Err(std::io::Error::other(String::from_utf8_lossy(&o.stderr).trim().to_string())) }))),
    ]);
    println!("cargo:rustc-env=PROBE_BUILD={res}");
}
`,
  "server/src/main.rs": `use std::{env, fs, io::{Read, Write}, net::TcpListener};
${rustAttempt}
fn main() {
    let listener = TcpListener::bind(format!("{}:{}", env::var("HOST").unwrap(), env::var("PORT").unwrap())).unwrap();
    for stream in listener.incoming() {
        let Ok(mut s) = stream else { continue };
        let _ = s.read(&mut [0u8; 4096]);
        let run = json(&[
            ("read_runner_token", attempt(fs::read("/apps/.runner-token").map(|_| ()))),
            ("list_other_apps", attempt(fs::read_dir("/apps").map(|_| ()))),
            ("write_own_data", attempt(fs::write(format!("{}/ok.txt", env::var("DATA_DIR").unwrap()), "x"))),
        ]);
        let body = format!(r#"{{"build":{},"run":{run}}}"#, env!("PROBE_BUILD"));
        let _ = write!(s, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len());
    }
}
`,
};

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
  assert.match(runtime.rust || "", /^1\.\d+/, runtime.rust_note);
  assert.match(runtime.rust_target, /-unknown-linux-musl$/);
  assert.equal(docker(["exec", runnerName, "id", "-u"]), "10001");
  console.log(`runner: ${runtime.go} and Rust ${runtime.rust} (${runtime.rust_target}) ${runtime.os}/${runtime.arch}; sandbox: ${runtime.sandbox.detail}`);

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

  // The Rust template: crates download and compile in the sidecar, confined.
  let started = Date.now();
  const rustNotes = await api("/api/apps", { name: "Notes (Rust)", template: "notes-rust" });
  const rustDep = await deployed(rustNotes.app.id, 1);
  assert.equal(rustDep.status, "ready", JSON.stringify(rustDep));
  assert.equal(rustDep.backend, "rust");
  console.log(`notes-rust: cold build and start in ${((Date.now() - started) / 1000).toFixed(0)}s`);
  const rustLive = `/run/${rustNotes.app.id}/live/`;
  const note = await fetch(base + rustLive + "api/notes", { method: "POST", headers: { "Content-Type": "application/json", "X-Shartifacts-User": '{"name":"forged"}' }, body: '{"text":"from rust"}' });
  assert.equal(note.status, 201, await note.clone().text());
  assert.equal((await note.json()).author, undefined, "identity comes from shartifacts, not the client");
  assert.match(await (await fetch(base + rustLive + "api/notes")).text(), /from rust/);
  // A frontend-only change reuses the backend; a backend change builds on the cached crates.
  const v2r = await api(`/api/apps/${rustNotes.app.id}/deployments`, { base_version: 1, merge: true, edits: [{ file: "index.html", old_str: "</head>", new_str: "<meta name=x></head>" }] });
  const v2rDep = await deployed(rustNotes.app.id, v2r.deployment.version);
  assert.equal(v2rDep.status, "ready", JSON.stringify(v2rDep));
  const v2rLog = (await api(`/api/apps/${rustNotes.app.id}?logs`)).deployments.find((d) => d.version === 2).log;
  assert.match(v2rLog, /reused that build/, v2rLog);
  started = Date.now();
  const v3r = await api(`/api/apps/${rustNotes.app.id}/deployments`, { base_version: 2, merge: true, edits: [{ file: "server/src/main.rs", old_str: '"ok": true', new_str: '"ok": true, "rust": true' }] });
  const v3rDep = await deployed(rustNotes.app.id, v3r.deployment.version);
  assert.equal(v3rDep.status, "ready", JSON.stringify(v3rDep));
  console.log(`notes-rust: rebuild after a backend change in ${((Date.now() - started) / 1000).toFixed(0)}s`);
  assert.match(await (await fetch(base + rustLive + "api/health")).text(), /"rust":true/);

  // The Rust probe: what can a build script and a Rust backend reach?
  const rprobe = await api("/api/apps", { name: "Rust probe", files: rustProbe });
  const rprobeDep = await deployed(rprobe.app.id, 1);
  assert.equal(rprobeDep.status, "ready", JSON.stringify(rprobeDep));
  assert.ok(rprobeDep.warnings.some((w) => /no server\/Cargo\.lock/.test(w)), JSON.stringify(rprobeDep.warnings));
  const rust = await api(`/run/${rprobe.app.id}/live/api/probe`);
  console.log("rust build script:", rust.build, "\nrust backend:", rust.run);
  assert.equal(rust.build.read_source, "allowed");
  assert.equal(rust.build.write_own_cache, "allowed");
  assert.equal(rust.run.write_own_data, "allowed");
  if (runtime.sandbox.mechanism === "landlock") {
    for (const k of ["read_runner_token", "list_other_apps", "write_source", "shell_reads_token", "read_etc", "list_other_caches", "write_cache_root"]) assert.match(rust.build[k], /denied/, `build script ${k}`);
    for (const k of ["read_runner_token", "list_other_apps"]) assert.match(rust.run[k], /denied/, `backend ${k}`);
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
    `PASS: compose shape (shartifacts + runner sidecar, no root, no capabilities): Go and Rust backends build in the sidecar and serve through shartifacts, survive a runner restart, Rust builds reuse unchanged backends; the sidecar cannot see artifacts; sandbox ${runtime.sandbox.mechanism}${runtime.sandbox.mechanism === "landlock" ? ` ABI ${runtime.sandbox.abi} blocks reading the runner token, other apps, writes outside DATA_DIR, running a shell${runtime.sandbox.network ? " and TCP connections unless the app enables network" : ""}, and keeps Rust build scripts to the source and build cache` : ""}.`,
  );
} finally {
  spawnSync("docker", ["rm", "-f", name, runnerName], { stdio: "ignore" });
  spawnSync("docker", ["network", "rm", net], { stdio: "ignore" });
  spawnSync("docker", ["volume", "rm", volume], { stdio: "ignore" });
}
