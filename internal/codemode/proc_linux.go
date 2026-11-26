package codemode

import "syscall"

// procAttr kills the child if this process dies before it.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
