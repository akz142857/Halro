package ledger

import (
	"context"
	"errors"
	"fmt"
)

var ErrProjectionCursor = errors.New("Ledger projection cursor is not a native prefix")

// InspectProjectionCursor resolves a caller-authorized generation/sequence to
// its exact record or offset-zero roll position. It authenticates the complete
// source, including excluded durable bytes, without collecting history. The
// caller freezes native mutation and establishes ordering/role authority; this
// lookup grants neither authority nor trust to any existing derived file.
func InspectProjectionCursor(ctx context.Context, path string, key []byte, generation, sequence uint64) (ProjectionPrefix, ChainReport, error) {
	if err := ctx.Err(); err != nil {
		return ProjectionPrefix{}, ChainReport{}, err
	}
	if generation == 0 && sequence != 0 {
		return ProjectionPrefix{}, ChainReport{}, ErrProjectionCursor
	}
	var prefix ProjectionPrefix
	found := generation == 0
	match := func(head Watermark, hash [32]byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if head.Generation == generation && head.Sequence == sequence {
			candidate := ProjectionPrefix{Head: head, Hash: hash}
			if found && candidate != prefix {
				return fmt.Errorf("%w: ambiguous physical position", ErrProjectionCursor)
			}
			prefix, found = candidate, true
		}
		return nil
	}
	report, partial, err := inspectReplayAuthenticated(ctx, path, key, func(r Record) error {
		return match(Watermark{Generation: r.Generation, Offset: r.Offset, Sequence: r.Sequence}, r.Hash)
	}, match)
	if err != nil {
		return ProjectionPrefix{}, ChainReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return ProjectionPrefix{}, ChainReport{}, err
	}
	if partial {
		return ProjectionPrefix{}, ChainReport{}, fmt.Errorf("%w: projection source has a partial tail", ErrCorrupt)
	}
	if !found {
		return ProjectionPrefix{}, ChainReport{}, ErrProjectionCursor
	}
	return prefix, report, nil
}

// BuildProjectionFromCursor creates a fresh exclusive selected-owner candidate
// under the keyed native lock. The logical cursor must come from authenticated
// role/member/ordering evidence, not the durable head or an existing projection.
// Position lookup and full fresh build share one frozen source. No saved DB is
// reopened; no business generation/readiness is published. Callers keep native
// ownership alive, admit space/work costs, and own candidate cleanup and handoff.
func (l *Log) BuildProjectionFromCursor(ctx context.Context, generation, sequence uint64, output string) (*LedgerProjection, ProjectionPrefix, error) {
	return l.buildProjectionFromCursor(ctx, generation, sequence, output, BuildLedgerProjectionPrefix)
}

// BuildProjectionFromCursorWithLimit builds the same authenticated fresh
// prefix as BuildProjectionFromCursor, bounding the bbolt data file with
// Options.MaxSize.
func (l *Log) BuildProjectionFromCursorWithLimit(ctx context.Context, generation, sequence uint64, output string, maxDiskBytes int64) (*LedgerProjection, ProjectionPrefix, error) {
	if maxDiskBytes <= 0 {
		return nil, ProjectionPrefix{}, ErrProjectionDiskLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, ProjectionPrefix{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, ProjectionPrefix{}, err
	}
	if l.file == nil || len(l.chainKey) != 32 || l.status.Load() != AccountingHealthy {
		return nil, ProjectionPrefix{}, errors.New("projection build requires an open healthy keyed native owner")
	}
	expected := ChainReport{
		Head:          Watermark{Generation: l.generation, Offset: l.offset, Sequence: l.sequence},
		ChainSequence: l.chainSequence,
		ChainOffset:   l.chainOffset,
		ChainHash:     l.chainHash,
		ChainVerified: l.chainSawFrames,
	}
	p, prefix, err := buildLedgerProjectionAtCursorWithLimit(ctx, l.path, l.chainKey, output, expected, generation, sequence, maxDiskBytes)
	if err != nil {
		if errors.Is(err, ErrProjectionSource) {
			l.status.RequireRecovery()
		}
		return nil, ProjectionPrefix{}, err
	}
	return p, prefix, nil
}

// The builder seam permits deterministic faults between the two native passes.
// Production always invokes the complete authenticated fresh-prefix builder.
func (l *Log) buildProjectionFromCursor(ctx context.Context, generation, sequence uint64, output string, build func(context.Context, string, []byte, string, ChainReport, ProjectionPrefix) (*LedgerProjection, error)) (*LedgerProjection, ProjectionPrefix, error) {
	if err := ctx.Err(); err != nil {
		return nil, ProjectionPrefix{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, ProjectionPrefix{}, err
	}
	if l.file == nil || len(l.chainKey) != 32 || l.status.Load() != AccountingHealthy {
		return nil, ProjectionPrefix{}, errors.New("projection build requires an open healthy keyed native owner")
	}
	prefix, report, err := InspectProjectionCursor(ctx, l.path, l.chainKey, generation, sequence)
	if err != nil {
		if errors.Is(err, ErrProjectionSource) || !errors.Is(err, ErrProjectionCursor) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			l.status.RequireRecovery()
		}
		return nil, ProjectionPrefix{}, err
	}
	head := Watermark{Generation: l.generation, Offset: l.offset, Sequence: l.sequence}
	if report.Head != head || report.ChainHash != l.chainHash || report.ChainVerified != l.chainSawFrames {
		l.status.RequireRecovery()
		return nil, ProjectionPrefix{}, fmt.Errorf("%w: projection source differs from frozen native head", ErrTampered)
	}
	p, err := build(ctx, l.path, l.chainKey, output, report, prefix)
	if err != nil {
		if errors.Is(err, ErrProjectionSource) {
			l.status.RequireRecovery()
		}
		return nil, ProjectionPrefix{}, err
	}
	return p, prefix, nil
}
