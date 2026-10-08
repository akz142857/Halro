package budget

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func newProjectedManagerFixture(t testing.TB, options ledger.Options) (*Manager, *ledger.LedgerProjection, string) {
	t.Helper()
	options.ChainKey = testChainKey
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log, err := ledger.OpenWithOptions(path, ledger.NewStatus(), options)
	if err != nil {
		t.Fatal(err)
	}
	var p *ledger.LedgerProjection
	t.Cleanup(func() {
		if err := log.Close(); err != nil {
			t.Error(err)
		}
		if p != nil {
			if err := p.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	lease, err := log.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.WithHead(context.Background(), func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, nil)
		if err != nil || partial || report.Head != head || report.ChainHash != hash {
			return errors.Join(err, errors.New("fixture native head mismatch"))
		}
		p, err = ledger.BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "projection.db"), report)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC) }
	m, err := NewWithProjection(context.Background(), log, p, mustResolver(t, "UTC"), lease, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if m.legacyPending != nil {
		t.Fatal("projected Manager retained legacy full State recovery")
	}
	if owner, ok := m.owner.(projectedAccountingOwner); !ok || owner.LedgerProjection != p {
		t.Fatal("reads/Apply did not bind the disk owner")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	return m, p, path
}

func TestProjectedManagerNormalLifecycleMatchesAuthenticatedWALWithoutState(t *testing.T) {
	m, p, path := newProjectedManagerFixture(t, ledger.Options{MaxBatch: 8, FlushInterval: time.Millisecond})
	ctx := context.Background()
	type attribution struct {
		project string
		unit    domain.WorkUnit
		run     domain.Run
	}
	var scopes []attribution
	for i := 0; i < 8; i++ {
		project := fmt.Sprintf("prj_%d", i)
		unit, _, err := m.CreateWorkUnit(ctx, project, "key_1", 10, testGovernanceIntent("unit"))
		if err != nil {
			t.Fatal(err)
		}
		run, _, err := m.CreateRun(ctx, project, "key_1", unit.ID, 100, 48*time.Hour, 10, testGovernanceIntent("run"))
		if err != nil {
			t.Fatal(err)
		}
		scopes = append(scopes, attribution{project, unit, run})
	}
	var observed []ledger.Record
	m.AddObserver(func(r ledger.Record) {
		head, hash, err := p.ReadPosition(ctx)
		if err != nil || head != (ledger.Watermark{Generation: r.Generation, Offset: r.Offset, Sequence: r.Sequence}) || hash != r.Hash {
			t.Error("observer ran before disk Apply", head, err)
		}
		observed = append(observed, r)
	})
	for epoch := 1; epoch <= 2; epoch++ {
		var wg sync.WaitGroup
		errs := make(chan error, len(scopes))
		for i, scope := range scopes {
			wg.Add(1)
			go func(i int, scope attribution) {
				defer wg.Done()
				request, err := m.BeginRequestAttributed(ctx, scope.project, "key_1", fmt.Sprintf("req_%d_%d", epoch, i), "model", scope.unit.ID, scope.run.ID)
				if err != nil {
					errs <- err
					return
				}
				attempt, err := m.ReserveAttempt(ctx, request, 1000, 7)
				if err != nil {
					errs <- err
					return
				}
				if err := m.MarkStarted(ctx, attempt); err != nil {
					errs <- err
					return
				}
				if err := m.Settle(ctx, attempt, Settlement{CommittedMicrosUSD: 3, Outcome: "success"}); err != nil {
					errs <- err
					return
				}
				if err := m.Finalize(ctx, request, "success"); err != nil {
					errs <- err
				}
			}(i, scope)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		if epoch == 1 {
			if _, err := m.log.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(observed) != 80 {
		t.Fatal("ordinary disk accounting omitted lifecycle facts", len(observed))
	}
	before := m.log.Stats().Records
	for _, scope := range scopes {
		unit, replay, err := m.CreateWorkUnit(ctx, scope.project, "key_1", 10, testGovernanceIntent("unit"))
		if err != nil || !replay || unit.ID != scope.unit.ID {
			t.Fatal(unit, replay, err)
		}
		run, replay, err := m.CreateRun(ctx, scope.project, "key_1", scope.unit.ID, 100, 48*time.Hour, 10, testGovernanceIntent("run"))
		if err != nil || !replay || run.ID != scope.run.ID || run.CommittedMicrosUSD != 6 {
			t.Fatal(run, replay, err)
		}
	}
	if m.log.Stats().Records != before {
		t.Fatal("governance replay duplicated disk facts")
	}
	// Build the complete in-memory oracle only after the normal disk pipeline
	// has run; no State participated in its admission, Apply or observer path.
	oracle := ledger.NewState()
	actual := make(map[uint64]ledger.Record)
	report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, func(r ledger.Record) error { actual[r.Sequence] = r; return oracle.Apply(r) })
	if err != nil || partial || report.Head.Sequence != 96 || len(actual) != 96 {
		t.Fatal(report, partial, err)
	}
	for _, r := range observed {
		if !reflect.DeepEqual(actual[r.Sequence], r) {
			t.Fatal("native Record differs from WAL", r.Sequence)
		}
	}
	head, hash, err := p.ReadPosition(ctx)
	if err != nil || head != report.Head || hash != report.ChainHash {
		t.Fatal(head, report.Head, err)
	}
	for _, scope := range scopes {
		period, err := m.periods.PeriodAt(m.now())
		if err != nil {
			t.Fatal(err)
		}
		balance, err := p.ReadBalance(ctx, ledger.BalanceKey{ProjectID: scope.project, PeriodID: period.ID, TimezoneVersion: period.TimezoneVersion})
		if err != nil || balance != oracle.Balance(scope.project, period.ID, period.TimezoneVersion) || balance.CommittedMicrosUSD != 6 || balance.ReservedMicrosUSD != 0 {
			t.Fatal(balance, err)
		}
		run, found, err := m.ReadRun(ctx, scope.project, scope.run.ID)
		if err != nil || !found || run.RemainingMicrosUSD != 94 || run.BudgetState != domain.RunBudgetAvailable {
			t.Fatal(run, found, err)
		}
		admission, unlock := m.lockProject(scope.project)
		if len(admission.pending)+len(admission.runPending)+len(admission.runAdmissions) != 0 {
			t.Error("disk pipeline retained completed admissions")
		}
		unlock()
	}
	if err := m.RecoverPendingLeases(ctx); err == nil || m.log.Stats().Records != before {
		t.Fatal("projected Manager accepted legacy recovery", err)
	}
}

func TestProjectedManagerRequiredACKRunsAfterDiskApplyAndObserver(t *testing.T) {
	var p *ledger.LedgerProjection
	var observed uint64
	var waits []uint64
	m, projection, _ := newProjectedManagerFixture(t, ledger.Options{MaxBatch: 1,
		AfterDurable: func(batch ledger.DurableBatch) (uint64, error) { return batch.LastSequence, nil },
		WaitConfirmed: func(_ context.Context, index uint64) error {
			head, _, err := p.ReadPosition(context.Background())
			if err != nil || head.Sequence < index || observed < index {
				t.Error("ACK preceded disk Apply/observer", head, observed, index, err)
			}
			waits = append(waits, index)
			return nil
		},
	})
	p = projection
	m.AddObserver(func(r ledger.Record) { observed = r.Sequence })
	request, err := m.BeginRequest(context.Background(), "prj_1", "req_1")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := m.ReserveAttempt(context.Background(), request, 100, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := m.Settle(context.Background(), attempt, Settlement{CommittedMicrosUSD: 3, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(context.Background(), request, "success"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(waits, []uint64{2, 3}) {
		t.Fatal("required confirmation policy changed", waits)
	}
}

type faultAccountingOwner struct {
	accountingOwner
	point string
}

var projectedApplyFailure = errors.New("controlled projected Apply/position failure")

func (f faultAccountingOwner) appliedPosition(ctx context.Context) (ledger.Watermark, error) {
	head, err := f.accountingOwner.appliedPosition(ctx)
	if f.point == "position" {
		return head, errors.Join(err, projectedApplyFailure)
	}
	return head, err
}
func (f faultAccountingOwner) applyRecord(ctx context.Context, r ledger.Record) error {
	if f.point == "apply" {
		return projectedApplyFailure
	}
	return f.accountingOwner.applyRecord(ctx, r)
}

func TestProjectedManagerApplyFailuresLatchNativeHealthAndRetainDurableFact(t *testing.T) {
	for _, point := range []string{"position", "apply", "closed_projection"} {
		t.Run(point, func(t *testing.T) {
			m, p, path := newProjectedManagerFixture(t, ledger.Options{})
			if point == "closed_projection" {
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				m.owner = faultAccountingOwner{m.owner, point}
			}
			observed := 0
			m.AddObserver(func(ledger.Record) { observed++ })
			if _, err := m.BeginRequest(context.Background(), "prj_1", "req_1"); err == nil {
				t.Fatal("failed disk application was accepted")
			}
			if m.log.Stats().Records != 1 || observed != 0 || m.log.Status().Load() != ledger.AccountingRecoveryRequired {
				t.Fatal("failure lost fact or credited observer/health", m.log.Stats(), observed, m.log.Status().Load())
			}
			for i := 0; i < 3; i++ {
				if _, err := m.BeginRequest(context.Background(), "prj_1", "req_retry"); err == nil {
					t.Fatal("poisoned owner appended")
				}
			}
			if m.log.Stats().Records != 1 {
				t.Fatal("poison grew unapplied history")
			}
			report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, nil)
			if err != nil || partial || report.Head.Sequence != 1 {
				t.Fatal("failed apply erased durable native evidence", report, partial, err)
			}
		})
	}
}

func TestProjectedManagerCloseInterruptsMissingPredecessorWithoutSkippingFacts(t *testing.T) {
	m, p, _ := newProjectedManagerFixture(t, ledger.Options{})
	log := m.log
	now := m.now()
	if _, err := log.Append(context.Background(), ledger.Event{EventID: "evt_gap", Kind: ledger.EventRequestAccepted, RequestID: "req_gap", ProjectID: "prj_1", PeriodID: "2026-07-31", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: now}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := m.BeginRequest(context.Background(), "prj_1", "req_waiting"); done <- err }()
	deadline := time.Now().Add(time.Second)
	for log.Stats().Records < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second fact never reached durability")
		}
		time.Sleep(time.Millisecond)
	}
	// Retirement cannot discard an accepted fact or cancel the native Apply
	// owner. Terminal native Close must first wake this missing-predecessor wait.
	retireWithDeadline(t, m)
	closed := make(chan error, 1)
	go func() { closed <- log.Close() }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("gap wait did not exit native Close")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("native Close did not join callback")
	}
	head, _, err := p.ReadPosition(context.Background())
	if err != nil || head.Sequence != 0 || log.Stats().Records != 2 || log.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("missing prefix was skipped", head, log.Stats(), err)
	}
	assertRetiredOwnerReleased(t, m)
}

func TestProjectedManagerAcceptedCancellationStillAppliesReadyFact(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	m, p, _ := newProjectedManagerFixture(t, ledger.Options{MaxBatch: 1,
		AfterDurable: func(batch ledger.DurableBatch) (uint64, error) {
			if batch.FirstSequence == 1 {
				close(entered)
				<-release
			}
			return batch.LastSequence, nil
		},
		WaitConfirmed: func(ctx context.Context, _ uint64) error { return ctx.Err() },
	})
	// Use a required reservation fact with stable fields as the first record;
	// legacy untagged leases are valid without a preceding acceptance.
	value := int64(7)
	event := ledger.Event{EventID: "evt_ready", Kind: ledger.EventReservationCreated, RequestID: "req_ready", AttemptID: "att_ready", ProjectID: "prj_1", PeriodID: "2026-07-31", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, ReservationMicrosUSD: &value, OccurredAt: m.now()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.appendApplyRecord(ctx, event); done <- err }()
	<-entered
	cancel()
	once.Do(func() { close(release) })
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("ACK cancellation was ignored", err)
	}
	row, found, err := p.ReadLease(context.Background(), "att_ready")
	if err != nil || !found || *row.Event.ReservationMicrosUSD != 7 || row.Hash == ([32]byte{}) || m.log.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("accepted cancellation skipped disk fact", row, found, err)
	}
}

func TestProjectedManagerConstructionRequiresSameLiveLeaseAndMatchingHash(t *testing.T) {
	ctx := context.Background()
	resolver := mustResolver(t, "UTC")
	for _, mismatch := range []string{"position", "hash"} {
		t.Run(mismatch, func(t *testing.T) {
			open := func(name string) (*ledger.Log, string) {
				path := filepath.Join(t.TempDir(), name+".wal")
				log, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: testChainKey})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { log.Close() })
				return log, path
			}
			log, _ := open("native")
			foreign, foreignPath := open("foreign")
			e := ledger.Event{EventID: "evt_1", Kind: ledger.EventRequestAccepted, ProjectID: "prj_1", RequestID: "req_1", PeriodID: "2026-07-31", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)}
			if _, err := log.Append(ctx, e); err != nil {
				t.Fatal(err)
			}
			if mismatch == "hash" {
				e.ProjectID = "prj_2"
				if _, err := foreign.Append(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			report, partial, err := ledger.InspectReplayAuthenticated(foreignPath, testChainKey, nil)
			if err != nil || partial {
				t.Fatal(err)
			}
			p, err := ledger.BuildLedgerProjection(ctx, foreignPath, testChainKey, filepath.Join(t.TempDir(), "foreign.db"), report)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			lease, err := log.BeginMaintenance(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			head, hash, _ := log.ChainHead()
			if mismatch == "hash" && (head != report.Head || hash == report.ChainHash) {
				t.Fatal("fixture must differ only in chain hash", head, report.Head)
			}
			m, err := NewWithProjection(ctx, log, p, resolver, lease, Options{})
			if err == nil || m != nil || log.Status().Load() != ledger.AccountingRecoveryRequired || log.Stats().Records != 1 {
				t.Fatal("mismatched projection published", m, err, log.Status().Load())
			}
		})
	}
	m, p, _ := newProjectedManagerFixture(t, ledger.Options{})
	log := m.log
	if candidate, err := NewWithProjection(ctx, log, p, resolver, nil, Options{}); err == nil || candidate != nil {
		t.Fatal("missing lease accepted")
	}
	lease, err := log.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	otherPath := filepath.Join(t.TempDir(), "other.wal")
	other, err := ledger.OpenWithOptions(otherPath, ledger.NewStatus(), ledger.Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if candidate, err := NewWithProjection(ctx, other, p, resolver, lease, Options{}); err == nil || candidate != nil {
		t.Fatal("foreign native lease accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if candidate, err := NewWithProjection(canceled, log, p, resolver, lease, Options{}); !errors.Is(err, context.Canceled) || candidate != nil {
		t.Fatal(candidate, err)
	}
	if _, err := m.BeginRequest(ctx, "prj_1", "req_held"); !errors.Is(err, ledger.ErrLedgerMaintenance) || log.Stats().Records != 0 {
		t.Fatal("normal writes bypassed retained lease", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if candidate, err := NewWithProjection(ctx, log, p, resolver, lease, Options{}); err == nil || candidate != nil {
		t.Fatal("closed lease accepted")
	}
	if log.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("invalid caller arguments poisoned healthy facts")
	}
}

func TestProjectedManagerNativeCloseJoinsObserverBeforeProjectionRelease(t *testing.T) {
	m, p, _ := newProjectedManagerFixture(t, ledger.Options{})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	m.AddObserver(func(r ledger.Record) {
		head, _, err := p.ReadPosition(context.Background())
		if err != nil || head.Sequence != r.Sequence {
			t.Error("observer preceded disk Apply", head, err)
		}
		close(entered)
		<-release
	})
	done := make(chan error, 1)
	go func() { _, err := m.BeginRequest(context.Background(), "prj_join", "req_join"); done <- err }()
	<-entered
	leaseCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	lease, err := m.log.BeginMaintenance(leaseCtx)
	cancel()
	if lease != nil {
		lease.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("maintenance bypassed normal disk observer", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- m.log.Close() }()
	select {
	case err := <-closed:
		t.Fatal("native Close returned before observer", err)
	case <-time.After(30 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("native Close did not join observer")
	}
	if head, _, err := p.ReadPosition(context.Background()); err != nil || head.Sequence != 1 {
		t.Fatal("projection released before native callback joined", head, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}
