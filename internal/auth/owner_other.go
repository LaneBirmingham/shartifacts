//go:build !unix

package auth

// matchOwner has nothing to do where files have no Unix owner.
func matchOwner(name, dataDir string) error { return nil }
