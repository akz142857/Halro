package ledger

import (
	"context"
	"errors"
	"fmt"

	bbolt "go.etcd.io/bbolt"
)

// VerifiedProjectionRoll is a native-verified physical transition, not a
// semantic event or an HA authorization. Its fields are private: only an open
// keyed native owner can produce one after checking the sealed bytes. The caller
// separately proves this roll belongs to its confirmed/role-permitted prefix.
type VerifiedProjectionRoll struct{ from, to ReplayAnchor }

// WithVerifiedProjectionRoll freezes native mutation/close while validating the
// ordered segment against its local representation and authenticating its full
// contents/EOF. Local compression may change stored file/checksum, but never the
// ordered plain identity, positions, chain or epoch. The callback must not call
// back into the native Log; it may commit the selected projection synchronously.
func (l *Log) WithVerifiedProjectionRoll(ctx context.Context, expected Segment, apply func(VerifiedProjectionRoll) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if apply == nil || expected.Generation == 0 || expected.Generation == ^uint64(0) || expected.Length <= 0 || expected.LastSequence < expected.FirstSequence || expected.FirstSequence == 0 {
		return errors.New("projection roll requires a valid native transition and callback")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.file == nil || len(l.chainKey) != 32 || l.status.Load() == AccountingRecoveryRequired {
		return errors.New("projection roll requires an open healthy keyed native owner")
	}
	actual, _, err := l.lookupSegmentLocked(expected.Generation)
	if err != nil {
		return err
	}
	if actual.Generation == 0 || l.generation <= expected.Generation {
		return errors.New("projection roll has not been published by native owner")
	}
	actualEpoch := actual.EndEpoch
	if actualEpoch == 0 {
		actualEpoch = frameVersionLedgerIntegrity
	}
	if actual.FirstSequence != expected.FirstSequence || actual.LastSequence != expected.LastSequence || actual.Length != expected.Length || actual.StartHash != expected.StartHash || actual.EndHash != expected.EndHash || actual.PlainChecksum != expected.PlainChecksum || actualEpoch != expected.EndEpoch || !actual.SealedAt.Equal(expected.SealedAt) {
		return errors.New("projection roll does not match the ordered native segment")
	}
	start, err := decodeChainHash(expected.StartHash)
	if err != nil {
		return err
	}
	end, err := decodeChainHash(expected.EndHash)
	if err != nil {
		return err
	}
	epoch, err := l.segmentStartEpoch(actual.Generation)
	if err != nil {
		return err
	}
	verifier := &chainVerifier{key: l.chainKey, hash: start, sequence: actual.FirstSequence - 1, epoch: epoch}
	reader, err := openSegment(l.directory, actual)
	fail := func(err error) error {
		if !isCallerVisitError(err) {
			l.status.RequireRecovery()
		}
		return err
	}
	if err != nil {
		return fail(err)
	}
	head, partial, scanErr := scan(reader, actual.Generation, 0, actual.FirstSequence-1, func(Record) error { return ctx.Err() }, verifier)
	closeErr := reader.Close()
	if closeErr != nil {
		l.status.RequireRecovery()
	}
	if err := errors.Join(scanErr, closeErr); err != nil {
		return fail(err)
	}
	if partial || head.Offset != actual.Length || head.Sequence != actual.LastSequence || verifier.hash != end || verifier.epoch != actualEpoch {
		return fail(fmt.Errorf("%w: projection roll sealed content differs from ordered boundary", ErrTampered))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	roll := VerifiedProjectionRoll{from: ReplayAnchor{Head: head, Hash: end, Epoch: actualEpoch}, to: ReplayAnchor{Head: Watermark{Generation: actual.Generation + 1, Sequence: head.Sequence}, Hash: end, Epoch: actualEpoch}}
	return errors.Join(apply(roll), ctx.Err())
}

func (r VerifiedProjectionRoll) valid() bool {
	return r.from.Head.Generation > 0 && r.from.Head.Generation != ^uint64(0) && r.from.Head.Offset > 0 && r.from.Head.Sequence > 0 && r.to.Head == (Watermark{Generation: r.from.Head.Generation + 1, Sequence: r.from.Head.Sequence}) && r.from.Hash == r.to.Hash && r.from.Epoch == r.to.Epoch
}

// NextAnchor exposes the checked physical successor to a legacy compatibility
// owner that retains its chain resume evidence separately from historical State.
func (r VerifiedProjectionRoll) NextAnchor() (ReplayAnchor, error) {
	if !r.valid() {
		return ReplayAnchor{}, ErrProjectionCorrupt
	}
	return r.to, nil
}

// ApplyVerifiedRoll changes only the physical head in one transaction. Native
// facts, balances, events, request/attempt state and maturity remain unchanged.
// It is idempotent at the exact successor, and refuses missing semantic work.
func (p *LedgerProjection) ApplyVerifiedRoll(ctx context.Context, roll VerifiedProjectionRoll) error {
	if p == nil {
		return ErrProjectionUnavailable
	}
	if !roll.valid() {
		return ErrProjectionCorrupt
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ready || p.db == nil {
		return ErrProjectionUnavailable
	}
	err := p.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		position, found, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
		if err != nil {
			return err
		}
		if !found {
			return ErrProjectionCorrupt
		}
		current := ReplayAnchor{Head: position.Watermark, Hash: position.Hash, Epoch: position.Epoch}
		if current == roll.to {
			return ctx.Err()
		}
		if current != roll.from {
			return fmt.Errorf("%w: projection has not applied the exact sealed prefix", ErrProjectionCorrupt)
		}
		position.Watermark = roll.to.Head
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := projectionPut(tx, "meta", []byte("position"), position); err != nil {
			return err
		}
		return p.checkDiskLimit(tx)
	})
	return normalizeProjectionDiskError(err)
}

// Legacy State retains no hash/epoch. It can consume the same verified physical
// transition by exact watermark, keeping all semantic data and its trust limit.
func (s *State) ApplyVerifiedRoll(ctx context.Context, roll VerifiedProjectionRoll) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || !roll.valid() {
		return errors.New("State projection roll is absent or invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.watermark == roll.to.Head {
		return nil
	}
	if s.watermark != roll.from.Head {
		return errors.New("State has not applied the exact sealed prefix")
	}
	s.watermark = roll.to.Head
	return nil
}
