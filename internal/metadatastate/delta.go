package metadatastate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

const DeltaRAMBytes int64 = metadatalimits.DeltaRAMBytes

func validateDelta(ops []metadatajournal.Op) error {
	if err := metadatajournal.ValidateBoundOperations(ops); err != nil {
		return err
	}
	for _, op := range ops {
		class, err := boltstore.MetadataBaselineClass(op.Path, op.Key)
		if err != nil {
			return err
		}
		if class != "authoritative" && class != "session" && class != "key-envelope" {
			return ErrSource
		}
		if op.Path[0] == "meta" {
			if op.Kind == metadatajournal.OpDeleteBucket || op.Kind == metadatajournal.OpCreateBucket {
				return ErrSource
			}
			switch string(op.Key) {
			case "schema_version", "instance_id", "ledger_feature_epoch", "minimum_ledger_reader_version":
				return ErrSource
			}
		}
		path := make([][]byte, len(op.Path))
		for i, p := range op.Path {
			path[i] = []byte(p)
		}
		if _, err := RowKey(path, nil, true); err != nil {
			return err
		}
		if op.Kind == metadatajournal.OpPut || op.Kind == metadatajournal.OpDelete {
			if op.Kind == metadatajournal.OpDelete && len(op.Value) != 0 {
				return ErrSource
			}
			if _, err := RowKey(path, op.Key, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolveDeltaBucket(tx *authenticatedstate.Transaction, path [][]byte, create bool) (bool, error) {
	for i := range path {
		marker, err := RowKey(path[:i+1], nil, true)
		if err != nil {
			return false, err
		}
		value, found, err := tx.Get(marker)
		if err != nil {
			return false, err
		}
		if found {
			if len(value) != 8 {
				return false, ErrSource
			}
			continue
		}
		if !create {
			return false, nil
		}
		if i > 0 {
			counterpart := bytes.Clone(marker)
			counterpart[len(counterpart)-1] = 1
			if _, found, err := tx.Get(counterpart); err != nil || found {
				return false, errors.Join(ErrSource, err)
			}
		}
		if err := tx.Put(marker, make([]byte, 8)); err != nil {
			return false, err
		}
	}
	return true, nil
}

func reduceDelta(tx *authenticatedstate.Transaction, record metadatajournal.Record) error {
	for _, op := range record.Ops {
		path := make([][]byte, len(op.Path))
		for i, p := range op.Path {
			path[i] = []byte(p)
		}
		found, err := resolveDeltaBucket(tx, path, op.Kind == metadatajournal.OpCreateBucket)
		if err != nil {
			return err
		}
		switch op.Kind {
		case metadatajournal.OpCreateBucket:
		case metadatajournal.OpDeleteBucket:
			if !found {
				continue
			}
			lo, hi, err := BucketRange(path)
			if err != nil {
				return err
			}
			if _, err := tx.DeleteRange(lo, hi); err != nil {
				return err
			}
		case metadatajournal.OpPut, metadatajournal.OpDelete:
			if !found {
				if op.Kind == metadatajournal.OpDelete {
					continue
				}
				return ErrSource
			}
			key, err := RowKey(path, op.Key, false)
			if err != nil {
				return err
			}
			counterpart := bytes.Clone(key)
			counterpart[len(counterpart)-1] = 0
			if _, found, err := tx.Get(counterpart); err != nil || found {
				return errors.Join(ErrSource, err)
			}
			if op.Kind == metadatajournal.OpPut {
				err = tx.Put(key, op.Value)
			} else {
				err = tx.Delete(key)
			}
			if err != nil {
				return err
			}
		default:
			return ErrSource
		}
	}
	for _, entry := range []struct {
		name  string
		value uint64
	}{{"metadata_journal_epoch", record.Epoch}, {"applied_journal_sequence", record.Sequence}} {
		key, err := RowKey([][]byte{[]byte("meta")}, []byte(entry.name), false)
		if err != nil {
			return err
		}
		if err := tx.Put(key, binary.BigEndian.AppendUint64(nil, entry.value)); err != nil {
			return err
		}
	}
	return nil
}

func deltaCut(head metadatajournal.Head, record metadatajournal.Record) [32]byte {
	raw := append([]byte("halro:metadata:bound-delta-cut:v1\x00"), head.Baseline.Cut[:]...)
	raw = append(raw, head.Baseline.RootDigest[:]...)
	raw = binary.BigEndian.AppendUint64(raw, head.FileRevision)
	raw = binary.BigEndian.AppendUint64(raw, record.Epoch)
	raw = binary.BigEndian.AppendUint64(raw, record.Sequence)
	raw = binary.BigEndian.AppendUint64(raw, uint64(record.Offset))
	raw = append(raw, record.Hash[:]...)
	return sha256.Sum256(raw)
}

// AppendFrame is a private format2 write, not an HA/role or default Runtime
// entry. It owns complete bounded preparation, native Sync and root publish.
// Projection limits/errors occur before native append; uncertainty afterward
// permanently retires the candidate for independently authenticated recovery.
func (b *Baseline) AppendFrame(ctx context.Context, ops []metadatajournal.Op) (metadatajournal.Record, error) {
	return b.appendFrame(ctx, ops, nil)
}

func (b *Baseline) appendFrame(ctx context.Context, ops []metadatajournal.Op, phase func(string)) (result metadatajournal.Record, resultErr error) {
	if b == nil || !b.mu.TryLock() {
		return result, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	return b.appendFrameLocked(ctx, ops, phase)
}

// appendFrameLocked shares the one owner lock with the bounded transaction facade.
func (b *Baseline) appendFrameLocked(ctx context.Context, ops []metadatajournal.Op, phase func(string)) (result metadatajournal.Record, resultErr error) {
	return b.appendFacadeFrameLocked(ctx, ops, ops, phase)
}

// applyOps may additionally contain node-derived/local operations from the
// same qualified DAO transaction. Only ops enter the authoritative journal.
func (b *Baseline) appendFacadeFrameLocked(ctx context.Context, ops, applyOps []metadatajournal.Op, phase func(string)) (result metadatajournal.Record, resultErr error) {
	if b.readOnly || b.haPeerTransition != nil {
		return result, ErrSource
	}
	if !b.ready.Load() || !b.linked || b.journal == nil {
		return result, authenticatedstate.ErrClosed
	}
	if b.selectedStartup {
		if err := b.journal.HealthError(); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := validateDelta(ops); err != nil {
		return result, err
	}
	if err := validateFacadeOperations(applyOps); err != nil {
		return result, err
	}
	head := b.journal.Head()
	if head != b.position || head.Sequence == math.MaxUint64 {
		return result, ErrSource
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	permit, err := b.physicalWork(ctx, resourcebudget.Cost{RAM: DeltaRAMBytes})
	if err != nil {
		return result, err
	}
	ctx, finishPhysical, physicalErr := b.preparePhysicalAppendLocked(ctx, applyOps)
	if physicalErr != nil {
		return result, errors.Join(fmt.Errorf("metadata physical append preparation: %w", physicalErr), permit.Release())
	}
	defer func() { resultErr = errors.Join(resultErr, finishPhysical(resultErr)) }()
	succeeded := false
	b.ready.Store(false)
	defer func() {
		resultErr = errors.Join(resultErr, permit.Release())
		b.ready.Store(succeeded && resultErr == nil)
		if resultErr != nil {
			result = metadatajournal.Record{}
		}
	}()
	prepare := func(record metadatajournal.Record, durable func() error) error {
		if record.Epoch != head.Epoch || record.Sequence != head.Sequence+1 || record.Offset != head.Offset || record.FormatVersion != 2 {
			return ErrSource
		}
		_, err := b.store.UpdateNativeFrame(ctx, deltaCut(head, record), func(tx *authenticatedstate.Transaction) error {
			projected := record
			projected.Ops = applyOps
			if err := reduceDelta(tx, projected); err != nil {
				return err
			}
			if phase != nil {
				phase("candidate-prepared")
			}
			return ctx.Err()
		}, func() error {
			if err := durable(); err != nil {
				return err
			}
			if phase != nil {
				phase("native-durable")
			}
			return ctx.Err()
		})
		return err
	}
	var receipt metadatajournal.AppendReceipt
	if b.haPrimaryPrepared != nil {
		receipt, err = b.journal.AppendBoundPreparedContext(ctx, *b.haPrimaryPrepared, ops, prepare)
		result = receipt.Record
	} else {
		result, err = b.journal.AppendPreparedContext(ctx, ops, prepare)
	}
	if err != nil {
		return metadatajournal.Record{}, err
	}
	b.position = b.journal.Head()
	if b.haPrimaryPrepared != nil {
		if phase != nil {
			phase("projection-published")
		}
		if err := b.journal.WaitConfirmed(ctx, receipt); err != nil {
			return metadatajournal.Record{}, err
		}
	}
	succeeded = true
	return result, nil
}

// ReplayTo verifies the complete frozen journal before applying any selected
// facts, including its excluded tail. through is explicitly supplied by the
// independent authority; the durable head is not confirmation permission.
func (b *Baseline) ReplayTo(ctx context.Context, expected metadatajournal.Head, through uint64) (metadatajournal.Head, error) {
	if b != nil && b.haPeerTransition != nil {
		return metadatajournal.Head{}, ErrSource
	}
	return b.withHAReplicaReplayWorkspace(ctx, expected, through, func(ctx context.Context, work *resourcebudget.Permit) (metadatajournal.Head, error) {
		return b.replayTo(ctx, expected, through, work)
	})
}

// work is a private factory loan from the same Manager; retain it until actual
// replay/IO returns, rather than charging or refunding a second workspace.
func (b *Baseline) replayTo(ctx context.Context, expected metadatajournal.Head, through uint64, work *resourcebudget.Permit) (result metadatajournal.Head, resultErr error) {
	if b == nil || !b.mu.TryLock() {
		return result, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.ready.Load() || !b.linked || b.journal == nil {
		return result, authenticatedstate.ErrClosed
	}
	if expected != b.journal.Head() || expected.Baseline != b.position.Baseline || expected.FileRevision != b.position.FileRevision || expected.Epoch != b.position.Epoch || expected.FormatVersion != 2 || through < b.position.Sequence || through > expected.Sequence || b.position.Sequence < expected.TrimmedThrough {
		return result, ErrSource
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	var permit resourcebudget.Permit
	var err error
	if work != nil {
		permit, err = work.RetainCovering(b.budget, resourcebudget.Cost{RAM: DeltaRAMBytes})
	} else {
		permit, err = b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: DeltaRAMBytes})
	}
	if err != nil {
		return result, err
	}
	succeeded := false
	b.ready.Store(false)
	defer func() {
		resultErr = errors.Join(resultErr, permit.Release())
		b.ready.Store(succeeded && resultErr == nil)
		if resultErr != nil {
			result = metadatajournal.Head{}
		}
	}()
	// Static domain/class/schema checks also cover the excluded durable tail.
	err = b.journal.ReplayRangeValidatedContext(ctx, expected.TrimmedThrough, expected.Sequence, func(record metadatajournal.Record) error { return validateDelta(record.Ops) })
	if err != nil {
		return result, err
	}
	err = b.journal.ReplayRangeValidatedContext(ctx, b.position.Sequence, through, func(record metadatajournal.Record) error {
		if record.Epoch != b.position.Epoch || record.Sequence != b.position.Sequence+1 || record.FormatVersion != 2 || record.Offset < 0 {
			return ErrSource
		}
		if err := validateDelta(record.Ops); err != nil {
			return err
		}
		if err := b.reduceHAPhysicalFrameLocked(ctx, func(ctx context.Context) error {
			_, err := b.store.UpdateNativeFrame(ctx, deltaCut(expected, record), func(tx *authenticatedstate.Transaction) error { return reduceDelta(tx, record) }, func() error { return ctx.Err() })
			return err
		}); err != nil {
			return err
		}
		b.position.Sequence, b.position.Hash = record.Sequence, record.Hash
		// Replay scans the entire source. Offset is established independently
		// below by the next frame's start (or the actual complete head).
		b.position.Offset = record.Offset
		return nil
	})
	if err != nil {
		return result, err
	}
	if through == expected.Sequence {
		b.position.Offset = expected.Offset
	} else {
		// A second authenticated scan obtains the exact physical end of the
		// selected frame without guessing JSON/encoded payload length.
		err = b.journal.ReplayRangeValidatedContext(ctx, through, through+1, func(record metadatajournal.Record) error { b.position.Offset = record.Offset; return nil })
		if err != nil {
			return result, err
		}
	}
	succeeded = true
	return b.position, nil
}

func (b *Baseline) AppliedHead() (metadatajournal.Head, error) {
	if b == nil || !b.mu.TryLock() {
		return metadatajournal.Head{}, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.ready.Load() || !b.linked {
		return metadatajournal.Head{}, authenticatedstate.ErrClosed
	}
	if b.selectedStartup {
		if err := b.journal.HealthError(); err != nil {
			return metadatajournal.Head{}, err
		}
	}
	return b.position, nil
}
