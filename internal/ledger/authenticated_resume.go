package ledger

import (
	"context"
	"errors"
	"fmt"
)

// ReplayAnchor is an authenticated, exclusively owned applied position. Hash
// and Epoch carry the chain and anti-downgrade state across a nonzero offset.
// A checksum-era prefix retains zero Hash and Epoch until its first MAC frame.
// This is not a signature or authorization for an arbitrary persisted DB.
type ReplayAnchor struct {
	Head  Watermark
	Hash  [32]byte
	Epoch uint8
}

func (p *LedgerProjection) ReadReplayAnchor(ctx context.Context) (ReplayAnchor, error) {
	if p == nil {
		return ReplayAnchor{}, ErrProjectionUnavailable
	}
	v, found, err := projectionRead[projectionPosition](ctx, p, "meta", []byte("position"))
	if err == nil && !found {
		err = ErrProjectionCorrupt
	}
	return ReplayAnchor{Head: v.Watermark, Hash: v.Hash, Epoch: v.Epoch}, err
}

// ReplayAuthenticated authenticates the remaining native history from a trusted
// applied anchor, including active frames, sealed EOF/trailers and roll links.
// Every authenticated Record includes its actual chain hash. The caller must
// exclude other projection mutations and reject this generation after native
// integrity failure; it may retry an idempotent consumer after caller errors.
// The native lock freezes append/roll/close for the complete scan. No history
// collection is added. Directory lookups and physical reads needed to reach a
// compressed offset are separate costs from decoding the remaining facts.
func (l *Log) ReplayAuthenticated(ctx context.Context, from ReplayAnchor, visit func(Record) error) (Watermark, error) {
	return l.replayAuthenticated(ctx, from, 0, visit)
}

// ReplayAuthenticatedGeneration checks through the complete target generation,
// including its sealed EOF/trailer, without repeatedly inspecting later files.
// It cannot authorize whole-prefix publication: the caller must finish the native
// suffix barrier once after all ordered semantic/roll substeps complete.
func (l *Log) ReplayAuthenticatedGeneration(ctx context.Context, from ReplayAnchor, generation uint64, visit func(Record) error) (Watermark, error) {
	if generation == 0 {
		return Watermark{}, errors.New("authenticated generation replay requires a target")
	}
	return l.replayAuthenticated(ctx, from, generation, visit)
}

func (l *Log) replayAuthenticated(ctx context.Context, from ReplayAnchor, generation uint64, visit func(Record) error) (Watermark, error) {
	if err := ctx.Err(); err != nil {
		return Watermark{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.replayAuthenticatedLocked(ctx, from, generation, visit, nil)
}

// boundary reports only native physical roll/start positions. It does not
// authorize a role cut. Callers consume them only after the complete suffix
// returns successfully and hold the native lock throughout every pass.
func (l *Log) replayAuthenticatedLocked(ctx context.Context, from ReplayAnchor, generation uint64, visit func(Record) error, boundary func(ReplayAnchor) error) (Watermark, error) {
	if err := ctx.Err(); err != nil {
		return Watermark{}, err
	}
	if l.closeContext.Err() != nil || l.file == nil || len(l.chainKey) != 32 {
		return Watermark{}, errors.New("authenticated replay requires an open keyed Ledger")
	}
	if l.status.Load() == AccountingRecoveryRequired {
		return Watermark{}, errors.New("authenticated replay requires fresh native recovery")
	}
	if from.Head.Offset < 0 || from.Head.Generation > l.generation || from.Head.Generation == 0 && from != (ReplayAnchor{}) || from.Epoch > frameVersionRunAttribution || (from.Hash != ([32]byte{})) != authenticatedFrameEpoch(from.Epoch) {
		return Watermark{}, errors.New("authenticated replay anchor is invalid")
	}
	if generation != 0 && (generation < from.Head.Generation || generation > l.generation) {
		return Watermark{}, errors.New("authenticated generation target is outside native/anchor range")
	}
	verifier := &chainVerifier{key: l.chainKey, hash: from.Hash, sequence: from.Head.Sequence, sawFrames: from.Hash != ([32]byte{})}
	if authenticatedFrameEpoch(from.Epoch) {
		verifier.epoch = from.Epoch
	}
	sequence := from.Head.Sequence
	started := from.Head.Generation == 0
	fail := func(err error) (Watermark, error) {
		if !isCallerVisitError(err) {
			l.status.RequireRecovery()
		}
		return Watermark{}, err
	}
	consume := func(r Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if visit != nil {
			return visit(r)
		}
		return nil
	}
	startGeneration := uint64(1)
	if from.Head.Generation != 0 {
		startGeneration = from.Head.Generation
	}
	for currentGeneration := startGeneration; currentGeneration < l.generation; currentGeneration++ {
		segment, found, err := l.lookupSegmentLocked(currentGeneration)
		if err != nil {
			return fail(err)
		}
		if !found {
			return fail(ErrSegmentMissing)
		}
		if err := ctx.Err(); err != nil {
			return Watermark{}, err
		}
		if !started {
			if segment.Generation != from.Head.Generation {
				continue
			}
			started = true
		}
		offset := int64(0)
		if segment.Generation == from.Head.Generation {
			offset = from.Head.Offset
		}
		if offset > segment.Length {
			return fail(fmt.Errorf("%w: resume offset exceeds sealed length", ErrCorrupt))
		}
		if offset == 0 {
			start, err := decodeChainHash(segment.StartHash)
			if err != nil {
				return fail(err)
			}
			if start != verifier.hash {
				return fail(fmt.Errorf("%w: sealed resume does not continue applied hash", ErrTampered))
			}
		}
		if offset == segment.Length {
			end, err := decodeChainHash(segment.EndHash)
			if err != nil {
				return fail(err)
			}
			if sequence != segment.LastSequence || end != verifier.hash {
				return fail(fmt.Errorf("%w: sealed end does not match applied anchor", ErrTampered))
			}
		}
		open := openSegment
		if l.replaySegmentOpen != nil {
			open = l.replaySegmentOpen
		}
		reader, err := open(l.directory, segment)
		if err != nil {
			return fail(err)
		}
		verifier.offset = offset
		head, partial, scanErr := scan(reader, segment.Generation, offset, sequence, consume, verifier)
		closeErr := reader.Close()
		if closeErr != nil {
			// Unknown actual reader exit permanently pins its owner and the
			// native verification key. Close reports this same debt; it cannot
			// turn a later successful retry into proof of the original exit.
			l.replayReaderDebt = reader
			l.closeErr = errors.Join(l.closeErr, ErrSegmentRetirement, closeErr)
			l.status.RequireRecovery()
		}
		if err := errors.Join(scanErr, closeErr); err != nil {
			return fail(err)
		}
		if partial || head.Offset != segment.Length || head.Sequence != segment.LastSequence {
			return fail(fmt.Errorf("%w: sealed resume length/sequence mismatch", ErrCorrupt))
		}
		end, err := decodeChainHash(segment.EndHash)
		if err != nil {
			return fail(err)
		}
		if end != verifier.hash {
			return fail(fmt.Errorf("%w: sealed resume end hash mismatch", ErrTampered))
		}
		if verifier.epoch == frameVersionRunAttribution && segment.EndEpoch != uint8(frameVersionRunAttribution) || segment.EndEpoch != 0 && byte(segment.EndEpoch) != verifier.epoch {
			return fail(fmt.Errorf("%w: sealed generation %d ends at epoch %d, manifest says %d", ErrCorrupt, segment.Generation, verifier.epoch, segment.EndEpoch))
		}
		sequence = head.Sequence
		if boundary != nil {
			next := ReplayAnchor{Head: Watermark{Generation: currentGeneration + 1, Sequence: sequence}, Hash: verifier.hash, Epoch: verifier.epoch}
			if err := boundary(next); err != nil {
				return Watermark{}, visitError{err}
			}
		}
		if segment.Generation == generation {
			return head, ctx.Err()
		}
	}
	if !started && from.Head.Generation != l.generation {
		return Watermark{}, errors.New("authenticated replay anchor generation is absent")
	}
	offset := int64(0)
	if from.Head.Generation == l.generation {
		offset = from.Head.Offset
	}
	if offset == 0 && boundary != nil {
		start := ReplayAnchor{Head: Watermark{Generation: l.generation, Sequence: sequence}, Hash: verifier.hash, Epoch: verifier.epoch}
		if err := boundary(start); err != nil {
			return Watermark{}, visitError{err}
		}
	}
	verifier.offset = offset
	head, partial, err := scan(l.file, l.generation, offset, sequence, consume, verifier)
	if err != nil {
		return fail(err)
	}
	if partial {
		return fail(fmt.Errorf("%w: active authenticated replay has a partial tail", ErrCorrupt))
	}
	expected := Watermark{Generation: l.generation, Offset: l.offset, Sequence: l.sequence}
	if head != expected || verifier.hash != l.chainHash || verifier.epoch != l.chainEpoch {
		return fail(fmt.Errorf("%w: replay differs from frozen native head", ErrTampered))
	}
	return head, ctx.Err()
}

// AuthenticateAppliedAnchor derives native hash/epoch for a caller-established
// fresh legacy State cut. It authenticates from the root through that generation,
// retaining only the requested anchor. It does not authenticate State contents
// or authorize a role prefix; it is not a trusted-resume API for an existing DB.
func (l *Log) AuthenticateAppliedAnchor(ctx context.Context, head Watermark) (ReplayAnchor, error) {
	if err := ctx.Err(); err != nil {
		return ReplayAnchor{}, err
	}
	if head == (Watermark{}) {
		return ReplayAnchor{}, nil
	}
	if head.Generation == 0 || head.Offset < 0 {
		return ReplayAnchor{}, errors.New("legacy applied position is invalid")
	}
	result := ReplayAnchor{Head: head}
	found := head == (Watermark{Generation: 1})
	_, err := l.ReplayAuthenticatedGeneration(ctx, ReplayAnchor{}, head.Generation, func(r Record) error {
		exact := (Watermark{r.Generation, r.Offset, r.Sequence}) == head
		rolled := head.Offset == 0 && head.Generation > 1 && r.Generation == head.Generation-1 && r.Sequence == head.Sequence
		if exact || rolled {
			result.Hash = r.Hash
			result.Epoch = r.Epoch
			found = true
		}
		return nil
	})
	if err != nil {
		return ReplayAnchor{}, err
	}
	if !found {
		return ReplayAnchor{}, errors.New("legacy applied position is not an authenticated record/roll boundary")
	}
	return result, nil
}
