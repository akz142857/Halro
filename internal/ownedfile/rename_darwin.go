//go:build darwin

package ownedfile

import "golang.org/x/sys/unix"

func renameExclusiveAt(parent int, from, to string) error {
	return unix.RenameatxNp(parent, from, parent, to, unix.RENAME_EXCL)
}
