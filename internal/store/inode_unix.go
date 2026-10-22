//go:build unix

package store

import (
	"io/fs"
	"syscall"
)

// fileID names a file rather than a path: hard links to it share one.
type fileID struct{ dev, ino uint64 }

func identify(info fs.FileInfo) (fileID, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{uint64(st.Dev), uint64(st.Ino)}, true
}
