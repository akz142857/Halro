//go:build !darwin && !linux

package authenticatedstate

import "errors"

func renameRepackWithoutReplace(from, to string) error {
	return errors.New("opaque repack preservation requires atomic exclusive rename on Linux or macOS")
}
