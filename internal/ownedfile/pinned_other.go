//go:build !linux && !darwin

package ownedfile

import (
	"errors"
	"os"
)

var errPinnedUnsupported = errors.New("captured file ownership requires a supported native directory API")

func createPinned(string, int, os.FileMode, func(*os.File, *os.File) error) (*os.File, *Owner, error) {
	return nil, nil, errPinnedUnsupported
}
func retirePinned(string, os.FileInfo, os.FileInfo, func(*os.File, *os.File) error) (bool, error) {
	return false, errPinnedUnsupported
}
