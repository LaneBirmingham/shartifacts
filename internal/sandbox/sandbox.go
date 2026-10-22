// Package sandbox confines an app's backend process before it starts. On Linux it uses
// Landlock, which an unprivileged process can apply to itself: shartifacts re-executes
// itself as a small trampoline (Trampoline), restricts that one thread, and then execs the
// app binary, which inherits the restrictions and cannot lift them. The app may read its
// own build, read and write its data directory, bind only its assigned TCP port, and, unless
// the app asked for network access, open no TCP connections. On other systems, or kernels
// without Landlock, apps run unconfined and Probe says so.
//
// Landlock is a second line of defence for code you deployed yourself, not a boundary for
// hostile code: UDP, abstract sockets on older kernels and anything the kernel does not
// mediate stay open.
package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Arg is the argv[1] that turns the shartifacts binary into the trampoline; OKArg makes it
// exit at once, which Usable runs confined to prove confinement works.
const (
	Arg   = "__app-sandbox"
	OKArg = "__app-sandbox-ok"
)

// specEnv carries the Spec to the trampoline; it is removed before the app starts.
const specEnv = "SHARTIFACTS_SANDBOX_SPEC"

// Spec is what one app process may touch.
type Spec struct {
	// Exec is the binary to run; Args are its arguments after argv[0].
	Exec string   `json:"exec"`
	Args []string `json:"args,omitempty"`
	// ReadOnly paths may be read (and executed); ReadWrite paths may also be changed.
	// Missing paths are skipped.
	ReadOnly  []string `json:"read_only"`
	ReadWrite []string `json:"read_write"`
	// BindPort is the one TCP port the app may listen on.
	BindPort int `json:"bind_port"`
	// Network allows outbound TCP connections.
	Network bool `json:"network"`
}

// Status describes the confinement available on this system.
type Status struct {
	// Mechanism is "landlock" or "none".
	Mechanism string `json:"mechanism"`
	// ABI is the Landlock ABI version (0 without Landlock).
	ABI int `json:"abi,omitempty"`
	// Filesystem and Network report which kinds of access are confined.
	Filesystem bool   `json:"filesystem"`
	Network    bool   `json:"network"`
	Detail     string `json:"detail"`
}

// systemReadOnly are the files a static Go or Rust binary commonly reads: TLS roots, name
// resolution, time zones and the random and null devices.
var systemReadOnly = []string{
	"/etc/ssl", "/etc/pki", "/etc/ca-certificates", "/usr/share/ca-certificates", "/usr/local/share/ca-certificates",
	"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/localtime", "/etc/mime.types",
	"/usr/share/zoneinfo", "/usr/local/go/lib/time", "/dev/urandom", "/dev/random",
}
var systemReadWrite = []string{"/dev/null"}

// Command returns the argv and environment that start the app confined: the trampoline
// (self) with the spec in its environment. With no confinement available it returns the app
// itself.
func Command(self string, spec Spec, env []string) (string, []string, []string, error) {
	if Probe().Mechanism == "none" {
		return spec.Exec, append([]string{spec.Exec}, spec.Args...), env, nil
	}
	spec.ReadOnly = append(spec.ReadOnly, systemReadOnly...)
	spec.ReadWrite = append(spec.ReadWrite, systemReadWrite...)
	b, err := json.Marshal(spec)
	if err != nil {
		return "", nil, nil, err
	}
	return self, []string{self, Arg}, append(env, specEnv+"="+string(b)), nil
}

// Usable reports why confinement cannot run executables kept under dir, or nil when it
// can (or there is no confinement to try). Some filesystems lose Landlock's rules, so a
// confined exec there is refused: FUSE mounts such as Docker Desktop's shared folders and
// Unraid's /mnt/user. It proves it the only reliable way, by running a copy of self
// confined to a directory in dir. The verdict is a property of the filesystem, so it is
// remembered per filesystem and self: a process that opens many apps directories on one
// disk (a test run, say) pays for one probe, not one each.
func Usable(self, dir string) error {
	if Probe().Mechanism == "none" {
		return nil
	}
	if os.MkdirAll(dir, 0700) == nil {
		if dev, ok := deviceOf(dir); ok {
			k := usableKey{self, dev}
			usableMu.Lock()
			defer usableMu.Unlock()
			if err, seen := usableCache[k]; seen {
				return err
			}
			err := probeUsable(self, dir)
			usableCache[k] = err
			return err
		}
	}
	return probeUsable(self, dir)
}

type usableKey struct {
	self string
	dev  uint64
}

var (
	usableMu    sync.Mutex
	usableCache = map[usableKey]error{}
)

func probeUsable(self, dir string) error {
	tmp, err := os.MkdirTemp(dir, ".sandbox-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	probe := filepath.Join(tmp, "probe")
	if err := copyFile(self, probe); err != nil {
		return err
	}
	path, argv, env, err := Command(self, Spec{Exec: probe, Args: []string{OKArg}, ReadOnly: []string{tmp}}, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, argv[1:]...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (%v)", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Trampoline runs in the re-executed shartifacts binary: it applies the spec from the
// environment and execs the app. It never returns.
func Trampoline() {
	var spec Spec
	if err := json.Unmarshal([]byte(os.Getenv(specEnv)), &spec); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox: invalid spec:", err)
		os.Exit(126)
	}
	env := []string{}
	for _, kv := range os.Environ() {
		if len(kv) < len(specEnv)+1 || kv[:len(specEnv)+1] != specEnv+"=" {
			env = append(env, kv)
		}
	}
	err := restrictAndExec(spec, env)
	fmt.Fprintln(os.Stderr, "sandbox:", err)
	os.Exit(126)
}
