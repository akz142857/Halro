package governance

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

// NewProjectedManager binds writes, idempotency, current-head checks and finite
// pages to one freshly rebuilt disk owner. It retains no legacy State alias.
// Caller must serialize native/projection ownership and join Manager operations
// before closing either resource. This does not publish a Runtime generation.
func NewProjectedManager(ctx context.Context, log *Log, projection *GovernanceProjection, accounting ledger.AccountingWorkUnitReader, definitions DefinitionReader, checkpoint CheckpointStore) (*Manager, error) {
	return newProjectedManager(ctx, log, projection, accounting, definitions, checkpoint, false, false)
}
func newProjectedManager(ctx context.Context, log *Log, projection *GovernanceProjection, accounting ledger.AccountingWorkUnitReader, definitions DefinitionReader, checkpoint CheckpointStore, ha, readOnly bool) (*Manager, error) {
	m := NewManager(log, nil, accounting, definitions, checkpoint)
	m.projection, m.haSelected, m.haReadOnly = projection, ha, readOnly
	if projection == nil || accounting == nil || definitions == nil || log == nil || log.startupProvenance != nil && log.startupProvenance.ha != nil && !ha {
		return nil, ErrUnavailable
	}
	if _, err := m.ownerHead(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// Called under m.mu or before publication. Native rows must already be applied
// before any read/write is credited. A disk error never falls back to State.
func (m *Manager) ownerHead(ctx context.Context) (Summary, error) {
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	if m.unavailable != nil || m.log == nil {
		return Summary{}, ErrUnavailable
	}
	m.log.mu.Lock()
	closed := m.log.file == nil || m.log.terminalErr != nil
	head := Summary{Records: m.log.sequence, Bytes: m.log.offset, LastHash: m.log.lastHash}
	var nativeErr error
	if !closed {
		nativeErr = m.log.verifyHeadLocked(ctx, head)
		if errors.Is(nativeErr, ErrCorrupt) {
			m.log.terminalErr = nativeErr
			m.log.startupFault.Store(true)
		}
	}
	m.log.mu.Unlock()
	if closed {
		return Summary{}, ErrUnavailable
	}
	if nativeErr != nil {
		if errors.Is(nativeErr, ErrCorrupt) {
			m.unavailable = errors.Join(ErrUnavailable, nativeErr)
		}
		return Summary{}, errors.Join(ErrUnavailable, nativeErr)
	}
	if m.projection != nil {
		projected, err := m.projection.ReadHead(ctx)
		if err != nil || !m.haReadOnly && projected != head || projected.Records > head.Records || projected.Bytes > head.Bytes {
			return Summary{}, errors.Join(ErrUnavailable, err)
		}
		if m.haReadOnly {
			return projected, nil
		}
	} else if m.state == nil || m.state.Sequence() != head.Records {
		return Summary{}, ErrUnavailable
	}
	return head, nil
}

func (m *Manager) readIdempotency(ctx context.Context, project, hash string) (idemRecord, bool, error) {
	if m.projection != nil {
		return m.projection.readIdempotency(ctx, project, hash)
	}
	if m.state == nil {
		return idemRecord{}, false, ErrUnavailable
	}
	v, found := m.state.Idempotency(project, hash)
	return v, found, nil
}

func (m *Manager) readOutcome(ctx context.Context, id string) (domain.Outcome, bool, error) {
	if m.projection != nil {
		return m.projection.ReadOutcome(ctx, id)
	}
	if m.state == nil {
		return domain.Outcome{}, false, ErrUnavailable
	}
	v, found := m.state.Outcome(id)
	return v, found, nil
}

func (m *Manager) readCurrent(ctx context.Context, project, unit, definition string, version uint64) (domain.Outcome, bool, error) {
	if m.projection != nil {
		return m.projection.ReadCurrent(ctx, project, unit, definition, version)
	}
	if m.state == nil {
		return domain.Outcome{}, false, ErrUnavailable
	}
	v, found := m.state.Current(project, unit, definition, version)
	return v, found, nil
}

func (m *Manager) applyOutcome(ctx context.Context, record Record) error {
	if m.projection != nil {
		return m.projection.Apply(ctx, record)
	}
	if m.state == nil {
		return ErrUnavailable
	}
	return m.state.Apply(record)
}
