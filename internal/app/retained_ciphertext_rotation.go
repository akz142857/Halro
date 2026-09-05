package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
)

var errRetainedVaultCiphertext = errors.New("retained Vault ciphertext prevents Master Key rotation")

// refuseRotationWithRetainedCiphertext is the fail-closed boundary used until
// Master Key rotation can publish metadata and every retained object directory
// as one recoverable generation. Rotating only metadata would strand captures,
// queued inputs and locally held results under the retired key.
func refuseRotationWithRetainedCiphertext(cfg config.Config) error {
	for _, target := range []struct {
		name string
		path string
	}{
		{name: "failure captures", path: filepath.Join(cfg.Storage.DataDir, "failures")},
		{name: "provider objects", path: filepath.Join(cfg.Storage.DataDir, "provider-objects")},
	} {
		present, err := retainedDirectoryHasObjects(target.path)
		if err != nil {
			return fmt.Errorf("inspect %s before Master Key rotation: %w", target.name, err)
		}
		if present {
			return fmt.Errorf("%w: %s are still present; let their retention lifecycle remove them before retrying", errRetainedVaultCiphertext, target.name)
		}
	}
	return nil
}

func retainedDirectoryHasObjects(root string) (bool, error) {
	present := false
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		present = true
		return fs.SkipAll
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return present, err
}
