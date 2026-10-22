package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		path := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackendInputs(t *testing.T) {
	src := t.TempDir()
	writeFiles(t, src, map[string]string{
		"go.mod":                 "module app\n\ngo 1.24\n",
		"server/main.go":         "package main",
		"server/static/page.txt": "embedded",
		"index.html":             "<h1>frontend</h1>",
		"src/App.tsx":            "export default 1",
	})
	inputs := func() string {
		t.Helper()
		h, err := backendInputs(src, "./server", "go1.24.0", "-mod=mod")
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := inputs()
	// The frontend is not a build input.
	writeFiles(t, src, map[string]string{"index.html": "<h1>changed</h1>", "src/App.tsx": "export default 2", "src/new.ts": ""})
	if inputs() != base {
		t.Fatal("a frontend change changed the backend's inputs")
	}
	// Go files, what go:embed can reach below them, and the module files are.
	for p, body := range map[string]string{"server/main.go": "package main // v2", "server/static/page.txt": "embedded v2", "go.sum": "x", "vendor/modules.txt": "# x", "lib/util.go": "package lib"} {
		writeFiles(t, src, map[string]string{p: body})
		if next := inputs(); next == base {
			t.Fatalf("%s changed but the inputs did not", p)
		} else {
			base = next
		}
	}
	// So are the package, toolchain and module mode.
	for _, other := range [][3]string{{"./cmd", "go1.24.0", "-mod=mod"}, {"./server", "go1.25.0", "-mod=mod"}, {"./server", "go1.24.0", "-mod=vendor"}} {
		if h, _ := backendInputs(src, other[0], other[1], other[2]); h == base {
			t.Fatalf("%v gave the same inputs", other)
		}
	}
}

func TestReuseBuild(t *testing.T) {
	l := &Local{Dir: t.TempDir()}
	app := "0123456789abcdef01234567"
	out := func(v int) string {
		dir, _ := l.versionDir(app, v)
		return filepath.Join(dir, "out")
	}
	// Version 1 built from inputs A; version 2 from B, where -mod=mod rewrote go.sum to C;
	// version 3 failed (no record); version 4 is being built.
	writeFiles(t, out(1), map[string]string{"server": "binary A", buildInputsFile: "A\n"})
	writeFiles(t, out(2), map[string]string{"server": "binary B", buildInputsFile: "B\nC\n"})
	writeFiles(t, out(3), map[string]string{})
	writeFiles(t, out(4), map[string]string{"server": "stale"})
	for inputs, want := range map[string]int{"A": 1, "B": 2, "C": 2} {
		from, ok := l.reuseBuild(BuildRequest{App: app, Version: 4}, inputs, out(4))
		if !ok || from != want {
			t.Fatalf("%s: reused %d %v, want %d", inputs, from, ok, want)
		}
		a, _ := os.Stat(filepath.Join(out(want), "server"))
		b, _ := os.Stat(filepath.Join(out(4), "server"))
		if !os.SameFile(a, b) {
			t.Fatalf("%s: version 4's backend is not version %d's", inputs, want)
		}
	}
	// The record travels with the binary, so a later version can match either line.
	l.reuseBuild(BuildRequest{App: app, Version: 4}, "C", out(4))
	if rec, _ := os.ReadFile(filepath.Join(out(4), buildInputsFile)); string(rec) != "B\nC\n" {
		t.Fatalf("record %q", rec)
	}
	if _, ok := l.reuseBuild(BuildRequest{App: app, Version: 4}, "D", out(4)); ok {
		t.Fatal("reused a build from other inputs")
	}
	if p := l.previousServer(BuildRequest{App: app, Version: 4}); p != filepath.Join(out(2), "server") {
		t.Fatalf("previous server %q", p)
	}
}

func TestInstallBinarySharesAnUnchangedOne(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, v := range []string{"1", "2"} {
		writeFiles(t, filepath.Join(dir, v, "src"), map[string]string{"server/app": string(exe)})
		os.MkdirAll(filepath.Join(dir, v, "out"), 0700)
	}
	first := filepath.Join(dir, "1", "out", "server")
	if res := installBinary(filepath.Join(dir, "1", "src"), "", first, ""); !res.OK {
		t.Fatal(res.Error)
	}
	second := filepath.Join(dir, "2", "out", "server")
	res := installBinary(filepath.Join(dir, "2", "src"), "", second, first)
	if !res.OK || !strings.Contains(res.Log, "unchanged") {
		t.Fatalf("%+v", res)
	}
	a, _ := os.Stat(first)
	b, _ := os.Stat(second)
	if !os.SameFile(a, b) || b.Mode()&0100 == 0 {
		t.Fatalf("not shared, or not executable: %v", b.Mode())
	}
}

func newTestLocal(dir string) *Local {
	return &Local{Dir: dir, procs: map[key]*proc{}, logs: map[key]*Logs{}, failures: map[key][]time.Time{}, dataSize: map[string]measured{}}
}

func TestForgetBoundsWhatEndedProcessesLeave(t *testing.T) {
	l := newTestLocal(t.TempDir())
	now := time.Now()
	for v := 1; v <= keptLogs+8; v++ {
		l.logs[key{"a", v}] = &Logs{State: "stopped", ended: now.Add(-time.Duration(v) * time.Minute)}
	}
	l.logs[key{"b", 1}] = &Logs{State: "stopped", ended: now.Add(-keepLogsFor - time.Minute)}
	// A version that started again shows its process's output, not what it left before.
	l.logs[key{"c", 1}] = &Logs{State: "stopped", ended: now}
	l.procs[key{"c", 1}] = &proc{}
	l.failures[key{"a", 1}] = []time.Time{now.Add(-2 * time.Minute)}
	l.failures[key{"a", 2}] = []time.Time{now.Add(-2 * time.Minute), now.Add(-10 * time.Second)}
	l.forget(now)
	if len(l.logs) != keptLogs {
		t.Fatalf("%d logs kept", len(l.logs))
	}
	for v := 1; v <= keptLogs; v++ {
		if l.logs[key{"a", v}] == nil {
			t.Fatalf("lost the output of version %d, among the latest", v)
		}
	}
	if _, ok := l.failures[key{"a", 1}]; ok || len(l.failures[key{"a", 2}]) != 2 {
		t.Fatalf("failures %v", l.failures)
	}
}

func TestDataQuota(t *testing.T) {
	app := "0123456789abcdef01234567"
	l := newTestLocal(t.TempDir())
	l.DataQuota = 1000
	data := filepath.Join(l.Dir, app, "data")
	writeFiles(t, data, map[string]string{"notes.json": strings.Repeat("x", 600), ".tmp/scratch": "y"})
	if err := l.checkQuota(key{app, 1}); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, data, map[string]string{"photos/big.bin": strings.Repeat("x", 600)})
	// A start measures again unless a measurement is seconds old.
	l.dataSize[app] = measured{bytes: 601, at: time.Now().Add(-time.Minute)}
	err := l.checkQuota(key{app, 1})
	if err == nil || !strings.Contains(err.Error(), "1.2 KiB, over its 1000 bytes quota") {
		t.Fatalf("%v", err)
	}
	// A running backend is not measured on every request; reap does that.
	l.procs[key{app, 1}] = &proc{}
	if err := l.checkQuota(key{app, 1}); err != nil {
		t.Fatal(err)
	}
	if err := newTestLocal(l.Dir).checkQuota(key{app, 2}); err != nil {
		t.Fatalf("no quota: %v", err)
	}
	for n, want := range map[int64]string{0: "0 bytes", 1023: "1023 bytes", 1536: "1.5 KiB", 2 << 30: "2 GiB", 5 << 40: "5 TiB"} {
		if got := sizeText(n); got != want {
			t.Errorf("%d: %q", n, got)
		}
	}
}
