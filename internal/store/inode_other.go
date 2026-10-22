//go:build !unix

package store

import "io/fs"

// fileID names a file rather than a path; without Unix inodes every path counts.
type fileID struct{ dev, ino uint64 }

func identify(info fs.FileInfo) (fileID, bool) { return fileID{}, false }
