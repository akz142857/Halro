package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/ownedfile"
	"golang.org/x/sys/unix"
)

// A prior process may have committed the compressed manifest and died before
// removing plain bytes. A filename alone never grants cleanup authority. Both
// actual representations must match the selected descriptor, and every plain
// frame must be MAC-authenticated. Legacy/checksum-only duplicates are retained.
func qualifyCompactedDuplicate(ctx context.Context, directory string, selected Segment, key []byte) (os.FileInfo, os.FileInfo, os.FileInfo, error) {
	return qualifyCompactedDuplicateWithPhase(ctx, directory, selected, key, nil)
}

func qualifyCompactedDuplicateWithPhase(ctx context.Context, directory string, selected Segment, key []byte, phase func(*os.File, *os.File) error) (plainInfo, packedInfo, parentInfo os.FileInfo, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	plain := filepath.Join(directory, fmt.Sprintf("ledger-%d.wal", selected.Generation))
	plainInfo, err := os.Lstat(plain)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, nil
	}
	if err != nil || !selected.Compressed || len(key) != 32 || !plainInfo.Mode().IsRegular() || plainInfo.Size() != selected.Length || selected.FirstSequence == 0 {
		return nil, nil, nil, errors.Join(ErrCorrupt, err)
	}
	packed := filepath.Join(directory, selected.File)
	packedInfo, err = os.Lstat(packed)
	if err != nil || !packedInfo.Mode().IsRegular() || packedInfo.Size() != selected.StoredLength {
		return nil, nil, nil, errors.Join(ErrCorrupt, err)
	}
	parentInfo, err = os.Lstat(directory)
	if err != nil || !parentInfo.IsDir() {
		return nil, nil, nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	f, err := os.OpenFile(plain, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			resultErr = errors.Join(resultErr, ErrSegmentRetirement, err)
		}
		if resultErr != nil {
			plainInfo, packedInfo, parentInfo = nil, nil, nil
		}
	}()
	compressed, err := os.OpenFile(packed, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		if err := compressed.Close(); err != nil {
			resultErr = errors.Join(resultErr, ErrSegmentRetirement, err)
		}
	}()
	if err := errors.Join(unchangedSegmentDescriptor(f, plainInfo), unchangedSegmentDescriptor(compressed, packedInfo)); err != nil {
		return nil, nil, nil, err
	}
	stored, err := verifyCompressedReaderContext(ctx, compressed, selected.StoredLength, selected.PlainChecksum, selected.Length)
	if err != nil || stored != selected.StoredChecksum {
		return nil, nil, nil, errors.Join(ErrCorrupt, err)
	}
	anchor, err := decodeChainHash(selected.StartHash)
	if err != nil {
		return nil, nil, nil, err
	}
	verifier := &chainVerifier{key: key, hash: anchor, sequence: selected.FirstSequence - 1}
	digest := sha256.New()
	if n, err := io.Copy(digest, segmentContextReader{ctx, io.NewSectionReader(f, 0, selected.Length)}); err != nil || n != selected.Length {
		return nil, nil, nil, errors.Join(ErrCorrupt, err)
	}
	head, partial, err := scan(segmentContextReadSeeker{ctx, io.NewSectionReader(f, 0, selected.Length)}, selected.Generation, 0, selected.FirstSequence-1, func(record Record) error {
		if !authenticatedFrameEpoch(record.Epoch) {
			return errors.New("retained compacted duplicate lacks complete native MAC coverage")
		}
		return ctx.Err()
	}, verifier)
	end, endErr := decodeChainHash(selected.EndHash)
	if err != nil || endErr != nil || partial || head.Offset != selected.Length || head.Sequence != selected.LastSequence || verifier.hash != end || verifier.epoch != segmentEndEpoch(selected) || hex.EncodeToString(digest.Sum(nil)) != selected.PlainChecksum {
		return nil, nil, nil, errors.Join(ErrCorrupt, err, endErr)
	}
	if phase != nil {
		if err := phase(f, compressed); err != nil {
			return nil, nil, nil, err
		}
	}
	if err := errors.Join(ctx.Err(), unchangedSegmentDescriptor(f, plainInfo), unchangedSegmentDescriptor(compressed, packedInfo), unchangedSegment(plain, plainInfo), unchangedSegment(packed, packedInfo)); err != nil {
		return nil, nil, nil, err
	}
	currentParent, err := os.Lstat(directory)
	if err != nil || !os.SameFile(parentInfo, currentParent) {
		return nil, nil, nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	return plainInfo, packedInfo, parentInfo, nil
}

func (l *Log) retireSelectedCompactedDuplicate(ctx context.Context, selected Segment) (result Segment, resultErr error) {
	// Caller has mu and a native mutation ticket; release the writer during
	// immutable full-byte qualification, retaining only one zero-queue job.
	if l.compacting {
		l.mu.Unlock()
		return result, ErrSegmentCompactionBusy
	}
	l.compacting = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.compacting = false
		l.mu.Unlock()
	}()
	plain, packed, parent, err := qualifyCompactedDuplicate(ctx, l.directory, selected, l.chainKey)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			l.status.RequireRecovery()
		}
		return result, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	current, found, err := l.lookupSegmentLocked(selected.Generation)
	if err != nil || !found || current != selected || l.status.Load() != AccountingHealthy {
		return result, errors.Join(ErrCorrupt, err)
	}
	if plain == nil {
		return selected, nil
	}
	if err := errors.Join(ctx.Err(), unchangedSegment(filepath.Join(l.directory, selected.File), packed)); err != nil {
		return result, err
	}
	if parent == nil || l.directoryInfo == nil || !os.SameFile(parent, l.directoryInfo) {
		l.status.RequireRecovery()
		return result, ownedfile.ErrReplaced
	}
	path := filepath.Join(l.directory, fmt.Sprintf("ledger-%d.wal", selected.Generation))
	if _, err := retireCompactedNative(path, plain, parent); err != nil {
		l.status.RequireRecovery()
		return result, err
	}
	return selected, nil
}

type segmentContextReadSeeker struct {
	ctx context.Context
	io.ReadSeeker
}

func (r segmentContextReadSeeker) Read(raw []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadSeeker.Read(raw)
}
