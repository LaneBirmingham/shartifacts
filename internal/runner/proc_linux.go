package runner

import "syscall"

// procAttr puts a backend in its own process group, and kills it if the runner dies
// without stopping it.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
