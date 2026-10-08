package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/dirscan"
)

var errRetainedVaultCiphertext = errors.New("retained Vault ciphertext prevents Master Key rotation")

// refuseRotationWithRetainedCiphertext is the fail-closed boundary used until
// Master Key rotation can publish metadata and every retained object directory
// as one recoverable generation. Rotating only metadata would strand captures,
// queued inputs and locally held results under the retired key.
func refuseRotationWithRetainedCiphertext(cfg config.Config) error {
	return refuseRotationWithRetainedCiphertextContext(context.Background(), cfg)
}

func refuseRotationWithRetainedCiphertextContext(ctx context.Context, cfg config.Config) error {
	for _, target := range []struct {
		name string
		path string
	}{
		{name: "failure captures", path: filepath.Join(cfg.Storage.DataDir, "failures")},
		{name: "provider objects", path: filepath.Join(cfg.Storage.DataDir, "provider-objects")},
	} {
		present, err := retainedDirectoryHasObjectsContext(ctx, target.path)
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
	return retainedDirectoryHasObjectsContext(context.Background(), root)
}

func retainedDirectoryHasObjectsContext(ctx context.Context, root string) (bool, error) {
	present := false
	err := dirscan.Tree(ctx, root, 32, func(_ string, _ *os.File, entry os.FileInfo) error {
		if entry.IsDir() {
			return nil
		}
		present = true
		return dirscan.Stop
	})
	if dirscan.Missing(err) {
		return false, nil
	}
	return present, err
}
