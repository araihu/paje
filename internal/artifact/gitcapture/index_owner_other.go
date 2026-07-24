//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package gitcapture

import "os"

func indexOwnerSafe(_ os.FileInfo) bool { return false }
