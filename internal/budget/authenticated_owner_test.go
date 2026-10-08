package budget

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func newAuthenticatedManagerFixture(t *testing.T, options ledger.Options) (*Manager, *ledger.AuthenticatedLedgerProjection, string) {
	t.Helper()
	m, p, path, _ := newAuthenticatedManagerFixtureWithResources(t, options)
	return m, p, path
}

func newAuthenticatedManagerFixtureWithResources(t *testing.T, options ledger.Options, outputDirectories ...string) (*Manager, *ledger.AuthenticatedLedgerProjection, string, *resourcebudget.Manager) {
	return newAuthenticatedManagerFixtureWithLimits(t, options, nil, outputDirectories...)
}

func newAuthenticatedManagerFixtureWithLimits(t *testing.T, options ledger.Options, customLimits *resourcebudget.Limits, outputDirectories ...string) (*Manager, *ledger.AuthenticatedLedgerProjection, string, *resourcebudget.Manager) {
	t.Helper()
	ctx := context.Background()
	options.ChainKey = testChainKey
	path := filepath.Join(t.TempDir(), "native.wal")
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), options)
	if err != nil {
		t.Fatal(err)
	}
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 256 << 20}, MaxClaims: 16, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	if customLimits != nil {
		limits = *customLimits
	}
	resources, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	var p *ledger.AuthenticatedLedgerProjection
	outputDirectory := filepath.Join(t.TempDir(), "auth")
	if len(outputDirectories) > 0 {
		outputDirectory = outputDirectories[0]
	}
	t.Cleanup(func() {
		if err := native.Close(); err != nil {
			t.Error(err)
		}
		if p != nil {
			if err := p.Discard(); err != nil {
				t.Error(err)
			}
		}
		if used := resources.Snapshot().Used; used != (resourcebudget.Cost{}) {
			t.Error("authenticated accounting leaked resources", used)
		}
	})
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := lease.WithHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, nil)
		if err != nil || partial || report.Head != head || report.ChainHash != hash {
			return errors.Join(err, errors.New("fixture native prefix mismatch"))
		}
		p, err = ledger.BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, outputDirectory, report, ledger.AuthenticatedLedgerOptions{
			Identity: sha256.Sum256([]byte("independent-accounting-test-identity")), Budget: resources, MaxDiskBytes: 128 << 20, MaxAppendBytes: 8 << 20,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m, err := NewWithAuthenticatedProjection(ctx, native, p, mustResolver(t, "UTC"), lease, Options{Now: func() time.Time {
		return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := m.owner.(authenticatedAccountingOwner)
	if !ok || owner.AuthenticatedLedgerProjection != p || owner.native != native || m.reads != m.owner || m.legacyPending != nil {
		t.Fatal("authenticated reads and native Apply were not bound to one owner")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	return m, p, path, resources
}

func TestAuthenticatedDurableApplyRetainsActualRecoveryScopeUnderPressure(t *testing.T) {
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 512 << 20, Disk: 256 << 20}, MaxClaims: 32, MaxOwners: 4, MaxPressureAge: time.Hour, RecoveryClaims: 8, RecoveryReserve: resourcebudget.Cost{RAM: 256 << 20}}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = limits.RecoveryReserve
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: int64(limits.MaxClaims) * resourcebudget.ClaimRAMBytes}
	var resources *resourcebudget.Manager
	m, projection, _, manager := newAuthenticatedManagerFixtureWithLimits(t, ledger.Options{MaxBatch: 1,
		AfterDurable: func(batch ledger.DurableBatch) (uint64, error) {
			if err := resources.SetPressure(resourcebudget.Pressured); err != nil {
				t.Error(err)
			}
			return batch.LastSequence, nil
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	}, &limits)
	resources = manager
	parent, err := resources.TryAcquireRecovery(resourcebudget.Cost{RAM: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Release()
	err = parent.WithRecoveryScope(context.Background(), func(ctx context.Context) error {
		_, err := m.BeginRequest(ctx, "prj_recovery", "req_owned_recovery")
		return err
	})
	if err != nil || m.log.Status().Load() != ledger.AccountingHealthy || m.log.Stats().Records != 1 {
		t.Fatal("pressure stranded a durable fact despite its actual recovery owner", err, m.log.Status().Load(), m.log.Stats())
	}
	if _, _, err := projection.ReadRequest(context.Background(), "req_owned_recovery"); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatal("ordinary work borrowed recovery authority", err)
	}
	if err := resources.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	if request, found, err := projection.ReadRequest(context.Background(), "req_owned_recovery"); err != nil || !found || request.Accepted.Sequence != 1 {
		t.Fatal("actual authenticated root omitted accepted fact", request, found, err)
	}
	if got := resources.Snapshot(); got.RecoveryClaims != 1 || got.Recovery.RAM != 4096+resourcebudget.ClaimRAMBytes {
		t.Fatal("durable callback leaked descendants", got)
	}
}

func TestAuthenticatedPreAppendWorkspaceFinishesActualFactAfterPressure(t *testing.T) {
	var resources *resourcebudget.Manager
	m, projection, _, manager := newAuthenticatedManagerFixtureWithResources(t, ledger.Options{MaxBatch: 1,
		AfterDurable: func(batch ledger.DurableBatch) (uint64, error) {
			return batch.LastSequence, resources.SetPressure(resourcebudget.Pressured)
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	resources = manager
	if _, err := m.BeginRequest(context.Background(), "prj_prepaid", "req_prepaid"); err != nil || m.log.Status().Load() != ledger.AccountingHealthy || m.log.Stats().Records != 1 {
		t.Fatal("post-durable pressure stranded the pre-admitted real COW owner", err, m.log.Status().Load())
	}
	if _, _, err := projection.ReadRequest(context.Background(), "req_prepaid"); !errors.Is(err, resourcebudget.ErrPressure) {
		t.Fatal("ordinary query borrowed terminal RAM", err)
	}
	resources.SetPressure(resourcebudget.Healthy)
	if request, found, err := projection.ReadRequest(context.Background(), "req_prepaid"); err != nil || !found || request.Accepted.Sequence != 1 {
		t.Fatal(request, found, err)
	}
}

func TestAuthenticatedPreAppendWorkspaceRefusesBeforeNativeFact(t *testing.T) {
	m, _, _, resources := newAuthenticatedManagerFixtureWithResources(t, ledger.Options{MaxBatch: 1})
	occupied, err := resources.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 128 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Release()
	before := resources.Snapshot()
	if _, err := m.BeginRequest(context.Background(), "prj_prepaid", "req_refused"); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("incomplete workspace admitted the actual append", err)
	}
	if m.log.Stats().Records != 0 || m.log.Status().Load() != ledger.AccountingHealthy || resources.Snapshot().Used != before.Used || resources.Snapshot().Owners != before.Owners {
		t.Fatal("pre-admission failure changed the native source or leaked fees", m.log.Stats(), resources.Snapshot())
	}
}

func TestAuthenticatedManagerLifecycleACKAndNativeOracle(t *testing.T) {
	ctx := context.Background()
	var p *ledger.AuthenticatedLedgerProjection
	var observed uint64
	var waits []uint64
	m, projection, path := newAuthenticatedManagerFixture(t, ledger.Options{MaxBatch: 1,
		AfterDurable: func(b ledger.DurableBatch) (uint64, error) { return b.LastSequence, nil },
		WaitConfirmed: func(_ context.Context, index uint64) error {
			head, _, err := p.ReadPosition(ctx)
			if err != nil || head.Sequence < index || observed < index {
				t.Error("confirmation preceded authenticated Apply/observer", head, observed, index, err)
			}
			waits = append(waits, index)
			return nil
		},
	})
	p = projection
	m.AddObserver(func(r ledger.Record) {
		head, hash, err := p.ReadPosition(ctx)
		if err != nil || head != (ledger.Watermark{Generation: r.Generation, Offset: r.Offset, Sequence: r.Sequence}) || hash != r.Hash {
			t.Error("observer did not see the exact authenticated native cut", head, err)
		}
		observed = r.Sequence
	})
	wunit, _, err := m.CreateWorkUnit(ctx, "prj_1", "key_1", 10, testGovernanceIntent("unit"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := m.CreateRun(ctx, "prj_1", "key_1", wunit.ID, 100, 48*time.Hour, 10, testGovernanceIntent("run"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		request, err := m.BeginRequestAttributed(ctx, "prj_1", "key_1", fmt.Sprintf("req_%d", i), "model", wunit.ID, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := m.ReserveAttempt(ctx, request, 100, 7)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.MarkStarted(ctx, attempt); err != nil {
			t.Fatal(err)
		}
		if err := m.Settle(ctx, attempt, Settlement{CommittedMicrosUSD: 3, Outcome: "success"}); err != nil {
			t.Fatal(err)
		}
		if err := m.Finalize(ctx, request, "success"); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := m.log.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !reflect.DeepEqual(waits, []uint64{4, 5, 9, 10}) {
		t.Fatal("confirmation policy changed", waits)
	}
	before := m.log.Stats().Records
	if _, replay, err := m.CreateRun(ctx, "prj_1", "key_1", wunit.ID, 100, 48*time.Hour, 10, testGovernanceIntent("run")); err != nil || !replay || m.log.Stats().Records != before {
		t.Fatal("governance replay duplicated a native fact", replay, err)
	}
	// No State participated in the writer. Construct an independent complete
	// oracle only afterward from the actual MAC WAL, including the native Roll.
	oracle := ledger.NewState()
	report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, oracle.Apply)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	head, hash, err := p.ReadPosition(ctx)
	if err != nil || head != report.Head || hash != report.ChainHash || head.Sequence != 12 {
		t.Fatal(head, hash, report, err)
	}
	period, err := m.periods.PeriodAt(m.now())
	if err != nil {
		t.Fatal(err)
	}
	balance, err := m.ReadBalance(ctx, ledger.BalanceKey{ProjectID: "prj_1", PeriodID: period.ID, TimezoneVersion: period.TimezoneVersion})
	if err != nil || balance != oracle.Balance("prj_1", period.ID, period.TimezoneVersion) || balance.CommittedMicrosUSD != 6 || balance.ReservedMicrosUSD != 0 {
		t.Fatal(balance, err)
	}
	a, found, err := m.ReadRun(ctx, "prj_1", run.ID)
	b, refFound, refErr := oracle.ReadRun(ctx, run.ID)
	if err != nil || refErr != nil || !found || !refFound || a.CommittedMicrosUSD != b.CommittedMicrosUSD || a.ReservedMicrosUSD != b.ReservedMicrosUSD || a.RemainingMicrosUSD != b.BudgetMicrosUSD-b.CommittedMicrosUSD-b.ReservedMicrosUSD || a.Status != domain.RunActive {
		t.Fatal(a, b, found, err, refErr)
	}
	if err := m.RecoverPendingLeases(ctx); err == nil {
		t.Fatal("authenticated owner enabled legacy full-State recovery")
	}
}

func TestAuthenticatedManagerFinalizedRequestStillSettlesPendingAttempt(t *testing.T) {
	m, p, path := newAuthenticatedManagerFixture(t, ledger.Options{})
	ctx := context.Background()
	wu, _, err := m.CreateWorkUnit(ctx, "prj_1", "key_1", 10, testGovernanceIntent("unit"))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := m.CreateRun(ctx, "prj_1", "key_1", wu.ID, 100, 48*time.Hour, 10, testGovernanceIntent("run"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := m.BeginRequestAttributed(ctx, "prj_1", "key_1", "req_late", "model", wu.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := m.ReserveAttempt(ctx, request, 100, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(ctx, request, "interrupted"); err != nil {
		t.Fatal(err)
	}
	stats, err := p.ReadPendingLeaseStats(ctx, m.now())
	if err != nil || stats.Count != 1 {
		t.Fatal("request terminal fact lost pending attempt", stats, err)
	}
	if err := m.Settle(ctx, attempt, Settlement{CommittedMicrosUSD: 3, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	stats, err = p.ReadPendingLeaseStats(ctx, m.now())
	if err != nil || stats.Count != 0 {
		t.Fatal("late attempt did not settle", stats, err)
	}
	parent, found, err := p.ReadRequest(ctx, request.RequestID)
	if err != nil || !found || parent.Finalized == nil || parent.Finalized.Event.Outcome != "interrupted" || parent.Finalized.Sequence != 5 {
		t.Fatal("late settlement rewrote original request terminal", parent, found, err)
	}
	settled, found, err := p.ReadSettlement(ctx, attempt.AttemptID)
	if err != nil || !found || settled.CostMicrosUSD != 3 {
		t.Fatal("late settlement lost original amount", settled, found, err)
	}
	report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial || report.Head.Sequence != 6 {
		t.Fatal(report, partial, err)
	}
	ref, err := ledger.BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "reference.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Discard()
	want, wantFound, wantErr := ref.ReadRequest(ctx, request.RequestID)
	if wantErr != nil || !wantFound || !reflect.DeepEqual(parent, want) {
		t.Fatal("fresh native replay differed", parent, want, wantErr)
	}
}

func TestAuthenticatedManagerConcurrentAcceptedFactsStayInNativeOrder(t *testing.T) {
	m, p, path := newAuthenticatedManagerFixture(t, ledger.Options{MaxBatch: 8, FlushInterval: time.Millisecond})
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	var observed uint64
	m.AddObserver(func(r ledger.Record) {
		if r.Sequence != observed+1 {
			t.Error("native observers reordered", observed, r.Sequence)
		}
		observed = r.Sequence
	})
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// The deliberately small claim fixture can refuse a request before
			// its native append. Retry that explicit backpressure while keeping
			// accepted facts and observers subject to the same ordering oracle.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			for {
				_, err = m.BeginRequest(ctx, fmt.Sprintf("prj_%d", i), fmt.Sprintf("req_%d", i))
				if !errors.Is(err, resourcebudget.ErrExhausted) || ctx.Err() != nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, nil)
	head, hash, readErr := p.ReadPosition(context.Background())
	if err != nil || partial || readErr != nil || head != report.Head || hash != report.ChainHash || observed != 12 {
		t.Fatal(report, head, observed, err, readErr)
	}
}

func TestAuthenticatedManagerFailedApplyPreservesWALAndStopsFurtherWrites(t *testing.T) {
	m, p, path := newAuthenticatedManagerFixture(t, ledger.Options{})
	native := m.log
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginRequest(context.Background(), "prj_1", "req_1"); err == nil || native.Stats().Records != 1 || native.Status().Load() != ledger.AccountingRecoveryRequired {
		t.Fatal("failed authenticated Apply was published or lost", err, native.Stats(), native.Status().Load())
	}
	if _, err := m.BeginRequest(context.Background(), "prj_1", "req_2"); err == nil || native.Stats().Records != 1 {
		t.Fatal("terminal failure appended another fact", err)
	}
	report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial || report.Head.Sequence != 1 {
		t.Fatal("accepted native fact was not recoverable", report, err)
	}
}

func TestAuthenticatedManagerConstructorRequiresLiveMatchingNativeLease(t *testing.T) {
	m, p, _ := newAuthenticatedManagerFixture(t, ledger.Options{})
	ctx := context.Background()
	native := m.log
	resolver := mustResolver(t, "UTC")
	if candidate, err := NewWithAuthenticatedProjection(ctx, native, p, resolver, nil, Options{}); candidate != nil || err == nil {
		t.Fatal("missing lease accepted")
	}
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	other, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "other.wal"), ledger.NewStatus(), ledger.Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if candidate, err := NewWithAuthenticatedProjection(ctx, other, p, resolver, lease, Options{}); candidate != nil || err == nil {
		t.Fatal("foreign lease accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if candidate, err := NewWithAuthenticatedProjection(canceled, native, p, resolver, lease, Options{}); candidate != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(candidate, err)
	}
	if _, err := m.BeginRequest(ctx, "prj_1", "req_held"); !errors.Is(err, ledger.ErrLedgerMaintenance) || native.Stats().Records != 0 {
		t.Fatal("business bypassed retained lease", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if candidate, err := NewWithAuthenticatedProjection(ctx, native, p, resolver, lease, Options{}); candidate != nil || err == nil {
		t.Fatal("closed lease accepted")
	}
	if native.Status().Load() != ledger.AccountingHealthy {
		t.Fatal("invalid arguments poisoned healthy native facts")
	}
}

func TestAuthenticatedManagerCallerCancellationAfterDurabilityStillApplies(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	m, p, _ := newAuthenticatedManagerFixture(t, ledger.Options{MaxBatch: 1,
		AfterDurable: func(b ledger.DurableBatch) (uint64, error) {
			close(entered)
			<-release
			return b.LastSequence, nil
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := m.BeginRequest(ctx, "prj_1", "req_canceled")
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("native append did not reach its durable hook")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatal("accepted caller returned before durable Apply", err)
	default:
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("accepted caller cancellation skipped native fact", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native Apply did not finish after durable hook returned")
	}
	parent, found, err := p.ReadRequest(context.Background(), "req_canceled")
	if err != nil || !found || parent.Accepted.Sequence != 1 {
		t.Fatal("accepted canceled request is absent", parent, found, err)
	}
}

func TestAuthenticatedManagerNativeCloseAndRetireJoinObserver(t *testing.T) {
	m, p, _ := newAuthenticatedManagerFixture(t, ledger.Options{})
	native := m.log
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	m.AddObserver(func(r ledger.Record) {
		head, _, err := p.ReadPosition(context.Background())
		if err != nil || head.Sequence != r.Sequence {
			t.Error("observer preceded authenticated publication", head, err)
		}
		close(entered)
		<-release
	})
	done := make(chan error, 1)
	go func() { _, err := m.BeginRequest(context.Background(), "prj_1", "req_1"); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("observer not reached")
	}
	m.WithdrawAdmission()
	closed, retired := make(chan error, 1), make(chan error, 1)
	go func() { closed <- native.Close() }()
	go func() { retired <- m.Retire(context.Background()) }()
	for _, wait := range []chan error{closed, retired} {
		select {
		case err := <-wait:
			t.Fatal("native/Manager retired before accepted observer returned", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	once.Do(func() { close(release) })
	for _, wait := range []chan error{done, closed, retired} {
		select {
		case err := <-wait:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("native/Manager did not join completed observer")
		}
	}
	if m.owner != nil || m.reads != nil || m.log != nil {
		t.Fatal("retired Manager retained the authenticated/native owner")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}
