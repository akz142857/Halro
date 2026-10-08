package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

var ErrLedgerMaintenance = errors.New("Ledger mutation is owned by maintenance")

// MaintenanceLease exclusively owns semantic Ledger writes and structure. It
// is not HA role/term authorization, projection trust or business readiness.
// Only a maintenance runtime that closes business admission may acquire it.
// Callbacks run outside the native writer lock and must not reenter this lease
// or Close the log/lease. Ignoring callback cancellation can delay Close.
type MaintenanceLease struct {
	log    *Log
	ctx    context.Context
	cancel context.CancelFunc
	gate   chan struct{}
	// Guarded by log.admission.
	ready    bool
	closed   bool
	readOnly bool
}

// ValidateOwner binds a caller's maintenance component to this exact Log and
// reports the native replication-hook mode. It does not authorize HA roles.
func (m *MaintenanceLease) ValidateOwner(log *Log) (bool, error) {
	if m == nil || log == nil || m.log != log {
		return false, errors.New("maintenance lease belongs to another Ledger")
	}
	log.lifecycle.RLock()
	defer log.lifecycle.RUnlock()
	log.admission.Lock()
	defer log.admission.Unlock()
	if log.closed || log.closeContext.Err() != nil || log.maintenance != m || !m.ready || m.closed || log.status.Load() != AccountingHealthy {
		return false, errors.New("maintenance Ledger owner is unavailable")
	}
	return log.options.Replica || log.options.AfterDurable != nil, nil
}

func (l *Log) beginMutation(owner *MaintenanceLease) (func(), error) {
	l.lifecycle.RLock()
	defer l.lifecycle.RUnlock()
	if l.closed || l.closeContext.Err() != nil {
		return nil, errors.New("ledger is closed")
	}
	l.admission.Lock()
	defer l.admission.Unlock()
	if owner == nil && l.maintenance != nil || owner != nil && (l.maintenance != owner || !owner.ready || owner.closed) {
		return nil, ErrLedgerMaintenance
	}
	if l.mutating == 0 {
		l.drain = make(chan struct{})
	}
	l.mutating++
	return func() {
		l.admission.Lock()
		defer l.admission.Unlock()
		l.mutating--
		if l.mutating == 0 {
			close(l.drain)
		}
	}, nil
}

func (l *Log) waitMutations() {
	for {
		l.admission.Lock()
		count, drain := l.mutating, l.drain
		l.admission.Unlock()
		if count == 0 {
			return
		}
		<-drain
	}
}

// BeginMaintenance first withdraws semantic mutation admission, then joins all
// accepted appends/Roll/Compact before returning. Acquisition cancellation
// releases its gate without exposing a ready lease. It does not interrupt a
// source fsync or claim those previously accepted facts were uncommitted.
func (l *Log) BeginMaintenance(ctx context.Context) (*MaintenanceLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.options.Replica {
		return nil, errors.New("Replica cannot own semantic Ledger recovery")
	}
	return l.beginMaintenance(ctx, false)
}

// BeginReplicaReadMaintenance excludes and joins actual replicated writes for
// bounded read/build jobs. It grants no semantic append or recovery-confirmation
// authority. The ordinary BeginMaintenance guard remains unchanged.
func (l *Log) BeginReplicaReadMaintenance(ctx context.Context) (*MaintenanceLease, error) {
	if l == nil || !l.options.Replica {
		return nil, ErrLedgerMaintenance
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return l.beginMaintenance(ctx, true)
}

func (l *Log) beginMaintenance(ctx context.Context, readOnly bool) (*MaintenanceLease, error) {
	leaseCtx, cancel := context.WithCancel(context.Background())
	m := &MaintenanceLease{log: l, ctx: leaseCtx, cancel: cancel, gate: make(chan struct{}, 1), readOnly: readOnly}
	l.lifecycle.RLock()
	l.admission.Lock()
	if l.closed || l.closeContext.Err() != nil || l.maintenance != nil {
		l.admission.Unlock()
		l.lifecycle.RUnlock()
		cancel()
		return nil, errors.New("Ledger is closed or already owned by maintenance")
	}
	l.maintenance = m
	l.admission.Unlock()
	l.lifecycle.RUnlock()
	// A failed acquisition has admitted no maintenance operation. It may remove
	// only its own gate, even if another acquisition starts after cancellation.
	failed := true
	defer func() {
		if failed {
			l.admission.Lock()
			m.closed = true
			if l.maintenance == m {
				l.maintenance = nil
			}
			l.admission.Unlock()
			cancel()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l.lifecycle.RLock()
		l.admission.Lock()
		if l.closed || l.closeContext.Err() != nil || m.closed || l.maintenance != m || l.status.Load() != AccountingHealthy {
			l.admission.Unlock()
			l.lifecycle.RUnlock()
			return nil, errors.New("Ledger maintenance acquisition is unavailable")
		}
		count, drain := l.mutating, l.drain
		if count == 0 {
			l.mu.Lock()
			authenticated := len(l.chainKey) == 32 && l.file != nil
			l.mu.Unlock()
			if !authenticated {
				l.admission.Unlock()
				l.lifecycle.RUnlock()
				return nil, errors.New("Ledger maintenance requires an authenticated native writer")
			}
			m.ready = true
			failed = false
			l.admission.Unlock()
			l.lifecycle.RUnlock()
			return m, nil
		}
		l.admission.Unlock()
		l.lifecycle.RUnlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.closeSignal:
			return nil, errors.New("ledger is closed")
		case <-drain:
		}
	}
}

func (m *MaintenanceLease) enter(ctx context.Context) (context.Context, func(), error) {
	if m == nil || m.log == nil || m.ctx == nil {
		return nil, nil, errors.New("invalid Ledger maintenance lease")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if m.borrowsHASelectedMaintenance(ctx) {
		return ctx, func() {}, nil
	}
	waitCtx, stopWait := resourcebudget.WorkQueueContext(ctx)
	defer stopWait()
	select {
	case m.gate <- struct{}{}:
	case <-waitCtx.Done():
		return nil, nil, errors.Join(waitCtx.Err(), context.Cause(waitCtx))
	case <-m.ctx.Done():
		return nil, nil, errors.New("Ledger maintenance lease is closed")
	}
	if waitCtx.Err() != nil {
		<-m.gate
		return nil, nil, errors.Join(waitCtx.Err(), context.Cause(waitCtx))
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	return opCtx, func() { stop(); cancel(); <-m.gate }, nil
}

// AppendApply retains ownership through the native durable result and derived
// Apply. A failed Apply latches recovery-required; releasing this lease cannot
// reopen healthy business writes onto an unapplied native fact.
func (m *MaintenanceLease) AppendApply(ctx context.Context, event Event, apply func(context.Context, Record) error) (Record, AppendReceipt, error) {
	if m == nil || m.readOnly {
		return Record{}, AppendReceipt{}, ErrLedgerMaintenance
	}
	if apply == nil {
		return Record{}, AppendReceipt{}, errors.New("maintenance append requires Apply")
	}
	opCtx, release, err := m.enter(ctx)
	if err != nil {
		return Record{}, AppendReceipt{}, err
	}
	defer release()
	var record Record
	receipt, err := m.log.appendWithReceipt(opCtx, event, &record, m, func(r Record) error {
		// An already accepted native fact retains its resource values but no
		// caller cancellation. Actual lease/Log retirement still cancels Apply.
		applyCtx, cancel := context.WithCancel(context.WithoutCancel(opCtx))
		stopLease := context.AfterFunc(m.ctx, cancel)
		stopNative := context.AfterFunc(m.log.closeContext, cancel)
		defer cancel()
		defer stopLease()
		defer stopNative()
		if m.ctx.Err() != nil || m.log.closeContext.Err() != nil {
			cancel()
		}
		scope := &acceptedApplyReadScope{lease: m, ctx: applyCtx, anchor: ReplayAnchor{Head: Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash, Epoch: r.Epoch}}
		defer scope.close()
		return apply(context.WithValue(applyCtx, acceptedApplyReadKey{}, scope), r)
	})
	if err != nil {
		return Record{}, receipt, err
	}
	return record, receipt, nil
}

// WithHead freezes native position/hash while the callback checks derived
// application and, for HA, binds/waits for a full ordering target. No native
// lock is held across the callback. It is serialized with AppendApply, and
// mutation admission stays closed through confirmation. A callback failure is
// fatal to this writer's readiness and latches recovery-required.
func (m *MaintenanceLease) WithHead(ctx context.Context, verify func(context.Context, Watermark, [32]byte) error) error {
	if m == nil || m.readOnly {
		return ErrLedgerMaintenance
	}
	if verify == nil {
		return errors.New("maintenance head requires verification")
	}
	opCtx, release, err := m.enter(ctx)
	if err != nil {
		return err
	}
	defer release()
	finish, err := m.log.beginMutation(m)
	if err != nil {
		return err
	}
	defer finish()
	if m.log.status.Load() != AccountingHealthy {
		return errors.New("accounting is not healthy")
	}
	head, hash, _ := m.log.ChainHead()
	if err := opCtx.Err(); err != nil {
		return err
	}
	err = verify(opCtx, head, hash)
	if err == nil {
		err = opCtx.Err()
	}
	if err != nil {
		// A bounded derived-projection candidate can be refused solely because
		// its configured file ceiling is too small. The candidate builder owns
		// and removes its inode; native facts and the frozen head remain intact,
		// so this capacity outcome must not poison the authoritative Ledger.
		if !errors.Is(err, ErrProjectionDiskLimit) {
			m.log.status.RequireRecovery()
		}
		return fmt.Errorf("verify maintenance Ledger prefix: %w", err)
	}
	return nil
}

// ReadFrozenHead excludes native mutations and joins one component read/build
// job through the same lease lifecycle. A rejected disposable candidate does
// not retire healthy native facts. Native decoder/integrity failures retain
// their own recovery gate. Unlike WithHead this grants no application or HA
// confirmation success, and callbacks must not reenter the lease or close it.
func (m *MaintenanceLease) ReadFrozenHead(ctx context.Context, visit func(context.Context, Watermark, [32]byte) error) error {
	if visit == nil {
		return errors.New("maintenance read requires a visitor")
	}
	opCtx, release, err := m.enter(ctx)
	if err != nil {
		return err
	}
	defer release()
	finish, err := m.log.beginMutation(m)
	if err != nil {
		return err
	}
	defer finish()
	if m.log.status.Load() != AccountingHealthy {
		return errors.New("accounting is not healthy")
	}
	head, hash, valid := m.log.ChainHead()
	if err := opCtx.Err(); err != nil {
		return err
	}
	visitErr := visit(opCtx, head, hash)
	actual, actualHash, actualValid := m.log.ChainHead()
	if actualValid != valid || actual != head || actualHash != hash {
		m.log.status.RequireRecovery()
		return errors.Join(visitErr, ErrCorrupt, opCtx.Err())
	}
	return errors.Join(visitErr, opCtx.Err())
}

// Close cancels confirmation/application work, rejects new lease operations,
// and joins accepted mutations before returning admission to the log. Failed
// operations keep accounting health latched. Repeated/old closes cannot revoke
// a later lease. A callback must not invoke Close on its own lease.
func (m *MaintenanceLease) Close() error {
	if m == nil || m.log == nil {
		return nil
	}
	l := m.log
	for {
		l.admission.Lock()
		if l.maintenance != m {
			l.admission.Unlock()
			return nil
		}
		m.closed = true
		m.cancel()
		if l.mutating == 0 {
			l.maintenance = nil
			l.admission.Unlock()
			return nil
		}
		drain := l.drain
		l.admission.Unlock()
		<-drain
	}
}
