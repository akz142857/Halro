package authenticatedstate

import (
	"context"
	"encoding/binary"
	"errors"
	"io"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// WriteImage authenticates and streams the complete ordinal-chunk namespace
// used by the Usage/Governance image codecs. It accepts only the independently
// selected root, pins that exact root before writing, and never opens a saved
// bbolt file. The caller owns an exclusive destination, disk admission, physical
// Sync/Close, and deletion of any partial output after an error.
func (s *Store) WriteImage(ctx context.Context, writer io.Writer, schema SealRootSchema, selected SealRoot, chunkBytes int, maxBytes int64) (written int64, resultErr error) {
	if writer == nil || chunkBytes <= 0 || chunkBytes > MaxValueBytes || maxBytes <= 0 ||
		!validSealRootReference(selected) || selected.Empty ||
		selected.Component != schema.Component || selected.IndexID != schema.IndexID || selected.Codec != schema.Codec {
		return 0, ErrLimit
	}
	// All but the final chunk must be full. Prove the minimum image extent
	// fits before creating a pin or giving the writer any bytes; avoid overflow.
	if selected.Rows-1 > uint64((maxBytes-1)/int64(chunkBytes)) {
		return 0, ErrLimit
	}
	if s == nil || !s.mu.TryLock() {
		return 0, ErrBusy
	}
	if s.closing || s.closed {
		s.mu.Unlock()
		return 0, ErrClosed
	}
	if sealRootFor(s.current, s.rootDigest, schema) != selected {
		s.mu.Unlock()
		return 0, ErrRecovery
	}
	// snapshotLocked performs owner/file checks and charges the root pin.
	pin, err := s.snapshotLocked(ctx)
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	reader, err := pin.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { pin.end(ctx, reader, &resultErr) }()
	// Get's reader charge covers decoding. This additional whole-job charge
	// covers the returned chunk while the destination writer consumes it.
	workspace, err := s.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: int64(chunkBytes)})
	if err != nil {
		return 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, workspace.Release()) }()
	var key [9]byte
	key[0] = 1
	for ordinal := uint64(0); ordinal < selected.Rows; ordinal++ {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		binary.BigEndian.PutUint64(key[1:], ordinal)
		value, found, err := pin.getLocked(ctx, key[:])
		if err != nil {
			return written, err
		}
		if !found || len(value) == 0 || len(value) > chunkBytes || ordinal+1 < selected.Rows && len(value) != chunkBytes {
			return written, ErrCorrupt
		}
		if int64(len(value)) > maxBytes-written {
			return written, ErrLimit
		}
		n, err := writer.Write(value)
		if n < 0 || n > len(value) {
			return written, io.ErrShortWrite
		}
		written += int64(n)
		if err != nil {
			return written, err
		}
		if n != len(value) {
			return written, io.ErrShortWrite
		}
		if err := ctx.Err(); err != nil {
			return written, err
		}
	}
	return written, nil
}
