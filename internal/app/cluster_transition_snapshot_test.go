package app

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

func TestVerifyMemberTransitionSnapshotUsesFrozenConfiguredMember(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	standalone, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{ClusterID: "production-a", NodeID: "halro-0", Listen: "127.0.0.1:9910",
		Peers: []config.ReplicationPeer{{Name: "halro-1", Address: "127.0.0.1:9911", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}}}
	if err := EstablishMemberState(context.Background(), cfg, replication.RolePrimary, "inc_01", 1); err != nil {
		t.Fatal(err)
	}
	masterKey, err := unlockMemberMasterKey(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	state, clusterKey, err := readMemberState(cfg, masterKey)
	clear(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != replication.StateVersion {
		t.Fatalf("unexpected initial state version %d", state.Version)
	}
	migrated, err := replication.MigrateStateTransitionJournal(cfg.ReplicationStatePath(), clusterKey[:])
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := replication.NewDurableStatePublisher(cfg.ReplicationStatePath(), clusterKey[:], migrated)
	if err != nil {
		t.Fatal(err)
	}
	var metrics bytes.Buffer
	writer := bufio.NewWriter(&metrics)
	(&Runtime{replication: &replicationRuntime{publisher: publisher}}).writeReplicationMetrics(writer)
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	assertMetricsExpositionContract(t, metrics.String())
	for _, expected := range []string{
		"halro_replication_member_state_version 3",
		"halro_replication_transition_journal_capacity_readable 1",
		"halro_replication_transition_journal_segments 1",
		"halro_replication_transition_journal_bytes ",
	} {
		if !strings.Contains(metrics.String(), expected) {
			t.Fatalf("version-3 journal capacity metric %q missing", expected)
		}
	}
	publisher.Close()
	clear(clusterKey[:])
	snapshotDir := filepath.Join(t.TempDir(), "member-snapshot")
	clusterDir := filepath.Join(snapshotDir, "cluster")
	if err := os.MkdirAll(clusterDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state.json", "transitions.journal"} {
		data, err := os.ReadFile(filepath.Join(cfg.ClusterDirectoryPath(), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(clusterDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(clusterDir, "transitions.journal"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := VerifyMemberTransitionSnapshot(context.Background(), cfg, snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "member_mac_verified" || report.NodeID != "halro-0" || report.BaselineKind != "legacy_baseline" || len(report.Files) != 2 {
		t.Fatalf("snapshot report=%+v", report)
	}
	pageReport, page, err := ReadMemberTransitionSnapshotPage(context.Background(), cfg, snapshotDir,
		replication.DurableTransitionCursor{JournalID: report.JournalID, Digest: report.BaselineDigest})
	if err != nil || pageReport.InventorySHA256 != report.InventorySHA256 || page.CommittedSequence != 0 || len(page.Events) != 0 {
		t.Fatalf("authenticated snapshot page=%+v report=%+v err=%v", page, pageReport, err)
	}
	archivalCfg := cfg
	archivalCfg.Storage.DataDir = filepath.Join(t.TempDir(), "original-member-not-mounted")
	if archived, err := VerifyMemberTransitionSnapshot(context.Background(), archivalCfg, snapshotDir); err != nil || archived.InventorySHA256 != report.InventorySHA256 {
		t.Fatalf("archived member copy could not be verified away from original data directory: %+v err=%v", archived, err)
	}
	readbackDir := filepath.Join(t.TempDir(), "retrieved-member")
	readbackCluster := filepath.Join(readbackDir, "cluster")
	if err := os.MkdirAll(readbackCluster, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range report.Files {
		data, err := os.ReadFile(filepath.Join(clusterDir, file.Name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(readbackCluster, file.Name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readback, err := VerifyMemberTransitionSnapshotReadback(context.Background(), cfg, snapshotDir, readbackDir)
	if err != nil || readback.Status != "member_readback_mac_and_bytes_match_local_only" || readback.InventorySHA256 != report.InventorySHA256 {
		t.Fatalf("valid member archive readback rejected: %+v err=%v", readback, err)
	}
	if _, err := VerifyMemberTransitionSnapshotReadback(context.Background(), cfg, snapshotDir, snapshotDir); err == nil {
		t.Fatal("source directory accepted as its own archive readback")
	}
	readbackJournal := filepath.Join(readbackCluster, "transitions.journal")
	if err := os.Remove(readbackJournal); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyMemberTransitionSnapshotReadback(context.Background(), cfg, snapshotDir, readbackDir); err == nil {
		t.Fatal("missing retrieved journal passed member archive readback")
	}
	if err := os.Link(filepath.Join(clusterDir, "transitions.journal"), readbackJournal); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyMemberTransitionSnapshotReadback(context.Background(), cfg, snapshotDir, readbackDir); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("shared member journal was not rejected: %v", err)
	}
	if err := os.Remove(readbackJournal); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readbackJournal, append(append([]byte(nil), before...), ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyMemberTransitionSnapshotReadback(context.Background(), cfg, snapshotDir, readbackDir); err == nil {
		t.Fatal("modified retrieved journal passed member archive readback")
	}
	after, err := os.ReadFile(filepath.Join(clusterDir, "transitions.journal"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("verification modified snapshot journal: %v", err)
	}
	if _, err := VerifyMemberTransitionSnapshot(context.Background(), cfg, cfg.Storage.DataDir); err == nil || !strings.Contains(err.Error(), "separate") {
		t.Fatalf("live data directory accepted: %v", err)
	}
	wrong := cfg
	replicationConfig := *cfg.Replication
	replicationConfig.NodeID = "halro-other"
	wrong.Replication = &replicationConfig
	if _, err := VerifyMemberTransitionSnapshot(context.Background(), wrong, snapshotDir); err == nil {
		t.Fatal("snapshot accepted mismatched configured node")
	}
	before[20] ^= 1
	if err := os.WriteFile(filepath.Join(clusterDir, "transitions.journal"), before, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyMemberTransitionSnapshot(context.Background(), cfg, snapshotDir); err == nil {
		t.Fatal("snapshot accepted tampered member journal")
	}
}
