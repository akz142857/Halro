package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replication"
)

func primaryCompletionFixture(t *testing.T) *replicationRuntime {
	t.Helper()
	coordinator, journal := newStepdownTestCoordinator(t)
	key := []byte("0123456789abcdef0123456789abcdef")
	state := replication.MemberState{Version: replication.StateVersion, ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Role: replication.RolePrimary, Term: 7, PromisedTerm: 7,
		Peers: []replication.StatePeer{{Name: "halro-1", Address: "127.0.0.1:9911", SPKISHA256: "sha256:" + strings.Repeat("a", 64)}}}
	pub, err := replication.NewStatePublisher(filepath.Join(t.TempDir(), "state.json"), key, state)
	if err != nil {
		_ = journal.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &replicationRuntime{role: replication.RolePrimary, coordinator: coordinator, journal: journal, publisher: pub, startupCtx: ctx, startupStop: cancel, fatal: make(chan error, 1)}
	t.Cleanup(func() { _ = r.close() })
	return r
}

func awaitPrimaryCompletionSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("Primary completion lifecycle signal did not arrive")
	}
}

func TestPrimaryStartupCompletionConfirmedBaselineWaitsForPeerAndCloseJoins(t *testing.T) {
	for _, connect := range []bool{false, true} {
		t.Run(fmt.Sprintf("connect=%v", connect), func(t *testing.T) {
			r := primaryCompletionFixture(t)
			commit, err := r.coordinator.RecordDurable(replication.Frame{Kind: replication.KindLeadershipEstablished})
			if err != nil {
				t.Fatal(err)
			}
			r.coordinator.Stop()
			c, err := replication.NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 7, commit.Index, []string{"halro-1"}, r.journal, emptyAdmissionOutbound{}, func(replication.PrimaryProgress) error { return nil }, emptyAdmissionSource{}, replication.DefaultPrimaryWindowLimits())
			if err != nil {
				t.Fatal(err)
			}
			r.coordinator = c
			var calls atomic.Int32
			r.startupFinish = func(context.Context) error { calls.Add(1); return nil }
			r.startPrimaryCompletion(commit.Index)
			select {
			case <-r.startupDone:
				t.Fatal("startup finished without real peer window")
			case <-time.After(20 * time.Millisecond):
			}
			if calls.Load() != 0 || r.startupReady.Load() {
				t.Fatal("recovery ran before peer availability")
			}
			if connect {
				if err := c.PreparePeer("halro-1", commit.Index, commit.Index, func([]byte) error { return nil }); err != nil {
					t.Fatal(err)
				}
				awaitPrimaryCompletionSignal(t, r.startupDone)
				if calls.Load() != 1 || !r.startupReady.Load() {
					t.Fatal("actual peer did not enable startup", calls.Load())
				}
			}
			if err := r.close(); err != nil {
				t.Fatal(err)
			}
			awaitPrimaryCompletionSignal(t, r.startupDone)
			if r.startupReady.Load() || (!connect && calls.Load() != 0) {
				t.Fatal("Close restored readiness or entered recovery")
			}
		})
	}
}

func TestPrimaryStartupCompletionCloseJoinsContinuationBeforeDependencies(t *testing.T) {
	r := primaryCompletionFixture(t)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r.startupFinish = func(ctx context.Context) error {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		close(canceled)
		// Model physical work that must finish after its caller is canceled.
		<-release
		return ctx.Err()
	}
	r.startPrimaryCompletion(0)
	awaitPrimaryCompletionSignal(t, entered)
	closed := make(chan struct{})
	var closeErr error
	go func() { closeErr = r.close(); close(closed) }()
	awaitPrimaryCompletionSignal(t, canceled)
	select {
	case <-closed:
		close(release)
		t.Fatal("Close returned before actual continuation completion")
	default:
	}
	// Member authority must still be open while the worker uses it.
	_, promiseErr := r.publisher.Promise(8)
	close(release)
	awaitPrimaryCompletionSignal(t, closed)
	if promiseErr != nil || closeErr != nil || r.startupReady.Load() {
		t.Fatal("startup joined after dependency closure or restored readiness", promiseErr, closeErr)
	}
	r.startPrimaryCompletion(0)
	if calls.Load() != 1 {
		t.Fatal("retired startup admitted another continuation")
	}
}

func TestPrimaryStartupCompletionWaitsForSuccessfulContinuation(t *testing.T) {
	r := primaryCompletionFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r.startupFinish = func(context.Context) error { close(entered); <-release; return nil }
	r.startPrimaryCompletion(0)
	awaitPrimaryCompletionSignal(t, entered)
	before := r.startupReady.Load()
	r.startPrimaryCompletion(0) // A second peer cannot run recovery twice.
	close(release)
	awaitPrimaryCompletionSignal(t, r.startupDone)
	if before || !r.startupReady.Load() {
		t.Fatal("readiness did not wait for successful startup work", before)
	}
}

func TestPrimaryStartupCompletionCanceledRetirePreservesDependenciesForRealJoin(t *testing.T) {
	r := primaryCompletionFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r.startupFinish = func(context.Context) error { close(entered); <-release; return nil }
	r.startPrimaryCompletion(0)
	awaitPrimaryCompletionSignal(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retireErr := r.retirePrimaryCompletion(ctx)
	_, promiseErr := r.publisher.Promise(8)
	select {
	case <-r.startupDone:
		close(release)
		t.Fatal("canceled observer claimed physical completion")
	default:
	}
	close(release)
	awaitPrimaryCompletionSignal(t, r.startupDone)
	if !errors.Is(retireErr, context.Canceled) || promiseErr != nil || r.startupReady.Load() {
		t.Fatal("failed retirement lost its real owner or republished readiness", retireErr, promiseErr)
	}
	if err := r.retirePrimaryCompletion(context.Background()); err != nil {
		t.Fatal("retry failed to join the same completion handle", err)
	}
}

func TestPrimaryStartupCompletionCoordinatorWithdrawalRefusesReadiness(t *testing.T) {
	r := primaryCompletionFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r.startupFinish = func(context.Context) error { close(entered); <-release; return nil }
	r.startPrimaryCompletion(0)
	awaitPrimaryCompletionSignal(t, entered)
	cause := errors.New("injected transport authority withdrawal")
	r.coordinator.MarkUnavailable(cause)
	close(release)
	awaitPrimaryCompletionSignal(t, r.startupDone)
	if r.startupReady.Load() {
		t.Fatal("unavailable coordinator published startup readiness")
	}
	select {
	case err := <-r.fatal:
		if !errors.Is(err, cause) {
			t.Fatal("startup lost coordinator withdrawal cause", err)
		}
	default:
		t.Fatal("coordinator withdrawal did not report startup failure")
	}
}

func TestPrimaryStartupCompletionFailureOrTermChangeKeepsReadinessWithdrawn(t *testing.T) {
	for _, changeTerm := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovery_failure", true: "term_change"}[changeTerm], func(t *testing.T) {
			r := primaryCompletionFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			failure := errors.New("injected recovery publication failure")
			r.startupFinish = func(context.Context) error {
				close(entered)
				<-release
				if !changeTerm {
					return failure
				}
				return nil
			}
			r.startPrimaryCompletion(0)
			awaitPrimaryCompletionSignal(t, entered)
			var promiseErr error
			if changeTerm {
				_, promiseErr = r.publisher.Promise(8)
			}
			close(release)
			awaitPrimaryCompletionSignal(t, r.startupDone)
			if promiseErr != nil || r.startupReady.Load() {
				t.Fatal("failed/revoked recovery restored readiness", promiseErr)
			}
			select {
			case err := <-r.fatal:
				if !changeTerm && !errors.Is(err, failure) {
					t.Fatal("startup lost recovery failure cause", err)
				}
			default:
				t.Fatal("failed/revoked completion did not report its failure")
			}
		})
	}
}
