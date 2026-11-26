//go:build !linux

package codemode

import (
	"os"
	"syscall"
	"time"
)

// procAttr has nothing to add where there is no parent-death signal; watchParent stands in
// for it.
func procAttr() *syscall.SysProcAttr { return nil }

// watchParent stops the child once its parent has gone. The child reads its stdin only
// while it waits for an operation's reply, so code sleeping on a long timer would not notice
// the pipe close; an orphan is handed to another parent, which this sees.
func watchParent() {
	parent := os.Getppid()
	for range time.Tick(500 * time.Millisecond) {
		if os.Getppid() != parent {
			os.Exit(exitParent)
		}
	}
}
