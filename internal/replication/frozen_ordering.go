package replication

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"

	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

var ErrOrderingFrozen = errors.New("ordering native mutation is frozen by a read job")
var ErrOrderingSource = errors.New("ordering frozen source failed authentication")

const OrderingFrozenReadRAMBytes int64 = replicationlimits.OrderingFrozenReadRAMBytes

type FrozenOrderingHead struct {
	Header       OrderingHeader
	Index, Term  uint64
	MAC          [sha256.Size]byte
	Offset       int64
	StoreCursors [4]StoreCursor
}

// WithFrozenHead authenticates the complete native chain/header and fixed four
// store cursors, excludes append and retains the actual descriptor until the
// whole callback exits. Close cancels and joins the callback before physical IO
// retirement. The callback may read but must not Close/reenter the freeze; its
// context cannot escape. This evidence grants no member role/confirmed prefix.
func (j *OrderingJournal) WithFrozenHead(ctx context.Context, budget *resourcebudget.Manager, visit func(context.Context, FrozenOrderingHead) error) (resultErr error) {
	if j == nil || budget == nil || visit == nil {
		return ErrOrderingSource
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	credit, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: OrderingFrozenReadRAMBytes})
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithCancel(ctx)
	j.mu.Lock()
	if j.closed || j.closing || j.file == nil || j.poisoned != nil || j.freezeCancel != nil {
		failure := errors.Join(ErrOrderingSource, j.poisoned)
		if j.freezeCancel != nil {
			failure = ErrOrderingFrozen
		}
		j.mu.Unlock()
		cancel()
		return errors.Join(failure, credit.Release())
	}
	frozen := FrozenOrderingHead{Header: j.header, Index: j.lastIndex, Term: j.lastTerm, MAC: j.head, Offset: j.nextOffset, StoreCursors: j.tailCursors}
	j.freezeCancel = cancel
	j.freezeWG.Add(1)
	j.mu.Unlock()
	defer j.freezeWG.Done()
	defer func() {
		cancel()
		j.mu.Lock()
		j.freezeCancel = nil
		j.mu.Unlock()
		if err := credit.Release(); err != nil {
			resultErr = errors.Join(resultErr, err)
			j.mu.Lock()
			j.freezeDebt = credit
			j.poisoned = errors.Join(j.poisoned, err)
			j.invalidateLocked()
			j.mu.Unlock()
		}
	}()
	validate := func() error {
		j.mu.Lock()
		defer j.mu.Unlock()
		if err := opCtx.Err(); err != nil {
			return err
		}
		if j.closing || j.closed || j.file == nil {
			return ErrOrderingSource
		}
		err := j.verifyFrozenOrderingLocked(opCtx, frozen)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			j.poisoned = errors.Join(ErrOrderingSource, err)
			j.invalidateLocked()
		}
		return err
	}
	if err := validate(); err != nil {
		return err
	}
	callbackErr := visit(opCtx, frozen)
	if err := opCtx.Err(); err != nil {
		return errors.Join(callbackErr, err)
	}
	return errors.Join(callbackErr, validate(), opCtx.Err())
}

// One header and one fixed-size record are retained; no repair/recover mutation
// and no historical cursor slice is used by this source authentication.
func (j *OrderingJournal) verifyFrozenOrderingLocked(ctx context.Context, expected FrozenOrderingHead) error {
	check := func() error {
		actual, err := j.file.Stat()
		named, nameErr := os.Lstat(j.file.Name())
		if err != nil || nameErr != nil || !actual.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(actual, named) || actual.Size() != expected.Offset || named.Size() != expected.Offset {
			return errors.Join(ErrOrderingSource, err, nameErr)
		}
		return ctx.Err()
	}
	if err := check(); err != nil {
		return err
	}
	reader := io.NewSectionReader(j.file, 0, expected.Offset)
	encoded, err := ReadLengthDelimited(reader, orderingHeaderFixedBytes+2*MaxIdentityBytes)
	if err != nil {
		return errors.Join(ErrOrderingSource, err)
	}
	header, err := UnmarshalOrderingHeader(encoded, j.key[:])
	if err != nil || header != expected.Header || int64(len(encoded)) != j.dataOffset {
		return errors.Join(ErrOrderingSource, err)
	}
	cursors := header.StoreCursors
	var previous [sha256.Size]byte
	var term uint64
	offset := int64(len(encoded))
	var frame [orderingRecordBytes]byte
	for index := uint64(1); index <= expected.Index; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := io.ReadFull(reader, frame[:]); err != nil {
			return errors.Join(ErrOrderingSource, err)
		}
		record, err := UnmarshalOrderingRecord(frame[:], j.key[:], header.ClusterID, header.Incarnation, previous)
		if err != nil || record.Index != index || record.Term < term || record.Term > term && record.Kind != KindLeadershipEstablished || record.Term == term && record.Kind == KindLeadershipEstablished {
			return errors.Join(ErrOrderingSource, err)
		}
		cursors, err = advanceOrderingCursors(cursors, record)
		if err != nil {
			return errors.Join(ErrOrderingSource, err)
		}
		previous, term = record.MAC, record.Term
		offset += orderingRecordBytes
	}
	if offset != expected.Offset || previous != expected.MAC || term != expected.Term || cursors != expected.StoreCursors || j.tailCursorErr != nil {
		return ErrOrderingSource
	}
	return check()
}
