//go:build unix

package auth

import (
	"os"
	"syscall"
)

// matchOwner gives name the owner of the data directory when root writes it, as
// `sudo shartifacts auth` does, so a server running as that owner can still read it.
func matchOwner(name, dataDir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	fi, err := os.Stat(dataDir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return nil
	}
	return os.Lchown(name, int(st.Uid), int(st.Gid))
}
