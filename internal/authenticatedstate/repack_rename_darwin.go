//go:build darwin

package authenticatedstate

import "golang.org/x/sys/unix"

func renameRepackWithoutReplace(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
