package server

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/LaneBirmingham/shartifacts/internal/runner"
)

// The tests that deploy apps each get a fresh store, and with it a fresh apps directory. A
// backend build keeps the Go toolchain's cache in that directory, so on its own every build
// would compile the standard library again: about eight seconds each, a dozen times over in
// this package. One cache for the test binary is what a machine running the same builds by
// hand uses, and it brings the packages' apps tests down from a few minutes to well under one.
var (
	backendCacheOnce sync.Once
	backendCacheDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if backendCacheDir != "" {
		os.RemoveAll(backendCacheDir)
	}
	os.Exit(code)
}

// sharedBackendCache is the one build cache every backend these tests deploy compiles into.
func sharedBackendCache() string {
	backendCacheOnce.Do(func() {
		dir, err := os.MkdirTemp("", "shartifacts-backend-cache")
		if err != nil {
			panic("no directory for the test backend cache: " + err.Error())
		}
		backendCacheDir = dir
	})
	return backendCacheDir
}

// goBinary is the toolchain running the tests, which is the version a backend is built for.
func goBinary() string {
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// localRunner is a Local that builds backends with the shared cache, for the tests that
// would otherwise pay for a cold standard library each time.
func localRunner(dir string, remote bool) *runner.Local {
	return &runner.Local{
		Dir:      dir,
		GoBinary: goBinary(),
		Remote:   remote,
		CacheDir: sharedBackendCache(),
	}
}
