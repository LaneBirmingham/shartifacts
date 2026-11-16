package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRustInputs(t *testing.T) {
	src := t.TempDir()
	writeFiles(t, src, map[string]string{
		"server/Cargo.toml":           "[package]\nname = \"app\"\n",
		"server/src/main.rs":          "fn main() {}",
		"server/migrations/1.sql":     "create table x (y)",
		"index.html":                  "<h1>frontend</h1>",
		"src/App.tsx":                 "export default 1",
		"server/target/release/stale": "a target directory deployed by mistake",
		"node_modules/react/index.js": "a frontend package",
		".git/HEAD":                   "ref: refs/heads/main",
	})
	inputs := func() string {
		t.Helper()
		h, err := rustInputs(src, "rustc 1.90.0", "x86_64-unknown-linux-musl", "server", "")
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := inputs()
	// Neither the frontend, its packages, git's files nor a deployed target directory is a
	// build input.
	writeFiles(t, src, map[string]string{"index.html": "<h1>changed</h1>", "src/App.tsx": "export default 2", "server/target/release/stale": "changed",
		"node_modules/react/index.js": "changed", ".git/HEAD": "changed"})
	if inputs() != base {
		t.Fatal("a frontend, node_modules, .git or target/ change changed the backend's inputs")
	}
	// The crate's sources, what lies beneath it (a directory named target or node_modules
	// that is not cargo's or npm's included), the lock, build settings at any depth, vendored
	// crates and a crate elsewhere in the app are.
	for _, change := range [][2]string{
		{"server/src/main.rs", "fn main() { }"}, {"server/build.rs", "fn main() {}"}, {"server/migrations/1.sql", "create table x (z)"},
		{"server/Cargo.lock", "# lock"}, {".cargo/config.toml", "[net]"}, {"apps/.cargo/config.toml", "[build]"}, {"vendor/serde/lib.rs", ""},
		{"shared/Cargo.toml", "[package]"}, {"shared/data.json", "{}"}, {"rust-toolchain.toml", "[toolchain]"},
		{"server/assets/target/x.txt", "not cargo's target"}, {"server/node_modules/x.txt", "inside the crate"},
	} {
		p := change[0]
		writeFiles(t, src, map[string]string{p: change[1]})
		if next := inputs(); next == base {
			t.Fatalf("%s changed but the inputs did not", p)
		} else {
			base = next
		}
	}
	// So are the compiler, the target, the crate's directory and the binary.
	for _, other := range [][4]string{
		{"rustc 1.91.0", "x86_64-unknown-linux-musl", "server", ""}, {"rustc 1.90.0", "aarch64-unknown-linux-musl", "server", ""},
		{"rustc 1.90.0", "x86_64-unknown-linux-musl", "shared", ""}, {"rustc 1.90.0", "x86_64-unknown-linux-musl", "server", "api"},
	} {
		if h, _ := rustInputs(src, other[0], other[1], other[2], other[3]); h == base {
			t.Fatalf("%v gave the same inputs", other)
		}
	}
}

func TestPickBinary(t *testing.T) {
	src, tgt, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeFiles(t, tgt, map[string]string{"release/app": "app", "release/migrate": "migrate", "release/tool": "tool"})
	writeFiles(t, outside, map[string]string{"token": "secret"})
	os.Symlink(filepath.Join(outside, "token"), filepath.Join(tgt, "release", "link"))
	msg := func(id, name, kind, exe string) string {
		return `{"reason":"compiler-artifact","package_id":"` + id + `","target":{"name":"` + name + `","kind":["` + kind + `"]},"executable":` + exe + "}\n"
	}
	exe := func(name string) string { return `"` + filepath.Join(tgt, "release", name) + `"` }
	app, other := "path+file://"+filepath.Join(src, "server")+"#app@0.1.0", "registry+https://github.com/rust-lang/crates.io-index#tool@1.0.0"
	out := msg(other, "tool", "bin", exe("tool")) + msg(app, "app", "lib", "null") + `{"reason":"build-finished","success":true}` + "\n"
	one := out + msg(app, "app", "bin", exe("app"))
	pick := func(stdout, bin string) (string, string, error) {
		return pickBinary(strings.NewReader(stdout), bin, tgt, src)
	}
	if got, name, err := pick(one, ""); err != nil || !strings.HasSuffix(got, "/release/app") || name != "app" {
		t.Fatalf("one binary: %q %q %v", got, name, err)
	}
	if _, _, err := pick(out, ""); err == nil || !strings.Contains(err.Error(), "no binary target") {
		t.Fatalf("a registry crate's binary is not the app's: %v", err)
	}
	two := one + msg(app, "migrate", "bin", exe("migrate"))
	if _, _, err := pick(two, ""); err == nil || !strings.Contains(err.Error(), `"bin": "app"`) {
		t.Fatalf("two binaries: %v", err)
	}
	if got, _, err := pick(two, "migrate"); err != nil || !strings.HasSuffix(got, "/release/migrate") {
		t.Fatalf("named binary: %q %v", got, err)
	}
	if _, _, err := pick(two, "nope"); err == nil || !strings.Contains(err.Error(), "no binary named nope") {
		t.Fatalf("missing named binary: %v", err)
	}
	// Lines a build script or macro forged name files outside the target directory, through
	// a symlink in it, or a package outside the app: none is the backend.
	for name, forged := range map[string]string{
		"outside the target": msg(app, "app", "bin", `"`+filepath.Join(outside, "token")+`"`),
		"a symlink out":      msg(app, "app", "bin", exe("link")),
		"another package":    msg("path+file://"+outside+"#app@0.1.0", "app", "bin", exe("app")),
		"a relative package": msg("path+file://"+filepath.Join(src, "..", filepath.Base(outside))+"#app@0.1.0", "app", "bin", exe("app")),
		"not a path package": msg("git+https://example.com/app#app@0.1.0", "app", "bin", exe("app")),
		"a missing file":     msg(app, "app", "bin", exe("gone")),
	} {
		if got, _, err := pick(forged, ""); err == nil {
			t.Errorf("%s: picked %s", name, got)
		}
	}
}

func TestPruneRustCaches(t *testing.T) {
	l := &Local{Dir: t.TempDir()}
	gone, kept, building := "0123456789abcdef01234567", "1123456789abcdef01234567", "2123456789abcdef01234567"
	os.MkdirAll(filepath.Join(l.Dir, kept), 0700)
	for _, id := range []string{gone, kept, building} {
		writeFiles(t, filepath.Join(l.cacheDir(), "rust", id), map[string]string{"target/release/x": "x", "home/registry/x": "x"})
	}
	l.pruneRustCaches(building)
	for id, want := range map[string]bool{gone: false, kept: true, building: true} {
		if _, err := os.Stat(filepath.Join(l.cacheDir(), "rust", id)); (err == nil) != want {
			t.Errorf("%s: kept %v, want %v", id, err == nil, want)
		}
	}
	// A cache outside the apps directory holds other directories' apps: leave it alone.
	shared := &Local{Dir: t.TempDir(), CacheDir: l.cacheDir()}
	shared.pruneRustCaches("")
	if _, err := os.Stat(filepath.Join(l.cacheDir(), "rust", kept)); err != nil {
		t.Fatal("pruned a shared cache")
	}
}

// Confined builds read the toolchain and what compiling and linking need, not the places a
// server keeps its settings and data.
func TestRustBuildPaths(t *testing.T) {
	for _, p := range buildSystemPaths {
		if p == "/etc" || p == "/opt" || p == "/srv" || p == "/home" || p == "/root" || p == "/var" || p == "/mnt" || p == "/data" {
			t.Errorf("builds may read all of %s", p)
		}
	}
	if p := (&Local{Dir: "/usr/local/share/shartifacts"}).rustExposes(); p != "/usr" {
		t.Errorf("an apps directory under /usr: %q", p)
	}
	if p := (&Local{Dir: t.TempDir()}).rustExposes(); p != "" {
		t.Errorf("an apps directory of its own: %q", p)
	}
	if n := rustJobs(); n < 1 {
		t.Errorf("%d jobs", n)
	}
}

// Registry credentials reach the steps that run only cargo; the build, which runs the app's
// code, gets none, and only it may use a rustc wrapper the app configures.
func TestRustEnv(t *testing.T) {
	t.Setenv("CARGO_REGISTRIES_PRIVATE_TOKEN", "secret")
	t.Setenv("CARGO_REGISTRY_TOKEN", "secret")
	t.Setenv("CARGO_NET_OFFLINE", "true")
	t.Setenv("SHARTIFACTS_RUNNER_TOKEN", "secret")
	b := &rustBuild{home: "/c/home", tgt: "/c/target", tmp: "/c/tmp"}
	has := func(env []string, kv string) bool { return slices.Contains(env, kv) }
	lock, build := b.env(rustStep{lock: true}), b.env(rustStep{jobs: 2})
	for _, kv := range []string{"CARGO_REGISTRIES_PRIVATE_TOKEN=secret", "CARGO_REGISTRY_TOKEN=secret", "CARGO_NET_OFFLINE=true", "RUSTC_WRAPPER=", "RUSTC_WORKSPACE_WRAPPER="} {
		if !has(lock, kv) {
			t.Errorf("a lock step lacks %s", kv)
		}
	}
	for _, kv := range []string{"CARGO_REGISTRIES_PRIVATE_TOKEN=secret", "CARGO_REGISTRY_TOKEN=secret", "RUSTC_WRAPPER="} {
		if has(build, kv) {
			t.Errorf("the build has %s", kv)
		}
	}
	if !has(build, "CARGO_NET_OFFLINE=true") || !has(build, "CARGO_BUILD_JOBS=2") || !has(build, "CARGO_HOME=/c/home") {
		t.Errorf("the build's environment: %v", build)
	}
	for _, env := range [][]string{lock, build} {
		if has(env, "SHARTIFACTS_RUNNER_TOKEN=secret") {
			t.Error("a cargo step has the runner's token")
		}
	}
}

// rustLocal is a Local that builds Rust, or skips the test on a machine without cargo.
func rustLocal(t *testing.T) *Local {
	t.Helper()
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("no cargo on PATH")
	}
	l := &Local{Dir: t.TempDir()}
	l.init()
	t.Cleanup(l.Close)
	if l.rust.missing != "" {
		t.Skip(l.rust.missing)
	}
	return l
}

// A standard-library crate builds offline, so this runs wherever cargo does.
func TestBuildRust(t *testing.T) {
	l := rustLocal(t)
	app := "0123456789abcdef01234567"
	src := func(v int, files map[string]string) {
		dir, _ := l.versionDir(app, v)
		writeFiles(t, filepath.Join(dir, "src"), files)
	}
	crate := map[string]string{
		"server/Cargo.toml":  "[package]\nname = \"hello\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"server/src/main.rs": `fn main() { println!("hello from {}", env!("CARGO_PKG_NAME")); }`,
		"index.html":         "<h1>one</h1>",
	}
	build := func(v int, req BuildRequest) BuildResult {
		t.Helper()
		req.App, req.Version, req.Kind = app, v, "rust"
		return l.Build(context.Background(), req)
	}

	src(1, crate)
	res := build(1, BuildRequest{})
	if !res.OK {
		t.Fatalf("build: %s\n%s", res.Error, res.Log)
	}
	if !strings.Contains(res.Log, "cargo build --release --frozen --target "+l.rust.target) || !strings.Contains(res.Log, "built hello") {
		t.Fatalf("log: %s", res.Log)
	}
	// No Cargo.lock: cargo wrote one into the version's source, and the build says so.
	v1, _ := l.versionDir(app, 1)
	if _, err := os.Stat(filepath.Join(v1, "src", "server", "Cargo.lock")); err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "Cargo.lock") {
		t.Fatalf("lockfile: %v %v", err, res.Warnings)
	}
	out, err := exec.Command(filepath.Join(v1, "out", "server")).Output()
	if err != nil || string(out) != "hello from hello\n" {
		t.Fatalf("the binary: %q %v", out, err)
	}

	// A frontend change, with or without the Cargo.lock the build wrote, reuses the build.
	lock, _ := os.ReadFile(filepath.Join(v1, "src", "server", "Cargo.lock"))
	src(2, map[string]string{"server/Cargo.toml": crate["server/Cargo.toml"], "server/src/main.rs": crate["server/src/main.rs"], "index.html": "<h1>two</h1>"})
	src(3, map[string]string{"server/Cargo.toml": crate["server/Cargo.toml"], "server/src/main.rs": crate["server/src/main.rs"], "server/Cargo.lock": string(lock)})
	for v := 2; v <= 3; v++ {
		if res := build(v, BuildRequest{}); !res.OK || !strings.Contains(res.Log, "reused that build") {
			t.Fatalf("version %d: %+v", v, res)
		}
	}

	// A crate with two binaries needs one named.
	two := map[string]string{"server/src/bin/tool.rs": "fn main() {}"}
	for p, b := range crate {
		two[p] = b
	}
	src(4, two)
	if res := build(4, BuildRequest{}); res.OK || !strings.Contains(res.Error, `"bin": "hello"`) {
		t.Fatalf("two binaries: %+v", res)
	}
	if res := build(4, BuildRequest{Bin: "tool"}); !res.OK || !strings.Contains(res.Log, "built tool") {
		t.Fatalf("named binary: %+v", res)
	}

	// Compiler errors come back with what failed, not the crates that compiled before it.
	src(5, map[string]string{"server/Cargo.toml": crate["server/Cargo.toml"], "server/src/main.rs": "fn main() { let x: u8 = \"no\"; }"})
	if res := build(5, BuildRequest{}); res.OK || !strings.Contains(res.Error, "mismatched types") || !strings.Contains(res.Error, "cargo build failed") {
		t.Fatalf("compile error: %+v", res)
	}
	src(6, map[string]string{"index.html": "x"})
	if res := build(6, BuildRequest{}); res.OK || !strings.Contains(res.Error, "no Cargo.toml in server/") {
		t.Fatalf("no crate: %+v", res)
	}
	for _, dir := range []string{"../escape", "server", "./../x"} {
		if res := build(6, BuildRequest{Package: dir}); res.OK || !strings.Contains(res.Error, "inside the app") {
			t.Fatalf("%s: %+v", dir, res)
		}
	}
	if res := build(6, BuildRequest{Package: "./server", Bin: "../x"}); res.OK || !strings.Contains(res.Error, "binary target's name") {
		t.Fatalf("a bad bin: %+v", res)
	}

	// The app's crates and compiled code are its own: no other app's build reads or writes them.
	if _, err := os.Stat(filepath.Join(l.cacheDir(), "rust", app, "target")); err != nil {
		t.Fatalf("no target directory of the app's own: %v", err)
	}

	// A Cargo.lock older than Cargo.toml (here, a path dependency added since) is updated
	// for what changed, and the build says so.
	withDep := map[string]string{
		"server/Cargo.toml":     crate["server/Cargo.toml"] + "\n[dependencies]\ngreeting = { path = \"../greeting\" }\n",
		"server/Cargo.lock":     string(lock),
		"server/src/main.rs":    `fn main() { println!("{}", greeting::GREETING); }`,
		"greeting/Cargo.toml":   "[package]\nname = \"greeting\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"greeting/src/lib.rs":   `pub const GREETING: &str = include_str!("../../words.txt");`,
		"words.txt":             "hello",
		"shartifacts.json":      "{}",
		"index.html":            "<h1>seven</h1>",
		"server/assets/one.txt": "x",
	}
	src(7, withDep)
	res = build(7, BuildRequest{})
	if !res.OK || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "server/Cargo.lock did not match Cargo.toml") {
		t.Fatalf("a stale lock: %+v", res)
	}
	v7, _ := l.versionDir(app, 7)
	if b, _ := os.ReadFile(filepath.Join(v7, "src", "server", "Cargo.lock")); !strings.Contains(string(b), `name = "greeting"`) {
		t.Fatalf("the lock was not updated: %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(v1, "src", "server", "Cargo.lock")); string(b) != string(lock) {
		t.Fatal("updating version 7's lock changed version 1's")
	}
	// words.txt is above every crate, so only the compiler's dep-info names it: a change to
	// it rebuilds, and the same file again reuses.
	updated, _ := os.ReadFile(filepath.Join(v7, "src", "server", "Cargo.lock"))
	withDep["server/Cargo.lock"] = string(updated)
	src(8, withDep)
	if res := build(8, BuildRequest{}); !res.OK || !strings.Contains(res.Log, "reused that build") {
		t.Fatalf("the same files: %+v", res)
	}
	withDep["words.txt"] = "howdy"
	src(9, withDep)
	if res := build(9, BuildRequest{}); !res.OK || strings.Contains(res.Log, "reused") {
		t.Fatalf("a changed include_str! file: %+v", res)
	}
	v9, _ := l.versionDir(app, 9)
	if out, err := exec.Command(filepath.Join(v9, "out", "server")).Output(); err != nil || string(out) != "howdy\n" {
		t.Fatalf("the rebuilt binary: %q %v", out, err)
	}
}

func TestBuildRustWithoutAToolchain(t *testing.T) {
	l := &Local{Dir: t.TempDir(), CargoBinary: "no-such-cargo"}
	t.Cleanup(l.Close)
	l.init()
	if info := l.Info(context.Background()); info.Rust != "" || info.RustNote != "" {
		t.Fatalf("info: %+v", info)
	}
	dir, _ := l.versionDir("0123456789abcdef01234567", 1)
	writeFiles(t, filepath.Join(dir, "src"), map[string]string{"server/Cargo.toml": "[package]"})
	res := l.Build(context.Background(), BuildRequest{App: "0123456789abcdef01234567", Version: 1, Kind: "rust"})
	if res.OK || !strings.Contains(res.Error, "no Rust toolchain") || !strings.Contains(res.Error, "prebuilt binary") {
		t.Fatalf("%+v", res)
	}
}
