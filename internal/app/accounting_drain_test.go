package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

func accountingDrainFixture(t *testing.T) *Runtime {
	t.Helper()
	log, _, _ := projectedWriterLog(t, nil, ledger.Options{})
	state := ledger.NewState()
	periods, err := budget.NewFixedPeriodResolver("UTC")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := budget.New(log, state, periods)
	if err != nil {
		t.Fatal(err)
	}
	aggregate := usage.NewAggregate()
	collector, err := usage.NewCollector(aggregate, log, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AddObserver(collector.Observe); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runtime{config: testConfig(t), ledger: log, accountingState: runtimeAccountingState{reader: state}, accounting: manager,
		usage: &runtimeUsage{Aggregate: aggregate}, usageCollector: collector, backgroundCtx: ctx, backgroundCancel: cancel}
	t.Cleanup(func() { r.stopBackgroundWork() })
	return r
}

func drainFixtureAttempt(t *testing.T, r *Runtime, id string) (budget.Request, budget.Attempt) {
	t.Helper()
	request, err := r.accounting.BeginRequest(context.Background(), "prj_drain", id)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := r.accounting.ReserveAttempt(context.Background(), request, 100, 7)
	if err != nil {
		t.Fatal(err)
	}
	return request, attempt
}

func finishDrainAttempt(r *Runtime, request budget.Request, attempt budget.Attempt) error {
	ctx := context.Background() // cleanup remains independent of producer cancellation
	if err := r.accounting.MarkStarted(ctx, attempt); err != nil {
		return err
	}
	if err := r.accounting.Settle(ctx, attempt, budget.Settlement{CommittedMicrosUSD: 3, Outcome: "success"}); err != nil {
		return err
	}
	return r.accounting.Finalize(ctx, request, "success")
}

func awaitDrainSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("drain fixture phase did not arrive")
	}
}

func TestBusinessDrainJoinsRealHTTPThenLateBackgroundAccountingAndUsage(t *testing.T) {
	r := accountingDrainFixture(t)
	backgroundRequest, backgroundAttempt := drainFixtureAttempt(t, r, "req_background")
	backgroundCanceled, releaseBackground := make(chan struct{}), make(chan struct{})
	var backgroundOnce sync.Once
	defer backgroundOnce.Do(func() { close(releaseBackground) })
	backgroundResult := make(chan error, 1)
	r.backgroundWait.Add(1)
	go func() {
		defer r.backgroundWait.Done()
		<-r.backgroundCtx.Done()
		close(backgroundCanceled)
		<-releaseBackground
		backgroundResult <- finishDrainAttempt(r, backgroundRequest, backgroundAttempt)
	}()
	request, attempt := drainFixtureAttempt(t, r, "req_http")
	enteredHTTP, releaseHTTP := make(chan struct{}), make(chan struct{})
	var httpOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(enteredHTTP)
		<-releaseHTTP
		if err := finishDrainAttempt(r, request, attempt); err != nil {
			t.Error(err)
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	defer httpOnce.Do(func() { close(releaseHTTP) })
	clientResult := make(chan error, 1)
	go func() {
		response, err := server.Client().Get(server.URL)
		if err == nil {
			response.Body.Close()
			if response.StatusCode != 200 {
				err = errors.New("accepted HTTP accounting did not finish")
			}
		}
		clientResult <- err
	}()
	awaitDrainSignal(t, enteredHTTP)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- r.drainAccountingBusiness(ctx, []shutdownHTTPServer{server.Config}) }()
	// A real HTTP handler still owns accounting cleanup. Its background peers
	// must not be canceled merely because the drain has been requested.
	select {
	case <-backgroundCanceled:
		t.Fatal("background canceled before HTTP joined")
	default:
	}
	httpOnce.Do(func() { close(releaseHTTP) })
	if err := <-clientResult; err != nil {
		t.Fatal(err)
	}
	awaitDrainSignal(t, backgroundCanceled)
	if _, _, err := r.accounting.ReadWorkUnit(context.Background(), "", "absent"); err != nil {
		t.Fatal("Manager retired ahead of background cleanup", err)
	}
	select {
	case err := <-drained:
		t.Fatal("drain returned while late cleanup was blocked", err)
	default:
	}
	backgroundOnce.Do(func() { close(releaseBackground) })
	if err := <-backgroundResult; err != nil {
		t.Fatal(err)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if r.ledger.Stats().Records != 10 || usageWatermarkForTest(t, r) != accountingReaderHead(t, r.accountingState.reader) || r.usageCollector.Stats().QueueDepth != 0 || r.usageCollector.Stats().Lagging {
		t.Fatal("drain did not reconcile every late durable fact", r.ledger.Stats(), r.usageCollector.Stats())
	}
	oracle := usage.NewAggregate()
	if _, err := r.ledger.Replay(ledger.Watermark{}, oracle.Apply); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(usageSnapshotForTest(t, r), oracle.Snapshot()) || usageMetricsForTest(t, r) != oracle.Metrics() {
		t.Fatal("drained Usage differs from full durable replay")
	}
	balance := accountingReaderBalance(t, r.accountingState.reader, ledger.BalanceKey{ProjectID: "prj_drain", PeriodID: request.Period.ID, TimezoneVersion: request.Period.TimezoneVersion})
	if balance.ReservedMicrosUSD != 0 || balance.CommittedMicrosUSD != 6 {
		t.Fatal(balance)
	}
	if _, err := r.accounting.BeginRequest(context.Background(), "prj_drain", "stale"); !errors.Is(err, budget.ErrAccountingRetired) {
		t.Fatal(err)
	}
	lease, err := r.beginAccountingMaintenance(context.Background(), nil)
	if err != nil {
		t.Fatal("complete business drain did not transfer native ownership", err)
	}
	defer lease.Close()
	if _, err := lease.ValidateOwner(r.ledger); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ledger.Append(context.Background(), projectedWriterAcceptance("outside_lease")); err == nil || r.ledger.Stats().Records != 10 {
		t.Fatal("native maintenance admitted a stray append", err)
	}
}

func TestBusinessDrainBackgroundTimeoutReusesJoinHandleBeforeRetiring(t *testing.T) {
	r := accountingDrainFixture(t)
	request, attempt := drainFixtureAttempt(t, r, "req_late")
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	result := make(chan error, 1)
	r.backgroundWait.Add(1)
	go func() {
		defer r.backgroundWait.Done()
		<-r.backgroundCtx.Done()
		<-release
		result <- finishDrainAttempt(r, request, attempt)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if lease, err := r.beginAccountingMaintenance(ctx, nil); !errors.Is(err, context.DeadlineExceeded) || lease != nil {
		t.Fatal(err)
	}
	handle := r.backgroundWait.stopped
	if handle == nil || r.backgroundWait.drainState.Load() != businessDrainStarted {
		t.Fatal("timeout lost the pending join owner")
	}
	if _, _, err := r.accounting.ReadWorkUnit(context.Background(), "", "absent"); err != nil {
		t.Fatal("pending cleanup lost its Manager", err)
	}
	retry, retryCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer retryCancel()
	if err := r.stopBackgroundWorkContext(retry); !errors.Is(err, context.DeadlineExceeded) || r.backgroundWait.stopped != handle {
		t.Fatal("retry replaced a live join handle", err)
	}
	once.Do(func() { close(release) })
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	lease, err := r.beginAccountingMaintenance(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if r.backgroundWait.stopped != handle || r.backgroundWait.drainState.Load() != businessDrainComplete || accountingReaderHead(t, r.accountingState.reader).Sequence != 5 || usageWatermarkForTest(t, r) != accountingReaderHead(t, r.accountingState.reader) {
		t.Fatal("retry lost facts or reused a different lifetime")
	}
	if _, err := r.accounting.BeginRequest(context.Background(), "prj_drain", "stale"); !errors.Is(err, budget.ErrAccountingRetired) {
		t.Fatal(err)
	}
}

func TestBusinessDrainHTTPFailureKeepsCleanupOwnersAndRefusesCompletion(t *testing.T) {
	r := accountingDrainFixture(t)
	failure := errors.New("controlled HTTP drain failure")
	if lease, err := r.beginAccountingMaintenance(context.Background(), []shutdownHTTPServer{&fakeShutdownServer{shutdownError: failure}}); !errors.Is(err, failure) || lease != nil {
		t.Fatal(err)
	}
	if r.backgroundCtx.Err() != nil || r.backgroundWait.stopped != nil || r.backgroundWait.drainState.Load() != businessDrainStarted {
		t.Fatal("HTTP failure prematurely stopped producers or declared completion")
	}
	if release, err := r.governance.exports.enter(context.Background()); !errors.Is(err, errGovernanceExportRetired) {
		if release != nil {
			release()
		}
		t.Fatal("failed HTTP drain reopened export admission", err)
	}
	request, attempt := drainFixtureAttempt(t, r, "accepted_cleanup")
	if err := finishDrainAttempt(r, request, attempt); err != nil {
		t.Fatal("failed HTTP drain revoked legitimate cleanup", err)
	}
}

type failingDrainReplayer struct{ err error }

func (r failingDrainReplayer) Replay(ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error) {
	return ledger.Watermark{}, r.err
}

func TestBusinessDrainUsageFailureDoesNotRetireOrComplete(t *testing.T) {
	r := accountingDrainFixture(t)
	failure := errors.New("controlled drained Usage replay failure")
	collector, err := usage.NewCollector(r.usage.Aggregate, failingDrainReplayer{failure}, 1)
	if err != nil {
		t.Fatal(err)
	}
	r.usageCollector = collector // fixture has no running collector goroutine
	if lease, err := r.beginAccountingMaintenance(context.Background(), nil); !errors.Is(err, failure) || lease != nil {
		t.Fatal(err)
	}
	if r.backgroundWait.drainState.Load() != businessDrainStarted {
		t.Fatal("Usage failure credited drain completion")
	}
	if _, _, err := r.accounting.ReadWorkUnit(context.Background(), "", "absent"); err != nil {
		t.Fatal("Usage failure prematurely retired the owner", err)
	}
}

func TestFailedBusinessDrainCloseDoesNotWriteSuccessfulShutdownAudit(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	masterKey, err := unlockMemberMasterKey(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	key, err := loadAuditHMACKey(r.store, r.vault, masterKey)
	clear(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	failure := errors.New("controlled shutdown HTTP failure")
	if err := r.drainAccountingBusiness(context.Background(), []shutdownHTTPServer{&fakeShutdownServer{shutdownError: failure}}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = audit.VerifyWithVisitor(cfg.AuditPath(), key, func(record audit.Record) error {
		if record.Event.Action == "system.shutdown" {
			return errors.New("failed drain was reported as a successful shutdown")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAudit(context.Background(), cfg); err != nil {
		t.Fatal("terminal Close lost the authenticated audit checkpoint", err)
	}
}
