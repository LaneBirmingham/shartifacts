package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LaneBirmingham/shartifacts/internal/runner"
	"github.com/LaneBirmingham/shartifacts/internal/store"
)

func TestBackendBuild(t *testing.T) {
	layout := func(files ...string) string {
		dir := t.TempDir()
		for _, f := range files {
			os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0700)
			os.WriteFile(filepath.Join(dir, f), []byte("x"), 0600)
		}
		return dir
	}
	var none appManifest
	for name, tc := range map[string]struct {
		src  string
		m    appManifest
		kind string
		pkg  string
		warn bool
	}{
		"go":                  {src: layout("server/main.go"), kind: "go", pkg: "./server"},
		"rust":                {src: layout("server/Cargo.toml", "server/src/main.rs"), kind: "rust", pkg: "./server"},
		"prebuilt":            {src: layout("server/app"), kind: "binary"},
		"prebuilt beats rust": {src: layout("server/app", "server/Cargo.toml"), kind: "binary", warn: true},
		"go beats rust":       {src: layout("server/main.go", "server/Cargo.toml"), kind: "go", pkg: "./server"},
		"frontend only":       {src: layout("index.html")},
	} {
		req, warnings := backendBuild(tc.src, none)
		if req.Kind != tc.kind || req.Package != tc.pkg || (len(warnings) > 0) != tc.warn {
			t.Errorf("%s: %+v %v", name, req, warnings)
		}
	}
	var m appManifest
	m.Backend.Rust, m.Backend.Bin = "./api", "serve"
	if req, _ := backendBuild(layout("server/main.go"), m); req.Kind != "rust" || req.Package != "./api" || req.Bin != "serve" {
		t.Errorf("manifest: %+v", req)
	}
}

func TestRustManifest(t *testing.T) {
	read := func(manifest string) error {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, manifestName), []byte(manifest), 0600)
		_, err := readManifest(dir)
		return err
	}
	for manifest, want := range map[string]string{
		`{"backend":{"rust":"./server"}}`:                       "",
		`{"backend":{"rust":"."}}`:                              "",
		`{"backend":{"rust":"./server","bin":"api"}}`:           "",
		`{"backend":{"rust":"./server","go":"./server"}}`:       "set one of",
		`{"backend":{"rust":"./server","binary":"server/app"}}`: "set one of",
		`{"backend":{"bin":"api"}}`:                             "set backend.rust too",
		`{"backend":{"rust":"server"}}`:                         "like ./server",
		`{"backend":{"rust":"./../x"}}`:                         "like ./server",
		`{"backend":{"rust":"/abs"}}`:                           "like ./server",
		`{"backend":{"rust":"./server","binn":"x"}}`:            "backend.bin",
		`{"backend":{"rust":"./server","bin":"../x"}}`:          "a binary target's name",
		`{"backend":{"rust":"./server","bin":"-x"}}`:            "a binary target's name",
	} {
		err := read(manifest)
		if (want == "") != (err == nil) || want != "" && !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", manifest, err, want)
		}
	}
}

func TestServerOnlyRust(t *testing.T) {
	custom := appSite{Backend: "./api"}
	for p, want := range map[string]bool{
		"server/Cargo.toml": true, "server/src/main.rs": true, "Cargo.toml": true, "Cargo.lock": true, "lib.rs": true,
		".cargo/config.toml": true, "rust-toolchain.toml": true, "rust-toolchain": true, "target/release/app": true,
		"api/migrations/1.sql": true, "src/main.tsx": false, "public/rust.png": false, "docs/cargo.md": false,
	} {
		if got := serverOnly(p, custom); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
}

func TestRustTemplate(t *testing.T) {
	files, err := templateFiles("notes-rust")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"index.html", "src/App.tsx", "server/Cargo.toml", "server/Cargo.lock", "server/src/main.rs", "shartifacts.json"} {
		if files[p] == nil {
			t.Errorf("no %s", p)
		}
	}
	for _, p := range []string{"server/main.go", "go.mod"} {
		if files[p] != nil {
			t.Errorf("the Rust template has the Go template's %s", p)
		}
	}
	if !strings.Contains(string(files["src/App.tsx"]), "Rust behind") {
		t.Error("the Rust template's page names Go as its backend")
	}
	if !strings.Contains(string(files["server/src/main.rs"]), `join("notes.json")`) {
		t.Error("the Rust backend should keep the Go template's data file")
	}
	if _, err := templateFiles("nope"); err == nil || !strings.Contains(err.Error(), "notes-rust") {
		t.Fatalf("unknown template: %v", err)
	}
}

// rustRunner is a test runner that builds Rust, or skips the test without cargo.
func rustRunner(t *testing.T) func(st *store.Store) runner.Runtime {
	t.Helper()
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("no cargo on PATH")
	}
	return func(st *store.Store) runner.Runtime {
		local := localRunner(st.AppsDir(), false)
		t.Cleanup(local.Close)
		if note := local.Info(context.Background()).RustNote; note != "" {
			t.Skip(note)
		}
		return local
	}
}

// A Rust backend deploys like a Go one: built from server/, never served, and rebuilt only
// when its crate changes. The crate uses only the standard library, so it builds offline.
func TestRustApp(t *testing.T) {
	_, h := appServer(t, rustRunner(t))
	server := `use std::{env, io::{Read, Write}, net::TcpListener};
fn main() {
    let l = TcpListener::bind(format!("{}:{}", env::var("HOST").unwrap(), env::var("PORT").unwrap())).unwrap();
    for s in l.incoming() {
        let Ok(mut s) = s else { continue };
        let mut buf = [0u8; 4096];
        let n = s.read(&mut buf).unwrap_or(0);
        let path = String::from_utf8_lossy(&buf[..n]).split_whitespace().nth(1).unwrap_or("").to_string();
        let body = format!("{{\"path\":{path:?},\"version\":{:?}}}", env::var("APP_VERSION").unwrap_or_default());
        let _ = write!(s, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len());
    }
}
`
	files := map[string]string{
		"index.html":         "<h1>Rust</h1>",
		"server/Cargo.toml":  "[package]\nname = \"hello\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"server/src/main.rs": server,
	}
	b, _ := json.Marshal(map[string]any{"name": "Rust", "files": files})
	created := decodeBody(t, call(h, "POST", "/api/apps", string(b)))
	dep := created["deployment"].(map[string]any)
	if dep["status"] != "ready" || dep["backend"] != "rust" || dep["frontend"] != true {
		t.Fatalf("deploy: %v", dep)
	}
	id := created["app"].(map[string]any)["id"].(string)
	live := "/run/" + id + "/live/"
	if w := call(h, "GET", live+"api/hello", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"path":"/api/hello","version":"1"`) {
		t.Fatalf("backend: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"server/Cargo.toml", "server/src/main.rs", "server/Cargo.lock"} {
		if w := call(h, "GET", live+p, ""); w.Code != 404 {
			t.Fatalf("%s served: %d", p, w.Code)
		}
	}
	// A frontend change keeps the backend's build.
	b, _ = json.Marshal(map[string]any{"base_version": 1, "merge": true, "files": map[string]string{"index.html": "<h1>Rust 2</h1>"}})
	v2 := decodeBody(t, call(h, "POST", "/api/apps/"+id+"/deployments", string(b)))["deployment"].(map[string]any)
	if v2["status"] != "ready" {
		t.Fatalf("v2: %v", v2)
	}
	logs := decodeBody(t, call(h, "GET", "/api/apps/"+id+"?logs", ""))
	if log := logs["deployments"].([]any)[0].(map[string]any)["log"].(string); !strings.Contains(log, "reused that build") {
		t.Fatalf("v2 log: %s", log)
	}
	// A compile error fails the version and never makes it live.
	b, _ = json.Marshal(map[string]any{"base_version": 2, "merge": true, "files": map[string]string{"server/src/main.rs": "fn main() { nope }"}})
	v3 := decodeBody(t, call(h, "POST", "/api/apps/"+id+"/deployments", string(b)))["deployment"].(map[string]any)
	if v3["status"] != "failed" || !strings.Contains(v3["error"].(string), "cargo build failed") {
		t.Fatalf("v3: %v", v3)
	}
	if w := call(h, "GET", live+"api/x", ""); !strings.Contains(w.Body.String(), `"version":"2"`) {
		t.Fatalf("a failed build went live: %s", w.Body)
	}
}

// On a server that cannot build Rust, a frontend with a crate beside it deploys as the
// frontend it was before shartifacts built Rust, unless shartifacts.json asks for the crate.
func TestRustWithoutAToolchain(t *testing.T) {
	_, h := appServer(t, func(st *store.Store) runner.Runtime {
		local := localRunner(st.AppsDir(), false)
		local.CargoBinary = "no-such-cargo"
		t.Cleanup(local.Close)
		return local
	})
	files := map[string]string{"index.html": "<h1>hi</h1>", "server/Cargo.toml": "[package]\nname = \"x\"\n", "server/src/main.rs": "fn main() {}"}
	b, _ := json.Marshal(map[string]any{"name": "Crate", "files": files})
	dep := decodeBody(t, call(h, "POST", "/api/apps", string(b)))["deployment"].(map[string]any)
	if dep["status"] != "ready" || dep["backend"] != nil || !strings.Contains(strings.Join(anyStrings(dep["warnings"]), " "), "server/Cargo.toml was not built") {
		t.Fatalf("auto-detected crate: %v", dep)
	}
	files["shartifacts.json"] = `{"backend":{"rust":"./server"}}`
	b, _ = json.Marshal(map[string]any{"name": "Crate", "files": files})
	dep = decodeBody(t, call(h, "POST", "/api/apps", string(b)))["deployment"].(map[string]any)
	if dep["status"] != "failed" || !strings.Contains(dep["error"].(string), "no Rust toolchain") {
		t.Fatalf("a crate the manifest names: %v", dep)
	}
}

func anyStrings(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, s := range list {
		str, _ := s.(string)
		out = append(out, str)
	}
	return out
}

func TestRustGuide(t *testing.T) {
	s, _ := appServer(t, nil)
	guide := s.appGuide(context.Background(), "http://127.0.0.1:8080")
	for _, want := range []string{"server/Cargo.toml", "notes-rust", `"rust": "./server"`, "Cargo.lock"} {
		if !strings.Contains(guide, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
	if s.Runtime.Info(context.Background()).Sandbox.Mechanism != "landlock" && !strings.Contains(guide, "UNCONFINED on this server") {
		t.Error("the guide claims build confinement this server lacks")
	}
	if strings.Contains(guide, "{RUSTBUILD}") || strings.Contains(guide, "{RUSTCONFINE}") {
		t.Error("the guide has an unfilled placeholder")
	}
}
