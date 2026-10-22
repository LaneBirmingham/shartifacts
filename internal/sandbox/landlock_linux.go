//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The kernel's structures; landlock_path_beneath_attr is packed, and the kernel copies only
// its 12 bytes, so Go's trailing padding is never read.
type rulesetAttr struct {
	handledFS  uint64
	handledNet uint64
	scoped     uint64
}
type pathBeneathAttr struct {
	allowed  uint64
	parentFD int32
}
type netPortAttr struct {
	allowed uint64
	port    uint64
}

const (
	fsRead = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	// fileRights are the only rights a rule on a regular file (not a directory) may carry.
	fileRights = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// fsRights is every filesystem right the given ABI knows.
func fsRights(abi int) uint64 {
	r := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		r |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		r |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		r |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return r
}

var (
	probeOnce sync.Once
	probed    Status
)

// Probe reports whether this kernel offers Landlock, and which version.
func Probe() Status {
	probeOnce.Do(func() {
		abi, _, errno := syscall.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
		if errno != 0 || int(abi) < 1 {
			detail := "this kernel does not offer Landlock"
			if errno == syscall.EPERM {
				detail = "Landlock is blocked here (a seccomp profile refuses its system calls)"
			} else if errno == syscall.EOPNOTSUPP {
				detail = "Landlock is built into this kernel but disabled (add landlock to the lsm= boot parameter)"
			}
			probed = Status{Mechanism: "none", Detail: detail + "; app backends run unconfined as the shartifacts user"}
			return
		}
		probed = Status{Mechanism: "landlock", ABI: int(abi), Filesystem: true, Network: abi >= 4}
		probed.Detail = fmt.Sprintf("Landlock ABI %d: backends read only their build, write only their data directory", abi)
		if abi >= 4 {
			probed.Detail += ", bind only their port and open TCP connections only when the app enables network"
		} else {
			probed.Detail += "; this kernel cannot confine their network access (Landlock ABI 4, Linux 6.7, can)"
		}
	})
	return probed
}

func restrictAndExec(spec Spec, env []string) error {
	st := Probe()
	if st.Mechanism != "landlock" {
		return errors.New(st.Detail)
	}
	// Landlock and no_new_privs apply to the calling thread; execve from that same thread
	// carries them into the app.
	runtime.LockOSThread()
	abi := st.ABI
	attr := rulesetAttr{handledFS: fsRights(abi)}
	size := unsafe.Sizeof(attr.handledFS)
	if abi >= 4 {
		attr.handledNet = unix.LANDLOCK_ACCESS_NET_BIND_TCP
		if !spec.Network {
			attr.handledNet |= unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
		}
		size += unsafe.Sizeof(attr.handledNet)
	}
	if abi >= 6 {
		// Nor may it signal processes outside its domain (shartifacts itself, other apps).
		attr.scoped = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
		size += unsafe.Sizeof(attr.scoped)
	}
	fd, _, errno := syscall.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), size, 0)
	if errno != 0 {
		return fmt.Errorf("create ruleset: %w", errno)
	}
	defer syscall.Close(int(fd))
	add := func(path string, rights uint64) error {
		f, err := os.OpenFile(path, unix.O_PATH|syscall.O_CLOEXEC, 0)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		rights &= attr.handledFS
		if !info.IsDir() {
			rights &= fileRights
		}
		rule := pathBeneathAttr{allowed: rights, parentFD: int32(f.Fd())}
		if _, _, errno := syscall.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
			return fmt.Errorf("allow %s: %w", path, errno)
		}
		return nil
	}
	for _, p := range spec.ReadOnly {
		if err := add(p, fsRead); err != nil {
			return err
		}
	}
	for _, p := range spec.ReadWrite {
		if err := add(p, fsRights(abi)); err != nil {
			return err
		}
	}
	if abi >= 4 && spec.BindPort > 0 {
		rule := netPortAttr{allowed: unix.LANDLOCK_ACCESS_NET_BIND_TCP, port: uint64(spec.BindPort)}
		if _, _, errno := syscall.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, 2 /* LANDLOCK_RULE_NET_PORT */, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
			return fmt.Errorf("allow port %d: %w", spec.BindPort, errno)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if _, _, errno := syscall.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return fmt.Errorf("restrict: %w", errno)
	}
	return syscall.Exec(spec.Exec, append([]string{spec.Exec}, spec.Args...), env)
}

// deviceOf names the filesystem holding path, so Usable can remember its verdict for every
// directory on it.
func deviceOf(path string) (uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}
