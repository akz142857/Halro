package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
)

func TestRequiredConfirmationWaitOutcomesExcludeNonRequiredCoordinatorCalls(t *testing.T) {
	coordinator, journal := newStepdownTestCoordinator(t)
	defer journal.Close()
	runtime := &replicationRuntime{coordinator: coordinator}
	var options ledger.Options
	runtime.ledgerHooks(&options)
	commit, err := coordinator.RecordDurable(replication.Frame{Kind: replication.KindLeadershipEstablished, Store: replication.StoreNone})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := options.WaitConfirmed(ctx, commit.Index); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline wait=%v", err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := runtime.waitRequiredConfirmation(canceled, commit.Index, requiredMetadata); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait=%v", err)
	}
	if _, err := coordinator.Acknowledge(replication.Acknowledgement{ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-1", Term: 7, Index: commit.Index, DurableIndex: commit.Index}); err != nil {
		t.Fatal(err)
	}
	if err := options.WaitConfirmed(context.Background(), commit.Index); err != nil {
		t.Fatal(err)
	}
	coordinator.MarkUnavailable(errors.New("peer quorum absent"))
	if err := options.WaitConfirmed(context.Background(), commit.Index+1); !errors.Is(err, replication.ErrReplicationUnavailable) {
		t.Fatalf("unavailable wait=%v", err)
	}
	if err := coordinator.WaitConfirmed(context.Background(), commit.Index); err != nil {
		t.Fatal(err)
	}
	if runtime.requiredWaits[requiredLedger][0].Load() != 1 || runtime.requiredWaits[requiredLedger][1].Load() != 1 ||
		runtime.requiredWaits[requiredLedger][2].Load() != 1 || runtime.requiredWaits[requiredMetadata][3].Load() != 1 {
		t.Fatal("required confirmation wait outcome counters did not match observed returns")
	}
	_, _, head := journal.Head()
	state := replication.MemberState{
		Version: replication.StateVersion, ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0",
		Role: replication.RolePrimary, Term: 7, PromisedTerm: 7, DurableIndex: commit.Index,
		ConfirmedIndex: commit.Index, AppliedIndex: commit.Index, OrderingHeadMAC: head,
		Projection: replication.ProjectionState{Index: commit.Index},
		Peers:      []replication.StatePeer{{Name: "halro-1", Address: "peer.invalid:9910", SPKISHA256: "sha256:" + strings.Repeat("a", 64)}},
	}
	publisher, err := replication.NewStatePublisher(filepath.Join(t.TempDir(), "state.json"), make([]byte, 32), state)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	runtime.publisher = publisher
	var body bytes.Buffer
	writer := bufio.NewWriter(&body)
	(&Runtime{replication: runtime}).writeReplicationMetrics(writer)
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`halro_replication_required_confirmation_wait_total{store="ledger",outcome="confirmed"} 1`,
		`halro_replication_required_confirmation_wait_total{store="ledger",outcome="deadline"} 1`,
		`halro_replication_required_confirmation_wait_total{store="ledger",outcome="unavailable"} 1`,
		`halro_replication_required_confirmation_wait_total{store="metadata",outcome="canceled"} 1`,
	} {
		if !strings.Contains(body.String(), expected) {
			t.Fatalf("required wait metric missing %q", expected)
		}
	}
}
