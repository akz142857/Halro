package replication

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/durable"
)

// ensureDurableDirectory creates the cluster directory and persists the new
// name in its parent before any file inside it can report a durable write. HA
// state lives directly below an already-existing data directory; refusing a
// missing parent keeps this one-step publication contract explicit.
func ensureDurableDirectory(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("replication state path %s is not a directory", path)
		}
		// This is intentionally repeated even for an existing directory. If an
		// earlier creation reached disk but its parent fsync returned an error, a
		// retry must not convert that uncertain publication into success merely
		// because Stat can already see the name.
		if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
			return fmt.Errorf("persist replication state directory: %w", err)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		return ensureDurableDirectory(path)
	}
	if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("persist replication state directory: %w", err)
	}
	return nil
}
