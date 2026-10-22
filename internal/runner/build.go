package runner

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
		return installBinary(src, req.Binary, filepath.Join(out, "server"))
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
	err := cmd.Run()
	res.Log = fmt.Sprintf("$ go build %s %s  (%s, %s/%s)\n%s", mod, pkg, l.goVer, runtime.GOOS, runtime.GOARCH, buf.String())
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.Error = fmt.Sprintf("the Go build took longer than %s", BuildTimeout)
	case err != nil:
		res.Error = "go build failed: " + firstLines(buf.String(), 12)
	default:
		res.OK = true
		res.Log += fmt.Sprintf("built in %.1fs\n", time.Since(started).Seconds())
	}
	return res
}

// installBinary checks that a prebuilt backend runs on this system and installs it.
func installBinary(src, rel, dst string) BuildResult {
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
