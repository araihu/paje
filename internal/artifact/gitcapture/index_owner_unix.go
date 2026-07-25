//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package gitcapture

import (
	"os"
	"syscall"
)

func indexOwnerSafe(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}
