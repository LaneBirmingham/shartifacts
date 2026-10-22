//go:build !linux

package runner

import "syscall"

// procAttr puts a backend in its own process group.
func procAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
