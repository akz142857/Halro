//go:build linux

package authenticatedstate

import "golang.org/x/sys/unix"

func renameRepackWithoutReplace(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}
