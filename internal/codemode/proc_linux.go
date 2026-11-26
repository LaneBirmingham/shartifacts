package codemode

import "syscall"

// procAttr kills the child if this process dies before it.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// watchParent has nothing to do: the parent-death signal stops the child.
func watchParent() {}
