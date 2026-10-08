//go:build !linux

package ownedfile

import (
	"errors"
	"os"
)

func exchangeCapturedPinned(from, to string, owner *Owner, previous os.FileInfo) (*Owner, bool, error) {
	return nil, false, errors.New("physical publication requires Linux captured name exchange")
}
