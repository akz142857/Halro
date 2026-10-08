//go:build !linux && !darwin

package app

import "errors"

func renameMigrationWithoutReplace(from, to string) error {
	return errors.New("migration preservation requires an atomic exclusive rename on Linux or macOS")
}
