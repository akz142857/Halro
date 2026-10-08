package ownedfile

import "os"

// actualCloseFailure keeps the actual descriptor alive with its uncertainty.
// A pathname and PathError alone cannot retain the original IO owner.
type actualCloseFailure struct {
	file  *os.File
	cause error
}

func (e *actualCloseFailure) Error() string { return e.cause.Error() }
func (e *actualCloseFailure) Unwrap() []error {
	return []error{ErrRetirement, e.cause}
}

// CloseError preserves a failure from an actual descriptor Close, including a
// private IO seam. It grants no file deletion or ownership-transfer authority.
func CloseError(file *os.File, err error) error {
	if err == nil {
		return nil
	}
	return &actualCloseFailure{file: file, cause: err}
}

func CloseFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return CloseError(file, file.Close())
}
