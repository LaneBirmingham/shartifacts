package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// BuildTimeout bounds one backend build, module downloads included.
const BuildTimeout = 5 * time.Minute

func (l *Local) Build(ctx context.Context, req BuildRequest) BuildResult {
	l.init()
	vdir, err := l.versionDir(req.App, req.Version)
	if err != nil {
		return BuildResult{Error: err.Error()}
	}
	src, out := filepath.Join(vdir, "src"), filepath.Join(vdir, "out")
	if err := os.MkdirAll(out, 0700); err != nil {
		return BuildResult{Error: err.Error()}
	}
	switch req.Kind {
	case "go":
		return l.buildGo(ctx, req, src, out)
	case "binary":
		return installBinary(src, req.Binary, filepath.Join(out, "server"), l.previousServer(req))
	}
	return BuildResult{Error: "unknown backend kind " + req.Kind}
}

func (l *Local) buildGo(ctx context.Context, req BuildRequest, src, out string) BuildResult {
	if l.goPath == "" {
		where := "on this server"
		if !l.Remote {
			where += "; run the runner sidecar (see the README) or deploy a prebuilt binary"
		}
		return BuildResult{Error: "no Go toolchain " + where}
	}
	res := BuildResult{}
	if _, err := os.Stat(filepath.Join(src, "go.mod")); os.IsNotExist(err) {
		goVersion := strings.TrimPrefix(l.goVer, "go")
		if parts := strings.SplitN(goVersion, ".", 3); len(parts) >= 2 {
			goVersion = parts[0] + "." + parts[1]
		}
		if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module app\n\ngo "+goVersion+"\n"), 0600); err != nil {
			return BuildResult{Error: err.Error()}
		}
		res.Warnings = append(res.Warnings, "no go.mod: built as module app with the standard library only; deploy your own go.mod (and go.sum) to use modules")
	}
	// The go command updates go.mod and go.sum in place; this version's copies may be hard
	// links shared with the previous version, so give it its own before building.
	for _, name := range []string{"go.mod", "go.sum"} {
		if err := unshare(filepath.Join(src, name)); err != nil {
			return BuildResult{Error: err.Error()}
		}
	}
	pkg := req.Package
	if pkg == "" {
		pkg = "./server"
	}
	if !strings.HasPrefix(pkg, "./") || strings.Contains(pkg, "..") {
		return BuildResult{Error: "the Go package must be a path inside the app, like ./server"}
	}
	cache := filepath.Join(l.Dir, ".cache")
	mod := "-mod=mod"
	if _, err := os.Stat(filepath.Join(src, "vendor", "modules.txt")); err == nil {
		mod = "-mod=vendor"
	}
	// Fingerprint the inputs before building: -mod=mod may rewrite go.mod and go.sum.
	inputs, err := backendInputs(src, pkg, l.goVer, mod)
	if err != nil {
		return BuildResult{Error: err.Error()}
	}
	if from, ok := l.reuseBuild(req, inputs, out); ok {
		res.OK = true
		res.Log = fmt.Sprintf("$ go build %s %s  (%s, %s/%s)\nthe backend is built from the same files as version %d's: reused that build\n", mod, pkg, l.goVer, runtime.GOOS, runtime.GOARCH, from)
		return res
	}
	// out/server may be a hard link another version shares; the build must replace it, not
	// write through it.
	os.Remove(filepath.Join(out, "server"))
	ctx, cancel := context.WithTimeout(ctx, BuildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.goPath, "build", "-trimpath", "-ldflags=-s -w", "-o", filepath.Join(out, "server"), pkg)
	cmd.Dir = src
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(cache, "home"),
		"GOCACHE=" + filepath.Join(cache, "go-build"), "GOMODCACHE=" + filepath.Join(cache, "gomod"), "GOPATH=" + filepath.Join(cache, "gopath"),
		"CGO_ENABLED=0", "GOTOOLCHAIN=local", "GOFLAGS=" + mod,
	}
	// The operator's module settings (proxy, private modules, an offline GOPROXY=off).
	for _, name := range []string{"GOPROXY", "GOSUMDB", "GONOSUMDB", "GOPRIVATE", "GONOPROXY", "GOINSECURE", "GOAUTH", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	started := time.Now()
	err = cmd.Run()
	res.Log = fmt.Sprintf("$ go build %s %s  (%s, %s/%s)\n%s", mod, pkg, l.goVer, runtime.GOOS, runtime.GOARCH, buf.String())
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.Error = fmt.Sprintf("the Go build took longer than %s", BuildTimeout)
	case err != nil:
		res.Error = "go build failed: " + firstLines(buf.String(), 12)
	default:
		res.OK = true
		res.Log += fmt.Sprintf("built in %.1fs\n", time.Since(started).Seconds())
		// The record lists the inputs as deployed and, when -mod=mod filled in go.mod or
		// go.sum, as built, which is what a later merge deploy copies forward. Without it
		// the next version only builds again; nothing else depends on it.
		record := inputs + "\n"
		if after, err := backendInputs(src, pkg, l.goVer, mod); err == nil && after != inputs {
			record += after + "\n"
		}
		os.WriteFile(filepath.Join(out, buildInputsFile), []byte(record), 0600)
	}
	return res
}

// buildInputsFile records, beside a Go backend's binary, the fingerprint of what it was
// built from.
const buildInputsFile = "build-inputs"

// backendInputs fingerprints everything a Go build of pkg in src can read: the module files,
// vendor/, and every directory holding Go files with all that lies beneath it (go:embed
// reaches only below its package), along with the package, toolchain, platform and module
// mode. Builds are reproducible (-trimpath, no cgo), so equal fingerprints make the same
// binary, and a version whose backend did not change can reuse the build before it.
func backendInputs(src, pkg, goVersion, mod string) (string, error) {
	goDirs := map[string]bool{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".go") {
			goDirs[filepath.Dir(path)] = true
		}
		return err
	})
	if err != nil {
		return "", err
	}
	input := func(rel, path string) bool {
		if rel == "go.mod" || rel == "go.sum" || rel == "go.work" || rel == "go.work.sum" || strings.HasPrefix(rel, "vendor/") {
			return true
		}
		for dir := filepath.Dir(path); len(dir) >= len(src); dir = filepath.Dir(dir) {
			if goDirs[dir] {
				return true
			}
			if dir == src {
				break
			}
		}
		return false
	}
	h := sha256.New()
	fmt.Fprintf(h, "go build -trimpath -ldflags=-s -w\x00%s\x00%s\x00%s/%s\x00%s\n", goVersion, pkg, runtime.GOOS, runtime.GOARCH, mod)
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		rel = filepath.ToSlash(rel)
		if !input(rel, path) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sum := sha256.New()
		if _, err := io.Copy(sum, f); err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%x\n", rel, sum.Sum(nil))
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// reuseBuild hard-links into out the backend of the newest earlier version built from the
// same inputs, and reports which version that was. Versions pruned, failed or without a Go
// backend have no record and are passed over.
func (l *Local) reuseBuild(req BuildRequest, inputs, out string) (int, bool) {
	for v := req.Version - 1; v >= 1; v-- {
		vdir, err := l.versionDir(req.App, v)
		if err != nil {
			return 0, false
		}
		prev := filepath.Join(vdir, "out")
		b, err := os.ReadFile(filepath.Join(prev, buildInputsFile))
		if err != nil || !slices.Contains(strings.Fields(string(b)), inputs) {
			continue
		}
		os.Remove(filepath.Join(out, "server"))
		if os.Link(filepath.Join(prev, "server"), filepath.Join(out, "server")) != nil {
			return 0, false
		}
		os.WriteFile(filepath.Join(out, buildInputsFile), b, 0600)
		return v, true
	}
	return 0, false
}

// previousServer is the backend binary of the newest earlier version that kept its files,
// or "" when there is none.
func (l *Local) previousServer(req BuildRequest) string {
	for v := req.Version - 1; v >= 1; v-- {
		vdir, err := l.versionDir(req.App, v)
		if err != nil {
			return ""
		}
		if p := filepath.Join(vdir, "out", "server"); fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// sameBytes reports whether two files hold the same bytes.
func sameBytes(a, b string) bool {
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	if err != nil || ia.Size() != ib.Size() {
		return false
	}
	if os.SameFile(ia, ib) {
		return true
	}
	ba, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	bb, err := os.ReadFile(b)
	return err == nil && bytes.Equal(ba, bb)
}

// installBinary checks that a prebuilt backend runs on this system and installs it, sharing
// previous (the version before's installed backend) when the binary did not change.
func installBinary(src, rel, dst, previous string) BuildResult {
	if rel == "" {
		rel = "server/app"
	}
	path := filepath.Join(src, filepath.FromSlash(rel))
	f, err := os.Open(path)
	if err != nil {
		return BuildResult{Error: fmt.Sprintf("no prebuilt backend at %s", rel)}
	}
	defer f.Close()
	res := BuildResult{}
	osName, arch, static, err := identify(f)
	if err != nil {
		return BuildResult{Error: fmt.Sprintf("%s is not an executable for %s/%s: %v", rel, runtime.GOOS, runtime.GOARCH, err)}
	}
	if osName != runtime.GOOS || arch != runtime.GOARCH {
		return BuildResult{Error: fmt.Sprintf("%s is built for %s/%s; this server runs %s/%s (Go: GOOS=%s GOARCH=%s CGO_ENABLED=0; Rust: --target %s)", rel, osName, arch, runtime.GOOS, runtime.GOARCH, runtime.GOOS, runtime.GOARCH, rustTarget())}
	}
	if !static {
		res.Warnings = append(res.Warnings, rel+" is dynamically linked and may not start here; build it statically (CGO_ENABLED=0 for Go, a musl target for Rust)")
	}
	if previous != "" && sameBytes(path, previous) {
		os.Remove(dst)
		if os.Link(previous, dst) == nil {
			res.OK = true
			res.Log = fmt.Sprintf("installed prebuilt %s (%s/%s), unchanged from the version before\n", rel, osName, arch)
			return res
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return BuildResult{Error: err.Error()}
	}
	out, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0700)
	if err != nil {
		return BuildResult{Error: err.Error()}
	}
	if _, err = io.Copy(out, f); err != nil {
		out.Close()
		return BuildResult{Error: err.Error()}
	}
	if err = out.Close(); err != nil {
		return BuildResult{Error: err.Error()}
	}
	if err = os.Rename(dst+".tmp", dst); err != nil {
		return BuildResult{Error: err.Error()}
	}
	res.OK = true
	res.Log = fmt.Sprintf("installed prebuilt %s (%s/%s)\n", rel, osName, arch)
	return res
}

func identify(f *os.File) (osName, arch string, static bool, err error) {
	if e, err := elf.NewFile(f); err == nil {
		defer e.Close()
		arch = map[elf.Machine]string{elf.EM_X86_64: "amd64", elf.EM_AARCH64: "arm64", elf.EM_386: "386", elf.EM_ARM: "arm", elf.EM_RISCV: "riscv64"}[e.Machine]
		static = true
		for _, p := range e.Progs {
			if p.Type == elf.PT_INTERP {
				static = false
			}
		}
		return "linux", arch, static, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", "", false, err
	}
	if m, err := macho.NewFile(f); err == nil {
		defer m.Close()
		arch = map[macho.Cpu]string{macho.CpuAmd64: "amd64", macho.CpuArm64: "arm64"}[m.Cpu]
		return "darwin", arch, true, nil
	}
	return "", "", false, errors.New("not an ELF or Mach-O executable")
}

func rustTarget() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "x86_64-unknown-linux-musl"
	case "linux/arm64":
		return "aarch64-unknown-linux-musl"
	case "darwin/arm64":
		return "aarch64-apple-darwin"
	case "darwin/amd64":
		return "x86_64-apple-darwin"
	}
	return runtime.GOARCH + "-" + runtime.GOOS
}

// unshare gives a hard-linked file its own inode.
func unshare(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0600)
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("… (%d more lines in the build log)", len(lines)-n))
	}
	return strings.Join(lines, "\n")
}
