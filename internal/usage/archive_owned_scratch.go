package usage

import (
	"errors"
	"os"
)

// Retirement is confined to a captured inode, never a directory's contents.
// A renamed original or an unknown entry keeps its owner's fee outstanding.
func unlinkOwnedArchiveScratch(path string, identity os.FileInfo) error {
	named, err := os.Lstat(path)
	if err != nil || identity == nil || !named.Mode().IsRegular() || !os.SameFile(named, identity) {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	return os.Remove(path)
}

func retireArchiveSpool(file *os.File) error {
	identity, statErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		return err
	}
	return unlinkOwnedArchiveScratch(file.Name(), identity)
}
