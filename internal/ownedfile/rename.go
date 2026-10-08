package ownedfile

import (
	"errors"
	"path/filepath"
)

// RenameCaptured publishes the actual newly created inode at an absent sibling
// name with one atomic no-replace rename. It creates no hard-link alias and the
// original creation receipt follows the inode. Caller owns exclusion and Sync.
func (o *Owner) RenameCaptured(destination string) error {
	if o == nil || o.file == nil {
		return ErrReplaced
	}
	o.retirementMu.Lock()
	defer o.retirementMu.Unlock()
	if o.retired || o.retirementErr != nil {
		return errors.Join(ErrReplaced, o.retirementErr)
	}
	absolute, err := filepath.Abs(destination)
	if err != nil || filepath.Dir(absolute) != filepath.Dir(o.path) {
		return errors.Join(ErrReplaced, err)
	}
	moved, err := renameCapturedPinned(o.path, absolute, o)
	if moved {
		o.path = absolute
	}
	if errors.Is(err, ErrRetirement) {
		o.retirementErr = errors.Join(o.retirementErr, err)
	}
	return err
}
