package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

func legacySeedMemberFixture(t *testing.T, role replication.Role) config.Config {
	t.Helper()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	r, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Replication = &config.Replication{ClusterID: "seed-authority-cluster", NodeID: "source", Peers: []config.ReplicationPeer{{Name: "target", Address: "127.0.0.1:9911", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}}}
	if err := EstablishMemberState(context.Background(), cfg, role, "inc_seed_authority", 1); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func seedAuthorityReservedNames() []string {
	names := []string{"memory-state", "memory-state.preparing", "memory-state.bootstrap", "ha-metadata-baseline", "ha-metadata-baseline.preparing", "ha-memory-state", "ha-memory-state.bootstrap", "ha-memory-state.bootstrap.pending"}
	for i := 0; i < 4; i++ {
		names = append(names, fmt.Sprintf("ha-metadata-baseline.unqualified-%d", i))
		names = append(names, fmt.Sprintf("ha-metadata-baseline.source-unqualified-%d", i))
		names = append(names, fmt.Sprintf("ha-memory-state.unqualified-%d", i))
	}
	for i := 0; i < migrationInterruptedSlots; i++ {
		names = append(names, fmt.Sprintf("memory-state.interrupted-%d", i), fmt.Sprintf("memory-state.unqualified-%d", i))
	}
	return names
}

func TestLegacySeedRejectsUnqualifiedMemoryStateBeforePublication(t *testing.T) {
	ctx := context.Background()
	source := legacySeedMemberFixture(t, replication.RolePrimary)
	manifestPath := filepath.Join(t.TempDir(), "seed.json")
	manifest, err := CreateSeedManifest(ctx, source, "target", manifestPath, "admin", []byte("correct horse battery staple"), "")
	if err != nil || manifest.Version != 1 {
		t.Fatal(manifest, err)
	}
	targetRoot := t.TempDir()
	target := source
	target.Storage.DataDir = filepath.Join(targetRoot, "data")
	target.Replication = &config.Replication{ClusterID: source.Replication.ClusterID, NodeID: "target", Peers: []config.ReplicationPeer{{Name: "source", Address: "127.0.0.1:9910", SPKISHA256: "sha256:" + strings.Repeat("cd", 32)}}}
	before, err := os.ReadFile(source.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"cluster", "target", "source", "incarnation", "version"} {
		t.Run("approved-context-"+kind, func(t *testing.T) {
			bad := manifest
			switch kind {
			case "cluster":
				bad.ClusterID = "foreign-cluster"
			case "target":
				bad.TargetNode = "foreign-member"
			case "source":
				bad.SourceNode = "unconfigured-source"
			case "incarnation":
				bad.Incarnation = "inc_foreign_seed"
			case "version":
				bad.Version = 2 // a new authority schema cannot enter the legacy path
			}
			master, err := unlockMemberMasterKey(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			key, err := replication.DeriveClusterKey(master, bad.Incarnation)
			clear(master)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(key[:])
			mac, err := seedManifestMAC(bad, key[:])
			if err != nil {
				t.Fatal(err)
			}
			bad.MAC = fmt.Sprintf("sha256:%x", mac)
			path := filepath.Join(t.TempDir(), "wrong-context.seed.json")
			if err := writeSeedManifest(path, bad); err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(targetRoot, ".wrong-context-"+kind)
			copyTestTree(t, source.Storage.DataDir, stage)
			if err := os.Remove(filepath.Join(stage, "cluster", "state.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := InstallSeedSnapshot(ctx, target, stage, path); err == nil {
				t.Fatal("valid MAC authorized mismatched seed context")
			}
			if _, err := os.Lstat(target.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("mismatched context published target", err)
			}
			if _, err := os.Lstat(filepath.Join(stage, "cluster", "state.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("mismatched context manufactured Replica state", err)
			}
			if got, err := os.ReadFile(filepath.Join(stage, source.Storage.MetadataFile)); err != nil || !bytes.Equal(got, before) {
				t.Fatal("mismatched context altered approved Metadata", err)
			}
		})
	}
	for _, name := range seedAuthorityReservedNames() {
		t.Run(name, func(t *testing.T) {
			candidate := filepath.Join(source.Storage.DataDir, name)
			unknown := []byte("unqualified candidate; its header is not a seed grant")
			if err := os.WriteFile(candidate, unknown, 0600); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(candidate)
			if _, err := hashSeedFilesContext(ctx, source.Storage.DataDir, source.Storage.MetadataFile); !errors.Is(err, errSeedMemoryStateAuthority) {
				t.Fatalf("omitted candidate was hash-qualified: %v", err)
			}
			output := filepath.Join(t.TempDir(), "unqualified.seed.json")
			if _, err := CreateSeedManifest(ctx, source, "target", output, "admin", []byte("wrong password"), ""); !errors.Is(err, errSeedMemoryStateAuthority) {
				t.Fatalf("candidate reached operator/native open: %v", err)
			}
			if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused seed emitted approval: %v", err)
			}
			stage := filepath.Join(targetRoot, ".staging-"+name)
			copyTestTree(t, source.Storage.DataDir, stage)
			if err := os.Remove(filepath.Join(stage, "cluster", "state.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := InstallSeedSnapshot(ctx, target, stage, manifestPath); !errors.Is(err, errSeedMemoryStateAuthority) {
				t.Fatalf("approved legacy file hashes qualified candidate: %v", err)
			}
			if _, err := os.Lstat(target.Storage.DataDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused seed published target: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(stage, "cluster", "state.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused seed manufactured Replica state: %v", err)
			}
			for _, path := range []string{candidate, filepath.Join(stage, name)} {
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, unknown) {
					t.Fatalf("candidate changed or deleted: %q %v", got, err)
				}
			}
		})
	}
	if after, err := os.ReadFile(source.MetadataPath()); err != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal modified source Metadata", err)
	}
	// Existing Format1 seed still has a real consumer and atomic publication.
	stage := filepath.Join(targetRoot, ".legacy-valid")
	copyTestTree(t, source.Storage.DataDir, stage)
	if err := os.Remove(filepath.Join(stage, "cluster", "state.json")); err != nil {
		t.Fatal(err)
	}
	state, err := InstallSeedSnapshot(ctx, target, stage, manifestPath)
	if err != nil || state.Role != replication.RoleReplica || state.Incarnation != manifest.Incarnation {
		t.Fatal(state, err)
	}
}

func TestReplicaBackupRejectsWrongMemberConfigurationAndMemoryState(t *testing.T) {
	ctx := context.Background()
	cfg := legacySeedMemberFixture(t, replication.RoleReplica)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := make(map[string][]byte)
	for _, path := range []string{cfg.MetadataPath(), filepathMetadataJournal(cfg), cfg.LedgerPath(), cfg.AuditPath(), cfg.GovernancePath(), cfg.ReplicationStatePath(), cfg.OrderingJournalPath()} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	for _, kind := range []string{"cluster", "node", "peers", "memory-state"} {
		t.Run(kind, func(t *testing.T) {
			bad := cfg
			member := *cfg.Replication
			member.Peers = append([]config.ReplicationPeer(nil), member.Peers...)
			bad.Replication = &member
			want := ""
			switch kind {
			case "cluster":
				member.ClusterID = "foreign-cluster"
				want = "cluster_id"
			case "node":
				member.NodeID = "foreign-member"
				want = "node_id"
			case "peers":
				member.Peers[0].SPKISHA256 = "sha256:" + strings.Repeat("ef", 32)
				want = "replication.peers"
			case "memory-state":
				path := memoryStateBootstrapPath(cfg)
				if err := os.WriteFile(path, []byte("unqualified header"), 0600); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(path)
			}
			output := filepath.Join(t.TempDir(), "refused.hmbk")
			_, err := CreateBackupSummary(ctx, bad, configPath, output, bytes.Repeat([]byte{0x62}, 32), CreateBackupOptions{Replica: true})
			if kind == "memory-state" {
				if !errors.Is(err, errSeedMemoryStateAuthority) {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("wrong %s accepted: %v", kind, err)
			}
			if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refused source emitted backup", err)
			}
			for path, saved := range before {
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, saved) {
					t.Fatalf("refusal mutated %s: %v", path, err)
				}
			}
		})
	}
}
