//go:build linux

package gitworktree

import "golang.org/x/sys/unix"

func exchangeDirectoryEntries(parentDescriptor int, left, right string) error {
	return unix.Renameat2(
		parentDescriptor,
		left,
		parentDescriptor,
		right,
		unix.RENAME_EXCHANGE,
	)
}
