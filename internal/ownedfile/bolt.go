package ownedfile

import (
	"errors"
	"os"

	bbolt "go.etcd.io/bbolt"
)

// OpenExclusiveBolt transfers the actual exclusive creation FD to bbolt. A
// failed engine Open discards its internal Close error, so after handoff the
// returned creation receipt is cleanup evidence only, never proof of retirement.
// The caller retains its original mapping/file credits on ErrRetirement. This
// helper grants neither source authenticity nor saved-file reopen permission.
func OpenExclusiveBolt(path string, mode os.FileMode, options *bbolt.Options) (*bbolt.DB, *Owner, error) {
	return openExclusiveBolt(path, mode, options, Create)
}

func openExclusiveBolt(path string, mode os.FileMode, options *bbolt.Options, create func(string, int, os.FileMode) (*os.File, *Owner, error)) (*bbolt.DB, *Owner, error) {
	if options == nil {
		options = bbolt.DefaultOptions
	}
	if options.OpenFile != nil || options.ReadOnly {
		return nil, nil, errors.New("exclusive database construction requires its original creation hook")
	}
	copy := *options
	var receipt *Owner
	var actual *os.File
	handedToEngine := false
	copy.OpenFile = func(name string, flags int, mode os.FileMode) (*os.File, error) {
		file, owner, err := create(name, flags, mode)
		if err != nil {
			return nil, err
		}
		actual, receipt = file, owner
		handedToEngine = true
		return file, nil
	}
	db, err := bbolt.Open(path, mode, &copy)
	if err != nil && handedToEngine {
		err = errors.Join(ErrRetirement, err)
		// Join any descriptor still left open by the failed receiving engine.
		// A closed FD cannot prove mmap/freelist retirement, so keep the debt
		// even when this additional actual Close succeeds.
		if _, statErr := actual.Stat(); !errors.Is(statErr, os.ErrClosed) {
			err = errors.Join(err, CloseFile(actual))
		}
		receipt.retirementMu.Lock()
		receipt.retirementErr = errors.Join(receipt.retirementErr, err)
		receipt.retirementMu.Unlock()
	}
	return db, receipt, err
}
