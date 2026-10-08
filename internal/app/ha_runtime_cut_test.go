package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

func haRuntimeCutBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	limits := runtimeCandidateBudgetFixture().Limits(time.Hour)
	budget, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if budget.Snapshot().Used != (resourcebudget.Cost{}) {
			t.Error("HA cut leaked its real shared workspace", budget.Snapshot())
		}
	})
	return budget
}

func replaceHARuntimeMemberState(t *testing.T, cfg config.Config, edit func(*replication.MemberState)) {
	t.Helper()
	master, err := unlockMemberMasterKey(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	state, key, err := readMemberState(cfg, master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key[:])
	edit(&state)
	if err := replication.WriteState(cfg.ReplicationStatePath(), state, key[:]); err != nil {
		t.Fatal(err)
	}
}

func TestHARuntimeCommonCutAllowsPrimaryLocalAppliedAheadOfConfirmation(t *testing.T) {
	cfg, _, tail := replicaStartupPrefixFixture(t)
	replaceHARuntimeMemberState(t, cfg, func(s *replication.MemberState) {
		s.Role = replication.RolePrimary
	})
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	r.stopBackgroundWork()
	budget := haRuntimeCutBudget(t)
	cut, err := r.replication.authenticateHARuntimeCut(context.Background(), budget)
	if err != nil {
		t.Fatal(err)
	}
	if cut.member.ConfirmedIndex != 1 || cut.member.DurableIndex < 2 || cut.orderingIndex != cut.member.DurableIndex || cut.permitted[replication.StoreLedger-replication.StoreLedger].Sequence != tail.Sequence || cut.heads[0] == ([32]byte{}) {
		t.Fatal("Primary local Apply was lost or credited as confirmation", cut)
	}
	if cut.member.AppliedIndex != cut.member.ConfirmedIndex || r.replication.startupReady.Load() {
		t.Fatal("local cut incorrectly granted transferable authority or business readiness", cut.member)
	}
}

func TestHARuntimeCommonCutRejectsSignedMetadataCursorOutsideAppliedOrdering(t *testing.T) {
	cfg, _, _ := replicaStartupPrefixFixture(t)
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	r.stopBackgroundWork()
	state := r.replication.publisher.Snapshot()
	// This is a real, authenticated publication. Its individually valid
	// Metadata tuple is nevertheless not the Metadata tuple at applied_index.
	err = r.replication.publisher.PublishReplica(replication.ReplicaProgress{
		DurableIndex: state.DurableIndex, ConfirmedIndex: state.ConfirmedIndex, AppliedIndex: state.AppliedIndex,
		OrderingHeadMAC: state.OrderingHeadMAC,
		Projection:      replication.ProjectionState{Index: state.AppliedIndex, MetadataEpoch: state.Projection.MetadataEpoch, MetadataSequence: state.Projection.MetadataSequence + 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	cut, err := r.replication.authenticateHARuntimeCut(context.Background(), haRuntimeCutBudget(t))
	if err == nil || !strings.Contains(err.Error(), "Metadata projection cursor does not belong") || cut.orderingIndex != 0 || cut.member.NodeID != "" {
		t.Fatal("individually signed but incoherent Metadata cut became common authority", cut, err)
	}
}

func TestHARuntimeOpenRefusesAheadMetadataBeforeConstructingReplicaGeneration(t *testing.T) {
	cfg, _, _ := replicaStartupPrefixFixture(t)
	ctx := context.Background()
	master, err := unlockMemberMasterKey(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	state, clusterKey, err := readMemberState(cfg, master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(clusterKey[:])
	v, err := vault.New(master)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	metadata, err := boltstore.OpenPrimary(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	key, err := loadMetadataJournalHMACKey(metadata, v, master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	if _, err := metadata.AttachMetadataJournal(key, "HA applied cut adversarial fixture"); err != nil {
		t.Fatal(err)
	}
	journal, err := replication.OpenExistingOrderingJournal(cfg.OrderingJournalPath(), clusterKey[:], state.ClusterID, state.Incarnation, state.DurableIndex, state.OrderingHeadMAC)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if err := metadata.SetMetadataJournalAfterDurable(func(batch metadatajournal.DurableBatch) (uint64, error) {
		frame := replication.Frame{ClusterID: state.ClusterID, Incarnation: state.Incarnation, Index: state.DurableIndex + 1, Term: state.Term, Kind: replication.KindData, Store: replication.StoreMetadata,
			StoreGeneration: batch.Epoch, StoreSequenceFirst: batch.FirstSequence, StoreSequenceLast: batch.LastSequence, Payload: batch.Frames}
		_, digest, err := frame.MarshalBinaryAndDigest()
		if err != nil {
			return 0, err
		}
		_, err = journal.Append(replication.OrderingRecord{Index: frame.Index, Term: frame.Term, Kind: frame.Kind, Store: frame.Store,
			StoreGeneration: frame.StoreGeneration, StoreSequenceFirst: frame.StoreSequenceFirst, StoreSequenceLast: frame.StoreSequenceLast, FrameDigest: digest})
		return frame.Index, err
	}, func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.PutProject(ctx, domain.Project{ID: "ahead_metadata", Name: "Excluded Metadata fact", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
	info, err := metadata.Info()
	if err != nil {
		t.Fatal(err)
	}
	state.DurableIndex, _, state.OrderingHeadMAC = journal.Head()
	state.Projection.MetadataEpoch, state.Projection.MetadataSequence = info.MetadataJournalEpoch, info.MetadataJournalSequence
	if err := replication.WriteState(cfg.ReplicationStatePath(), state, clusterKey[:]); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(journal.Close(), metadata.Close()); err != nil {
		t.Fatal(err)
	}
	// The individually valid signed projection now agrees with bbolt and the
	// durable journal, but it leads the allowed applied ordering prefix. This
	// must fail before old derivatives are removed or new readers are published.
	for _, path := range []string{replicaRuntimeProjectionPath(cfg), replicaRuntimeProjectionPath(cfg) + ".usage"} {
		if err := os.WriteFile(path, []byte("prior derived generation preserved on authority refusal"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if r != nil {
		_ = r.Close()
		t.Fatal("ahead Metadata returned a usable Runtime")
	}
	if err == nil || !strings.Contains(err.Error(), "Metadata projection cursor does not belong") {
		t.Fatal("default Replica did not apply the common ordering authority gate", err)
	}
	for _, path := range []string{replicaRuntimeProjectionPath(cfg), replicaRuntimeProjectionPath(cfg) + ".usage"} {
		b, err := os.ReadFile(path)
		if err != nil || string(b) != "prior derived generation preserved on authority refusal" {
			t.Fatal("refusal destroyed previous generation", path, err)
		}
	}
}

func TestHARuntimeRebuildsSmallerPermittedPrefixWithoutReusingOldGeneration(t *testing.T) {
	cfg, baseline, tail := replicaStartupPrefixFixture(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	r.stopBackgroundWork()
	state := r.replication.publisher.Snapshot()
	if err := r.replication.receiver.BindPrimary("halro-0"); err != nil {
		t.Fatal(err)
	}
	if err := r.replication.receiver.Confirm(replication.CommitNotice{ClusterID: state.ClusterID, Incarnation: state.Incarnation, NodeID: "halro-0", Term: state.Term, ConfirmedIndex: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.replication.applier.ApplyConfirmed(ctx); err != nil {
		t.Fatal(err)
	}
	oldLedger, oldUsage, oldGeneration := r.accountingState.projection, r.usage.disk, r.accountingState.replica
	if head, _, err := oldLedger.ReadPosition(ctx); err != nil || head != tail {
		t.Fatal(head, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Explicit offline authority establishes a smaller allowed prefix. The
	// native durable/ordering suffix stays intact; fresh generation rebuild is
	// the existing conservative recovery path, not a watermark assignment.
	replaceHARuntimeMemberState(t, cfg, func(s *replication.MemberState) {
		s.ConfirmedIndex, s.AppliedIndex, s.Projection.Index = 1, 1, 1
	})
	digests := make(map[string][32]byte)
	for _, path := range []string{cfg.LedgerPath(), cfg.AuditPath(), cfg.GovernancePath(), filepathMetadataJournal(cfg), cfg.OrderingJournalPath()} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		digests[path] = sha256.Sum256(b)
	}
	reopened, err := Open(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reopened.stopBackgroundWork()
	cut, err := reopened.replication.authenticateHARuntimeCut(ctx, haRuntimeCutBudget(t))
	if err != nil || cut.orderingIndex != 1 || cut.permitted[0].Sequence != baseline.Sequence || cut.member.DurableIndex != 2 {
		t.Fatal("smaller role cut selected durable suffix", cut, err)
	}
	if reopened.accountingState.replica == oldGeneration || reopened.accountingState.projection == oldLedger || reopened.usage.disk == oldUsage {
		t.Fatal("reused former derived generation")
	}
	head, _, err := reopened.accountingState.projection.ReadPosition(ctx)
	stats, statsErr := reopened.usage.disk.ReadStats(ctx)
	if err != nil || statsErr != nil || head != baseline || stats.Metrics.ActiveRequests != 0 {
		t.Fatal("new generation retained excluded semantic state", head, stats, err, statsErr)
	}
	if err := oldGeneration.ValidateLedgerForPublication(ctx); !errors.Is(err, errReplicaAccountingRetired) {
		t.Fatal("old generation retained mutation authority", err)
	}
	if _, err := oldUsage.ReadStats(ctx); err == nil {
		t.Fatal("old Usage reader remained live")
	}
	for path, before := range digests {
		b, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(b) != before {
			t.Fatal("derived rebuild modified authoritative native bytes", path, err)
		}
	}
	state = reopened.replication.publisher.Snapshot()
	if err := reopened.replication.receiver.BindPrimary("halro-0"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.replication.receiver.Confirm(replication.CommitNotice{ClusterID: state.ClusterID, Incarnation: state.Incarnation, NodeID: "halro-0", Term: state.Term, ConfirmedIndex: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.replication.applier.ApplyConfirmed(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err = reopened.usage.disk.ReadStats(ctx)
	if err != nil || stats.Watermark != tail || stats.Metrics.ActiveRequests != 1 {
		t.Fatal("later actual confirmation failed or applied facts twice", stats, err)
	}
}
