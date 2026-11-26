//go:build !linux

package codemode

import "syscall"

// procAttr has nothing to add where there is no parent-death signal: the child exits when
// its stdin closes with this process.
func procAttr() *syscall.SysProcAttr { return nil }
