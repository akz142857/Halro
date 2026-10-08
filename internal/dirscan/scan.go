// Package dirscan enumerates directory entries with fixed working memory.
// It authenticates the opened directory's identity, not a frozen contents cut
// or authority to remove any entry. Callers own exclusion and deletion policy.
package dirscan

import (
	"context"
	"errors"
	"io"
	"os"
)

const BatchEntries = 32

var (
	Stop        = errors.New("stop directory enumeration")
	ErrIdentity = errors.New("directory identity changed or is not a directory")
	ErrNotEmpty = errors.New("directory is not empty")
)

// Walk keeps at most BatchEntries directory names live. Cancellation is checked
// before every callback; callbacks may remove entries. Returning Stop succeeds
// after physical Close. Neither order nor an unchanged directory cut is promised.
func Walk(ctx context.Context, path string, consume func(os.DirEntry) error) (resultErr error) {
	if ctx == nil || consume == nil {
		return errors.New("directory scan requires context and consumer")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	identity, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !identity.IsDir() {
		return ErrIdentity
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.Join(ErrIdentity, err)
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(identity, actual) {
		return errors.Join(ErrIdentity, err)
	}
	defer func() {
		named, err := os.Lstat(path)
		if err != nil || !os.SameFile(identity, named) {
			resultErr = errors.Join(resultErr, ErrIdentity, err)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := f.ReadDir(BatchEntries)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := consume(entry); err != nil {
				if err == Stop {
					return nil
				}
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return ctx.Err()
		}
		if readErr != nil {
			return readErr
		}
	}
}

// Missing distinguishes an absent directory before opening from an opened
// directory that disappeared during enumeration. The latter remains an error.
func Missing(err error) bool {
	return errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrIdentity) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// Empty refuses after the first entry, rather than collecting a hostile or
// historical directory solely to determine whether it has any contents.
func Empty(ctx context.Context, path string) error {
	return Walk(ctx, path, func(os.DirEntry) error { return ErrNotEmpty })
}
