// Package runner builds and runs app backends. A backend is a native HTTP server (Go,
// built here from source, or any prebuilt static binary such as Rust) started on demand,
// one process per app version, listening on a loopback port shartifacts proxies to.
//
// It runs in one of two places. Local runs processes beside shartifacts itself (a single
// container, or development). In the compose stack a runner sidecar does it: the same
// binary started with -runner, holding the Go toolchain and mounting only the apps
// directory, so backends never see artifacts or the rest of the data folder. shartifacts
// reaches the sidecar through Remote, which speaks the HTTP API Handler serves.
//
// Both read what they need from the apps directory that shartifacts writes:
//
//	{id}/app.json            the app's environment variables (Env)
//	{id}/v/{n}/src/          the version's source, built into
//	{id}/v/{n}/out/server    its backend binary, with
//	{id}/v/{n}/out/runtime.json  how to run it (network access)
//	{id}/data/               the backend's persistent data
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/sandbox"
)

// Info describes the runtime: where backends run and what they may be built from.
type Info struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// Go is the toolchain version that builds Go backends; empty when there is none.
	Go      string         `json:"go,omitempty"`
	Sandbox sandbox.Status `json:"sandbox"`
	// Remote is true when a runner sidecar runs backends.
	Remote bool `json:"remote"`
	// DataQuota is the most disk an app's DATA_DIR may take, in bytes; 0 is no limit.
	DataQuota int64 `json:"data_quota,omitempty"`
	// Error is why the runtime cannot be reached, when it cannot.
	Error string `json:"error,omitempty"`
}

// BuildRequest builds one version's backend into out/server.
type BuildRequest struct {
	App     string `json:"app"`
	Version int    `json:"version"`
	// Kind is "go" (build Package from source) or "binary" (validate and install Binary).
	Kind    string `json:"kind"`
	Package string `json:"package,omitempty"`
	Binary  string `json:"binary,omitempty"`
}

type BuildResult struct {
	OK       bool     `json:"ok"`
	Log      string   `json:"log,omitempty"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// RuntimeConfig is out/runtime.json, written with the build.
type RuntimeConfig struct {
	Network bool `json:"network"`
}

// Logs are a version's recent backend output and process state.
type Logs struct {
	State    string     `json:"state"`
	PID      int        `json:"pid,omitempty"`
	Started  *time.Time `json:"started,omitempty"`
	Requests int64      `json:"requests"`
	// Exit describes how the last process ended.
	Exit  string    `json:"exit,omitempty"`
	Lines []LogLine `json:"lines"`

	ended time.Time
}

type LogLine struct {
	At     time.Time `json:"at"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
}

// Runtime is what shartifacts asks of a runner.
type Runtime interface {
	Info(ctx context.Context) Info
	Build(ctx context.Context, req BuildRequest) BuildResult
	// Start runs a version's backend, if it is not running, and waits until it listens.
	Start(ctx context.Context, app string, version int) error
	// Proxy serves one request to a version's backend, starting it when needed. The
	// request's path is the backend path (/api/...).
	Proxy(w http.ResponseWriter, r *http.Request, app string, version int)
	Logs(ctx context.Context, app string, version int, tail int) (Logs, error)
	// Stop ends a version's process (version 0: every version of the app); the next
	// request starts it again.
	Stop(ctx context.Context, app string, version int) error
}

var (
	appID = regexp.MustCompile(`^[0-9a-f]{24}$`)
	// ErrNoBackend answers requests to a version without a backend.
	ErrNoBackend = errors.New("this version has no backend")
)

// Local runs backends as child processes.
type Local struct {
	// Dir is the apps directory.
	Dir string
	// GoBinary builds Go backends ("" finds go on PATH).
	GoBinary string
	// MaxProcs bounds running backends; the least recently used is stopped to make room.
	MaxProcs int
	// Idle stops a backend that has served nothing for this long.
	Idle time.Duration
	// Remote marks the runner sidecar, for Info.
	Remote bool
	// DataQuota bounds an app's DATA_DIR in bytes (0: no limit). A backend whose data
	// outgrows it is stopped and not started again until the data shrinks or the quota grows.
	DataQuota int64

	once  sync.Once
	mu    sync.Mutex
	procs map[key]*proc
	// logs keep the output of processes that ended, for a day and only for the latest
	// keptLogs versions, until the version starts again.
	logs     map[key]*Logs
	failures map[key][]time.Time
	// dataSize caches what each app's DATA_DIR measured, when DataQuota is set.
	dataSize map[string]measured
	goVer    string
	goPath   string
	stop     chan struct{}
	// sandbox is the confinement backends get; unusable notes why Landlock, though
	// offered, cannot confine executables in Dir.
	sandbox  sandbox.Status
	unusable bool
}

type key struct {
	app     string
	version int
}

type measured struct {
	bytes int64
	at    time.Time
}

const (
	// keptLogs and keepLogsFor bound the output kept from processes that ended: up to a
	// thousand lines each, which across every version ever started would only grow.
	keptLogs    = 32
	keepLogsFor = 24 * time.Hour
	// measureEvery is how often a running app's data is measured against DataQuota.
	measureEvery = 2 * time.Minute
)

type proc struct {
	key      key
	cmd      *exec.Cmd
	port     int
	proxy    *httputil.ReverseProxy
	started  time.Time
	ready    chan struct{}
	err      error // set before ready closes when the process failed to come up
	done     chan struct{}
	lastUsed time.Time
	requests int64
	exit     string
	ring     *ring
}

func (l *Local) init() {
	l.once.Do(func() {
		l.procs, l.logs, l.failures, l.stop = map[key]*proc{}, map[key]*Logs{}, map[key][]time.Time{}, make(chan struct{})
		l.dataSize = map[string]measured{}
		if l.MaxProcs <= 0 {
			l.MaxProcs = 16
		}
		if l.Idle <= 0 {
			l.Idle = 15 * time.Minute
		}
		bin := l.GoBinary
		if bin == "" {
			bin = "go"
		}
		if p, err := exec.LookPath(bin); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, p, "env", "GOVERSION").Output(); err == nil {
				l.goPath, l.goVer = p, strings.TrimSpace(string(out))
			}
		}
		l.sandbox = sandbox.Probe()
		if self, err := os.Executable(); err == nil && l.sandbox.Mechanism != "none" {
			os.MkdirAll(l.Dir, 0700)
			if err := sandbox.Usable(self, l.Dir); err != nil {
				l.unusable = true
				l.sandbox = sandbox.Status{Mechanism: "none", ABI: l.sandbox.ABI, Detail: fmt.Sprintf(
					"Landlock is available but cannot confine programs in %s, so app backends run unconfined: its filesystem loses Landlock's rules, as FUSE mounts do (Docker Desktop shared folders, Unraid's /mnt/user). Keep the apps directory on a local disk path, such as /mnt/cache/appdata on Unraid or a named volume. (%v)", l.Dir, err)}
				log.Printf("apps: %s", l.sandbox.Detail)
			}
		}
		go l.reap()
	})
}

func (l *Local) Info(ctx context.Context) Info {
	l.init()
	return Info{OS: runtime.GOOS, Arch: runtime.GOARCH, Go: l.goVer, Sandbox: l.sandbox, Remote: l.Remote, DataQuota: l.DataQuota}
}

func (l *Local) appDir(app string) (string, error) {
	if !appID.MatchString(app) {
		return "", errors.New("invalid app id")
	}
	return filepath.Join(l.Dir, app), nil
}

func (l *Local) versionDir(app string, version int) (string, error) {
	dir, err := l.appDir(app)
	if err != nil || version < 1 {
		return "", errors.New("invalid app version")
	}
	return filepath.Join(dir, "v", strconv.Itoa(version)), nil
}

func (l *Local) Start(ctx context.Context, app string, version int) error {
	p, err := l.ensure(app, version)
	if err != nil {
		return err
	}
	select {
	case <-p.ready:
		return p.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Local) Proxy(w http.ResponseWriter, r *http.Request, app string, version int) {
	p, err := l.ensure(app, version)
	if err == nil {
		select {
		case <-p.ready:
			err = p.err
		case <-r.Context().Done():
			return
		}
	}
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, ErrNoBackend) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	l.mu.Lock()
	p.lastUsed = time.Now()
	p.requests++
	l.mu.Unlock()
	p.proxy.ServeHTTP(w, r)
}

// keepForwarded carries the X-Forwarded-Host and -Proto that shartifacts set over a proxy
// hop, which drops them otherwise. shartifacts removes any a client sent before it sets
// them, and only token holders reach the runner, so a backend can trust them.
func keepForwarded(pr *httputil.ProxyRequest) {
	for _, h := range []string{"X-Forwarded-Host", "X-Forwarded-Proto"} {
		if v := pr.In.Header.Get(h); v != "" {
			pr.Out.Header.Set(h, v)
		}
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ensure returns the version's process, starting one when none runs.
func (l *Local) ensure(app string, version int) (*proc, error) {
	l.init()
	k := key{app, version}
	if err := l.checkQuota(k); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if p, ok := l.procs[k]; ok {
		return p, nil
	}
	// A backend that keeps dying is left alone for a while instead of restarted per request.
	recent := l.failures[k][:0]
	for _, t := range l.failures[k] {
		if time.Since(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	l.failures[k] = recent
	if len(recent) >= 3 && time.Since(recent[len(recent)-1]) < 30*time.Second {
		msg := "the backend crashed 3 times in a minute; it restarts on the next request after 30 seconds. See its logs."
		if lg := l.logs[k]; lg != nil && lg.Exit != "" {
			msg += " Last exit: " + lg.Exit
		}
		return nil, errors.New(msg)
	}
	if len(l.procs) >= l.MaxProcs {
		var oldest *proc
		for _, p := range l.procs {
			if oldest == nil || p.lastUsed.Before(oldest.lastUsed) {
				oldest = p
			}
		}
		l.kill(oldest, "stopped to make room for another backend")
	}
	p, err := l.spawn(k)
	if err != nil {
		return nil, err
	}
	l.procs[k] = p
	return p, nil
}

// spawn starts a version's backend. l.mu is held.
func (l *Local) spawn(k key) (*proc, error) {
	vdir, err := l.versionDir(k.app, k.version)
	if err != nil {
		return nil, err
	}
	bin := filepath.Join(vdir, "out", "server")
	if _, err := os.Stat(bin); err != nil {
		return nil, ErrNoBackend
	}
	var cfg RuntimeConfig
	if b, err := os.ReadFile(filepath.Join(vdir, "out", "runtime.json")); err == nil {
		json.Unmarshal(b, &cfg)
	}
	appDir, _ := l.appDir(k.app)
	var meta struct {
		Env map[string]string `json:"env"`
	}
	if b, err := os.ReadFile(filepath.Join(appDir, "app.json")); err == nil {
		json.Unmarshal(b, &meta)
	}
	data := filepath.Join(appDir, "data")
	tmp := filepath.Join(data, ".tmp")
	if err := os.MkdirAll(tmp, 0700); err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + data, "TMPDIR=" + tmp, "DATA_DIR=" + data, "HOST=127.0.0.1",
		"PORT=" + strconv.Itoa(port), "APP_ID=" + k.app, "APP_VERSION=" + strconv.Itoa(k.version)}
	if tz := os.Getenv("TZ"); tz != "" {
		env = append(env, "TZ="+tz)
	}
	if l.DataQuota > 0 {
		env = append(env, "DATA_QUOTA="+strconv.FormatInt(l.DataQuota, 10))
	}
	names := make([]string, 0, len(meta.Env))
	for name := range meta.Env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+meta.Env[name])
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	path, argv := bin, []string{bin}
	if !l.unusable {
		if path, argv, env, err = sandbox.Command(self, sandbox.Spec{Exec: bin, ReadOnly: []string{filepath.Join(vdir, "out")}, ReadWrite: []string{data}, BindPort: port, Network: cfg.Network}, env); err != nil {
			return nil, err
		}
	}
	p := &proc{key: k, port: port, started: time.Now(), lastUsed: time.Now(), ready: make(chan struct{}), done: make(chan struct{}), ring: newRing(1000)}
	p.cmd = &exec.Cmd{Path: path, Args: argv, Env: env, Dir: data, Stdout: p.ring.writer("stdout"), Stderr: p.ring.writer("stderr"), SysProcAttr: procAttr()}
	p.ring.add("system", fmt.Sprintf("starting version %d on port %d (sandbox: %s)", k.version, port, l.sandbox.Mechanism))
	if err := p.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start backend: %w", err)
	}
	target := "127.0.0.1:" + strconv.Itoa(port)
	p.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", target
			pr.Out.Host = pr.In.Host
			keepForwarded(pr)
		},
		FlushInterval: -1,
		ModifyResponse: func(res *http.Response) error {
			// shartifacts answers CORS for the sandboxed page; a backend's own headers would
			// duplicate it.
			for _, h := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Expose-Headers"} {
				res.Header.Del(h)
			}
			filterCookies(res)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				writeError(w, http.StatusBadGateway, "backend unavailable: "+err.Error())
			}
		},
	}
	go l.watch(p)
	go l.waitReady(p)
	return p, nil
}

// waitReady closes p.ready once the backend accepts connections, or it exits or times out.
func (l *Local) waitReady(p *proc) {
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			p.err = fmt.Errorf("the backend exited before listening on $PORT (%s). Recent output:\n%s", p.exit, p.ring.tail(15))
			close(p.ready)
			return
		case <-deadline:
			p.err = fmt.Errorf("the backend did not listen on 127.0.0.1:$PORT within 30 seconds. Recent output:\n%s", p.ring.tail(15))
			close(p.ready)
			l.mu.Lock()
			l.kill(p, "did not start listening")
			l.mu.Unlock()
			return
		case <-tick.C:
			if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(p.port), 200*time.Millisecond); err == nil {
				c.Close()
				p.ring.add("system", "listening")
				close(p.ready)
				return
			}
		}
	}
}

// watch records the process's end and forgets it.
func (l *Local) watch(p *proc) {
	err := p.cmd.Wait()
	p.exit = "exited normally"
	if err != nil {
		p.exit = err.Error()
	}
	p.ring.add("system", "process "+p.exit)
	close(p.done)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.procs[p.key] == p {
		delete(l.procs, p.key)
		if !strings.HasPrefix(p.exit, "stopped") {
			l.failures[p.key] = append(l.failures[p.key], time.Now())
		}
	}
	l.logs[p.key] = &Logs{State: "stopped", Exit: p.exit, Requests: p.requests, Lines: p.ring.lines(), ended: time.Now()}
}

// kill stops p: SIGTERM, then SIGKILL after five seconds. l.mu is held.
func (l *Local) kill(p *proc, why string) {
	if p == nil || l.procs[p.key] != p {
		return
	}
	delete(l.procs, p.key)
	p.ring.add("system", why)
	terminate(p.cmd)
	go func() {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			forceKill(p.cmd)
		}
	}()
	// Report the stop, not a crash, when the process ends.
	go func() {
		<-p.done
		l.mu.Lock()
		if lg := l.logs[p.key]; lg != nil {
			lg.Exit = "stopped (" + why + "); " + p.exit
		}
		l.mu.Unlock()
	}()
}

func (l *Local) reap() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			l.mu.Lock()
			for _, p := range l.procs {
				if time.Since(p.lastUsed) > l.Idle {
					l.kill(p, fmt.Sprintf("idle for %s", l.Idle))
				}
			}
			l.forget(time.Now())
			l.mu.Unlock()
			l.enforceQuota()
		}
	}
}

// forget drops what ended processes left behind: output a day old, or beyond the latest
// keptLogs versions, and crash times too old to hold a backend back. l.mu is held.
func (l *Local) forget(now time.Time) {
	var ended []key
	for k, lg := range l.logs {
		if _, running := l.procs[k]; running || now.Sub(lg.ended) > keepLogsFor {
			delete(l.logs, k)
			continue
		}
		ended = append(ended, k)
	}
	if len(ended) > keptLogs {
		sort.Slice(ended, func(i, j int) bool { return l.logs[ended[i]].ended.After(l.logs[ended[j]].ended) })
		for _, k := range ended[keptLogs:] {
			delete(l.logs, k)
		}
	}
	for k, times := range l.failures {
		if len(times) == 0 || now.Sub(times[len(times)-1]) > time.Minute {
			delete(l.failures, k)
		}
	}
	for app, m := range l.dataSize {
		if now.Sub(m.at) > keepLogsFor {
			delete(l.dataSize, app)
		}
	}
}

// checkQuota refuses to start a backend whose app's data outgrew DataQuota. A running
// backend passes without a measurement, so requests do not walk the disk.
func (l *Local) checkQuota(k key) error {
	if l.DataQuota <= 0 {
		return nil
	}
	l.mu.Lock()
	_, running := l.procs[k]
	m, ok := l.dataSize[k.app]
	l.mu.Unlock()
	if running {
		return nil
	}
	// A backend the quota stopped would otherwise stay down until the next measurement,
	// however quickly its data shrank, so starts measure unless one just did.
	if !ok || time.Since(m.at) > 5*time.Second {
		m = l.measure(k.app)
	}
	if m.bytes > l.DataQuota {
		return fmt.Errorf("the app's data takes %s, over its %s quota (SHARTIFACTS_APP_DATA_QUOTA); free some of it or raise the quota", sizeText(m.bytes), sizeText(l.DataQuota))
	}
	return nil
}

// enforceQuota measures the data of running apps every measureEvery and stops the
// backends of any over DataQuota.
func (l *Local) enforceQuota() {
	if l.DataQuota <= 0 {
		return
	}
	apps := map[string]bool{}
	l.mu.Lock()
	for k := range l.procs {
		if time.Since(l.dataSize[k.app].at) >= measureEvery {
			apps[k.app] = true
		}
	}
	l.mu.Unlock()
	for app := range apps {
		if m := l.measure(app); m.bytes > l.DataQuota {
			l.mu.Lock()
			for k, p := range l.procs {
				if k.app == app {
					l.kill(p, fmt.Sprintf("stopped: the app's data takes %s, over its %s quota", sizeText(m.bytes), sizeText(l.DataQuota)))
				}
			}
			l.mu.Unlock()
		}
	}
}

// measure totals an app's DATA_DIR, outside l.mu, and caches the figure.
func (l *Local) measure(app string) measured {
	m := measured{at: time.Now()}
	if dir, err := l.appDir(app); err == nil {
		m.bytes = dirSize(filepath.Join(dir, "data"))
	}
	l.mu.Lock()
	l.dataSize[app] = m
	l.mu.Unlock()
	return m
}

// dirSize totals the regular files under dir; what cannot be read counts as nothing.
func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// sizeText writes a byte count the way people read one: 1.5 GiB.
func sizeText(n int64) string {
	units := []string{"bytes", "KiB", "MiB", "GiB", "TiB"}
	f, u := float64(n), 0
	for f >= 1024 && u < len(units)-1 {
		f /= 1024
		u++
	}
	if u == 0 {
		return fmt.Sprintf("%d bytes", n)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0") + " " + units[u]
}

func (l *Local) Stop(ctx context.Context, app string, version int) error {
	l.init()
	l.mu.Lock()
	var waits []chan struct{}
	for k, p := range l.procs {
		if k.app == app && (version == 0 || k.version == version) {
			l.kill(p, "stopped")
			waits = append(waits, p.done)
		}
	}
	for k := range l.failures {
		if k.app == app && (version == 0 || k.version == version) {
			delete(l.failures, k)
		}
	}
	l.mu.Unlock()
	for _, w := range waits {
		select {
		case <-w:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (l *Local) Logs(ctx context.Context, app string, version int, tail int) (Logs, error) {
	l.init()
	if _, err := l.versionDir(app, version); err != nil {
		return Logs{}, err
	}
	if tail <= 0 || tail > 1000 {
		tail = 200
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	k := key{app, version}
	var out Logs
	if p, ok := l.procs[k]; ok {
		started := p.started
		out = Logs{State: "starting", PID: p.cmd.Process.Pid, Started: &started, Requests: p.requests, Lines: p.ring.lines()}
		select {
		case <-p.ready:
			if p.err == nil {
				out.State = "running"
			}
		default:
		}
	} else if lg, ok := l.logs[k]; ok {
		out = *lg
	} else {
		out = Logs{State: "not started", Lines: []LogLine{}}
	}
	if len(out.Lines) > tail {
		out.Lines = out.Lines[len(out.Lines)-tail:]
	}
	return out, nil
}

// Close stops every backend.
func (l *Local) Close() {
	l.init()
	close(l.stop)
	l.mu.Lock()
	var waits []chan struct{}
	for _, p := range l.procs {
		l.kill(p, "shartifacts stopped")
		waits = append(waits, p.done)
	}
	l.mu.Unlock()
	for _, w := range waits {
		select {
		case <-w:
		case <-time.After(6 * time.Second):
		}
	}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

func forceKill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
