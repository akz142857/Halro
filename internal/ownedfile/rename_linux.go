//go:build linux

package ownedfile

import "golang.org/x/sys/unix"

func renameExclusiveAt(parent int, from, to string) error {
	return unix.Renameat2(parent, from, parent, to, unix.RENAME_NOREPLACE)
}
