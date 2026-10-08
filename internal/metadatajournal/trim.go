package metadatajournal

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

// Trim requires caller exclusion from append and whole-file publication. It
// authenticates the whole pinned source before copying any retained prefix,
// streams a second authenticated pass into an exclusive private candidate,
// verifies the complete candidate, then publishes it. No suffix byte slice or
// filename collection grows with history. Format2 retains its complete baseline
// binding and increments the physical file revision, including all-trim cuts.
// Resource/disk admission is still a caller obligation, not FS preallocation.
func Trim(path string, key []byte, throughSequence uint64) (Head, error) {
	return TrimContext(context.Background(), path, key, throughSequence)
}
func TrimContext(ctx context.Context, path string, key []byte, throughSequence uint64) (result Head, resultErr error) {
	return trimContext(ctx, path, key, throughSequence, nil)
}
func TrimContextWithLimit(ctx context.Context, path string, key []byte, throughSequence uint64, maxBytes int64) (Head, error) {
	if maxBytes < 0 {
		return Head{}, errors.New("metadata journal disk limit cannot be negative")
	}
	return trimWithState(ctx, path, key, throughSequence, nil, nil, nil, maxBytes, nil)
}
func trimContext(ctx context.Context, path string, key []byte, throughSequence uint64, phase func(string)) (Head, error) {
	return trimWithState(ctx, path, key, throughSequence, nil, nil, nil, 0, phase)
}

func trimWithState(ctx context.Context, path string, key []byte, throughSequence uint64, expected *Head, state *TrimStateBinding, published **ownedfile.Owner, maxBytes int64, phase func(string)) (result Head, resultErr error) {
	if len(key) != KeySize {
		return Head{}, fmt.Errorf("metadata journal key must be %d bytes", KeySize)
	}
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	if throughSequence == 0 {
		return VerifyContext(ctx, path, key)
	}
	if throughSequence == math.MaxUint64 {
		return Head{}, errors.New("metadata trim sequence overflow")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Head{}, err
	}
	defer func() {
		if err := closeDescriptor(file); err != nil {
			result = Head{}
			resultErr = errors.Join(resultErr, err)
		}
	}()
	identity, err := file.Stat()
	if err != nil {
		return Head{}, err
	}
	var cutHash [32]byte
	var frames uint64
	var trimmedBytes int64
	var retainedBytes int64
	var existing TrimAnchor
	stateSeen := state == nil
	firstHead, partial, err := scanContext(ctx, file, key, func(record Record) error {
		size := frameSize(record, file)
		if size <= 0 {
			return errors.New("metadata source frame has invalid size")
		}
		switch record.Kind {
		case KindEpoch:
			trimmedBytes = size
			return nil
		case KindTrim:
			existing = record.Trim
			frames = existing.TrimmedFrameCount
			if existing.TrimmedByteCount < 0 || existing.TrimmedByteCount > math.MaxInt64-size {
				return errors.New("metadata cumulative trim bytes overflow")
			}
			trimmedBytes = existing.TrimmedByteCount + size
			copy(cutHash[:], existing.ChainHeadAtTrim)
			return nil
		}
		if state != nil && record.Sequence == state.Sequence {
			stateSeen = record.Hash == state.SourceHash && record.Offset == state.SourceFrameOffset && record.Offset+size == state.SourceEndOffset
		}
		if record.Sequence <= throughSequence {
			if frames == math.MaxUint64 || trimmedBytes > math.MaxInt64-size {
				return errors.New("metadata cumulative trim count overflow")
			}
			frames++
			trimmedBytes += size
			cutHash = record.Hash
		} else {
			if retainedBytes > math.MaxInt64-size {
				return errors.New("metadata retained bytes overflow")
			}
			retainedBytes += size
		}
		return nil
	})
	if err != nil {
		return Head{}, err
	}
	if partial {
		return Head{}, fmt.Errorf("%w: partial metadata source cannot be trimmed", ErrCorrupt)
	}
	if expected != nil && (firstHead != *expected || !stateSeen) {
		return Head{}, errors.New("metadata trim state does not match actual source boundary")
	}
	if throughSequence < firstHead.TrimmedThrough || throughSequence > firstHead.Sequence {
		return Head{}, errors.New("metadata trim cut outside authenticated source")
	}
	if throughSequence == firstHead.TrimmedThrough {
		return firstHead, nil
	}
	if phase != nil {
		phase("source-verified")
	}
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	anchor := TrimAnchor{Epoch: firstHead.Epoch, TrimmedThrough: throughSequence, ChainHeadAtTrim: cutHash[:], TrimmedFrameCount: frames, TrimmedByteCount: trimmedBytes, RetainedFromSeqMin: throughSequence + 1}
	if firstHead.FormatVersion == boundFrameVersion {
		if firstHead.FileRevision == math.MaxUint64 {
			return Head{}, errors.New("metadata file revision overflow")
		}
		binding := firstHead.Baseline
		anchor.Baseline = &binding
		anchor.State = state
		anchor.FileRevision = firstHead.FileRevision + 1
	}
	payload, err := encodePayload(KindTrim, nil, EpochHeader{}, anchor)
	if err != nil {
		return Head{}, err
	}
	encoded, _ := encodeFrameVersion(firstHead.FormatVersion, key, KindTrim, firstHead.Epoch, throughSequence, [32]byte{}, payload)
	if maxBytes > 0 && (int64(len(encoded)) > maxBytes || retainedBytes > maxBytes-int64(len(encoded))) {
		return Head{}, errors.New("metadata trim candidate exceeds owned disk ceiling")
	}
	temporary, candidateOwner, err := ownedfile.CreateTemporary(filepath.Dir(path), ".metadata-journal-*")
	if err != nil {
		return Head{}, err
	}
	closed, moved := false, false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, closeDescriptor(temporary))
		}
		if !moved && !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, candidateOwner.Retire())
		}
		if resultErr != nil {
			result = Head{}
		}
	}()
	if err := candidateOwner.VerifyFile(temporary); err != nil {
		return Head{}, err
	}
	if err := writeFull(temporary, encoded); err != nil {
		return Head{}, err
	}
	buffer := make([]byte, 64<<10)
	second, partial, err := scanContext(ctx, file, key, func(record Record) error {
		if record.Kind != KindOperations || record.Sequence <= throughSequence {
			return nil
		}
		size := frameSize(record, file)
		if size <= 0 {
			return errors.New("metadata retained frame has invalid size")
		}
		hash := sha256.New()
		n, err := io.CopyBuffer(io.MultiWriter(temporary, hash), trimContextReader{ctx, io.NewSectionReader(file, record.Offset, size)}, buffer)
		if err != nil {
			return err
		}
		if n != size || !equalFrameDigest(hash.Sum(nil), record.Hash) {
			return fmt.Errorf("%w: retained metadata frame changed during copy", ErrCorrupt)
		}
		return nil
	})
	if err != nil {
		return Head{}, err
	}
	if partial || second != firstHead {
		return Head{}, errors.New("metadata journal changed between trim passes")
	}
	if err := temporary.Sync(); err != nil {
		return Head{}, err
	}
	if phase != nil {
		phase("candidate-synced")
	}
	verified, partial, err := scanContext(ctx, temporary, key, nil)
	if err != nil {
		return Head{}, err
	}
	if partial || verified.Epoch != firstHead.Epoch || verified.Sequence != firstHead.Sequence || verified.Hash != firstHead.Hash || verified.TrimmedThrough != throughSequence || verified.FormatVersion != firstHead.FormatVersion || verified.Baseline != firstHead.Baseline || verified.FileRevision != anchor.FileRevision {
		return Head{}, errors.New("metadata trim candidate does not preserve authenticated source")
	}
	if maxBytes > 0 {
		candidate, statErr := temporary.Stat()
		if statErr != nil || candidate.Size() > maxBytes {
			return Head{}, errors.Join(errors.New("metadata trim candidate exceeds owned disk ceiling"), statErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(identity, current) || identity.Size() != current.Size() || !identity.ModTime().Equal(current.ModTime()) {
		return Head{}, errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := closeDescriptor(temporary); err != nil {
		closed = true
		return Head{}, err
	}
	closed = true
	if err := candidateOwner.VerifyName(); err != nil {
		return Head{}, err
	}
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	if err := candidateOwner.ReplaceCaptured(path, identity); err != nil {
		return Head{}, err
	}
	moved = true
	if published != nil {
		*published = candidateOwner
	}
	if phase != nil {
		phase("journal-renamed")
	}
	if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
		return Head{}, err
	}
	return verified, nil
}
func equalFrameDigest(raw []byte, want [32]byte) bool {
	return len(raw) == len(want) && string(raw) == string(want[:])
}

type trimContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r trimContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(b)
}

// frameSize reads only fixed metadata from the same pinned source descriptor.
func frameSize(record Record, file *os.File) int64 {
	var header [frameHeaderSize]byte
	if _, err := file.ReadAt(header[:], record.Offset); err != nil {
		return 0
	}
	length := int64(uint32(header[24])<<24 | uint32(header[25])<<16 | uint32(header[26])<<8 | uint32(header[27]))
	if length > MaxPayloadSize {
		return 0
	}
	return frameHeaderSize + length + frameMACSize
}
func readAt(file *os.File, offset, size int64) ([]byte, error) {
	if size <= 0 || size > MaxPayloadSize+frameHeaderSize+frameMACSize {
		return nil, errors.New("invalid metadata frame size")
	}
	chunk := make([]byte, size)
	if _, err := file.ReadAt(chunk, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return chunk, nil
}
