//go:build darwin

package migration

import "golang.org/x/sys/unix"

func renameNoReplace(oldPath, newPath string) error {
	return unix.RenamexNp(oldPath, newPath, unix.RENAME_EXCL)
}
