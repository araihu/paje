//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package executil

import (
	"os/exec"
	"time"
)

// Configure bounds process cleanup where process groups are unavailable.
func Configure(command *exec.Cmd) {
	command.WaitDelay = 5 * time.Second
}
