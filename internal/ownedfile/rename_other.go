//go:build !linux && !darwin

package ownedfile

import "errors"

func renameCapturedPinned(from, to string, o *Owner) (bool, error) {
	return false, errors.New("captured no-replace rename is unsupported on this platform")
}
