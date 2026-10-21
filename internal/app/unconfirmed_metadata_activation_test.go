package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replication"
)

// A withdrawing metadata write whose confirmation fails is already committed
// locally. Found in a three-member drill: with both Replicas stopped, revoking
// a Gateway Key answered 503 replication_unavailable, the Replicas came back
// and confirmed it, the console listed the key as deleted — and the Primary's
// gateway went on accepting it, because the handler had returned before
// activating and nothing else ever rebuilt the snapshot.

// Every failed wait leaves a local commit behind, so every failure has to reach
// Runtime; a wait that confirms must not.
func TestAFailedMetadataConfirmationIsReportedToRuntime(t *testing.T) {
	coordinator, journal := newStepdownTestCoordinator(t)
	defer journal.Close()
	var reported atomic.Int32
	runtime := &replicationRuntime{coordinator: coordinator, unconfirmedMetadata: func() { reported.Add(1) }}

	commit, err := coordinator.RecordDurable(replication.Frame{Kind: replication.KindLeadershipEstablished, Store: replication.StoreNone})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Acknowledge(replication.Acknowledgement{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-1", Term: 7, Index: commit.Index, DurableIndex: commit.Index}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.waitMetadataConfirmation(context.Background(), commit.Index); err != nil {
		t.Fatal(err)
	}
	if got := reported.Load(); got != 0 {
		t.Fatalf("a confirmed write was reported as unconfirmed %d time(s)", got)
	}

	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := runtime.waitMetadataConfirmation(canceled, commit.Index+1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait=%v", err)
	}
	coordinator.MarkUnavailable(errors.New("peer quorum absent"))
	if err := runtime.waitMetadataConfirmation(context.Background(), commit.Index+1); !errors.Is(err, replication.ErrReplicationUnavailable) {
		t.Fatalf("unavailable wait=%v", err)
	}
	if got := reported.Load(); got != 2 {
		t.Fatalf("failed waits reported=%d, want 2", got)
	}
}

// The revocation is in the store and the live snapshot has not seen it. Once
// Runtime hears of the unconfirmed commit the data plane refuses at once, and
// the revocation is enforced without waiting for a tick.
//
// The loop under test is the one Open started, on its production five-second
// interval, so recovery inside one second has to come from the wake. One round
// could still land on a tick by chance (about one in five); three rounds in a
// row without the wake would be under one in a hundred.
func TestAnUnconfirmedRevocationIsEnforcedWithoutWaitingForATick(t *testing.T) {
	runtime, bootstrap, _ := activationTestRuntime(t)
	ctx := context.Background()
	if _, err := runtime.auth.Authenticate(bootstrap.GatewayKey, time.Now()); err != nil {
		t.Fatalf("the key did not authenticate before revocation: %v", err)
	}

	key, err := runtime.store.GetGatewayKey(ctx, bootstrap.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	key.Enabled = false
	key.DeletedAt = &now
	if _, err := runtime.store.PutGatewayKey(ctx, key, key.Revision, nil); err != nil {
		t.Fatal(err)
	}
	// The defect state: the store holds the revocation, the snapshot does not.
	if _, err := runtime.auth.Authenticate(bootstrap.GatewayKey, time.Now()); err != nil {
		t.Fatalf("the precondition did not hold — the snapshot already saw the revocation: %v", err)
	}

	for round := 1; round <= 3; round++ {
		runtime.noteUnconfirmedMetadataCommit()
		refused := httptest.NewRecorder()
		runtime.gatewayRouter().ServeHTTP(refused, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
		if refused.Code != http.StatusServiceUnavailable {
			t.Fatalf("round %d: the data plane served from a snapshot known to be behind: status=%d body=%s", round, refused.Code, refused.Body.String())
		}
		deadline := time.Now().Add(time.Second)
		for runtime.activation.status().Stale {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: still stale after 1s; recovery is waiting for the %s tick instead of the wake", round, activationRetryInterval)
			}
			time.Sleep(2 * time.Millisecond)
		}
		if _, err := runtime.auth.Authenticate(bootstrap.GatewayKey, time.Now()); err == nil {
			t.Fatalf("round %d: the revoked key still authenticates after recovery", round)
		}
	}
}
