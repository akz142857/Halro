package metadatastate

import (
	"context"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// PrepareHAReplicaPhysicalEscrow installs a fixed original COW/native window
// before the actual receiver can land a fact. Each future frame needs a target
// even when the Collector subsequently applies one frame per job. The window
// owns one shared workspace; it is not allocated once for every received batch.
func (b *Baseline) PrepareHAReplicaPhysicalEscrow(ctx context.Context, count int) error {
	if b == nil || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	s := b.haPhysicalSource
	if s == nil || s.role != authenticatedstate.SealReplica || !b.physicalEnabled || s.receiveWindow != nil || !b.ready.Load() {
		b.mu.Unlock()
		return ErrSource
	}
	b.mu.Unlock()
	w, err := b.ReservePhysicalTerminals(ctx, count)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s != b.haPhysicalSource || !b.ready.Load() || s.receiveWindow != nil {
		return errors.Join(ErrSource, w.closeLocked())
	}
	s.receiveWindow = w
	if err := b.installHAPhysicalReceiveLocked(); err != nil {
		s.receiveWindow = nil
		return errors.Join(err, w.closeLocked())
	}
	return nil
}

func (b *Baseline) installHAPhysicalReceiveLocked() error {
	s := b.haPhysicalSource
	if s.receiveRegistration != nil {
		return s.receiveRegistration.ValidateOwner(b.journal)
	}
	r, err := b.journal.InstallBoundPhysicalReceive(s.permission, b.withHAPhysicalReceiveWorkspace, b.prepareHAPhysicalReceive)
	if err == nil {
		s.receiveRegistration = r
	}
	return err
}

func (b *Baseline) withHAPhysicalReceiveWorkspace(ctx context.Context, visit func(context.Context) error) error {
	b.mu.Lock()
	s := b.haPhysicalSource
	if s == nil || s.closing || !b.ready.Load() || s.receiveWindow == nil || s.receiveWindow.closed.Load() {
		b.mu.Unlock()
		return authenticatedstate.ErrClosed
	}
	w := s.receiveWindow
	s.receiveJobs.Add(1)
	b.mu.Unlock()
	defer s.receiveJobs.Done()
	job, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	return resourcebudget.WithCompletionWorkspace(w.completion.Bind(w.Bind(job)), visit)
}

func (b *Baseline) joinHAPhysicalReceives() {
	b.mu.Lock()
	s := b.haPhysicalSource
	if s != nil {
		s.closing = true
	}
	b.mu.Unlock()
	if s != nil {
		s.receiveJobs.Wait()
	}
}

func (b *Baseline) prepareHAPhysicalReceive(ctx context.Context, before, after metadatajournal.Head) (context.Context, func(bool, error) error, error) {
	if !b.mu.TryLock() {
		return ctx, nil, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	s := b.haPhysicalSource
	if s == nil || s.role != authenticatedstate.SealReplica || !b.physicalEnabled || !b.ready.Load() || s.receiveWindow == nil {
		return ctx, nil, ErrSource
	}
	w := s.receiveWindow
	if w.closed.Load() || before != b.journal.Head() || before.Baseline != s.permission.Baseline || before.FileRevision != s.permission.FileRevision || after.Epoch != before.Epoch || after.Sequence <= before.Sequence || after.Sequence < b.position.Sequence || after.Sequence-b.position.Sequence > uint64(w.remaining) {
		return ctx, nil, resourcebudget.ErrPhysicalAllocation
	}
	var appendToken *metadatajournal.PhysicalAppend
	err := b.withTerminalPhysicalNativeLocked(w.completion.Bind(ctx), func(q *metadatajournal.PhysicalNativeLoan) error {
		var err error
		appendToken, err = q.Append()
		return err
	})
	if err != nil {
		return ctx, nil, err
	}
	finish := func(accepted bool, operationErr error) error {
		if !accepted || operationErr == nil {
			return nil
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		b.ready.Store(false)
		b.journal.HoldPhysicalForRecovery()
		failure := errors.Join(ownedfile.ErrRetirement, operationErr, b.store.HoldPhysicalPreparationForRecovery(context.Background()))
		b.retirementErr = errors.Join(b.retirementErr, failure)
		w.closeErr = failure
		return &PhysicalRetirementFailure{Owner: b, Cause: failure}
	}
	return appendToken.Bind(w.Bind(ctx)), finish, nil
}

// withHAReplicaReplayWorkspace lends only the original accepted receive pool.
// Expected/through still undergo the existing full native verification and the
// caller's independently confirmed HA source scope; this loan grants neither.
func (b *Baseline) withHAReplicaReplayWorkspace(ctx context.Context, expected metadatajournal.Head, through uint64, visit func(context.Context, *resourcebudget.Permit) (metadatajournal.Head, error)) (metadatajournal.Head, error) {
	if b == nil || !b.mu.TryLock() {
		return metadatajournal.Head{}, authenticatedstate.ErrBusy
	}
	s := b.haPhysicalSource
	if s == nil || s.role != authenticatedstate.SealReplica || !b.physicalEnabled {
		b.mu.Unlock()
		return visit(ctx, nil)
	}
	w := s.receiveWindow
	if w == nil && through == b.position.Sequence && b.ready.Load() && !s.closing && s.withConfirmedApply != nil {
		// A Ledger-only confirmed prefix owes no Metadata COW. Preserve the
		// original confirmed-source gate and full journal authentication even
		// though no Metadata frame is reduced and no Applied offset advances.
		s.receiveJobs.Add(1)
		b.mu.Unlock()
		defer s.receiveJobs.Done()
		job, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(b.ctx, cancel)
		defer func() { stop(); cancel() }()
		var head metadatajournal.Head
		var mu sync.Mutex
		active, entered := true, false
		var callbackErr error
		err := s.withConfirmedApply(job, b, expected, through, func(ctx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if !active || entered || ctx == nil {
				callbackErr = errors.Join(callbackErr, ErrSource)
				return ErrSource
			}
			entered = true
			head, callbackErr = visit(ctx, nil)
			return callbackErr
		})
		mu.Lock()
		defer mu.Unlock()
		active = false
		if !entered {
			return metadatajournal.Head{}, errors.Join(ErrSource, err)
		}
		return head, errors.Join(err, callbackErr)
	}
	if w == nil || w.closed.Load() || w.completion == nil || !b.ready.Load() || s.closing || s.withConfirmedApply == nil {
		b.mu.Unlock()
		return metadatajournal.Head{}, resourcebudget.ErrPhysicalAllocation
	}
	s.receiveJobs.Add(1)
	b.mu.Unlock()
	defer s.receiveJobs.Done()
	job, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	var head metadatajournal.Head
	var mu sync.Mutex
	active, entered := true, false
	var callbackErr error
	// The actual confirmed receiver scope authorizes this one finite reduction.
	// Native head/receipt bytes alone cannot authorize an Applied advance.
	err := resourcebudget.WithCompletionWorkspace(w.completion.Bind(w.Bind(job)), func(ctx context.Context) error {
		return s.withConfirmedApply(ctx, b, expected, through, func(ctx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if !active || entered || ctx == nil {
				callbackErr = errors.Join(callbackErr, ErrSource)
				return ErrSource
			}
			entered = true
			head, callbackErr = visit(ctx, &w.work)
			return callbackErr
		})
	})
	mu.Lock()
	defer mu.Unlock()
	active = false
	if !entered {
		return metadatajournal.Head{}, errors.Join(ErrSource, err)
	}
	return head, errors.Join(err, callbackErr)
}

func (b *Baseline) reduceHAPhysicalFrameLocked(ctx context.Context, visit func(context.Context) error) (resultErr error) {
	s := b.haPhysicalSource
	if !b.physicalEnabled || s == nil {
		return visit(ctx)
	}
	w := s.receiveWindow
	recoveryPrimary := s.role == authenticatedstate.SealPrimary && b.haPrimaryPrepared == nil && s.recoveryPermitted.Offset > 0
	if (s.role != authenticatedstate.SealReplica && !recoveryPrimary) || w == nil || w.closed.Load() || w.remaining < 1 || b.physicalDebt < 1 {
		return resourcebudget.ErrPhysicalAllocation
	}
	u, err := b.store.PreparePhysicalUpdate(ctx, w.cow)
	if err != nil {
		return err
	}
	resultErr = visit(u.Bind(ctx))
	resultErr = errors.Join(resultErr, u.Finish(true, resultErr))
	if resultErr == nil {
		w.remaining--
		b.physicalDebt--
	} else {
		b.retirementErr = errors.Join(b.retirementErr, resultErr)
	}
	return resultErr
}
