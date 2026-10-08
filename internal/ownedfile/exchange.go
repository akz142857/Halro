package ownedfile

import (
	"errors"
	"os"
	"path/filepath"
)

// ExchangeCaptured atomically swaps two existing names without retiring either
// inode or requiring a new directory entry. The publisher must independently
// authorize the captured destination. A filesystem exchange grants no source,
// role or selected-root authority. After mutation uncertainty both names and
// every original fee must survive.
func (o *Owner) ExchangeCaptured(destination string, previous os.FileInfo) (*Owner, error) {
	if o == nil || previous == nil {
		return nil, ErrReplaced
	}
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	o.retirementMu.Lock()
	defer o.retirementMu.Unlock()
	if o.retired || o.retirementErr != nil || filepath.Dir(absolute) != filepath.Dir(o.path) || absolute == o.path {
		return nil, errors.Join(ErrReplaced, o.retirementErr)
	}
	original := o.path
	other, moved, err := exchangeCapturedPinned(original, absolute, o, previous)
	if moved {
		o.path = absolute
	}
	if errors.Is(err, ErrRetirement) {
		o.retirementErr = err
	}
	return other, err
}
