package app

import (
	"bytes"
	"context"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPagedReplicaBackupReportsAuthenticatedPrimarySuffix(t *testing.T) {
	primary := testConfig(t)
	password := []byte("correct horse battery staple")
	if err := Initialize(primary); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), primary, "admin", password); err != nil {
		t.Fatal(err)
	}
	seed, err := OpenWithOptions(context.Background(), primary, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	replicaRoot := t.TempDir()
	replica := primary
	replica.Storage.DataDir = filepath.Join(replicaRoot, "data")
	replica.Storage.MasterKey.File = filepath.Join(replicaRoot, "master.key")
	copyTestTree(t, primary.Storage.DataDir, replica.Storage.DataDir)
	copyTestRegularFile(t, primary.Storage.MasterKey.File, replica.Storage.MasterKey.File)
	primary.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-0", Listen: "127.0.0.1:9910",
		Peers: []config.ReplicationPeer{{Name: "halro-1", Address: "127.0.0.1:9911", SPKISHA256: "sha256:" + strings.Repeat("ab", 32)}},
	}
	replica.Replication = &config.Replication{
		ClusterID: "production-a", NodeID: "halro-1", Listen: "127.0.0.1:9911",
		Peers: []config.ReplicationPeer{{Name: "halro-0", Address: "127.0.0.1:9910", SPKISHA256: "sha256:" + strings.Repeat("cd", 32)}},
	}
	if err := EstablishMemberState(context.Background(), primary, replication.RolePrimary, "inc_backup_01", 1); err != nil {
		t.Fatal(err)
	}
	if err := EstablishMemberState(context.Background(), replica, replication.RoleReplica, "inc_backup_01", 1); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "replica.hmbk")
	backupKey := bytes.Repeat([]byte{0x62}, 32)
	created, err := CreateBackupSummary(context.Background(), replica, configPath, archivePath, backupKey, CreateBackupOptions{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.FormatVersion != 5 || created.CompatibilityVersion != 4 || created.Catalog.Files < 8 || created.Files != nil {
		t.Fatal(created.FormatVersion, created.CompatibilityVersion, created.Catalog)
	}
	reported, err := ReportReplicaBackupSummary(context.Background(), primary, archivePath, backupKey, "admin", password, "")
	if err != nil {
		t.Fatal(err)
	}
	if reported.BackupID != created.BackupID || reported.SourceNodeID != "halro-1" || reported.ClusterIncarnation != "inc_backup_01" {
		t.Fatalf("reported manifest=%#v", reported)
	}
	state, err := ClusterStatus(context.Background(), primary)
	if err != nil {
		t.Fatal(err)
	}
	if state.DurableIndex != 2 || state.ConfirmedIndex != 0 || state.Role != replication.RolePrimary {
		t.Fatalf("reported backup ordering state=%#v", state)
	}
}
