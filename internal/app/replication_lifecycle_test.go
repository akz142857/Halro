package app

import (
	"errors"
	"os"
	"path/filepath"
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
