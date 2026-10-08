package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

// Use a real Standalone seed and EstablishMemberState to retain all four native
// store baselines. Append one real Ledger batch and bind it to authenticated
// ordering/member state without applying its semantic fact yet.
func replicaStartupPrefixFixture(t *testing.T) (config.Config, ledger.Watermark, ledger.Watermark) {
	t.Helper()
	ctx := context.Background()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	baseline, _, _ := r.ledger.ChainHead()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	ca, caKey := makeReplicationCertificate(t, nil, nil, true, "replica-prefix-ca")
	cert, certKey := makeReplicationCertificate(t, ca, caKey, false, "halro-1")
	peer, _ := makeReplicationCertificate(t, ca, caKey, false, "halro-0")
	pin := sha256.Sum256(peer.RawSubjectPublicKeyInfo)
	cfg.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-1", Listen: "127.0.0.1:9911",
		TLS:   writeReplicationTLSFiles(t, t.TempDir(), ca, cert, certKey),
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: "127.0.0.1:9910", SPKISHA256: "sha256:" + hex.EncodeToString(pin[:])}},
	}
	if err := EstablishMemberState(ctx, cfg, replication.RoleReplica, "inc_prefix", 7); err != nil {
		t.Fatal(err)
	}
	master, err := unlockMemberMasterKey(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	metadata, err := boltstore.OpenReadOnly(cfg.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(master)
	if err != nil {
		_ = metadata.Close()
		t.Fatal(err)
	}
	key, keyErr := loadLedgerHMACKey(metadata, v, master)
	closeErr := metadata.Close()
	v.Close()
	if keyErr != nil || closeErr != nil {
		t.Fatal(keyErr, closeErr)
	}
	defer clear(key)
	state, err := replication.ReadStateWithMasterKey(cfg.ReplicationStatePath(), master)
	if err != nil {
		t.Fatal(err)
	}
	clusterKey, err := replication.DeriveClusterKey(master, state.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(clusterKey[:])
	var batch ledger.DurableBatch
	native, err := ledger.OpenWithOptions(cfg.LedgerPath(), ledger.NewStatus(), ledger.Options{
		ChainKey: key, MaxBatch: 1,
		AfterDurable: func(b ledger.DurableBatch) (uint64, error) {
			batch = b
			batch.Frames = append([]byte(nil), b.Frames...)
			return 2, nil
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, appendErr := native.Append(ctx, projectedWriterAcceptance("excluded_startup"))
	tail, _, _ := native.ChainHead()
	closeErr = native.Close()
	if appendErr != nil || closeErr != nil || tail.Sequence != baseline.Sequence+1 {
		t.Fatal(appendErr, closeErr, baseline, tail)
	}
	journal, err := replication.OpenExistingOrderingJournal(cfg.OrderingJournalPath(), clusterKey[:], state.ClusterID, state.Incarnation, state.DurableIndex, state.OrderingHeadMAC)
	if err != nil {
		t.Fatal(err)
	}
	for index := uint64(1); index <= 2; index++ {
		frame := replication.Frame{ClusterID: state.ClusterID, Incarnation: state.Incarnation, Index: index, Term: state.Term, Kind: replication.KindLeadershipEstablished}
		if index == 2 {
			frame.Kind, frame.Store = replication.KindData, replication.StoreLedger
			frame.StoreGeneration, frame.StoreSequenceFirst, frame.StoreSequenceLast = batch.Generation, batch.FirstSequence, batch.LastSequence
			frame.Payload = batch.Frames
		}
		_, digest, err := frame.MarshalBinaryAndDigest()
		if err == nil {
			_, err = journal.Append(replication.OrderingRecord{Kind: frame.Kind, Store: frame.Store, StoreGeneration: frame.StoreGeneration, StoreSequenceFirst: frame.StoreSequenceFirst, StoreSequenceLast: frame.StoreSequenceLast, Index: index, Term: frame.Term, FrameDigest: digest})
		}
		if err != nil {
			_ = journal.Close()
			t.Fatal(err)
		}
	}
	_, _, state.OrderingHeadMAC = journal.Head()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	state.DurableIndex, state.ConfirmedIndex = 2, 1
	if err := replication.WriteState(cfg.ReplicationStatePath(), state, clusterKey[:]); err != nil {
		t.Fatal(err)
	}
	return cfg, baseline, tail
}

func TestReplicaRuntimeStartupExcludesDurableTailThenAppliesActualConfirmation(t *testing.T) {
	cfg, baseline, tail := replicaStartupPrefixFixture(t)
	// Saved local derivatives must not authorize the newer native head.
	for _, path := range []string{replicaRuntimeProjectionPath(cfg), replicaRuntimeProjectionPath(cfg) + ".usage"} {
		if err := os.WriteFile(path, []byte("stale untrusted disk projection"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	r.stopBackgroundWork()
	if r.accountingState.replica == nil || r.usage.Aggregate != nil || r.usageCollector != nil || r.usage.owner != nil {
		t.Fatal("ordinary Replica Open did not select the sole disk generation")
	}
	head, _, err := r.accountingState.projection.ReadPosition(context.Background())
	stats, statsErr := r.usage.disk.ReadStats(context.Background())
	state := r.replication.publisher.Snapshot()
	// An empty native Ledger retains its physical generation, while Usage has
	// no semantic record and retains the all-zero genesis watermark.
	usageBaseline := baseline
	if baseline.Sequence == 0 {
		usageBaseline = ledger.Watermark{}
	}
	if err != nil || statsErr != nil || head != baseline || stats.Watermark != usageBaseline || stats.Metrics.ActiveRequests != 0 || state.AppliedIndex != 1 || state.ConfirmedIndex != 1 || state.DurableIndex != 2 {
		t.Fatal("ordinary startup applied unconfirmed native bytes", head, stats, state, err, statsErr)
	}
	if err := r.replication.receiver.BindPrimary("halro-0"); err != nil {
		t.Fatal(err)
	}
	if err := r.replication.receiver.Confirm(replication.CommitNotice{ClusterID: state.ClusterID, Incarnation: state.Incarnation, NodeID: "halro-0", Term: state.Term, ConfirmedIndex: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.replication.applier.ApplyConfirmed(context.Background()); err != nil {
		t.Fatal(err)
	}
	head, _, err = r.accountingState.projection.ReadPosition(context.Background())
	stats, statsErr = r.usage.disk.ReadStats(context.Background())
	state = r.replication.publisher.Snapshot()
	if err != nil || statsErr != nil || head != tail || stats.Watermark != tail || stats.Metrics.ActiveRequests != 1 || state.AppliedIndex != 2 || state.Projection.Index != 2 {
		t.Fatal("actual confirmed apply missed the selected disk readers", head, stats, state, err, statsErr)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{replicaRuntimeProjectionPath(cfg), replicaRuntimeProjectionPath(cfg) + ".usage"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("closed Replica retained its owned derivative", path, err)
		}
	}
}

func TestReplicaRuntimeStartupRefusesCorruptExcludedNativeTail(t *testing.T) {
	cfg, _, _ := replicaStartupPrefixFixture(t)
	contents, err := os.ReadFile(cfg.LedgerPath())
	if err != nil || len(contents) < 2 {
		t.Fatal(err)
	}
	contents[len(contents)-2] ^= 1
	if err := os.WriteFile(cfg.LedgerPath(), contents, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if r != nil || err == nil {
		if r != nil {
			_ = r.Close()
		}
		t.Fatal("corrupt excluded native tail yielded a usable Runtime", err)
	}
	for _, path := range []string{replicaRuntimeProjectionPath(cfg), replicaRuntimeProjectionPath(cfg) + ".usage"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("failed startup retained a partially built derivative", path, err)
		}
	}
}

func TestReplicaRuntimeStartupAppliesConfirmedSuffixAndReopens(t *testing.T) {
	cfg, _, tail := replicaStartupPrefixFixture(t)
	ctx := context.Background()
	master, err := unlockMemberMasterKey(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(master)
	state, err := replication.ReadStateWithMasterKey(cfg.ReplicationStatePath(), master)
	if err != nil {
		t.Fatal(err)
	}
	key, err := replication.DeriveClusterKey(master, state.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key[:])
	state.ConfirmedIndex = state.DurableIndex
	if err := replication.WriteState(cfg.ReplicationStatePath(), state, key[:]); err != nil {
		t.Fatal(err)
	}
	// First startup applies the already-confirmed semantic suffix. Second
	// startup rebuilds its persisted applied prefix without double counting.
	for open := 0; open < 2; open++ {
		r, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		r.stopBackgroundWork()
		head, _, readErr := r.accountingState.projection.ReadPosition(ctx)
		stats, statsErr := r.usage.disk.ReadStats(ctx)
		state = r.replication.publisher.Snapshot()
		closeErr := r.Close()
		if readErr != nil || statsErr != nil || closeErr != nil || head != tail || stats.Watermark != tail || stats.Metrics.ActiveRequests != 1 || stats.Totals.Requests != 0 || stats.Totals.Attempts != 0 || state.AppliedIndex != 2 || state.Projection.Index != 2 {
			t.Fatal("confirmed startup/reopen lost or duplicated semantic facts", open, head, stats, state, readErr, statsErr, closeErr)
		}
	}
}
