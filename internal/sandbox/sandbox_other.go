//go:build !linux

package sandbox

import (
	"errors"
	"runtime"
)

// Probe reports that this system has no confinement shartifacts can apply.
func Probe() Status {
	return Status{Mechanism: "none", Detail: "app backends run unconfined as the shartifacts user: confinement uses Linux Landlock, unavailable on " + runtime.GOOS}
}

func restrictAndExec(Spec, []string) error {
	return errors.New("confinement needs Linux Landlock")
}
