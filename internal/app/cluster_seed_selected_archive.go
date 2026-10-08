package app

import (
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"os"
	"path/filepath"
)

var selectedSeedArchiveSuffixes = [4]string{".seed-source", ".seed-source-1", ".seed-source-2", ".seed-source-3"}

func selectedSeedArchiveName(name string) bool {
	for _, suffix := range selectedSeedArchiveSuffixes {
		if name == "ha-memory-state"+suffix || name == "ha-memory-state.bootstrap"+suffix || name == "ha-group-cluster.key"+suffix {
			return true
		}
	}
	return false
}

// Existing opaque archives are never reused, opened as selected roots, or
// deleted. A pair of absent fixed names limits repeated cold installations.
func selectedSeedArchiveSuffix(cfg config.Config) (string, error) {
	for _, suffix := range selectedSeedArchiveSuffixes {
		free := true
		for _, base := range [3]string{haMemoryStateDirectory(cfg), haMemoryStateBootstrapPath(cfg), haGroupClusterKeyPath(cfg)} {
			_, err := os.Lstat(filepath.Clean(base + suffix))
			if err == nil {
				free = false
				continue
			}
			if !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
		}
		if free {
			return suffix, nil
		}
	}
	return "", authenticatedstate.ErrLimit
}
