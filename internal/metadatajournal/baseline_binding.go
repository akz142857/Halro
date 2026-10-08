package metadatajournal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// BaselineBinding binds a complete externally authenticated Metadata root to
// the initial state of an epoch. The journal authenticates this association;
// it does not manufacture content trust from a caller-supplied ordinary DB.
// The domain factory must establish these fields from an authenticated root.
type BaselineBinding struct {
	Version       uint8    `json:"version"`
	Epoch         uint64   `json:"epoch"`
	Identity      [32]byte `json:"identity"`
	KeyGeneration uint64   `json:"key_generation"`
	Schema        uint64   `json:"schema"`
	Catalog       [32]byte `json:"catalog"`
	Scope         [32]byte `json:"scope"`
	Cut           [32]byte `json:"cut"`
	RootDigest    [32]byte `json:"root_digest"`
	RootID        [16]byte `json:"root_id"`
	Rows          uint64   `json:"rows"`
}

func (b BaselineBinding) valid() bool {
	return b.Version == 1 && b.Epoch > 0 && b.Identity != ([32]byte{}) && b.KeyGeneration > 0 && b.Schema > 0 && b.Catalog != ([32]byte{}) && b.Scope != ([32]byte{}) && b.Cut != ([32]byte{}) && b.RootDigest != ([32]byte{}) && b.RootID != ([16]byte{}) && b.Rows > 0
}

func validateBoundRecord(record Record, payload []byte) error {
	var binding *BaselineBinding
	var revision uint64
	var object any
	switch record.Kind {
	case KindEpoch:
		binding, revision, object = record.Header.Baseline, record.Header.FileRevision, &EpochHeader{}
	case KindTrim:
		binding, revision, object = record.Trim.Baseline, record.Trim.FileRevision, &TrimAnchor{}
	default:
		return nil
	}
	if record.FormatVersion == frameVersion {
		if binding != nil || revision != 0 || record.Trim.State != nil {
			return errors.New("baseline control requires metadata frame format 2")
		}
		return nil
	}
	if binding == nil || !binding.valid() || binding.Epoch != record.Epoch || revision == 0 {
		return errors.New("bound metadata control has no complete baseline or file revision")
	}
	if record.Kind == KindTrim && record.Trim.State != nil && !record.Trim.State.valid(*binding, revision, record.Trim.TrimmedThrough, record.Trim.ChainHeadAtTrim) {
		return errors.New("bound metadata trim has invalid content-state association")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(object); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("bound metadata control has trailing JSON")
	}
	canonical, err := json.Marshal(object)
	if err != nil || !bytes.Equal(canonical, payload) {
		return errors.New("bound metadata control is not canonical")
	}
	if record.Kind == KindEpoch {
		h := record.Header
		if h.Epoch != record.Epoch || h.Epoch == 0 || h.PreviousEpoch >= h.Epoch || h.Reason == "" || len(h.Reason) > 128 || len(h.PreviousChainHead) != 0 && len(h.PreviousChainHead) != 32 || revision != 1 {
			return errors.New("invalid bound metadata epoch")
		}
	} else {
		a := record.Trim
		if a.Epoch != record.Epoch || a.TrimmedThrough != record.Sequence || a.TrimmedThrough == 0 || a.TrimmedThrough == math.MaxUint64 || len(a.ChainHeadAtTrim) != 32 || a.TrimmedFrameCount == 0 || a.TrimmedByteCount <= 0 || a.RetainedFromSeqMin != a.TrimmedThrough+1 || revision < 2 {
			return errors.New("invalid bound metadata trim")
		}
	}
	return nil
}

// StartBoundEpoch exclusively creates a private format-2 journal. Unlike a
// whole-file production publish it never replaces an existing file. The
// supplied maximum bounds subsequent encoded appends. Resource admission and
// physical ownership remain the domain factory's obligations.
func StartBoundEpoch(ctx context.Context, path string, key []byte, header EpochHeader, maxBytes int64) (result *Log, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(key) != KeySize || !filepath.IsAbs(path) || header.Baseline == nil || !header.Baseline.valid() || maxBytes <= 0 || header.Reason == "" || len(header.Reason) > 128 || len(header.PreviousChainHead) != 0 && len(header.PreviousChainHead) != 32 {
		return nil, errors.New("invalid bound metadata epoch request")
	}
	// Copy before encoding; the caller cannot mutate a live association.
	binding := *header.Baseline
	header.Baseline = &binding
	canonical, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	if err := validateBoundRecord(Record{FormatVersion: boundFrameVersion, Kind: KindEpoch, Epoch: header.Epoch, Header: header}, canonical); err != nil {
		return nil, err
	}
	frame, hash := encodeFrameVersion(boundFrameVersion, key, KindEpoch, header.Epoch, 0, [32]byte{}, canonical)
	if int64(len(frame)) > maxBytes {
		return nil, errors.New("bound metadata header exceeds disk limit")
	}
	file, owner, err := ownedfile.Create(path, os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	l := &Log{file: file, durability: durabilityFor(file), path: path, key: bytes.Clone(key), epoch: header.Epoch, formatVersion: boundFrameVersion, fileRevision: 1, baseline: binding, maxBytes: maxBytes, created: owner}
	defer func() {
		if resultErr != nil {
			if cleanup := l.Discard(); cleanup != nil {
				result, resultErr = l, errors.Join(resultErr, cleanup)
			} else {
				result = nil
			}
		}
	}()
	if l.durability == nil {
		return nil, errors.New("metadata durability writer is absent")
	}
	if err := writeFull(file, frame); err != nil {
		return nil, err
	}
	if err := errors.Join(ctx.Err(), file.Sync(), durable.SyncDirectory(filepath.Dir(path))); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	actual, statErr := file.Stat()
	named, nameErr := os.Lstat(path)
	if statErr != nil || nameErr != nil || !os.SameFile(actual, named) || actual.Size() != int64(len(frame)) {
		return nil, errors.Join(ownedfile.ErrReplaced, statErr, nameErr)
	}
	l.offset, l.lastHash = int64(len(frame)), hash
	return l, nil
}

// OpenBoundContext verifies the expected content-root association and physical
// file revision before any partial-tail repair. A matching MAC alone never
// grants root eligibility. The caller provides independent role/cut/disk proof.
func OpenBoundContext(ctx context.Context, path string, key []byte, expected BaselineBinding, revision uint64, maxBytes int64) (*Log, error) {
	if !expected.valid() || revision == 0 || maxBytes <= 0 {
		return nil, errors.New("invalid expected metadata baseline")
	}
	return openExpected(ctx, path, key, false, &expected, revision, maxBytes, nil)
}

// Discard removes only an exclusively created journal after its actual Close.
// A reopened journal has no deletion permission. Source replacement is kept.
func (l *Log) Discard() error {
	if l == nil {
		return nil
	}
	if l.created == nil {
		return errors.New("metadata journal has no created-file deletion authority")
	}
	if err := l.Close(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.retirementErr != nil {
		return l.retirementErr
	}
	err := l.created.Remove()
	if err == nil {
		sync := l.syncRetiredDirectory
		if sync == nil {
			sync = durable.SyncDirectory
		}
		err = sync(filepath.Dir(l.path))
	}
	if errors.Is(err, ownedfile.ErrRetirement) {
		l.retirementErr = err
		l.closeErr = err
	}
	return err

}

// Bound operation encoding is capped before json.Marshal allocates. Path
// depth, operation count and cumulative worst-case JSON size are fixed bounds.
func validateBoundOps(ops []Op) error {
	if len(ops) == 0 || len(ops) > 1024 {
		return errors.New("bound metadata operation count exceeds limit")
	}
	var size int64 = 2
	for _, op := range ops {
		if err := op.validate(); err != nil {
			return err
		}
		if len(op.Kind) > 32 {
			return errors.New("bound metadata operation kind exceeds limit")
		}
		if len(op.Key) > 32768 || len(op.Value) > 3<<20 {
			return errors.New("bound metadata key/value exceeds limit")
		}
		if len(op.Path) == 0 || len(op.Path) > 32 {
			return errors.New("bound metadata operation depth exceeds limit")
		}
		size += 128 + int64(len(op.Key)+len(op.Value))*2
		for _, part := range op.Path {
			if !utf8.ValidString(part) || len(part) > 32768 {
				return errors.New("bound metadata path component exceeds limit")
			}
			size += int64(len(part))*6 + 4
			if size > MaxPayloadSize {
				return errors.New("bound metadata operation encoding exceeds limit")
			}
		}
		if size > MaxPayloadSize {
			return fmt.Errorf("bound metadata operation encoding exceeds %d bytes", MaxPayloadSize)
		}
	}
	return nil
}
