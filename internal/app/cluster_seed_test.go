package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

func TestSeedApprovalAuthenticatesStagingBeforeAtomicReplicaPublication(t *testing.T) {
	source := testConfig(t)
	password := []byte("correct horse battery staple")
	if err := Initialize(source); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), source, "admin", password); err != nil {
		t.Fatal(err)
	}
	runtime, err := OpenWithOptions(context.Background(), source, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	source.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-0", Listen: "127.0.0.1:9910",
		Peers: []config.ReplicationPeer{{Name: "halro-1", Address: "127.0.0.1:9911", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}},
	}
	if err := EstablishMemberState(context.Background(), source, replication.RolePrimary, "inc_seed_01", 1); err != nil {
		t.Fatal(err)
	}
	masterKey, err := unlockMemberMasterKey(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	state, clusterKey, err := readMemberState(source, masterKey)
	clear(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := replication.NewStatePublisher(source.ReplicationStatePath(), clusterKey[:], state)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := replication.OpenExistingOrderingJournal(source.OrderingJournalPath(), clusterKey[:], state.ClusterID, state.Incarnation, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	outbound, err := replication.NewConnectionOutbound([]string{"halro-1"}, seedPeerSender{})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := replication.NewPrimaryCoordinator(state.ClusterID, state.Incarnation, state.NodeID, state.Term, 0, []string{"halro-1"}, journal, outbound, publisher.PublishPrimary)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := coordinator.RecordDurable(replication.Frame{Kind: replication.KindLeadershipEstablished, Store: replication.StoreNone})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Acknowledge(replication.Acknowledgement{
		ClusterID: state.ClusterID, Incarnation: state.Incarnation, NodeID: "halro-1", Term: state.Term,
		Index: commit.Index, DurableIndex: commit.Index,
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	publisher.Close()
	clear(clusterKey[:])
	if err := replication.PersistProviderObjectSource(
		filepath.Join(source.ClusterDirectoryPath(), "provider-object-sources"),
		"seeded.content", []byte("sealed-seed-object"),
	); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "halro-1.seed.json")
	manifest, err := CreateSeedManifest(context.Background(), source, "halro-1", manifestPath, "admin", password, "")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.TargetNode != "halro-1" || manifest.Index != 1 || len(manifest.Files) == 0 || manifest.MAC == "" || manifest.OrderingHeadMAC == "" {
		t.Fatalf("seed manifest=%#v", manifest)
	}

	targetRoot := t.TempDir()
	target := source
	target.Storage.DataDir = filepath.Join(targetRoot, "data")
	target.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-1", Listen: "127.0.0.1:9911",
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: "127.0.0.1:9910", SPKISHA256: "sha256:" + strings.Repeat("cd", 32)}},
	}
	staging := filepath.Join(targetRoot, ".seed-staging")
	copyTestTree(t, source.Storage.DataDir, staging)
	if err := os.Remove(filepath.Join(staging, replication.ClusterDirectoryName, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "metadata.journal"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallSeedSnapshot(context.Background(), target, staging, manifestPath); err == nil || !strings.Contains(err.Error(), "approved manifest") {
		t.Fatalf("tampered seed install error=%v", err)
	}
	if _, err := os.Stat(target.Storage.DataDir); !os.IsNotExist(err) {
		t.Fatalf("failed seed changed target data directory: %v", err)
	}
	if err := os.RemoveAll(staging); err != nil {
		t.Fatal(err)
	}
	copyTestTree(t, source.Storage.DataDir, staging)
	if err := os.Remove(filepath.Join(staging, replication.ClusterDirectoryName, "state.json")); err != nil {
		t.Fatal(err)
	}
	seededState, err := InstallSeedSnapshot(context.Background(), target, staging, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if seededState.Role != replication.RoleReplica || seededState.NodeID != "halro-1" || seededState.Incarnation != "inc_seed_01" || seededState.Term != 1 ||
		seededState.DurableIndex != 1 || seededState.ConfirmedIndex != 1 || seededState.AppliedIndex != 1 {
		t.Fatalf("seeded state=%#v", seededState)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging survived atomic publication: %v", err)
	}
	objectSource, err := os.ReadFile(filepath.Join(target.ClusterDirectoryPath(), "provider-object-sources", "seeded.content"))
	if err != nil || string(objectSource) != "sealed-seed-object" {
		t.Fatalf("seeded provider-object source=%q err=%v", objectSource, err)
	}
}

type seedPeerSender struct{}

func (seedPeerSender) Send(string, []byte) error { return nil }

func TestSeedManifestDecoderRejectsTrailingJSON(t *testing.T) {
	decoder := json.NewDecoder(strings.NewReader(`{} {}`))
	var first map[string]any
	if err := decoder.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if err := requireJSONEOF(decoder); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("trailing JSON error=%v", err)
	}
}
