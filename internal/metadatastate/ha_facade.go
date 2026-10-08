package metadatastate

import (
	"context"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// HAFacadePermission must freeze the independently authenticated member,
// ordering and native source while invoking a read callback. Its position is
// the actual permitted Metadata prefix, never a candidate header's grant.
type HAFacadePermission func(context.Context, func(context.Context, authenticatedstate.SealPosition) error) error

type haFacadeReadCheckKey struct{}
type haFacadeReadCheck struct {
	position authenticatedstate.SealPosition
}

func (c haFacadeReadCheck) matches(actual authenticatedstate.SealPosition) bool {
	p := c.position
	return actual.Component == p.Component && actual.Epoch == p.Epoch && actual.Sequence == p.Sequence && actual.Hash == p.Hash && actual.Revision == p.Revision && actual.MetadataBaseline == p.MetadataBaseline
}

type haFacade struct {
	base       *standaloneFacade
	role       authenticatedstate.SealRole
	permission HAFacadePermission
}

func (f *haFacade) withPermission(ctx context.Context, visit func(context.Context, authenticatedstate.SealPosition) error) error {
	var mu sync.Mutex
	active, entered := true, false
	var callbackErr error
	err := f.permission(ctx, func(ctx context.Context, p authenticatedstate.SealPosition) error {
		mu.Lock()
		defer mu.Unlock()
		if !active || entered {
			callbackErr = errors.Join(callbackErr, ErrSource)
			return ErrSource
		}
		entered = true
		callbackErr = visit(ctx, p)
		return callbackErr
	})
	// Join an actual borrowed callback before revoking its private scope.
	mu.Lock()
	defer mu.Unlock()
	active = false
	if !entered {
		return errors.Join(ErrSource, err)
	}
	return errors.Join(err, callbackErr)
}

// HAFacade lends the existing DAO only from the actual selected source owner
// with installed Primary admission/ordering/confirmation hooks or a bound
// Replica sink. The caller supplies independent same-term permission on each
// operation. Closing this DAO joins its original Baseline before source fees
// may retire. This constructor does not grant Runtime readiness or promotion.
func (b *Baseline) HAFacade(ctx context.Context, manager *resourcebudget.Manager, identity [32]byte, instanceID string, keyGeneration uint64, selected AuthenticatedMetadataSealReference, role authenticatedstate.SealRole, permission HAFacadePermission) (boltstore.FacadeBackend, error) {
	if b == nil || manager == nil || permission == nil || !b.mu.TryLock() {
		return nil, ErrSource
	}
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.readOnly || b.haPeerTransition != nil || !b.ready.Load() || !b.linked || b.selectedStartup || b.budget != manager || b.evidence.Identity != identity || b.evidence.InstanceID != instanceID || b.evidence.KeyGeneration != keyGeneration {
		return nil, ErrSource
	}
	switch role {
	case authenticatedstate.SealPrimary:
		if b.haPrimaryPrepared == nil || b.haPrimaryPrepared.Baseline != b.position.Baseline || b.haPrimaryPrepared.FileRevision != b.position.FileRevision {
			return nil, ErrSource
		}
	case authenticatedstate.SealReplica:
		if b.haReplicaPrepared == nil || b.haReplicaPrepared.Baseline != b.position.Baseline || b.haReplicaPrepared.FileRevision != b.position.FileRevision {
			return nil, ErrSource
		}
	default:
		return nil, ErrSource
	}
	actual, err := b.sealReferenceLocked(ctx)
	if err != nil || actual != selected {
		return nil, errors.Join(ErrSource, err)
	}
	b.businessOnce.Do(func() { b.businessLane = make(chan struct{}, 1) })
	return &haFacade{base: &standaloneFacade{owner: b}, role: role, permission: permission}, nil
}

func (f *haFacade) View(ctx context.Context, visit func(boltstore.FacadeTransaction) error) (err error) {
	// An active write may be waiting for an ACK that publishes Member progress.
	// Queue on the original bounded business lane before freezing Member, so a
	// reader cannot hold that publication hostage while waiting for the writer.
	release, err := f.base.admit(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, release()) }()
	return f.base.withPhysicalCompletion(ctx, func(ctx context.Context) error {
		return f.withPermission(ctx, func(ctx context.Context, p authenticatedstate.SealPosition) error {
			return f.base.owner.View(context.WithValue(ctx, haFacadeReadCheckKey{}, haFacadeReadCheck{p}), func(tx *Transaction) error { return visit(tx) })
		})
	})
}
func (f *haFacade) Update(ctx context.Context, visit func(boltstore.FacadeTransaction) error) (record metadatajournal.Record, err error) {
	if f.role != authenticatedstate.SealPrimary {
		return record, ErrSource
	}
	// Do not hold Member/Ordering freezes through native fsync and confirmed ACK:
	// those actual hooks must publish progress. Their original coordinator owns
	// term/lifetime admission and the Baseline does not return before confirmation.
	err = f.base.withPhysicalCompletion(ctx, func(ctx context.Context) error {
		if err := f.withPermission(ctx, func(ctx context.Context, _ authenticatedstate.SealPosition) error { return ctx.Err() }); err != nil {
			return err
		}
		var err error
		record, err = f.base.Update(ctx, visit)
		return err
	})
	return record, err
}
func (f *haFacade) Path() string                           { return f.base.Path() }
func (f *haFacade) JournalPath() string                    { return f.base.JournalPath() }
func (f *haFacade) Close() error                           { return f.base.Close() }
func (f *haFacade) CloseContext(ctx context.Context) error { return f.base.CloseContext(ctx) }
func (f *haFacade) HealthError() error                     { return f.base.HealthError() }
func (f *haFacade) JournalState(ctx context.Context) (state boltstore.JournalState, err error) {
	release, err := f.base.admit(ctx)
	if err != nil {
		return state, err
	}
	defer func() { err = errors.Join(err, release()) }()
	err = f.withPermission(ctx, func(ctx context.Context, p authenticatedstate.SealPosition) error {
		var e error
		state, e = f.base.journalStateAdmitted(ctx)
		if e == nil && (state.Epoch != p.Epoch || state.Applied != p.Sequence) {
			return ErrSource
		}
		return e
	})
	return
}

var _ boltstore.FacadeBackend = (*haFacade)(nil)
