package audit

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// The actual opened descriptor and independently selected retry capsule supply
// the byte offsets. Caller sequence/hash cannot manufacture a prefix or grant
// any received tail for ordinary applied reads.
func (l *Log) resolveHASelectedPrefix(ctx context.Context, role authenticatedstate.SealRole, receipt NativeStartupReceipt, permitted HASelectedNativePosition) (Summary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil || permitted.Sequence < receipt.Saved.Records || permitted.Sequence > receipt.Head.Records {
		return Summary{}, ErrStartupCache
	}
	if permitted.Sequence == receipt.Head.Records && permitted.Hash == receipt.Head.LastHash {
		return receipt.Head, ctx.Err()
	}
	if role != authenticatedstate.SealReplica {
		return Summary{}, ErrStartupCache
	}
	actual, err := l.file.Stat()
	named, nameErr := os.Lstat(l.path)
	if err != nil || nameErr != nil || !actual.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(actual, named) || actual.Size() != receipt.Head.Bytes {
		return Summary{}, errors.Join(ErrStartupCache, err, nameErr)
	}
	prefix := receipt.Saved
	seen := prefix.Records == permitted.Sequence && prefix.LastHash == permitted.Hash
	visit := func(record Record) error {
		if record.Sequence == permitted.Sequence && record.Hash == permitted.Hash {
			prefix = Summary{Records: record.Sequence, Bytes: record.Offset, LastHash: record.Hash}
			seen = true
		}
		return ctx.Err()
	}
	reader := io.NewSectionReader(l.file, receipt.Saved.Bytes, actual.Size()-receipt.Saved.Bytes)
	full, partial, err := scanFromContext(ctx, reader, l.key, receipt.Saved, visit)
	final, statErr := l.file.Stat()
	finalName, finalNameErr := os.Lstat(l.path)
	if err != nil || statErr != nil || finalNameErr != nil || partial || !seen || full != receipt.Head || !os.SameFile(actual, final) || !os.SameFile(actual, finalName) || final.Size() != actual.Size() || finalName.Size() != actual.Size() || !final.ModTime().Equal(actual.ModTime()) {
		return Summary{}, errors.Join(ErrStartupCache, err, statErr, finalNameErr)
	}
	return prefix, ctx.Err()
}
