package budget

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

type blockedRetirementReader struct {
	ledger.AccountingAdmissionReader
	entered chan context.Context
	release chan struct{}
}

type blockedWorkUnitAccountingReader struct {
	ledger.AccountingAdmissionReader
	entered chan struct{}
	release chan struct{}
}

func (r *blockedWorkUnitAccountingReader) ReadWorkUnitAccounting(ctx context.Context, project, unit string, runs bool) (ledger.WorkUnitAccountingView, bool, error) {
	close(r.entered)
	<-r.release
	return r.AccountingAdmissionReader.ReadWorkUnitAccounting(ctx, project, unit, runs)
}

func TestManagerRetireJoinsWholeWorkUnitAccountingRead(t *testing.T) {
	m, state, closeLog := newTestManager(t)
	defer closeLog()
	r := &blockedWorkUnitAccountingReader{state, make(chan struct{}), make(chan struct{})}
	var unblock sync.Once
	defer unblock.Do(func() { close(r.release) })
	m.reads = r
	done := make(chan error, 1)
	go func() {
		_, _, err := m.ReadWorkUnitAccounting(context.Background(), "prj_1", "missing", true)
		done <- err
	}()
	<-r.entered
	retireWithDeadline(t, m)
	m.operationMu.Lock()
	if m.owner == nil || m.activeOperations != 1 {
		t.Error("retirement dropped a live coherent reader")
	}
	m.operationMu.Unlock()
	if _, _, err := m.ReadWorkUnitAccounting(context.Background(), "prj_1", "missing", true); !errors.Is(err, ErrAccountingRetired) {
		t.Fatal(err)
	}
	unblock.Do(func() { close(r.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertRetiredOwnerReleased(t, m)
}

func TestManagerWithdrawAdmissionKeepsReaderUntilNativeCloseAndRetireJoin(t *testing.T) {
	m, state, closeLog := newTestManager(t)
	defer closeLog()
	native := m.log
	r := &blockedWorkUnitAccountingReader{state, make(chan struct{}), make(chan struct{})}
	var once sync.Once
	defer once.Do(func() { close(r.release) })
	m.reads = r
	result := make(chan error, 1)
	go func() {
		_, _, err := m.ReadWorkUnitAccounting(context.Background(), "prj_1", "missing", true)
		result <- err
	}()
	<-r.entered
	m.WithdrawAdmission()
	m.WithdrawAdmission()
	if _, _, err := m.ReadWorkUnitAccounting(context.Background(), "prj_1", "missing", true); !errors.Is(err, ErrAccountingRetired) {
		t.Fatal("withdrawal admitted fresh read", err)
	}
	m.operationMu.Lock()
	retained := m.owner != nil && m.reads == r && m.activeOperations == 1
	m.operationMu.Unlock()
	if !retained {
		t.Fatal("withdrawal dropped accepted reader")
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	retireWithDeadline(t, m)
	once.Do(func() { close(r.release) })
	if err := <-result; err != nil {
		t.Fatal("native Close revoked accepted State read", err)
	}
	if err := m.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertRetiredOwnerReleased(t, m)
}

func (r *blockedRetirementReader) ReadOpenWorkUnitCount(ctx context.Context, project string) (int64, error) {
	r.entered <- ctx
	<-r.release
	return r.AccountingAdmissionReader.ReadOpenWorkUnitCount(ctx, project)
}

func retireWithDeadline(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Retire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("retirement did not wait for the accepted operation", err)
	}
}

func assertRetiredOwnerReleased(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	if !m.retiring || m.activeOperations != 0 || m.owner != nil || m.reads != nil || m.legacyPending != nil || m.log != nil || m.now != nil {
		t.Fatal("retired Manager retained accounting owner or live operations")
	}
	retained := false
	m.projectLocks.Range(func(_, _ any) bool { retained = true; return false })
	if retained || len(m.observers) != 0 {
		t.Fatal("retired Manager retained admission maps or observers")
	}
	if m.Periods() == nil {
		t.Fatal("retirement removed immutable period diagnostics")
	}
}

func TestManagerRetireJoinsPreAppendReadAndNestedPostAppendRead(t *testing.T) {
	m, state, closeLog := newTestManager(t)
	log := m.log
	defer closeLog()
	reader := &blockedRetirementReader{state, make(chan context.Context, 1), make(chan struct{})}
	var unblock sync.Once
	defer unblock.Do(func() { close(reader.release) })
	m.reads = reader
	done := make(chan error, 1)
	go func() {
		unit, replay, err := m.CreateWorkUnit(context.Background(), "prj_1", "key_1", 10, testGovernanceIntent("unit"))
		if err == nil && (replay || unit.ID == "") {
			err = errors.New("accepted creation lost its nested read result")
		}
		done <- err
	}()
	captured := <-reader.entered
	retireWithDeadline(t, m)
	if _, err := m.BeginRequest(context.Background(), "prj_1", "req_refused"); !errors.Is(err, ErrAccountingRetired) {
		t.Fatal("timed-out retirement reopened new admission", err)
	}
	if log.Stats().Records != 0 {
		t.Fatal("blocked admission or rejected call appended")
	}
	m.operationMu.Lock()
	if m.owner == nil || m.reads == nil || m.activeOperations != 1 {
		t.Error("in-flight pre-append read lost its owner")
	}
	m.operationMu.Unlock()
	unblock.Do(func() { close(reader.release) })
	if err := <-done; err != nil {
		t.Fatal("withdrawal interrupted accepted nested append/read", err)
	}
	assertRetiredOwnerReleased(t, m)
	if _, _, err := m.ReadWorkUnit(captured, "prj_1", "absent"); !errors.Is(err, ErrAccountingRetired) {
		t.Fatal("stale internal context resurrected retired admission", err)
	}
	if state.Watermark().Sequence != 1 || log.Stats().Records != 1 || log.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("retirement poisoned or lost the accepted fact")
	}
}

func TestManagerRetireJoinsConfirmationAndPendingAdmissionCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	state := ledger.NewState()
	log, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), ledger.NewStatus(), ledger.Options{
		ChainKey: testChainKey, MaxBatch: 1,
		AfterDurable: func(b ledger.DurableBatch) (uint64, error) {
			// A real native hook can retain a State through its closure even
			// after the Manager's explicit State owner is gone.
			_ = state.Watermark()
			return b.LastSequence, nil
		},
		WaitConfirmed: func(ctx context.Context, index uint64) error {
			if index != 2 {
				return nil
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	m, err := New(log, state, mustResolver(t, "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := m.BeginRequest(context.Background(), "prj_1", "req_1")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := m.ReserveAttempt(context.Background(), request, 100, 7); done <- err }()
	<-entered
	retireWithDeadline(t, m)
	// Native mutations have already exited, but the Manager must retain both
	// its owner and its admitted money until ACK and the deferred cleanup exit.
	lease, err := log.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	admission, unlock := m.lockProject("prj_1")
	key := ledger.BalanceKey{ProjectID: "prj_1", PeriodID: request.Period.ID, TimezoneVersion: request.Period.TimezoneVersion}
	if admission.pending[key] != 7 || state.Watermark().Sequence != 2 {
		t.Error("confirmation wait lost applied fact or in-flight money")
	}
	unlock()
	unblock.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertRetiredOwnerReleased(t, m)
	admission.mu.Lock()
	if len(admission.pending) != 0 || len(admission.runAdmissions) != 0 {
		t.Error("owner was released before deferred money cleanup")
	}
	admission.mu.Unlock()
	if log.Status().Load() != ledger.AccountingHealthy || state.Balance("prj_1", request.Period.ID, request.Period.TimezoneVersion).ReservedMicrosUSD != 7 {
		t.Fatal("retirement manufactured a settlement or poisoned the native log")
	}
	// This explicit refusal also documents why whole business requests must
	// drain BEFORE Retire: Manager calls alone do not cover provider lifetimes.
	if err := m.Finalize(context.Background(), request, "late"); !errors.Is(err, ErrAccountingRetired) {
		t.Fatal("a late root call bypassed retirement", err)
	}
}

func TestManagerRetireJoinsSynchronousObserverBeforeOwnerRelease(t *testing.T) {
	m, p, _ := newProjectedManagerFixture(t, ledger.Options{})
	log := m.log
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	if err := m.AddObserver(func(ledger.Record) { close(entered); <-release }); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := m.BeginRequest(context.Background(), "prj_1", "req_1"); done <- err }()
	<-entered
	retireWithDeadline(t, m)
	if err := m.AddObserver(nil); !errors.Is(err, ErrAccountingRetired) {
		t.Fatal("retiring owner accepted observer registration", err)
	}
	head, _, err := p.ReadPosition(context.Background())
	if err != nil || head.Sequence != 1 {
		t.Fatal("observer did not follow disk Apply", head, err)
	}
	unblock.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertRetiredOwnerReleased(t, m)
	if _, _, err := p.ReadPosition(context.Background()); err != nil || log.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("Retire closed shared projection/log", err)
	}
}

func TestManagerCanceledRetireDoesNotWithdrawAndAllRetiredAPIsRefuse(t *testing.T) {
	m, state, closeLog := newTestManager(t)
	log := m.log
	defer closeLog()
	// The customizable clock is another indirect owner of legacy history;
	// successful retirement must release this closure as well as owner/reads.
	m.now = func() time.Time {
		_ = state.Watermark()
		return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Retire(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx := context.Background()
	request, err := m.BeginRequest(ctx, "prj_1", "req_1")
	if err != nil {
		t.Fatal("pre-canceled retirement withdrew admission", err)
	}
	assertRetiredOwnerReleased(t, m)
	before := log.Stats().Records
	intent := testGovernanceIntent("retired")
	checks := []struct {
		name string
		call func() error
	}{
		{"create_unit", func() error { _, _, e := m.CreateWorkUnit(ctx, "prj_1", "key_1", 10, intent); return e }},
		{"create_definitions", func() error {
			_, _, e := m.CreateWorkUnitWithDefinitions(ctx, "prj_1", "key_1", 10, nil, intent)
			return e
		}},
		{"close_unit", func() error { _, _, e := m.CloseWorkUnit(ctx, "prj_1", "key_1", "unit", intent); return e }},
		{"create_run", func() error {
			_, _, e := m.CreateRun(ctx, "prj_1", "key_1", "unit", 10, time.Hour, 10, intent)
			return e
		}},
		{"close_run", func() error { _, _, e := m.CloseRun(ctx, "prj_1", "key_1", "unit", "run", "reason", intent); return e }},
		{"read_unit", func() error {
			v, found, e := m.ReadWorkUnit(ctx, "prj_1", "unit")
			if found || v.ID != "" {
				t.Error("returned retired unit")
			}
			return e
		}},
		{"read_balance", func() error {
			v, e := m.ReadBalance(ctx, ledger.BalanceKey{ProjectID: "prj_1"})
			if v != (ledger.Balance{}) {
				t.Error("returned retired balance")
			}
			return e
		}},
		{"read_pending_stats", func() error {
			v, e := m.ReadPendingLeaseStats(ctx, time.Now())
			if v != (ledger.PendingLeaseStatistics{}) {
				t.Error("returned retired pending stats")
			}
			return e
		}},
		{"visit_cohort", func() error {
			head, e := m.VisitGovernanceCohort(ctx, ledger.GovernanceCohortQuery{}, func(ledger.GovernanceCohortRow) error {
				t.Error("retired visitor ran")
				return nil
			})
			if head != (ledger.Watermark{}) {
				t.Error("returned retired cohort head")
			}
			return e
		}},
		{"maturity_page", func() error {
			page, e := m.ReadWorkUnitMaturityPage(ctx, nil)
			if len(page.Items) != 0 || page.Watermark != (ledger.Watermark{}) {
				t.Error("returned retired maturity page")
			}
			return e
		}},
		{"read_unit_accounting", func() error {
			v, found, e := m.ReadWorkUnitAccounting(ctx, "prj_1", "unit", true)
			if found || v.WorkUnit.ID != "" || len(v.Runs) != 0 || v.Inflight {
				t.Error("returned retired accounting view")
			}
			return e
		}},
		{"unit_page", func() error {
			v, e := m.ReadWorkUnitPage(ctx, ledger.WorkUnitListQuery{})
			if len(v.Items) != 0 || v.NextID != "" {
				t.Error("returned retired unit page")
			}
			return e
		}},
		{"active_count", func() error {
			v, e := m.ReadActiveRunCount(ctx, "prj_1", time.Now())
			if v != 0 {
				t.Error("returned retired count")
			}
			return e
		}},
		{"read_run", func() error {
			v, found, e := m.ReadRun(ctx, "prj_1", "run")
			if found || v.ID != "" {
				t.Error("returned retired run")
			}
			return e
		}},
		{"active_run", func() error {
			v, e := m.ActiveRun(ctx, "prj_1", "run")
			if v.ID != "" {
				t.Error("returned retired active run")
			}
			return e
		}},
		{"run_page", func() error {
			v, e := m.ReadRunPage(ctx, ledger.RunListQuery{})
			if len(v.Items) != 0 || v.NextID != "" {
				t.Error("returned retired run page")
			}
			return e
		}},
		{"begin_attempt", func() error { _, e := m.BeginAttempt(ctx, "prj_1", 100, "req_2", 7); return e }},
		{"begin_request", func() error { _, e := m.BeginRequest(ctx, "prj_1", "req_2"); return e }},
		{"begin_detailed", func() error { _, e := m.BeginRequestDetailed(ctx, "prj_1", "key_1", "req_2", "model"); return e }},
		{"begin_attributed", func() error {
			_, e := m.BeginRequestAttributed(ctx, "prj_1", "key_1", "req_2", "model", "unit", "run")
			return e
		}},
		{"reserve", func() error { _, e := m.ReserveAttempt(ctx, request, 100, 7); return e }},
		{"reserve_detailed", func() error { _, e := m.ReserveAttemptDetailed(ctx, request, 100, 7, AttemptMetadata{}); return e }},
		{"reserve_lease", func() error {
			_, e := m.ReserveLeaseDetailed(ctx, request, 100, LeaseSpec{}, AttemptMetadata{})
			return e
		}},
		{"started", func() error { return m.MarkStarted(ctx, Attempt{}) }},
		{"settle", func() error { return m.Settle(ctx, Attempt{}, Settlement{}) }},
		{"finalize_request", func() error { return m.FinalizeRequest(ctx, Attempt{}, "done") }},
		{"finalize", func() error { return m.Finalize(ctx, request, "done") }},
		{"recovery", func() error { return m.RecoverPendingLeases(ctx) }},
		{"observer", func() error { return m.AddObserver(func(ledger.Record) {}) }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if e := check.call(); !errors.Is(e, ErrAccountingRetired) {
				t.Fatal(e)
			}
		})
	}
	if log.Stats().Records != before || log.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("retired calls mutated native facts or health")
	}
}

func TestManagerRetirementAllowsFreshDiskOwnerHandoffWithoutOldStateWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	oldState := ledger.NewState()
	m, err := NewWithOptions(log, oldState, mustResolver(t, "UTC"), Options{Now: func() time.Time {
		return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		log.Close()
		t.Fatal(err)
	}
	closeLog := func() { log.Close() }
	defer closeLog()
	ctx := context.Background()
	finish := func(manager *Manager, requestID string) {
		t.Helper()
		request, err := manager.BeginRequest(ctx, "prj_1", requestID)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := manager.ReserveAttempt(ctx, request, 100, 7)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.MarkStarted(ctx, attempt); err != nil {
			t.Fatal(err)
		}
		if err := manager.Settle(ctx, attempt, Settlement{CommittedMicrosUSD: 3, Outcome: "success"}); err != nil {
			t.Fatal(err)
		}
		if err := manager.Finalize(ctx, request, "success"); err != nil {
			t.Fatal(err)
		}
	}
	finish(m, "req_before")
	clock := m.now
	assertRetiredOwnerReleased(t, m)
	lease, err := log.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	p, err := ledger.BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "projection.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { closeLog(); p.Close() }()
	next, err := NewWithProjection(ctx, log, p, m.Periods(), lease, Options{Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	finish(next, "req_after")
	if _, err := m.BeginRequest(ctx, "prj_1", "req_stale"); !errors.Is(err, ErrAccountingRetired) {
		t.Fatal(err)
	}
	head, hash, err := p.ReadPosition(ctx)
	if err != nil || head.Sequence != 10 || oldState.Watermark().Sequence != 5 || log.Stats().Records != 10 {
		t.Fatal("old owner was reused or handoff lost facts", head, err)
	}
	oracle := ledger.NewState()
	final, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, oracle.Apply)
	if err != nil || partial || final.Head != head || final.ChainHash != hash {
		t.Fatal(final, partial, err)
	}
	period, err := next.Periods().PeriodAt(next.now())
	if err != nil {
		t.Fatal(err)
	}
	balance, err := p.ReadBalance(ctx, ledger.BalanceKey{ProjectID: "prj_1", PeriodID: period.ID, TimezoneVersion: period.TimezoneVersion})
	if err != nil || balance != oracle.Balance("prj_1", period.ID, period.TimezoneVersion) || balance.CommittedMicrosUSD != 6 || balance.ReservedMicrosUSD != 0 {
		t.Fatal(balance, err)
	}
}
