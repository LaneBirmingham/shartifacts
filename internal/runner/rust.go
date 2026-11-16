package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/sandbox"
)

// RustBuildTimeout bounds one Rust backend build. A cold build compiles every crate the app
// depends on (an Axum server is a hundred or so) in release mode, which takes minutes on a
// small server; later builds reuse the compiled crates and take seconds.
const RustBuildTimeout = 15 * time.Minute

// rustTargetLimit is how large an app's Cargo target directory may grow before a build
// starts it afresh. Cargo never deletes what it compiled for dependency versions an app has
// moved on from, so without a bound every upgrade would add to it for good.
const rustTargetLimit = 3 << 30

// rustToolchain is the Rust compiler that builds Rust backends, found once at start.
type rustToolchain struct {
	// cargo and rustc are the toolchain's own binaries, not rustup's proxies, so a build
	// needs no rustup settings and the toolchain is the one this server reported.
	cargo, rustc string
	// sysroot holds the standard library and the binaries; it is readable in confined builds.
	sysroot string
	version string
	// verbose is rustc -vV, which identifies the compiler exactly for build fingerprints.
	verbose string
	target  string
	// missing says why Rust backends cannot build although a toolchain is here.
	missing string
}

// findRust looks for cargo (bin, or "cargo" on PATH) and the rustc beside it, and checks
// that it can build what backends need: a static binary for this system. On Linux that is
// the musl target, which links the C library into the binary, so backends start in a
// confined process that cannot read the system's shared libraries.
func findRust(bin string) rustToolchain {
	if bin == "" {
		bin = "cargo"
	}
	cargo, err := exec.LookPath(bin)
	if err != nil {
		return rustToolchain{}
	}
	run := func(name string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, name, args...).Output()
		return strings.TrimSpace(string(out)), err
	}
	rustc := filepath.Join(filepath.Dir(cargo), "rustc")
	if _, err := os.Stat(rustc); err != nil {
		if rustc, err = exec.LookPath("rustc"); err != nil {
			return rustToolchain{missing: "cargo is installed but rustc is not"}
		}
	}
	tc := rustToolchain{cargo: cargo, rustc: rustc}
	if tc.sysroot, err = run(rustc, "--print", "sysroot"); err != nil {
		return rustToolchain{missing: fmt.Sprintf("%s does not run: %v", rustc, err)}
	}
	// Under rustup, cargo and rustc on PATH are proxies that pick a toolchain from rustup's
	// settings in the caller's home. Builds run with a home of their own, so use the
	// toolchain's binaries directly.
	for _, name := range []string{"cargo", "rustc"} {
		if p := filepath.Join(tc.sysroot, "bin", name); fileExists(p) {
			if name == "cargo" {
				tc.cargo = p
			} else {
				tc.rustc = p
			}
		}
	}
	if tc.verbose, err = run(tc.rustc, "-vV"); err != nil {
		return rustToolchain{missing: fmt.Sprintf("%s does not run: %v", tc.rustc, err)}
	}
	host := ""
	for _, line := range strings.Split(tc.verbose, "\n") {
		if v, ok := strings.CutPrefix(line, "release: "); ok {
			tc.version = v
		}
		if v, ok := strings.CutPrefix(line, "host: "); ok {
			host = v
		}
	}
	tc.target = host
	if runtime.GOOS == "linux" {
		tc.target = rustTarget()
		dir, err := run(tc.rustc, "--print", "target-libdir", "--target", tc.target)
		if err != nil || !dirExists(dir) {
			tc.missing = fmt.Sprintf("Rust %s cannot build static Linux binaries without its %s target: rustup target add %s", tc.version, tc.target, tc.target)
		}
	}
	return tc
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// cargoMessage is the part of cargo's JSON output that names what a build produced.
type cargoMessage struct {
	Reason    string `json:"reason"`
	PackageID string `json:"package_id"`
	Target    struct {
		Name string   `json:"name"`
		Kind []string `json:"kind"`
	} `json:"target"`
	Executable string `json:"executable"`
}

// rustBinName is what backend.bin may be: a Cargo binary target's name.
var rustBinName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$`)

// rustBuild is one Rust build's settings, shared by the cargo steps it runs.
type rustBuild struct {
	l     *Local
	tc    rustToolchain
	src   string // the version's source tree, readable only
	real  string // src with symlinks resolved, as cargo names the paths it reports
	crate string // the directory holding the backend's Cargo.toml
	// home (CARGO_HOME: downloaded crates, git checkouts, cargo's settings) and tgt
	// (CARGO_TARGET_DIR: compiled crates) are this app's alone. Build scripts may write
	// both, so an app shared with another could plant code there that the other's builds
	// would run, outside its own source.
	home, tgt string
	tmp       string // HOME and TMPDIR for this build
}

// rustStep is what one cargo step may do besides reading the source and toolchain.
type rustStep struct {
	// lock steps (generate-lockfile, update, fetch) run only cargo, so they get the
	// operator's registry credentials and no app-configured rustc wrapper. The build runs
	// the app's code and its crates' build scripts, so it gets the wrapper and no credentials.
	lock bool
	// writable is what the step may write besides the app's cache and its temporary
	// directory: the workspace root, for the steps that write Cargo.lock.
	writable []string
	// jobs is CARGO_BUILD_JOBS for the build, when the operator set none.
	jobs int
}

func (l *Local) buildRust(ctx context.Context, req BuildRequest, src, out string) BuildResult {
	tc := l.rust
	if tc.cargo == "" || tc.missing != "" {
		reason := tc.missing
		if reason == "" {
			reason = "no Rust toolchain on this server"
			if !l.Remote {
				reason += "; run the runner sidecar (see the README) or deploy a prebuilt binary"
			}
		}
		return BuildResult{Error: reason}
	}
	dir := req.Package
	if dir == "" {
		dir = "./server"
	}
	clean := path.Clean(dir)
	if (dir != "." && !strings.HasPrefix(dir, "./")) || strings.HasPrefix(clean, "../") || clean == ".." || strings.Contains(dir, "\\") {
		return BuildResult{Error: "the Rust crate must be a directory inside the app, like ./server"}
	}
	if req.Bin != "" && !rustBinName.MatchString(req.Bin) {
		return BuildResult{Error: fmt.Sprintf("backend.bin %q is not a binary target's name", req.Bin)}
	}
	shown := strings.TrimPrefix(clean+"/", "./")
	if clean == "." {
		shown = "the app's top directory"
	}
	b := &rustBuild{l: l, tc: tc, src: src, real: src, crate: filepath.Join(src, filepath.FromSlash(clean))}
	if !fileExists(filepath.Join(b.crate, "Cargo.toml")) {
		return BuildResult{Error: "no Cargo.toml in " + shown}
	}
	if r, err := filepath.EvalSymlinks(src); err == nil {
		b.real = r
	}
	cache := l.cacheDir()
	b.home = filepath.Join(cache, "rust", req.App, "home")
	b.tgt = filepath.Join(cache, "rust", req.App, "target")
	for _, d := range []string{b.home, b.tgt, filepath.Join(cache, "tmp")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return BuildResult{Error: err.Error()}
		}
	}
	var err error
	if b.tmp, err = os.MkdirTemp(filepath.Join(cache, "tmp"), "rust-"); err != nil {
		return BuildResult{Error: err.Error()}
	}
	defer os.RemoveAll(b.tmp)
	l.pruneRustCaches(req.App)

	res := BuildResult{}
	cmdline := "cargo build --release --frozen --target " + tc.target
	if req.Bin != "" {
		cmdline += " --bin " + req.Bin
	}
	header := fmt.Sprintf("$ %s  (in %s; Rust %s, %s)\n", cmdline, shown, tc.version, b.confinement())
	if p := l.rustExposes(); p != "" {
		res.Warnings = append(res.Warnings, fmt.Sprintf("the apps directory is inside %s, which Rust build scripts may read, so a build can read other apps' files: move the apps directory out of it", p))
	}
	inputs, err := rustInputs(src, tc.verbose, tc.target, clean, req.Bin)
	if err != nil {
		return BuildResult{Error: err.Error()}
	}
	if from, ok := l.reuseRustBuild(req, src, inputs, out); ok {
		res.OK = true
		res.Log = header + fmt.Sprintf("the backend is built from the same files as version %d's: reused that build\n", from)
		return res
	}
	var log strings.Builder
	log.WriteString(header)
	ctx, cancel := context.WithTimeout(ctx, RustBuildTimeout)
	defer cancel()
	select {
	case l.rustBuilds <- struct{}{}:
	default:
		log.WriteString("waiting for another app's Rust build to finish\n")
		select {
		case l.rustBuilds <- struct{}{}:
		case <-ctx.Done():
			res.Error, res.Log = b.failure(ctx, "waiting for another Rust build", ctx.Err(), ""), log.String()
			return res
		}
	}
	defer func() { <-l.rustBuilds }()

	root, err := b.workspaceRoot(ctx)
	if err != nil {
		res.Error, res.Log = err.Error(), log.String()
		return res
	}
	lock := filepath.Join(root, "Cargo.lock")
	lockRel := "Cargo.lock"
	if rel, err := filepath.Rel(b.real, lock); err == nil {
		lockRel = filepath.ToSlash(rel)
	}
	lockStep := rustStep{lock: true, writable: []string{root}}
	// Without a Cargo.lock cargo resolves the newest compatible versions and writes one into
	// the source tree. It is the version's own new file (only files a deploy carried over
	// are hard links shared with other versions), so later deploys that keep it build the
	// same crates.
	if !fileExists(lock) {
		res.Warnings = append(res.Warnings, "no "+lockRel+": built the newest compatible crates; deploy your Cargo.lock to pin them")
		lockOut, err := b.run(ctx, lockStep, nil, "generate-lockfile")
		log.WriteString(lockOut)
		if err != nil {
			res.Error, res.Log = b.failure(ctx, "cargo generate-lockfile", err, lockOut), log.String()
			return res
		}
	}
	// Downloads happen before the build, so only cargo itself talks to registries, and the
	// build runs --frozen: exactly the crates Cargo.lock names, from what was fetched.
	fetch := []string{"fetch", "--locked", "--target", tc.target}
	if host := tc.host(); host != "" && host != tc.target {
		// Build scripts and procedural macros are compiled for the machine building them.
		fetch = append(fetch, "--target", host)
	}
	fetchOut, err := b.run(ctx, rustStep{lock: true}, nil, fetch...)
	if err != nil && strings.Contains(fetchOut, "--locked was passed") {
		// The Cargo.lock deployed predates a change to Cargo.toml (a crate added or its
		// version raised). Updating the workspace resolves only what changed and leaves every
		// other crate at the version the lock names. Cargo.lock may be a hard link that
		// earlier versions share, so it becomes this version's own file first.
		log.WriteString(fetchOut)
		if err := unshare(lock); err != nil {
			res.Error, res.Log = err.Error(), log.String()
			return res
		}
		updOut, updErr := b.run(ctx, lockStep, nil, "update", "--workspace")
		log.WriteString(updOut)
		if updErr != nil {
			res.Error, res.Log = b.failure(ctx, "cargo update", updErr, updOut), log.String()
			return res
		}
		res.Warnings = append(res.Warnings, lockRel+" did not match Cargo.toml, so cargo updated it for what Cargo.toml changed; deploy the new Cargo.lock to keep it")
		fetchOut, err = b.run(ctx, rustStep{lock: true}, nil, fetch...)
	}
	log.WriteString(fetchOut)
	if err != nil {
		res.Error, res.Log = b.failure(ctx, "cargo fetch", err, fetchOut), log.String()
		return res
	}

	if size := dirSize(b.tgt); size > rustTargetLimit {
		os.RemoveAll(b.tgt)
		os.MkdirAll(b.tgt, 0700)
		fmt.Fprintf(&log, "the app's compiled crates had grown to %s: starting them afresh\n", sizeText(size))
	}
	args := []string{"build", "--release", "--frozen", "--target", tc.target, "--message-format=json-render-diagnostics"}
	if req.Bin != "" {
		args = append(args, "--bin", req.Bin)
	}
	step := rustStep{}
	if os.Getenv("CARGO_BUILD_JOBS") == "" {
		step.jobs = rustJobs()
		fmt.Fprintf(&log, "compiling %d crate%s at a time\n", step.jobs, map[bool]string{true: "", false: "s"}[step.jobs == 1])
	}
	var stdout bytes.Buffer
	started := time.Now()
	buildOut, err := b.run(ctx, step, &stdout, args...)
	log.WriteString(buildOut)
	if err != nil {
		res.Error, res.Log = b.failure(ctx, "cargo build", err, buildOut), log.String()
		return res
	}
	exe, name, err := pickBinary(&stdout, req.Bin, b.tgt, b.real)
	if err != nil {
		res.Error, res.Log = err.Error(), log.String()
		return res
	}
	// out/server may be a hard link another version shares, and the executable in the
	// target directory is rewritten by the next build: copy it to a file of this version's.
	dst := filepath.Join(out, "server")
	os.Remove(dst)
	if err := copyExecutable(exe, dst); err != nil {
		res.Error, res.Log = err.Error(), log.String()
		return res
	}
	info, _ := os.Stat(dst)
	fmt.Fprintf(&log, "built %s (%s) in %.1fs\n", name, sizeText(info.Size()), time.Since(started).Seconds())
	if f, err := os.Open(dst); err == nil {
		if _, _, static, err := identify(f); err == nil && !static {
			res.Warnings = append(res.Warnings, name+" is dynamically linked, so it may not start in the sandbox; drop the crates or build settings that link it against system libraries")
		}
		f.Close()
	}
	// The record holds the inputs as deployed and, when cargo wrote Cargo.lock, as built,
	// the same way a Go build's does. Files the compiler read beyond the ones fingerprinted
	// by name (include_str! of a file above the crate, say) are listed beside it, and their
	// contents join each fingerprint, so a change to one rebuilds.
	extras := depInfoExtras(exe, b.real, src)
	sum, err := extrasSum(src, extras)
	if err != nil {
		sum = ""
	}
	record := inputs + sum + "\n"
	if after, err := rustInputs(src, tc.verbose, tc.target, clean, req.Bin); err == nil && after != inputs {
		record += after + sum + "\n"
	}
	os.WriteFile(filepath.Join(out, buildInputsFile), []byte(record), 0600)
	if len(extras) > 0 && sum != "" {
		os.WriteFile(filepath.Join(out, rustExtrasFile), []byte(strings.Join(extras, "\n")+"\n"), 0600)
	}
	res.OK = true
	res.Log = log.String()
	return res
}

// host is the platform rustc runs on, from rustc -vV.
func (tc rustToolchain) host() string {
	for _, line := range strings.Split(tc.verbose, "\n") {
		if v, ok := strings.CutPrefix(line, "host: "); ok {
			return v
		}
	}
	return ""
}

// confinement describes how cargo runs, for the build log.
func (b *rustBuild) confinement() string {
	if b.l.sandbox.Mechanism == "landlock" && !b.l.unusable {
		return "build scripts confined to the source, the toolchain and the app's build cache"
	}
	return "unconfined"
}

// failure is the error a failed cargo step reports: the timeout, or where cargo says what failed.
func (b *rustBuild) failure(ctx context.Context, what string, err error, out string) string {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("the Rust build took longer than %s, waiting for other Rust builds included", RustBuildTimeout)
	}
	if out = strings.TrimSpace(out); out == "" {
		return what + " failed: " + err.Error()
	}
	// The end of cargo's output is where it says what failed; the start is crates compiling.
	lines := strings.Split(out, "\n")
	errAt := slices.IndexFunc(lines, func(s string) bool { return strings.HasPrefix(s, "error") })
	if errAt < 0 {
		errAt = max(0, len(lines)-12)
	}
	msg := what + " failed: " + firstLines(strings.Join(lines[errAt:], "\n"), 16)
	// The kernel ends a compiler that runs out of memory with SIGKILL, which cargo reports
	// as the signal and no more.
	if strings.Contains(out, "SIGKILL") {
		msg += "\na compiler was killed, most likely for running out of memory: give the runner more (SHARTIFACTS_RUNNER_MEMORY) or compile fewer crates at once (CARGO_BUILD_JOBS)"
	}
	return msg
}

// workspaceRoot is the directory holding the Cargo.lock of the crate's workspace: the crate
// itself, or a workspace above it inside the app.
func (b *rustBuild) workspaceRoot(ctx context.Context) (string, error) {
	var stdout bytes.Buffer
	out, err := b.run(ctx, rustStep{lock: true}, &stdout, "locate-project", "--workspace", "--message-format", "plain")
	if err != nil {
		return "", fmt.Errorf("cargo cannot read %s: %s", filepath.Join(filepath.Base(b.crate), "Cargo.toml"), firstLines(out, 12))
	}
	root := filepath.Dir(strings.TrimSpace(stdout.String()))
	if !within(b.real, root) {
		return "", fmt.Errorf("the crate's workspace root %s is outside the app", root)
	}
	return root, nil
}

// within reports whether p is dir or beneath it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && filepath.IsLocal(rel)
}

// run runs cargo in the crate directory and returns what it wrote to stderr (and to stdout,
// unless the caller takes stdout). Where Landlock works it runs confined, because a Rust
// build runs code from the app and from every crate it uses (build scripts, procedural
// macros): that code may read the app's source, the toolchain and the system files that
// compiling and linking need, write only the app's build cache and step.writable, and open
// connections. It cannot reach other apps, their data, their builds or the runner's token.
func (b *rustBuild) run(ctx context.Context, step rustStep, stdout io.Writer, args ...string) (string, error) {
	env := b.env(step)
	exe, argv := b.tc.cargo, append([]string{b.tc.cargo}, args...)
	if b.l.sandbox.Mechanism == "landlock" && !b.l.unusable {
		self, err := os.Executable()
		if err != nil {
			return "", err
		}
		spec := sandbox.Spec{Exec: b.tc.cargo, Args: args, ReadOnly: b.readable(), ReadWrite: append([]string{b.home, b.tgt, b.tmp}, step.writable...), Network: true}
		if exe, argv, env, err = sandbox.Command(self, spec, env); err != nil {
			return "", err
		}
	}
	var stderr bytes.Buffer
	cmd := &exec.Cmd{Path: exe, Args: argv, Env: env, Dir: b.crate, Stderr: &stderr, SysProcAttr: procAttr()}
	cmd.Stdout = stdout
	if stdout == nil {
		cmd.Stdout = &stderr
	}
	// Cargo runs rustc, the linker and build scripts as its children, all in its process
	// group: a build cut short ends every one of them, and none keeps the output open.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return "", err
	}
	stop := context.AfterFunc(ctx, func() { killGroup(cmd) })
	defer stop()
	err := cmd.Wait()
	// Nor does anything a build script left running outlive the step, to change the target
	// directory after shartifacts reads it.
	killGroup(cmd)
	return stderr.String(), err
}

// env is a cargo step's environment: the build's own directories, and the operator's
// registry and network settings (mirrors, proxies, an offline server).
func (b *rustBuild) env(step rustStep) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + b.tmp, "TMPDIR=" + b.tmp,
		"CARGO_HOME=" + b.home, "CARGO_TARGET_DIR=" + b.tgt, "RUSTC=" + b.tc.rustc,
		"CARGO_TERM_COLOR=never", "CARGO_TERM_PROGRESS_WHEN=never",
	}
	if step.lock {
		// The environment outranks the app's .cargo/config.toml, so no program the app names
		// runs while the workspace root is writable or credentials are in reach.
		env = append(env, "RUSTC_WRAPPER=", "RUSTC_WORKSPACE_WRAPPER=")
	}
	if step.jobs > 0 {
		env = append(env, fmt.Sprintf("CARGO_BUILD_JOBS=%d", step.jobs))
	}
	// The operator's settings pass through, but credentials only to the steps that run
	// nothing but cargo: the build runs code from the app and its crates.
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		secret := strings.Contains(upper, "TOKEN") || strings.Contains(upper, "CREDENTIAL") || strings.Contains(upper, "PASSWORD")
		if secret && !step.lock {
			continue
		}
		switch {
		case slices.Contains([]string{"CARGO_BUILD_JOBS", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "TZ"}, name),
			strings.HasPrefix(name, "CARGO_NET_"), strings.HasPrefix(name, "CARGO_HTTP_"),
			strings.HasPrefix(name, "CARGO_REGISTRIES_"), strings.HasPrefix(name, "CARGO_REGISTRY_"):
			env = append(env, kv)
		}
	}
	return env
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// readable is what a confined cargo step may read (and run) besides what it may write.
func (b *rustBuild) readable() []string {
	return append([]string{b.src, b.tc.sysroot, filepath.Dir(b.tc.cargo), filepath.Dir(b.tc.rustc)}, buildSystemPaths...)
}

// buildSystemPaths are what a confined build may read (and run) besides the source and the
// toolchain: the C compiler and linker that Rust links with and the libraries and headers
// they use, the dynamic loader's settings, and git's (for git dependencies). The sandbox
// adds TLS roots, name resolution and time zones. The rest of /etc, and places such as
// /opt and /srv where a server's own settings and data live, stay out of reach.
var buildSystemPaths = []string{
	"/usr", "/lib", "/lib64", "/bin", "/sbin",
	"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d", "/etc/ld-musl-x86_64.path", "/etc/ld-musl-aarch64.path",
	"/etc/gitconfig", "/etc/alternatives",
}

// rustExposes names the build-readable path that holds the apps directory, if one does:
// then a build script could read every app's files.
func (l *Local) rustExposes() string {
	dir := l.Dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	b := &rustBuild{tc: l.rust}
	for _, p := range b.readable() {
		real := p
		if r, err := filepath.EvalSymlinks(p); err == nil {
			real = r
		} else {
			continue
		}
		if within(real, dir) {
			return p
		}
	}
	return ""
}

// rustJobs is how many crates a build compiles at once: one per CPU, and no more than the
// memory this process may use allows at about 512 MiB a compiler, since a release build of
// a large crate takes that much and the kernel kills compilers past the limit.
func rustJobs() int {
	n := runtime.NumCPU()
	if limit := memoryLimit(); limit > 0 {
		n = min(n, int(max(1, limit/(512<<20))))
	}
	return max(1, n)
}

// memoryLimit is this process's cgroup memory limit in bytes, or 0 when there is none.
func memoryLimit() int64 {
	for _, f := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || n <= 0 || n >= 1<<60 { // "max", or cgroup v1's "unlimited"
			return 0
		}
		return n
	}
	return 0
}

// pickBinary finds, in cargo's JSON output, the executable the backend is: the app's one
// binary target, or the one named bin. Binaries of crates from a registry or git are tools
// the build used, not the app. Build scripts and procedural macros can write to that output
// too, so only an executable in the target directory, of a package in the app's source,
// counts: the runner copies it out of the build's reach, and must copy nothing else.
func pickBinary(stdout io.Reader, bin, tgt, src string) (exe, name string, err error) {
	if r, err := filepath.EvalSymlinks(tgt); err == nil {
		tgt = r
	}
	found := map[string]string{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var m cargoMessage
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Reason != "compiler-artifact" || m.Executable == "" || !slices.Contains(m.Target.Kind, "bin") {
			continue
		}
		u, err := url.Parse(strings.TrimPrefix(m.PackageID, "path+"))
		if !strings.HasPrefix(m.PackageID, "path+file://") || err != nil || !within(src, filepath.FromSlash(u.Path)) {
			continue
		}
		real, err := filepath.EvalSymlinks(m.Executable)
		if err != nil || !within(tgt, real) {
			continue
		}
		if info, err := os.Lstat(real); err != nil || !info.Mode().IsRegular() {
			continue
		}
		found[m.Target.Name] = real
	}
	if err := sc.Err(); err != nil {
		return "", "", err
	}
	names := slices.Sorted(maps.Keys(found))
	switch {
	case bin != "" && found[bin] != "":
		return found[bin], bin, nil
	case bin != "":
		return "", "", fmt.Errorf("the build made no binary named %s", bin)
	case len(names) == 1:
		return found[names[0]], names[0], nil
	case len(names) == 0:
		return "", "", fmt.Errorf("the crate has no binary target: give it src/main.rs, or a [[bin]] in Cargo.toml")
	}
	return "", "", fmt.Errorf("the crate builds %d binaries (%s): name the backend with \"backend\": {\"rust\": \"./server\", \"bin\": \"%s\"} in shartifacts.json", len(names), strings.Join(names, ", "), names[0])
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0700)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	return os.Rename(dst+".tmp", dst)
}

// rustSources lists the files of src a Rust build reads by rule: every Cargo.toml and
// Cargo.lock, cargo's settings (.cargo/), vendor/, toolchain files, every .rs file, and
// everything beneath each crate (a directory with a Cargo.toml: its sources, build.rs, and
// what include_str!, sqlx's migrations and the like read). The frontend is no input unless
// a crate holds it. A target directory beside a Cargo.toml (deployed by mistake) and .git
// are never read, nor node_modules outside a crate. The paths are relative, with slashes.
func rustSources(src string) ([]string, error) {
	skip := func(p, name string, crates map[string]bool) bool {
		switch name {
		case ".git":
			return true
		case "target":
			return fileExists(filepath.Join(filepath.Dir(p), "Cargo.toml"))
		case "node_modules":
			return crates == nil || !underCrate(src, p, crates)
		}
		return false
	}
	crates := map[string]bool{}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != src && skip(p, d.Name(), nil) {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "Cargo.toml" {
			crates[filepath.Dir(p)] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var files []string
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != src && skip(p, d.Name(), crates) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		rel = filepath.ToSlash(rel)
		name := path.Base(rel)
		if top, _, _ := strings.Cut(rel, "/"); top == "vendor" || strings.Contains("/"+rel, "/.cargo/") ||
			name == "Cargo.toml" || name == "Cargo.lock" || name == "rust-toolchain" || name == "rust-toolchain.toml" ||
			strings.HasSuffix(name, ".rs") || underCrate(src, p, crates) {
			files = append(files, rel)
		}
		return nil
	})
	return files, err
}

// underCrate reports whether p lies beneath one of the crate directories.
func underCrate(src, p string, crates map[string]bool) bool {
	for dir := filepath.Dir(p); len(dir) >= len(src); dir = filepath.Dir(dir) {
		if crates[dir] {
			return true
		}
		if dir == src {
			break
		}
	}
	return false
}

// rustInputs fingerprints the files rustSources lists, with the compiler, the target, the
// crate's directory and the binary. Equal fingerprints build the same backend, so a version
// whose backend did not change reuses the build before it.
func rustInputs(src, compiler, target, crate, bin string) (string, error) {
	files, err := rustSources(src)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "cargo build --release --frozen\x00%s\x00%s\x00%s\x00%s\n", compiler, target, crate, bin)
	for _, rel := range files {
		sum, err := fileSum(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%s\n", rel, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSum(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// rustExtrasFile lists, beside a Rust build's record, the source files the compiler read
// that rustSources does not name.
const rustExtrasFile = "rust-extras"

// depInfoExtras reads the dep-info cargo wrote beside the executable (every file the
// compiler read for the app's crates) and returns those inside the app's source that
// rustSources does not already fingerprint, relative and sorted.
func depInfoExtras(exe, real, src string) []string {
	b, err := os.ReadFile(strings.TrimSuffix(exe, ".exe") + ".d")
	if err != nil {
		return nil
	}
	named, err := rustSources(src)
	if err != nil {
		return nil
	}
	known := map[string]bool{}
	for _, f := range named {
		known[f] = true
	}
	extras := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		_, deps, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		// Make escapes the spaces inside paths with a backslash.
		for _, dep := range strings.Fields(strings.ReplaceAll(deps, `\ `, "\x00")) {
			dep = strings.ReplaceAll(dep, "\x00", " ")
			rel, err := filepath.Rel(real, dep)
			if err != nil || !filepath.IsLocal(rel) {
				continue
			}
			if rel = filepath.ToSlash(rel); !known[rel] && !strings.Contains(rel, "\n") && fileExists(filepath.Join(src, rel)) {
				extras[rel] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(extras))
}

// extrasSum fingerprints the extra files' contents ("" when there are none), to append to
// the inputs: "+" and the hash.
func extrasSum(src string, extras []string) (string, error) {
	if len(extras) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, rel := range extras {
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return "", fmt.Errorf("%s is outside the app", rel)
		}
		sum, err := fileSum(filepath.Join(src, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			sum = "missing"
		} else if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%s\n", rel, sum)
	}
	return "+" + hex.EncodeToString(h.Sum(nil)), nil
}

// reuseRustBuild is reuseBuild for Rust: an earlier version's binary serves when its record
// holds these inputs together with the current contents of the extra files it lists.
func (l *Local) reuseRustBuild(req BuildRequest, src, inputs, out string) (int, bool) {
	for v := req.Version - 1; v >= 1; v-- {
		vdir, err := l.versionDir(req.App, v)
		if err != nil {
			return 0, false
		}
		prev := filepath.Join(vdir, "out")
		record, err := os.ReadFile(filepath.Join(prev, buildInputsFile))
		if err != nil {
			continue
		}
		listed, _ := os.ReadFile(filepath.Join(prev, rustExtrasFile))
		sum, err := extrasSum(src, strings.Fields(string(listed)))
		if err != nil || !slices.Contains(strings.Fields(string(record)), inputs+sum) {
			continue
		}
		os.Remove(filepath.Join(out, "server"))
		if os.Link(filepath.Join(prev, "server"), filepath.Join(out, "server")) != nil {
			return 0, false
		}
		os.WriteFile(filepath.Join(out, buildInputsFile), record, 0600)
		if len(listed) > 0 {
			os.WriteFile(filepath.Join(out, rustExtrasFile), listed, 0600)
		}
		return v, true
	}
	return 0, false
}

// pruneRustCaches removes the build caches of apps that no longer exist. It runs only when
// the cache is this apps directory's own: a cache shared with other directories (as in
// tests) holds apps this one does not know.
func (l *Local) pruneRustCaches(keep string) {
	cache := l.cacheDir()
	if l.CacheDir != "" && !strings.HasPrefix(cache, filepath.Clean(l.Dir)+string(filepath.Separator)) {
		return
	}
	entries, err := os.ReadDir(filepath.Join(cache, "rust"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == keep || !appID.MatchString(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(l.Dir, e.Name())); os.IsNotExist(err) {
			os.RemoveAll(filepath.Join(cache, "rust", e.Name()))
		}
	}
}
