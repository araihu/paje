//go:build darwin

package filesystem

import "golang.org/x/sys/unix"

func renameNoReplace(oldDir int, oldName string, newDir int, newName string) error {
	return unix.RenameatxNp(oldDir, oldName, newDir, newName, unix.RENAME_EXCL)
}
