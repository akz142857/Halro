package metadatajournal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type boundPhysicalOwner struct {
	manager          *resourcebudget.Manager
	guard            *lock.Lock
	authority        BoundReplicationAuthority
	replica          bool
	debt             resourcebudget.Permit
	receiveGate      sync.Mutex
	receivePrepare   BoundPhysicalReceivePreparation
	receiveWorkspace func(context.Context, func(context.Context) error) error
	receiveCancel    context.CancelFunc
}

// BoundPhysicalReceivePreparation is installed by the actual selected Replica
// Metadata owner. The before/after heads have already been MAC-authenticated on
// the original native descriptor and original received bytes. No bytes have
// been written yet. Its finish joins preparation even on pre-write refusal.
type BoundPhysicalReceivePreparation func(context.Context, Head, Head) (context.Context, func(bool, error) error, error)

type BoundPhysicalReceiveRegistration struct {
	log   *Log
	owner *boundPhysicalOwner
}

func (r *BoundPhysicalReceiveRegistration) ValidateOwner(l *Log) error {
	if r == nil || l == nil || r.log != l {
		return authenticatedstate.ErrSealIneligible
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.availabilityError() != nil || l.physical == nil || l.physical.bound != r.owner || r.owner.receivePrepare == nil {
		return authenticatedstate.ErrSealIneligible
	}
	return nil
}

func (l *Log) InstallBoundPhysicalReceive(a BoundReplicationAuthority, workspace func(context.Context, func(context.Context) error) error, prepare BoundPhysicalReceivePreparation) (*BoundPhysicalReceiveRegistration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.availabilityError(); err != nil {
		return nil, err
	}
	if prepare == nil || workspace == nil || !l.replica || l.physical == nil || !l.physical.enabled || l.physical.bound == nil || !l.physical.bound.replica || l.physical.bound.authority != a || l.physical.bound.receivePrepare != nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	l.physical.bound.receivePrepare = prepare
	l.physical.bound.receiveWorkspace = workspace
	return &BoundPhysicalReceiveRegistration{log: l, owner: l.physical.bound}, nil
}

func (l *Log) withBoundPhysicalReceiveWorkspace(ctx context.Context, visit func(context.Context) error) error {
	l.mu.Lock()
	if l.physical == nil || !l.physical.enabled {
		l.mu.Unlock()
		return visit(ctx)
	}
	if l.physical.bound == nil || l.physical.bound.receiveWorkspace == nil || !l.replica {
		l.mu.Unlock()
		return authenticatedstate.ErrSealIneligible
	}
	workspace := l.physical.bound.receiveWorkspace
	l.mu.Unlock()
	return workspace(ctx, visit)
}

func (l *Log) beginBoundPhysicalReceive(ctx context.Context, epoch, first, last uint64, frames []byte) (context.Context, func(error) error, error) {
	l.mu.Lock()
	if l.physical == nil || !l.physical.enabled {
		l.mu.Unlock()
		return ctx, func(error) error { return nil }, nil
	}
	p := l.physical.bound
	if p == nil || !p.replica || p.receivePrepare == nil || l.availabilityError() != nil || l.terminalErr != nil {
		l.mu.Unlock()
		return ctx, nil, authenticatedstate.ErrSealIneligible
	}
	if !p.receiveGate.TryLock() {
		l.mu.Unlock()
		return ctx, nil, authenticatedstate.ErrBusy
	}
	if first <= l.sequence {
		l.mu.Unlock()
		p.receiveGate.Unlock()
		return ctx, func(error) error { return nil }, nil
	}
	before := Head{Epoch: l.epoch, Sequence: l.sequence, Offset: l.offset, Hash: l.lastHash, TrimmedThrough: l.trimmedThrough, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}
	if epoch != before.Epoch || first != before.Sequence+1 || last < first || last-first >= resourcebudget.MaxCompletionBatch || len(frames) == 0 || int64(len(frames)) > l.maxBytes-l.offset {
		l.mu.Unlock()
		p.receiveGate.Unlock()
		return ctx, nil, ErrCorrupt
	}
	work, err := p.manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: FrozenReadRAMBytes})
	if err != nil {
		l.mu.Unlock()
		p.receiveGate.Unlock()
		return ctx, nil, err
	}
	after, partial, err := scanFromContext(ctx, bytes.NewReader(frames), l.key, before, false, nil)
	if err != nil || partial || after.Sequence != last || after.Offset != before.Offset+int64(len(frames)) {
		l.mu.Unlock()
		p.receiveGate.Unlock()
		return ctx, nil, errors.Join(ErrCorrupt, err, work.Release())
	}
	job, cancel := context.WithCancel(ctx)
	if err := work.Release(); err != nil {
		cancel()
		l.mu.Unlock()
		p.receiveGate.Unlock()
		return ctx, nil, err
	}
	p.receiveCancel = cancel
	l.freezeWG.Add(1)
	l.mu.Unlock()
	finishJoin := func() {
		cancel()
		l.mu.Lock()
		p.receiveCancel = nil
		l.mu.Unlock()
		l.freezeWG.Done()
		p.receiveGate.Unlock()
	}
	prepared, finish, err := p.receivePrepare(job, before, after)
	if err != nil || prepared == nil || finish == nil || prepared.Done() != job.Done() {
		finishJoin()
		return ctx, nil, errors.Join(err, authenticatedstate.ErrSealIneligible)
	}
	appendToken, _ := prepared.Value(physicalAppendKey{}).(*PhysicalAppend)
	if appendToken == nil || appendToken.log != l {
		err = errors.Join(authenticatedstate.ErrSealIneligible, finish(false, authenticatedstate.ErrSealIneligible))
		finishJoin()
		return ctx, nil, err
	}
	return prepared, func(operationErr error) error {
		defer finishJoin()
		return finish(appendToken.Accepted(), operationErr)
	}, nil
}

func (l *Log) cancelBoundPhysicalReceiveLocked() {
	if l.physical != nil && l.physical.bound != nil && l.physical.bound.receiveCancel != nil {
		l.physical.bound.receiveCancel()
	}
}

// WithBoundPhysicalNative lends only the actual configured format2 HA writer.
// The independently selected Metadata owner must qualify its root and role
// before entering. This native loan grants no confirmation or root selection.
// Standalone physical loans continue to reject this writer.
func (l *Log) WithBoundPhysicalNative(ctx context.Context, manager *resourcebudget.Manager, guard *lock.Lock, authority BoundReplicationAuthority, replica bool, visit func(*PhysicalNativeLoan) error) (resultErr error) {
	return l.withBoundPhysicalNative(ctx, manager, guard, authority, replica, false, visit)
}
func (l *Log) WithBoundPhysicalNativeRecovery(ctx context.Context, manager *resourcebudget.Manager, guard *lock.Lock, authority BoundReplicationAuthority, replica bool, visit func(*PhysicalNativeLoan) error) error {
	return l.withBoundPhysicalNative(ctx, manager, guard, authority, replica, true, visit)
}
func (l *Log) withBoundPhysicalNative(ctx context.Context, manager *resourcebudget.Manager, guard *lock.Lock, authority BoundReplicationAuthority, replica, recovery bool, visit func(*PhysicalNativeLoan) error) (resultErr error) {
	if l == nil || manager == nil {
		return authenticatedstate.ErrSealIneligible
	}
	credit, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: FrozenReadRAMBytes})
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	defer func() {
		if l.physical != nil && (errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ownedfile.ErrReplaced) || errors.Is(resultErr, resourcebudget.ErrPhysicalAllocationUncertain)) {
			if l.physical.bound == nil {
				l.physical.bound = &boundPhysicalOwner{manager: manager, guard: guard, authority: authority, replica: replica}
			}
			if l.physical.bound.debt != (resourcebudget.Permit{}) {
				resultErr = errors.Join(resultErr, credit.Release())
				return
			}
			l.physical.bound.debt = credit
			l.physical.failure = resultErr
			l.terminalErr = resultErr
		} else {
			resultErr = errors.Join(resultErr, credit.Release())
		}
	}()
	if err := errors.Join(ctx.Err(), l.availabilityError()); err != nil {
		return err
	}
	h := Head{Epoch: l.epoch, Sequence: l.sequence, Hash: l.lastHash, Offset: l.offset, TrimmedThrough: l.trimmedThrough, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}
	if visit == nil || manager == nil || guard == nil || !guard.CoversNativePath(l.path) || l.readOnly || l.replica != replica || l.boundReplication == nil || *l.boundReplication != authority || authority.Validate() != nil || !authority.matches(h) || l.maxBytes != authority.MaxFileBytes || l.terminalErr != nil || (!replica && !recovery && (l.beforeDurable == nil || l.afterDurable == nil || l.waitConfirmed == nil)) || (recovery && !l.boundPhysicalRecovery) || (replica && (l.beforeDurable != nil || l.afterDurable != nil || l.waitConfirmed != nil)) {
		return authenticatedstate.ErrSealIneligible
	}
	if l.physical != nil && (l.physical.bound == nil || l.physical.bound.manager != manager || l.physical.bound.guard != guard || l.physical.bound.authority != authority || l.physical.bound.replica != replica) {
		return authenticatedstate.ErrSealIneligible
	}
	if l.physical == nil {
		if err := l.checkPreparedFile(); err != nil {
			return err
		}
		actual, partial, err := scanContext(ctx, io.NewSectionReader(l.file, 0, l.offset), l.key, authority.checkControl)
		if err != nil || partial || actual != h {
			return errors.Join(ErrCorrupt, err)
		}
	}
	err = l.withPhysicalNativeLoanLocked(ctx, func(q *PhysicalNativeLoan) error {
		if l.physical.bound == nil {
			l.physical.bound = &boundPhysicalOwner{manager: manager, guard: guard, authority: authority, replica: replica}
		}
		return visit(q)
	})
	return err
}

// OpenBoundPhysicalExactContext authenticates an independent complete native
// head without repairing any tail. The resulting owner permits only physical
// recovery loans until the original actual HA configuration completes.
func OpenBoundPhysicalExactContext(ctx context.Context, path string, key []byte, expected Head, a BoundReplicationAuthority, replica bool) (*Log, error) {
	if a.Validate() != nil || !a.matches(expected) {
		return nil, ErrIncompatibleFormat
	}
	l, err := OpenBoundExactContext(ctx, path, key, expected, a.MaxFileBytes)
	if err != nil {
		return nil, err
	}
	l.replica = replica
	l.boundReplication = &a
	l.boundPhysicalRecovery = true
	return l, nil
}

func (l *Log) CompleteBoundPhysicalRecovery(a BoundReplicationAuthority, replica bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.availabilityError(); err != nil {
		return err
	}
	if !l.boundPhysicalRecovery || l.replica != replica || l.boundReplication == nil || *l.boundReplication != a || (!replica && (l.beforeDurable == nil || l.afterDurable == nil || l.waitConfirmed == nil)) {
		return authenticatedstate.ErrSealIneligible
	}
	l.boundPhysicalRecovery = false
	return nil
}
