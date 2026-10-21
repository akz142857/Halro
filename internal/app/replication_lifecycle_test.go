package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

func TestPrimaryStartupRequiresAllPeersExceptRecoveredPromotionAnchor(t *testing.T) {
	if got := primaryStartupPeerRequirement(2, false); got != 2 {
		t.Fatalf("clean startup peer requirement=%d, want 2", got)
	}
	if got := primaryStartupPeerRequirement(2, true); got != 1 {
		t.Fatalf("promoted startup peer requirement=%d, want quorum peer 1", got)
	}
}

func TestPlannedStepdownBindsNamedPrimaryAndExpectedTerm(t *testing.T) {
	state := replication.MemberState{NodeID: "halro-0", Term: 7}
	proposal := replication.PromotionProposal{OldPrimaryNodeID: "halro-0", ExpectedTerm: 7}
	if err := validatePlannedStepdownTarget(proposal, state); err != nil {
		t.Fatal(err)
	}
	proposal.OldPrimaryNodeID = "halro-2"
	if err := validatePlannedStepdownTarget(proposal, state); err == nil {
		t.Fatal("planned stepdown accepted a proposal naming another Primary")
	}
	proposal.OldPrimaryNodeID = state.NodeID
	proposal.ExpectedTerm = 6
	if err := validatePlannedStepdownTarget(proposal, state); err == nil {
		t.Fatal("planned stepdown accepted a stale expected term")
	}
}

func TestPromotionProposalBindsAuthenticatedCandidateTermAndPrefix(t *testing.T) {
	proposal := replication.PromotionProposal{Term: 8, ExpectedTerm: 7, ExpectedAppliedIndex: 42}
	peer := replication.Hello{Term: 7, PromisedTerm: 8, DurableIndex: 42, AppliedIndex: 42}
	if err := validatePromotionCandidateHello(peer, proposal); err != nil {
		t.Fatal(err)
	}
	peer.PromisedTerm = 9
	if err := validatePromotionCandidateHello(peer, proposal); err == nil {
		t.Fatal("proposal accepted a candidate Hello from a different prepare term")
	}
	peer.PromisedTerm = proposal.Term
	peer.AppliedIndex--
	if err := validatePromotionCandidateHello(peer, proposal); err == nil {
		t.Fatal("proposal accepted a candidate Hello with a different applied prefix")
	}
}

func TestPlannedStepdownRefusesToFreezeBeforeTheHTTPDrain(t *testing.T) {
	state := replication.MemberState{NodeID: "halro-0", Term: 7}
	proposal := replication.PromotionProposal{OldPrimaryNodeID: "halro-0", ExpectedTerm: 7, ExpectedAppliedIndex: 42}
	runtime := &replicationRuntime{}
	if err := runtime.freezeForPlannedStepdown(proposal, state); err == nil || err.Error() != "planned stepdown drain is unavailable" {
		t.Fatalf("missing drain error=%v", err)
	}
	want := errors.New("injected active request did not drain")
	runtime.plannedStepdownDrain = func() error { return want }
	// coordinator is deliberately nil. Reaching it would panic, so this also
	// proves a failed drain cannot freeze or promise the old Primary.
	if err := runtime.freezeForPlannedStepdown(proposal, state); !errors.Is(err, want) {
		t.Fatalf("drain error=%v, want %v", err, want)
	}
}

type stepdownTestOutbound struct{}

func (stepdownTestOutbound) QueueFrame(uint64, []byte) error        { return nil }
func (stepdownTestOutbound) QueueCommitNotice(uint64, []byte) error { return nil }
func (stepdownTestOutbound) Acknowledge(string, uint64, uint64)     {}

func newStepdownTestCoordinator(t *testing.T) (*replication.PrimaryCoordinator, *replication.OrderingJournal) {
	t.Helper()
	journal, err := replication.OpenOrderingJournal(
		filepath.Join(t.TempDir(), "ordering.journal"),
		[]byte("0123456789abcdef0123456789abcdef"),
		replication.OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}, 0, [32]byte{},
	)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := replication.NewPrimaryCoordinator(
		"production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1"},
		journal, stepdownTestOutbound{}, func(replication.PrimaryProgress) error { return nil },
	)
	if err != nil {
		_ = journal.Close()
		t.Fatal(err)
	}
	return coordinator, journal
}

func TestPlannedStepdownDrainsBeforeFreezingTheConfirmedPrefix(t *testing.T) {
	coordinator, journal := newStepdownTestCoordinator(t)
	defer journal.Close()
	drained := false
	runtime := &replicationRuntime{coordinator: coordinator, plannedStepdownDrain: func() error {
		_, available, err := coordinator.Status()
		if !available || err != nil {
			t.Fatalf("coordinator was frozen before drain: available=%t err=%v", available, err)
		}
		drained = true
		return nil
	}}
	state := replication.MemberState{NodeID: "halro-0", Term: 7}
	proposal := replication.PromotionProposal{OldPrimaryNodeID: "halro-0", ExpectedTerm: 7, ExpectedAppliedIndex: 0}
	if err := runtime.freezeForPlannedStepdown(proposal, state); err != nil {
		t.Fatal(err)
	}
	if !drained {
		t.Fatal("planned stepdown froze without invoking the drain")
	}
	if _, available, _ := coordinator.Status(); available {
		t.Fatal("coordinator remained available after the successful drain and freeze")
	}
}

func TestPlannedStepdownRefusesPromiseWhenDrainAdvancesThePrefix(t *testing.T) {
	coordinator, journal := newStepdownTestCoordinator(t)
	defer journal.Close()
	runtime := &replicationRuntime{coordinator: coordinator}
	runtime.plannedStepdownDrain = func() error {
		// Model an admitted handler completing its final ordered write while
		// net/http Shutdown waits for it. The candidate proposed index 0 before
		// the drain began; the old Primary must not freeze or promise that stale
		// prefix after the handler advances it to 1.
		commit, err := coordinator.RecordDurable(replication.Frame{Kind: replication.KindLeadershipEstablished, Store: replication.StoreNone})
		if err != nil {
			return err
		}
		_, err = coordinator.Acknowledge(replication.Acknowledgement{
			ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-1", Term: 7,
			Index: commit.Index, DurableIndex: commit.Index, AppliedIndex: commit.Index,
		})
		return err
	}
	state := replication.MemberState{NodeID: "halro-0", Term: 7}
	proposal := replication.PromotionProposal{OldPrimaryNodeID: "halro-0", ExpectedTerm: 7, ExpectedAppliedIndex: 0}
	err := runtime.freezeForPlannedStepdown(proposal, state)
	if err == nil || !strings.Contains(err.Error(), "prefix 1/1") {
		t.Fatalf("stale post-drain proposal error=%v", err)
	}
	if _, available, cause := coordinator.Status(); !available || cause != nil {
		t.Fatalf("failed stale proposal froze coordinator: available=%t cause=%v", available, cause)
	}
	if _, err := coordinator.RecordDurable(replication.Frame{
		Kind: replication.KindData, Store: replication.StoreLedger, Payload: []byte{1},
		StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1,
	}); err != nil {
		t.Fatalf("failed stale proposal blocked later old-term work before any promise: %v", err)
	}
}

func TestHigherTermDemotionRequiresReseedForUnconfirmedOrAheadProgress(t *testing.T) {
	state := replication.MemberState{DurableIndex: 9, ConfirmedIndex: 8, AppliedIndex: 8}
	peer := replication.Hello{DurableIndex: 9, AppliedIndex: 9}
	if !primaryProgressRequiresReseed(state, peer) {
		t.Fatal("unconfirmed old-Primary suffix did not require re-seed")
	}
	state.DurableIndex = 8
	if primaryProgressRequiresReseed(state, peer) {
		t.Fatal("fully applied prefix behind a higher-term Primary unexpectedly required re-seed")
	}
	peer.DurableIndex = 7
	if !primaryProgressRequiresReseed(state, peer) {
		t.Fatal("old Primary ahead of the higher-term peer did not require re-seed")
	}
}

func TestReseedRequiredMarkerSurvivesRestartGate(t *testing.T) {
	directory := t.TempDir()
	cfg := config.Config{}
	cfg.Storage.DataDir = directory
	clusterDirectory := cfg.ClusterDirectoryPath()
	if err := os.MkdirAll(clusterDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(clusterDirectory, reseedRequiredMarker)
	if err := persistReseedRequiredMarker(path, "incompatible authenticated prefix"); err != nil {
		t.Fatal(err)
	}
	if err := refuseReseedRequiredMember(cfg); !errors.Is(err, replication.ErrMemberRequiresFullReseed) {
		t.Fatalf("restart gate error=%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode=%o, want 600", info.Mode().Perm())
	}
}
