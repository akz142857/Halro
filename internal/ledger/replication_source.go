package ledger

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// ReplicationCursor authenticates the complete Ledger history and returns the
// active generation and global sequence at its durable tail.
func ReplicationCursor(path string, key []byte) (uint64, uint64, error) {
	report, partial, err := InspectReplayAuthenticated(path, key, nil)
	if err != nil {
		return 0, 0, err
	}
	if partial {
		return 0, 0, fmt.Errorf("%w: active ledger ends mid-frame", ErrCorrupt)
	}
	return report.Head.Generation, report.Head.Sequence, nil
}

func ReplicationHeadAt(path string, key []byte, generation, sequence uint64) ([32]byte, error) {
	if generation == 0 {
		return [32]byte{}, errors.New("ledger replication generation must be positive")
	}
	if sequence == 0 {
		if generation != 1 {
			return [32]byte{}, errors.New("empty ledger baseline must be generation 1")
		}
		return [32]byte{}, nil
	}
	var head [32]byte
	var recordGeneration uint64
	report, partial, err := InspectReplayAuthenticated(path, key, func(record Record) error {
		if record.Sequence == sequence {
			head, recordGeneration = record.Hash, record.Generation
		}
		return nil
	})
	if err != nil {
		return [32]byte{}, err
	}
	if partial {
		return [32]byte{}, fmt.Errorf("%w: active ledger ends mid-frame", ErrCorrupt)
	}
	if report.Head.Sequence < sequence || recordGeneration == 0 {
		return [32]byte{}, errors.New("ledger replication baseline is outside the durable history")
	}
	if generation != recordGeneration && generation != recordGeneration+1 {
		return [32]byte{}, errors.New("ledger replication baseline generation does not contain its sequence")
	}
	if head == ([32]byte{}) || !report.ChainVerified {
		return [32]byte{}, errors.New("ledger replication baseline has no authenticated chain head")
	}
	return head, nil
}

// ReplicationSegment returns the authenticated structural description for one
// sealed generation. Startup recovery uses it when the native Roll completed
// but the global ordering record did not: the sealed manifest is the durable
// intent, and the returned Segment is accepted only after the whole chain has
// been authenticated.
func ReplicationSegment(path string, key []byte, generation uint64) (Segment, error) {
	return ReplicationSegmentContext(context.Background(), path, key, generation)
}

// ReplicationSegmentContext retains complete native authentication while
// allowing cancellation per record. Directory lookup remains the transitional
// streaming manifest walk; repeated roll lookups still cost historical IO.
func ReplicationSegmentContext(ctx context.Context, path string, key []byte, generation uint64) (Segment, error) {
	if err := ctx.Err(); err != nil {
		return Segment{}, err
	}
	if generation == 0 {
		return Segment{}, errors.New("ledger replication generation must be positive")
	}
	_, partial, err := inspectReplayAuthenticated(ctx, path, key, nil, nil)
	if err != nil {
		return Segment{}, err
	}
	if partial {
		return Segment{}, fmt.Errorf("%w: active ledger ends mid-frame", ErrCorrupt)
	}
	layout, err := findReplicationGeneration(filepath.Dir(path), generation)
	if err != nil {
		return Segment{}, err
	}
	if err := ctx.Err(); err != nil {
		return Segment{}, err
	}
	if !layout.sealed {
		return Segment{}, errors.New("ledger replication generation is not sealed")
	}
	return layout.segment, nil
}

// ReadReplicationFrames returns exact plaintext Ledger frame bytes. A sealed
// compressed generation is decompressed through the same verified reader used
// by replay; compression metadata is node-local and is not replicated.
func ReadReplicationFrames(path string, key []byte, generation, first, last uint64) ([]byte, error) {
	if generation == 0 || first == 0 || last < first {
		return nil, errors.New("ledger replication range is invalid")
	}
	var start, end int64 = -1, -1
	var previousGeneration uint64
	var previousOffset int64
	_, partial, err := InspectReplayAuthenticated(path, key, func(record Record) error {
		if record.Generation != previousGeneration {
			previousGeneration, previousOffset = record.Generation, 0
		}
		if record.Generation == generation && record.Sequence == first {
			start = previousOffset
		}
		if record.Generation == generation && record.Sequence == last {
			end = record.Offset
		}
		previousOffset = record.Offset
		return nil
	})
	if err != nil {
		return nil, err
	}
	if partial {
		return nil, fmt.Errorf("%w: active ledger ends mid-frame", ErrCorrupt)
	}
	if start < 0 || end < start {
		return nil, errors.New("ledger replication range is outside the durable history")
	}
	reader, err := replicationGenerationReader(path, generation)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	if _, err := reader.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	frames := make([]byte, end-start)
	if _, err := io.ReadFull(reader, frames); err != nil {
		return nil, err
	}
	return frames, nil
}

// TruncateReplicationTail removes a source-fsynced but unordered suffix from
// the active generation. Structural Roll recovery is intentionally refused:
// undoing a published manifest/rename is a different transaction and must be
// driven by the Roll protocol rather than guessed from two cursors.
func TruncateReplicationTail(path string, key []byte, generation, sequence uint64) error {
	manifest, err := walkReplicationManifest(filepath.Dir(path), nil)
	if err != nil {
		return err
	}
	if generation != manifest.active {
		return errors.New("ledger truncation may only target the active generation")
	}
	sealedSequence := uint64(0)
	if manifest.sealed {
		sealedSequence = manifest.segment.LastSequence
	}
	if sequence < sealedSequence {
		return errors.New("ledger truncation cursor precedes the active generation")
	}
	offset := int64(0)
	report, partial, err := InspectReplayAuthenticated(path, key, func(record Record) error {
		if record.Generation == generation && record.Sequence == sequence {
			offset = record.Offset
		}
		return nil
	})
	if err != nil {
		return err
	}
	if partial {
		return fmt.Errorf("%w: active ledger ends mid-frame", ErrCorrupt)
	}
	if sequence > report.Head.Sequence || sequence > sealedSequence && offset == 0 {
		return errors.New("ledger truncation cursor is outside the active generation")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(offset); err != nil {
		return err
	}
	return file.Sync()
}

// RepairReplicaTail removes torn bytes only when the complete authenticated
// Ledger prefix is exactly the cursor recovered from ordering. Interrupted
// Roll intents remain a separate structural transaction and are refused here.
func RepairReplicaTail(path string, key []byte, expectedGeneration, expectedSequence uint64) error {
	manifest, err := walkReplicationManifest(filepath.Dir(path), nil)
	if err != nil {
		return err
	}
	if manifest.pending {
		return errors.New("replica Ledger interrupted Roll requires ordering-aware structural reconciliation")
	}
	report, partial, err := InspectReplayAuthenticated(path, key, nil)
	if err != nil {
		return err
	}
	if report.Head.Generation != expectedGeneration || report.Head.Sequence != expectedSequence {
		return errors.New("Ledger complete prefix does not match ordering cursor")
	}
	if !partial {
		return nil
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(report.Head.Offset); err != nil {
		return err
	}
	return file.Sync()
}

// RepairReplicaRoll resolves only a pending Roll whose pre-roll cursor exactly
// matches the authenticated ordering prefix. If the rename happened but the
// successor creation did not, it publishes the empty successor before making
// the resolved manifest durable. A retransmitted Roll then follows the normal
// idempotent ApplyReplicatedRoll path and supplies the ordering record.
func RepairReplicaRoll(path string, key []byte, expectedGeneration, expectedSequence uint64) (resultErr error) {
	return RepairReplicaRollWithDisk(path, key, expectedGeneration, expectedSequence, nil)
}

// RepairReplicaRollWithDisk funds every newly created successor and manifest
// before its IO. Existing native facts remain independently inventoried; new
// published/uncertain repair artifacts retain their charges after failure.
func RepairReplicaRollWithDisk(path string, key []byte, expectedGeneration, expectedSequence uint64, reserve func(int64) (NativeFileDiskReservation, error)) (resultErr error) {
	directory := filepath.Dir(path)
	manifest, err := openInspectionManifest(directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, manifest.close()) }()
	if manifest.file == nil {
		return manifest.check()
	}
	hash := sha256.New()
	parsed, err := parseSegmentManifest(io.TeeReader(io.NewSectionReader(manifest.file, 0, manifest.info.Size()), hash), nil)
	if err != nil {
		return err
	}
	if err := manifest.check(); err != nil {
		return err
	}
	if parsed.pending == nil {
		return nil
	}
	pending := *parsed.pending
	if pending.Generation != expectedGeneration || pending.LastSequence != expectedSequence {
		return errors.New("replica Ledger pending Roll does not match ordering cursor")
	}
	var originalDigest [32]byte
	copy(originalDigest[:], hash.Sum(nil))
	layout, digest, err := manifest.walk(nil)
	if err != nil {
		return err
	}
	if digest != originalDigest {
		return fmt.Errorf("%w: replica Roll manifest changed before repair", ErrCorrupt)
	}
	rolled := filepath.Join(directory, pending.File)
	_, rolledErr := os.Stat(rolled)
	if rolledErr == nil {
		info, statErr := os.Stat(path)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			var reservation NativeFileDiskReservation
			if reserve != nil {
				reservation, err = reserve(64 << 10)
				if err != nil {
					return err
				}
			}
			// The empty successor is recoverable authority, not disposable
			// scratch: preserve it and its fee even if a later barrier fails.
			defer func() { resultErr = keepNativeFileDiskDebt(reservation, resultErr) }()
			file, _, createErr := ownedfile.Create(path, os.O_RDWR, 0o600)
			if createErr != nil {
				return createErr
			}
			if syncErr := file.Sync(); syncErr != nil {
				return errors.Join(syncErr, ownedfile.CloseFile(file))
			}
			if closeErr := ownedfile.CloseFile(file); closeErr != nil {
				return closeErr
			}
			if syncErr := durable.SyncDirectory(directory); syncErr != nil {
				return syncErr
			}
		case statErr != nil:
			return statErr
		case !info.Mode().IsRegular() || info.Size() != 0:
			return errors.New("replica Ledger pending Roll successor is not an empty regular file")
		}
	} else if !errors.Is(rolledErr, os.ErrNotExist) {
		return rolledErr
	}
	report, partial, err := InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		// Keep any recoverable successor in place. Removing it would make a
		// failed retry destructive and is unnecessary: the pending manifest
		// still prevents Replica open from treating it as committed.
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: replica Ledger pending Roll has a partial active tail", ErrCorrupt)
	}
	wantGeneration := expectedGeneration
	if rolledErr == nil {
		wantGeneration++
	}
	if report.Head.Generation != wantGeneration || report.Head.Sequence != expectedSequence {
		return errors.New("replica Ledger pending Roll history does not match ordering cursor")
	}
	// Resolve and emit one descriptor at a time into an unpublished temporary
	// manifest. Any changed source or pending verdict prevents its publication.
	source := segmentManifestSource{each: func(emit func(Segment) error) error {
		currentLayout, currentDigest, err := manifest.walk(emit)
		if err != nil {
			return err
		}
		if currentLayout != layout || currentDigest != originalDigest {
			return fmt.Errorf("%w: replica Roll manifest changed during repair", ErrCorrupt)
		}
		return nil
	}}
	_, err = saveSegmentManifestSourceWithDisk(directory, source, durable.SyncDirectory, reserve)
	return err
}

func replicationGenerationReader(path string, generation uint64) (io.ReadSeekCloser, error) {
	manifest, err := findReplicationGeneration(filepath.Dir(path), generation)
	if err != nil {
		return nil, err
	}
	if manifest.sealed {
		return openSegment(filepath.Dir(path), manifest.segment)
	}
	return os.Open(path)
}
