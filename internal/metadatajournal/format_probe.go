package metadatajournal

import (
	"context"
	"errors"
	"os"
	"syscall"
)

var ErrIncompatibleFormat = errors.New("metadata journal format is incompatible with this operation")

// ProbeExactContext authenticates every byte of an existing native file and
// returns its exact format and head. Unlike Open it cannot create, repair or
// truncate anything. A format byte alone is never a migration qualification.
// Callers own maintenance exclusion and whole-scan RAM/disk admission.
func ProbeExactContext(ctx context.Context, path string, key []byte, maxBytes int64) (result Head, resultErr error) {
	if len(key) != KeySize || maxBytes <= 0 {
		return Head{}, ErrCorrupt
	}
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Head{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeDescriptor(file))
		if resultErr != nil {
			result = Head{}
		}
	}()
	before, err := file.Stat()
	named, nameErr := os.Lstat(path)
	if err != nil || nameErr != nil || !before.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(before, named) || before.Size() <= 0 || before.Size() > maxBytes {
		return Head{}, errors.Join(ErrCorrupt, err, nameErr)
	}
	head, partial, err := scanContext(ctx, file, key, nil)
	if err != nil || partial || head.Offset != before.Size() || head.Epoch == 0 || head.FormatVersion == 0 {
		return Head{}, errors.Join(ErrCorrupt, err)
	}
	after, err := file.Stat()
	named, nameErr = os.Lstat(path)
	if err != nil || nameErr != nil || !os.SameFile(before, after) || !os.SameFile(before, named) || before.Size() != after.Size() || before.Size() != named.Size() || !before.ModTime().Equal(after.ModTime()) || !before.ModTime().Equal(named.ModTime()) {
		return Head{}, errors.Join(ErrCorrupt, err, nameErr)
	}
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	return head, nil
}
