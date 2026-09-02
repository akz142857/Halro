// Package durable holds the filesystem barriers this process relies on to say
// a write survived a crash.
//
// There is one function here, and it exists because there were five. Every
// atomic-rename sequence in this repository — the Ledger's segments, the Usage
// archive's partitions, a backup archive, the master key, the metadata
// snapshot — ends by fsyncing the directory that now names the new file, and
// each of them had written that step out for itself. Five copies of a rule is
// five places to change if the rule ever needs to change, and the rule is not
// obviously right: a rename is durable only once the *directory entry* is, and
// forgetting it is the classic way a file that verifies perfectly well is
// missing after a power cut.
//
// Callers wrap the returned error with what they were writing. The operation is
// the shared part; what it was for is not.
package durable

import (
	"fmt"
	"os"
)

// SyncDirectory forces the directory entry itself to disk, so a file created or
// renamed inside it survives a crash rather than only its contents doing so.
func SyncDirectory(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory to sync: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
