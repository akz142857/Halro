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
	foundMetadata := false
	for _, file := range manifest.Files {
		if file.Path == source.Storage.MetadataFile {
			foundMetadata = true
		}
	}
	if !foundMetadata {
		t.Fatalf("seed manifest does not authenticate configured metadata file %q", source.Storage.MetadataFile)
	}

	targetRoot := t.TempDir()
	target := source
	target.Storage.DataDir = filepath.Join(targetRoot, "data")
	target.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-1", Listen: "127.0.0.1:9911",
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: "127.0.0.1:9910", SPKISHA256: "sha256:" + strings.Repeat("cd", 32)}},
	}
	approvedCopy := filepath.Join(targetRoot, "approved-copy")
	if err := os.MkdirAll(approvedCopy, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range manifest.Files {
		copyTestRegularFile(t, filepath.Join(source.Storage.DataDir, filepath.FromSlash(file.Path)),
			filepath.Join(approvedCopy, filepath.FromSlash(file.Path)))
	}
	approval, err := VerifySeedApprovalSnapshot(context.Background(), target, approvedCopy, manifestPath)
	if err != nil || approval.Status != "seed_manifest_mac_files_ordering_verified" || approval.Index != manifest.Index ||
		approval.ManifestSHA256 == "" || approval.Ordering.VerifiedLastIndex != manifest.Index || approval.ApprovedFileCount != len(manifest.Files) {
		t.Fatalf("read-only seed approval=%+v err=%v", approval, err)
	}
	extra := filepath.Join(approvedCopy, "unapproved-file")
	if err := os.WriteFile(extra, []byte("extra"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySeedApprovalSnapshot(context.Background(), target, approvedCopy, manifestPath); err == nil {
		t.Fatal("seed approval accepted an extra unapproved file")
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(approvedCopy, source.Storage.MetadataFile)
	if err := os.WriteFile(metadata, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySeedApprovalSnapshot(context.Background(), target, approvedCopy, manifestPath); err == nil {
		t.Fatal("seed approval accepted changed authoritative bytes")
	}
	copyTestRegularFile(t, filepath.Join(source.Storage.DataDir, source.Storage.MetadataFile), metadata)
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
	masterKey, err = unlockMemberMasterKey(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	_, clusterKey, err = readMemberState(target, masterKey)
	clear(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replication.MigrateStateTransitionJournal(target.ReplicationStatePath(), clusterKey[:]); err != nil {
		t.Fatal(err)
	}
	clear(clusterKey[:])
	frozenTarget := filepath.Join(targetRoot, "frozen-target")
	copyTestTree(t, target.Storage.DataDir, frozenTarget)
	origin, err := VerifyMemberSeedOrigin(context.Background(), target, frozenTarget, approvedCopy, manifestPath)
	if err != nil || origin.Status != "replacement_seed_origin_mac_verified" || origin.Member.NodeID != "halro-1" ||
		origin.Approval.Index != 1 || origin.TargetOrdering.ExpectedIndex != 1 {
		t.Fatalf("replacement member seed origin=%+v err=%v", origin, err)
	}
	wrongTarget := target
	wrongReplication := *target.Replication
	wrongReplication.Peers = append([]config.ReplicationPeer(nil), target.Replication.Peers...)
	wrongReplication.Peers[0].SPKISHA256 = "sha256:" + strings.Repeat("ef", 32)
	wrongTarget.Replication = &wrongReplication
	if _, err := VerifyMemberSeedOrigin(context.Background(), wrongTarget, frozenTarget, approvedCopy, manifestPath); err == nil {
		t.Fatal("replacement journal accepted a different seeded peer identity")
	}
	masterKey, err = unlockMemberMasterKey(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	_, clusterKey, err = readMemberState(source, masterKey)
	clear(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replication.MigrateStateTransitionJournal(source.ReplicationStatePath(), clusterKey[:]); err != nil {
		t.Fatal(err)
	}
	clear(clusterKey[:])
	frozenSource := filepath.Join(targetRoot, "frozen-source")
	copyTestTree(t, source.Storage.DataDir, frozenSource)
	sourceOrigin, err := VerifySourceSeedOrigin(context.Background(), source, frozenSource, target, approvedCopy, manifestPath)
	if err != nil || sourceOrigin.Status != "source_primary_seed_files_mac_verified" ||
		sourceOrigin.SourceMember.NodeID != "halro-0" || sourceOrigin.SourceOrdering.FileSHA256 != approval.Ordering.FileSHA256 {
		t.Fatalf("approved source Primary origin=%+v err=%v", sourceOrigin, err)
	}
	if err := os.WriteFile(filepath.Join(frozenSource, "metadata.journal"), []byte("different source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySourceSeedOrigin(context.Background(), source, frozenSource, target, approvedCopy, manifestPath); err == nil {
		t.Fatal("source proof accepted bytes different from approved seed")
	}
}

func TestSeedApprovalAllowsTheInitialZeroOrderingIndex(t *testing.T) {
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
		ClusterID: "production-zero", NodeID: "halro-0", Listen: "127.0.0.1:9910",
		Peers: []config.ReplicationPeer{{Name: "halro-1", Address: "127.0.0.1:9911", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}},
	}
	if err := EstablishMemberState(context.Background(), source, replication.RolePrimary, "inc_seed_zero", 1); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "halro-1.seed.json")
	manifest, err := CreateSeedManifest(context.Background(), source, "halro-1", manifestPath, "admin", password, "")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Index != 0 || manifest.Projection.Index != 0 {
		t.Fatalf("initial seed index=%d projection=%d, want 0/0", manifest.Index, manifest.Projection.Index)
	}

	targetRoot := t.TempDir()
	target := source
	target.Storage.DataDir = filepath.Join(targetRoot, "data")
	target.Replication = &config.Replication{
		ClusterID: "production-zero", NodeID: "halro-1", Listen: "127.0.0.1:9911",
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: "127.0.0.1:9910", SPKISHA256: "sha256:" + strings.Repeat("cd", 32)}},
	}
	staging := filepath.Join(targetRoot, ".seed-staging")
	copyTestTree(t, source.Storage.DataDir, staging)
	if err := os.Remove(filepath.Join(staging, replication.ClusterDirectoryName, "state.json")); err != nil {
		t.Fatal(err)
	}
	seeded, err := InstallSeedSnapshot(context.Background(), target, staging, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if seeded.DurableIndex != 0 || seeded.ConfirmedIndex != 0 || seeded.AppliedIndex != 0 {
		t.Fatalf("initial seeded state=%#v", seeded)
	}
}

func TestSeedInstallRejectsSymlinkStagingRoot(t *testing.T) {
	targetRoot := t.TempDir()
	target := testConfig(t)
	target.Storage.DataDir = filepath.Join(targetRoot, "data")
	target.Replication = &config.Replication{ClusterID: "production-a", NodeID: "halro-1"}
	realStaging := filepath.Join(t.TempDir(), "real-staging")
	if err := os.MkdirAll(realStaging, 0o700); err != nil {
		t.Fatal(err)
	}
	stagingLink := filepath.Join(targetRoot, ".seed-staging")
	if err := os.Symlink(realStaging, stagingLink); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallSeedSnapshot(context.Background(), target, stagingLink, filepath.Join(t.TempDir(), "manifest.json")); err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("symlink staging error=%v", err)
	}
}

func TestSeedInstallRejectsSymlinkPublicationPathComponent(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real-parent")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(linkedParent, ".seed-staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	target := testConfig(t)
	target.Storage.DataDir = filepath.Join(linkedParent, "data")
	target.Replication = &config.Replication{ClusterID: "production-a", NodeID: "halro-1"}
	if _, err := InstallSeedSnapshot(context.Background(), target, staging, filepath.Join(t.TempDir(), "manifest.json")); err == nil || !strings.Contains(err.Error(), "publication directory") {
		t.Fatalf("symlink publication path error=%v", err)
	}
}

func TestSeedTreeRejectsNestedSymlinkAndForcesExactPrivateModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "seed")
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(nested, "store.log")
	if err := os.WriteFile(filePath, []byte("authenticated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syncSeedTree(root); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{root: 0o700, nested: 0o700, filePath: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("mode %s=%#o want=%#o", path, info.Mode().Perm(), want)
		}
	}
	linked := filepath.Join(nested, "linked.log")
	if err := os.Symlink(filePath, linked); err != nil {
		t.Fatal(err)
	}
	if err := syncSeedTree(root); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("nested symlink error=%v", err)
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
