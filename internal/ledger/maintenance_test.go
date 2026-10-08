package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMaintenanceAcceptedApplyRetainsOwnerAndNativeReadAfterCallerCancellation(t *testing.T) {
	type ownerKey struct{}
	owner := new(int)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ownerKey{}, owner))
	defer cancel()
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1,
		AfterDurable:  func(batch DurableBatch) (uint64, error) { cancel(); return batch.LastSequence, nil },
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	lease, err := l.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var escaped context.Context
	record, receipt, err := lease.AppendApply(ctx, validReservation("evt_owned_maintenance", "att_owned_maintenance"), func(applyCtx context.Context, record Record) error {
		escaped = applyCtx
		if ctx.Err() == nil || applyCtx.Err() != nil || applyCtx.Value(ownerKey{}) != owner {
			t.Fatal("actual accepted maintenance callback lost its owner", applyCtx.Err())
		}
		found, err := lease.ReadAcceptedApplyHead(applyCtx, func(readCtx context.Context, head Watermark, hash [32]byte) error {
			if head != (Watermark{record.Generation, record.Offset, record.Sequence}) || hash != record.Hash || readCtx.Value(ownerKey{}) != owner {
				t.Fatal("actual native read scope lost accepted evidence")
			}
			return readCtx.Err()
		})
		if !found {
			t.Fatal("accepted maintenance read scope absent")
		}
		return err
	})
	if err != nil || record.Sequence != 1 || receipt.Watermark.Sequence != 1 || l.Status().Load() != AccountingHealthy {
		t.Fatal(record, receipt, err)
	}
	if found, err := lease.ReadAcceptedApplyHead(context.WithoutCancel(escaped), func(context.Context, Watermark, [32]byte) error { t.Fatal("escaped read"); return nil }); !found || !errors.Is(err, ErrLedgerMaintenance) {
		t.Fatal("accepted read authority survived its actual callback", found, err)
	}
}

func TestLedgerMaintenanceValidateExactOwnerAndNativeMode(t *testing.T) {
	for _, replicated := range []bool{false, true} {
		t.Run(fmt.Sprint(replicated), func(t *testing.T) {
			options := Options{ChainKey: make([]byte, 32)}
			if replicated {
				options.AfterDurable = func(DurableBatch) (uint64, error) { return 1, nil }
				options.WaitConfirmed = func(context.Context, uint64) error { return nil }
			}
			l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), options)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			m, err := l.BeginMaintenance(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if mode, err := m.ValidateOwner(l); err != nil || mode != replicated {
				t.Fatal(mode, err)
			}
			for _, other := range []*Log{nil, {}} {
				if _, err := m.ValidateOwner(other); err == nil {
					t.Fatal("accepted different owner")
				}
			}
			var absent *MaintenanceLease
			if _, err := absent.ValidateOwner(l); err == nil {
				t.Fatal("accepted absent lease")
			}
			m.Close()
			next, err := l.BeginMaintenance(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if _, err := m.ValidateOwner(l); err == nil {
				t.Fatal("accepted old owner")
			}
			if mode, err := next.ValidateOwner(l); err != nil || mode != replicated {
				t.Fatal(mode, err)
			}
			l.Status().RequireRecovery()
			if _, err := next.ValidateOwner(l); err == nil {
				t.Fatal("accepted unhealthy owner")
			}
			l.Close()
			if _, err := next.ValidateOwner(l); err == nil {
				t.Fatal("accepted closed native owner")
			}
		})
	}
}

func TestMaintenanceFrozenReadCandidateRejectionPreservesNativeAuthority(t *testing.T) {
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	m, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("independent component root does not match")
	err = m.ReadFrozenHead(context.Background(), func(ctx context.Context, head Watermark, hash [32]byte) error {
		if _, err := l.Append(ctx, validReservation("excluded", "excluded")); !errors.Is(err, ErrLedgerMaintenance) {
			t.Fatal("read job allowed an ordinary native mutation", err)
		}
		return rejected
	})
	if !errors.Is(err, rejected) || l.Status().Load() != AccountingHealthy {
		t.Fatal("rejected disposable root retired authoritative facts", err, l.Status().Load())
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(context.Background(), validReservation("permitted", "permitted")); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceFrozenReadCloseCancelsAndJoinsCompleteVisitor(t *testing.T) {
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	m, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	done := make(chan error, 1)
	go func() {
		done <- m.ReadFrozenHead(context.Background(), func(ctx context.Context, _ Watermark, _ [32]byte) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil
		})
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	<-canceled
	select {
	case err := <-closed:
		t.Fatal("Close returned while visitor still owned its IO", err)
	default:
	}
	finish()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil || l.Status().Load() != AccountingHealthy {
		t.Fatal(err, l.Status().Load())
	}
}

func TestMaintenanceFrozenReadRealNativeCorruptionStillRequiresRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	l, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Append(context.Background(), validReservation("real", "real")); err != nil {
		t.Fatal(err)
	}
	m, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff}, 12); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	err = m.ReadFrozenHead(context.Background(), func(ctx context.Context, _ Watermark, _ [32]byte) error {
		_, err := l.ReplayAuthenticated(ctx, ReplayAnchor{}, nil)
		return err
	})
	if err == nil || l.Status().Load() != AccountingRecoveryRequired {
		t.Fatal("frozen component read erased actual source corruption", err, l.Status().Load())
	}
}

func waitMaintenanceGate(t *testing.T, l *Log) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		l.admission.Lock()
		owned := l.maintenance != nil
		l.admission.Unlock()
		if owned {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("maintenance did not withdraw mutation admission")
		}
		time.Sleep(time.Millisecond)
	}
}

func maintenanceBlockedLog(t *testing.T) (*Log, *snapshotBlockingDurability, func()) {
	t.Helper()
	var d *snapshotBlockingDurability
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1,
		WrapDurability: func(f *os.File) DurabilityWriter {
			d = &snapshotBlockingDurability{file: f, entered: make(chan struct{}), release: make(chan struct{})}
			return d
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { close(d.release) }) }
	t.Cleanup(func() {
		release()
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	d.block.Store(true)
	return l, d, release
}

func TestLedgerMaintenanceDrainsAndExcludesMutationsAndOldOwner(t *testing.T) {
	l, d, release := maintenanceBlockedLog(t)
	business := make(chan error, 1)
	go func() {
		_, err := l.Append(context.Background(), validReservation("evt_business", "att_business"))
		business <- err
	}()
	<-d.entered
	type acquired struct {
		lease *MaintenanceLease
		err   error
	}
	done := make(chan acquired, 1)
	go func() { lease, err := l.BeginMaintenance(context.Background()); done <- acquired{lease, err} }()
	waitMaintenanceGate(t, l)
	if _, err := l.Append(context.Background(), validReservation("evt_refused", "att_refused")); !errors.Is(err, ErrLedgerMaintenance) {
		t.Fatal(err)
	}
	if _, err := l.Roll(); !errors.Is(err, ErrLedgerMaintenance) {
		t.Fatal(err)
	}
	if _, err := l.Compact(1); !errors.Is(err, ErrLedgerMaintenance) {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		t.Fatal("lease did not drain accepted fsync", result.err)
	case <-time.After(10 * time.Millisecond):
	}
	release()
	if err := <-business; err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	m := result.lease
	defer m.Close()
	if _, err := l.BeginMaintenance(context.Background()); err == nil {
		t.Fatal("two owners admitted")
	}
	r, receipt, err := m.AppendApply(context.Background(), validReservation("evt_recovery", "att_recovery"), func(ctx context.Context, r Record) error {
		head, hash, _ := l.ChainHead() // Must not be called under l.mu.
		if head != (Watermark{r.Generation, r.Offset, r.Sequence}) || hash != r.Hash {
			return errors.New("Apply did not receive actual durable head")
		}
		return ctx.Err()
	})
	if err != nil || r.Sequence != 2 || receipt.Watermark.Sequence != 2 {
		t.Fatal(r, receipt, err)
	}
	if err := m.WithHead(context.Background(), func(ctx context.Context, head Watermark, hash [32]byte) error {
		if head.Sequence != 2 || hash != r.Hash {
			return errors.New("maintenance lost frozen head")
		}
		if _, err := l.Roll(); !errors.Is(err, ErrLedgerMaintenance) {
			return errors.New("Roll entered confirmation window")
		}
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(context.Background(), validReservation("evt_after", "att_after")); err != nil {
		t.Fatal(err)
	}
	if result, err := l.Roll(); err != nil || !result.Rolled {
		t.Fatal(result, err)
	}
	newOwner, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer newOwner.Close()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.AppendApply(context.Background(), validReservation("evt_old", "att_old"), func(context.Context, Record) error { return nil }); err == nil {
		t.Fatal("old owner wrote into new maintenance")
	}
	if err := newOwner.WithHead(context.Background(), func(context.Context, Watermark, [32]byte) error { return nil }); err != nil {
		t.Fatal("old release revoked new owner", err)
	}
}

func TestLedgerMaintenanceAcquisitionCancellationPreservesAcceptedAppend(t *testing.T) {
	l, d, release := maintenanceBlockedLog(t)
	business := make(chan error, 1)
	go func() {
		_, err := l.Append(context.Background(), validReservation("evt_business", "att_business"))
		business <- err
	}()
	<-d.entered
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		m, err := l.BeginMaintenance(ctx)
		if m != nil {
			m.Close()
		}
		done <- err
	}()
	waitMaintenanceGate(t, l)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() {
		_, err := l.Append(context.Background(), validReservation("evt_second", "att_second"))
		second <- err
	}()
	release()
	if err := <-business; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal("canceled acquisition left admission withdrawn", err)
	}
	head, _, _ := l.ChainHead()
	if head.Sequence != 2 || l.status.Load() != AccountingHealthy {
		t.Fatal(head, l.status.Load())
	}
}

func TestLedgerMaintenanceApplyFailurePoisonsBeforeReleasingOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	l := openChained(t, path, NewStatus())
	defer l.Close()
	m, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	want := errors.New("injected projection transaction failure")
	r, receipt, err := m.AppendApply(context.Background(), validReservation("evt_durable", "att_durable"), func(context.Context, Record) error { return want })
	if !errors.Is(err, want) || r.Sequence != 0 || receipt.Watermark.Sequence != 1 || l.status.Load() != AccountingRecoveryRequired {
		t.Fatal(r, receipt, err, l.status.Load())
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(context.Background(), validReservation("evt_after", "att_after")); err == nil {
		t.Fatal("failed Apply reopened healthy writes")
	}
	if _, err := l.BeginMaintenance(context.Background()); err == nil {
		t.Fatal("unapplied fact resumed without fresh recovery")
	}
	visits := 0
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, func(r Record) error {
		visits++
		if r.Event.EventID != "evt_durable" {
			return errors.New("failed Apply lost original fact")
		}
		return nil
	})
	if err != nil || partial || visits != 1 || report.Head.Sequence != 1 {
		t.Fatal(report, partial, visits, err)
	}
}

func TestLedgerMaintenanceCloseCancelsConfirmationAndJoinsCallback(t *testing.T) {
	l := openChained(t, filepath.Join(t.TempDir(), "ledger.wal"), NewStatus())
	defer l.Close()
	m, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer func() { once.Do(func() { close(release) }); m.Close() }()
	verified := make(chan error, 1)
	go func() {
		verified <- m.WithHead(context.Background(), func(ctx context.Context, _ Watermark, _ [32]byte) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return ctx.Err()
		})
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("lease Close did not cancel confirmation")
	}
	select {
	case err := <-closed:
		t.Fatal("Close returned while callback owned resources", err)
	case <-time.After(10 * time.Millisecond):
	}
	if _, err := l.Append(context.Background(), validReservation("evt_during_close", "att_during_close")); !errors.Is(err, ErrLedgerMaintenance) {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	if err := <-verified; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if l.status.Load() != AccountingRecoveryRequired {
		t.Fatal("unconfirmed maintenance reopened health", l.status.Load())
	}
}

func TestLedgerCloseJoinsMaintenanceApplyBeforeClearingNativeKey(t *testing.T) {
	l := openChained(t, filepath.Join(t.TempDir(), "ledger.wal"), NewStatus())
	m, err := l.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer func() { once.Do(func() { close(release) }); m.Close(); l.Close() }()
	written := make(chan error, 1)
	go func() {
		_, _, err := m.AppendApply(context.Background(), validReservation("evt_close", "att_close"), func(ctx context.Context, _ Record) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return ctx.Err()
		})
		written <- err
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("log Close did not cancel maintenance Apply")
	}
	select {
	case err := <-closed:
		t.Fatal("log closed before Apply exit", err)
	case <-time.After(10 * time.Millisecond):
	}
	l.mu.Lock()
	retained := len(l.chainKey) == 32 && l.file != nil
	l.mu.Unlock()
	if !retained {
		t.Fatal("native key/file released before owner joined")
	}
	once.Do(func() { close(release) })
	if err := <-written; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	cleared := l.chainKey == nil && l.file == nil
	l.mu.Unlock()
	if !cleared {
		t.Fatal("native resources retained after joined Close")
	}
}

func TestLedgerMaintenanceOwnsRealProjectionAppendAndHeadChecks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.wal")
	l := openChained(t, path, NewStatus())
	defer l.Close()
	events := projectionTestLifecycle(1, false)
	for _, e := range events[:2] {
		if _, err := l.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	m, err := l.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var p *LedgerProjection
	if err := m.WithHead(ctx, func(ctx context.Context, head Watermark, hash [32]byte) error {
		report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
		if err != nil || partial || report.Head != head || report.ChainHash != hash {
			return errors.Join(err, errors.New("input native cut does not match authenticated history"))
		}
		p, err = BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(dir, "projection.db"), report)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, e := range events[2:] {
		if _, _, err := m.AppendApply(ctx, e, func(ctx context.Context, r Record) error { return p.Apply(ctx, r) }); err != nil {
			t.Fatal(err)
		}
		if err := m.WithHead(ctx, func(ctx context.Context, head Watermark, hash [32]byte) error {
			applied, appliedHash, err := p.ReadPosition(ctx)
			if err != nil || applied != head || appliedHash != hash {
				return errors.Join(err, errors.New("derived head not applied through native cut"))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	request, found, err := p.ReadRequest(ctx, events[0].RequestID)
	if err != nil || !found || request.Finalized == nil || request.Finalized.Sequence != 5 {
		t.Fatal(request, found, err)
	}
}

func TestLedgerMaintenanceRefusesUnverifiedRolesAndCancelsQueuedOperation(t *testing.T) {
	for _, mode := range []string{"keyless", "replica", "closed"} {
		t.Run(mode, func(t *testing.T) {
			options := Options{}
			if mode != "keyless" {
				options.ChainKey = testChainKey
			}
			options.Replica = mode == "replica"
			path := filepath.Join(t.TempDir(), "ledger.wal")
			if mode == "replica" {
				seed := openChained(t, path, NewStatus())
				if err := seed.Close(); err != nil {
					t.Fatal(err)
				}
			}
			l, err := OpenWithOptions(path, NewStatus(), options)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			if mode == "closed" {
				if err := l.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if m, err := l.BeginMaintenance(context.Background()); err == nil || m != nil {
				if m != nil {
					m.Close()
				}
				t.Fatal("unverified/closed owner admitted", mode, err)
			}
		})
	}
	t.Run("queued-operation", func(t *testing.T) {
		l := openChained(t, filepath.Join(t.TempDir(), "ledger.wal"), NewStatus())
		m, err := l.BeginMaintenance(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer func() { once.Do(func() { close(release) }); m.Close(); l.Close() }()
		verified := make(chan error, 1)
		go func() {
			verified <- m.WithHead(context.Background(), func(context.Context, Watermark, [32]byte) error { close(entered); <-release; return nil })
		}()
		<-entered
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		called := false
		if _, _, err := m.AppendApply(ctx, validReservation("evt_queued", "att_queued"), func(context.Context, Record) error { called = true; return nil }); !errors.Is(err, context.DeadlineExceeded) || called {
			t.Fatal("queued operation ignored cancellation", called, err)
		}
		head, _, _ := l.ChainHead()
		if head.Sequence != 0 || l.status.Load() != AccountingHealthy {
			t.Fatal("unaccepted cancellation fabricated native fact or poisoned health", head, l.status.Load())
		}
		once.Do(func() { close(release) })
		if err := <-verified; err != nil {
			t.Fatal(err)
		}
	})
}
