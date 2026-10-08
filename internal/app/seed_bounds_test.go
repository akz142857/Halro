package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/dirscan"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestSeedManifestBoundedDecoderRefusesBeforeGraphGrowthOrSourceRead(t *testing.T) {
	// This valid JSON array is far below the byte limit. A byte-only decoder
	// would grow a declaration slice past the count limit before rejecting.
	payload := []byte(`{"files":[` + strings.Repeat(`{},`, seedManifestFiles) + `{}` + `]}`)
	if _, err := decodeSeedManifest(payload); !errors.Is(err, errSeedLimit) {
		t.Fatal("count limit checked after graph construction", err)
	}
	for _, invalid := range []string{`{"files":[],"files":[]}`, `{"version":1,"version":1}`, `{"unknown":[]}`, `{"files":[{"unexpected":true}]}`, `{"files":[]} {}`} {
		if _, err := decodeSeedManifest([]byte(invalid)); err == nil {
			t.Fatal("ambiguous seed decoded", invalid)
		}
	}
	manifest := SeedManifest{Version: 1, ClusterID: "cluster", Files: []SeedFile{{Path: "ledger/ledger.wal", Size: 10, SHA256: strings.Repeat("a", 64)}}}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeSeedManifest(encoded)
	if err != nil || decoded.ClusterID != manifest.ClusterID || !seedFilesEqual(decoded.Files, manifest.Files) {
		t.Fatal("legacy declaration changed", decoded, err)
	}
	path := filepath.Join(t.TempDir(), "oversized-sparse-manifest")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(seedManifestBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if payload, err := readSeedManifest(context.Background(), path); !errors.Is(err, errSeedLimit) || payload != nil {
		t.Fatal("oversized regular FD read", len(payload), err)
	}
	link := filepath.Join(filepath.Dir(path), "manifest-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readSeedManifest(context.Background(), link); !errors.Is(err, errSeedLimit) {
		t.Fatal("manifest symlink read", err)
	}
	manifest.Files[0].Path = strings.Repeat("x", 4097)
	output := filepath.Join(t.TempDir(), "not-created", "seed.json")
	if err := writeSeedManifest(output, manifest); !errors.Is(err, errSeedLimit) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(output)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversized declaration created output", err)
	}
}

func TestSeedOrderingVerificationRefusesPartialTailWithoutRepairAndSharesRoot(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	cfg.Replication = &config.Replication{ClusterID: "seed-cluster", NodeID: "replica"}
	cfg.ResourceBudget.OrderingJournalDiskBytes = 8 << 20
	cfg.ResourceBudget.ReplicationCandidateRAMBytes = replication.MinPrimaryCandidateRAMBytes
	cfg.ResourceBudget.ReplicationQueueRAMBytes = replicationlimits.MinQueueRAMBytes
	cfg.ResourceBudget.ReplicationReadRAMBytes = replicationlimits.MinReadRAMBytes
	cfg.ResourceBudget.ReplicationReaderRAMBytes = replicationlimits.MinNativeReaderRAMBytes
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	path := filepath.Join(staging, replication.ClusterDirectoryName, "ordering.journal")
	key := bytes.Repeat([]byte{0x73}, 32)
	header := replication.OrderingHeader{ClusterID: "seed-cluster", Incarnation: "seed-incarnation"}
	journal, err := replication.OpenOrderingJournal(path, key, header, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	record, err := journal.Append(replication.OrderingRecord{Index: 1, Term: 1, Kind: replication.KindLeadershipEstablished, Store: replication.StoreNone, FrameDigest: sha256.Sum256([]byte("seed-leadership"))})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := SeedManifest{ClusterID: header.ClusterID, Incarnation: header.Incarnation, Index: 1, Term: 1, OrderingHeadMAC: "sha256:" + hex.EncodeToString(record.MAC[:]), Projection: replication.ProjectionState{Index: 1}}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write([]byte{0x05})
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	before := doctorTree(t, staging)
	if state, err := seedReplicaStateContext(ctx, cfg, staging, manifest, key); err == nil || !reflect.DeepEqual(state, replication.MemberState{}) {
		t.Fatal("partial approved source became Replica state", state, err)
	}
	if after := doctorTree(t, staging); !reflect.DeepEqual(before, after) {
		t.Fatal("seed verification repaired or changed approved bytes")
	}
	if snapshot := pressure.logicalBudget().Snapshot(); snapshot.Used != (resourcebudget.Cost{}) {
		t.Fatal("failed read retained a retired source credit", snapshot)
	}
}

func TestSeedSharedScopeBeforeIOAndManifestPersistentDiskAlias(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	occupied, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: cfg.ResourceBudget.RecoveryRAMBytes - resourcebudget.ClaimRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := seedCommandContext(ctx, cfg); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("seed made a new root pool", err)
	}
	if err := occupied.Release(); err != nil {
		t.Fatal(err)
	}
	ctx, permit, err := seedCommandContext(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "seed.json")
	manifest := SeedManifest{Version: 1, Files: []SeedFile{{Path: "ledger/ledger.wal", SHA256: strings.Repeat("a", 64)}}}
	if err := writeSeedManifestContext(ctx, cfg, path, manifest); err != nil {
		t.Fatal(err)
	}
	if err := permit.Release(); err != nil {
		t.Fatal(err)
	}
	snapshot := pressure.logicalBudget().Snapshot()
	if snapshot.Classes[resourcebudget.Recovery].RAM != 0 || snapshot.Classes[resourcebudget.Recovery].Disk != seedManifestBytes || snapshot.RecoveryClaims != 1 {
		t.Fatal("temporary unlink refunded published manifest alias", snapshot)
	}
	if files, err := os.ReadDir(filepath.Dir(path)); err != nil || len(files) != 1 {
		t.Fatal("temporary original name survived close/retire", files, err)
	}
	if err := writeSeedManifestContext(ctx, cfg, path, manifest); !errors.Is(err, os.ErrExist) {
		t.Fatal("existing approved manifest overwritten", err)
	}
	if after := pressure.logicalBudget().Snapshot(); after.Used != snapshot.Used || after.RecoveryClaims != 1 {
		t.Fatal("failed publication leaked its private disk claim", after)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := checkSeedTreeContext(canceled, filepath.Join(t.TempDir(), "missing"), false); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel reached directory IO", err)
	}
	deep := t.TempDir()
	for i := 0; i < seedTreeDepth+1; i++ {
		deep = filepath.Join(deep, "level")
		if err := os.Mkdir(deep, 0700); err != nil {
			t.Fatal(err)
		}
	}
	root := deep
	for i := 0; i < seedTreeDepth+1; i++ {
		root = filepath.Dir(root)
	}
	if err := checkSeedTreeContext(ctx, root, false); !errors.Is(err, dirscan.ErrDepth) {
		t.Fatal("deep tree silently truncated", err)
	}
}
