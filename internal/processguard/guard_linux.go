//go:build linux

// Package processguard protects a credential-bearing worker from inspection by
// the same-UID agent and verification processes that it starts.
package processguard

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Harden makes the worker non-dumpable before it reads service credentials.
// With CAP_SYS_PTRACE absent, Linux then denies same-UID descendants access to
// sensitive procfs surfaces such as environ, mem, and fd.
func Harden() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("set worker non-dumpable: %w", err)
	}
	dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		return fmt.Errorf("verify worker non-dumpable: %w", err)
	}
	if dumpable != 0 {
		return fmt.Errorf("verify worker non-dumpable: state is %d", dumpable)
	}
	return nil
}
