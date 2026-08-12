package app

import (
	"context"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
)

// runActivationRecovery is the only thing that ends a stale snapshot, and a
// stale snapshot refuses the whole data plane. Until these tests existed the
// loop itself was never executed: its two helpers were covered, so deleting the
// line in Open that starts it failed nothing.

// The loop must end a staleness it can fix. Anything less means an instance that
// answers 503 for every Project until someone restarts it.
func TestActivationRecoveryClearsAStaleDomain(t *testing.T) {
	runtime, _, _ := openBootstrappedRuntime(t)
	runtime.activation.markStale(activationDomainRedaction, "injected for recovery", time.Now().UTC())
	if !runtime.activation.status().Stale {
		t.Fatal("the domain under test did not start stale")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runtime.runActivationRecovery(ctx, time.Millisecond)

	waitFor(t, "the stale domain to be replayed", func() bool {
		return !runtime.activation.status().Stale
	})
}

// Audit delivery is retried on every tick, deliberately not gated on staleness:
// a failed audit append does not make a serving snapshot stale, but it must not
// wait for the next process start either. Moving the drain inside the staleness
// branch reads like a tidy-up and would leave a backlog sitting until something
// else went wrong.
func TestActivationRecoveryDrainsAuditIntentsWhileNothingIsStale(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	if runtime.activation.status().Stale {
		t.Fatal("this test needs a runtime that is not stale")
	}
	writePendingAuditIntent(t, runtime, bootstrap)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runtime.runActivationRecovery(ctx, time.Millisecond)

	waitFor(t, "the pending audit intent to be delivered", func() bool {
		pending, err := runtime.store.PendingAdminAuditIntentCount(context.Background())
		return err == nil && pending == 0
	})
	if runtime.activation.status().Stale {
		t.Fatal("draining an audit backlog made the runtime refuse traffic")
	}
}

// Shutdown waits on this goroutine, so a loop that ignores cancellation holds
// the whole process past its shutdown budget.
func TestActivationRecoveryStopsWhenItsContextEnds(t *testing.T) {
	runtime, _, _ := openBootstrappedRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		runtime.runActivationRecovery(ctx, time.Millisecond)
		close(stopped)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the recovery loop kept running after its context was cancelled")
	}
}

// The tests above drive the loop directly, which proves the loop and not that
// anything starts it. This one uses the real interval against a runtime opened
// the ordinary way, so removing the goroutine in Open fails here. It costs one
// retry interval, which is why it is the only test that pays for it.
func TestOpenStartsTheRecoveryLoopSoAStaleRuntimeHealsItself(t *testing.T) {
	runtime, _, _ := openBootstrappedRuntime(t)
	runtime.activation.markStale(activationDomainTokenGuard, "injected for startup wiring", time.Now().UTC())

	deadline := time.Now().Add(3 * activationRetryInterval)
	for runtime.activation.status().Stale {
		if time.Now().After(deadline) {
			t.Fatalf("a stale runtime did not heal within %s: nothing is running the recovery loop",
				3*activationRetryInterval)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func writePendingAuditIntent(t *testing.T, runtime *Runtime, bootstrap BootstrapResult) {
	t.Helper()
	eventID, err := id.New("aud")
	if err != nil {
		t.Fatal(err)
	}
	intent := &domain.AdminAuditIntent{
		EventID: eventID, OccurredAt: time.Now().UTC(),
		ActorType: "admin_user", ActorID: "admin",
		Action: "project.update", TargetType: "project", TargetID: bootstrap.ProjectID,
	}
	project, err := runtime.store.GetProject(context.Background(), bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	// Committed through the store rather than over HTTP: the intent is durable
	// and undelivered, which is the state a crash between commit and audit append
	// leaves behind.
	if _, err := runtime.store.PutProject(context.Background(), project, project.Revision, intent); err != nil {
		t.Fatal(err)
	}
	pending, err := runtime.store.PendingAdminAuditIntentCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("test setup left %d pending audit intents, want 1", pending)
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
