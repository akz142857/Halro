package metadatajournal

import (
	"context"
	"errors"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// OpenRotationSourceReadOnly authenticates one independently authorized whole
// old epoch. Its actual descriptor cannot write, append or repair a torn tail.
func OpenRotationSourceReadOnly(ctx context.Context, path string, key []byte, expected Head, maxBytes int64) (result *Log, resultErr error) {
	if !expected.Baseline.valid() || expected.Epoch != expected.Baseline.Epoch || expected.FormatVersion != boundFrameVersion || expected.FileRevision == 0 || expected.Offset <= 0 || expected.Offset > maxBytes || expected.Sequence < expected.TrimmedThrough || expected.Hash == ([32]byte{}) || len(key) != KeySize {
		return nil, ErrCorrupt
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			return
		}
		if closeErr := closeDescriptor(file); closeErr != nil {
			resultErr = errors.Join(resultErr, closeErr)
			result = &Log{file: file, path: path, key: append([]byte(nil), key...), readOnly: true, closed: true, closeErr: resultErr, retirementErr: resultErr}
		}
	}()
	actual, err := file.Stat()
	named, nameErr := os.Lstat(path)
	if err != nil || nameErr != nil || !actual.Mode().IsRegular() || actual.Mode().Perm()&0077 != 0 || !os.SameFile(actual, named) || actual.Size() != expected.Offset {
		return nil, errors.Join(ErrCorrupt, err, nameErr)
	}
	head, partial, err := scanContext(ctx, file, key, nil)
	if err != nil || partial || head != expected {
		return nil, errors.Join(ErrCorrupt, err)
	}
	after, err := file.Stat()
	named, nameErr = os.Lstat(path)
	if err != nil || nameErr != nil || !os.SameFile(actual, after) || !os.SameFile(actual, named) || after.Size() != actual.Size() || !actual.ModTime().Equal(after.ModTime()) || !actual.ModTime().Equal(named.ModTime()) {
		return nil, errors.Join(ownedfile.ErrReplaced, err, nameErr)
	}
	result = &Log{file: file, path: path, key: append([]byte(nil), key...), readOnly: true, epoch: head.Epoch, sequence: head.Sequence, offset: head.Offset, lastHash: head.Hash, trimmedThrough: head.TrimmedThrough, formatVersion: head.FormatVersion, fileRevision: head.FileRevision, baseline: head.Baseline, maxBytes: maxBytes}
	return result, ctx.Err()
}
