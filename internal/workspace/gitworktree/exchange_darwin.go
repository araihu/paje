//go:build darwin

package gitworktree

import "golang.org/x/sys/unix"

func exchangeDirectoryEntries(parentDescriptor int, left, right string) error {
	return unix.RenameatxNp(
		parentDescriptor,
		left,
		parentDescriptor,
		right,
		unix.RENAME_SWAP,
	)
}
