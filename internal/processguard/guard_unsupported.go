//go:build !linux

// Package processguard protects a credential-bearing worker from inspection by
// the same-UID agent and verification processes that it starts.
package processguard

import (
	"fmt"
	"runtime"
)

// Harden fails closed on platforms where Pajé has no process-inspection guard.
func Harden() error {
	return fmt.Errorf("process credential isolation is unsupported on %s", runtime.GOOS)
}
