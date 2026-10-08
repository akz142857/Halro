//go:build darwin

package replication

import "golang.org/x/sys/unix"

func renamePortableReceivedOpaque(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
