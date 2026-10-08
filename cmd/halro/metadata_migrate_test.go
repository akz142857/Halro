package main

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
)

// Other native members are explicit Metadata-only placeholders. These tests
// prove actual command locking/archive/format transitions, not common Runtime.
func metadataMigrationCommandFixture(t *testing.T) (config.Config, metadataMigrationArguments, *resourcebudget.Manager, string, string) {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(data, "halro.db")
	s, err := boltstore.Open(metadata)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{73}, 32)
	if _, err := s.AttachMetadataJournal(key, "actual-cli-migration"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedInstanceID("ins_cli_metadata_migration"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutProject(context.Background(), domain.Project{ID: "prj_cli_metadata", Name: "CLI", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info()
	if err != nil {
		t.Fatal(err)
	}
	journal := s.MetadataJournalPath()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	placeholder := filepath.Join(dir, "metadata-only-placeholder")
	if err := os.WriteFile(placeholder, []byte("Metadata-only test fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	a := metadataMigrationArguments{backup: filepath.Join(dir, "source.hmbk"), output: filepath.Join(dir, "candidate"), instanceID: "ins_cli_metadata_migration", keyGeneration: 1, identity: sha256.Sum256([]byte("independent-cli-test-identity"))}
	backupKey := bytes.Repeat([]byte{61}, 32)
	_, err = backup.CreatePaged(backup.CreateOptions{OutputPath: a.backup, BackupKey: backupKey, Metadata: info, LedgerFeatureEpoch: info.LedgerFeatureEpoch, MinimumLedgerReaderVersion: info.MinimumLedgerReaderVersion, Build: buildinfo.Info{Version: "test"}, MasterKeyFingerprint: "sha256:" + strings.Repeat("1", 64), PricingStateSHA256: "sha256:" + strings.Repeat("2", 64), PendingIntentSHA256: "sha256:" + strings.Repeat("3", 64), Files: []backup.SourceFile{
		{ArchivePath: "config/config.yaml", LocalPath: placeholder}, {ArchivePath: "data/metadata.db", LocalPath: metadata}, {ArchivePath: "data/metadata.journal", LocalPath: journal}, {ArchivePath: "data/ledger/ledger.wal", LocalPath: placeholder}, {ArchivePath: "data/audit/audit.log", LocalPath: placeholder},
	}})
	if err != nil {
		t.Fatal(err)
	}
	a.backupKey, a.ledgerKey, a.sourceJournalKey = filepath.Join(dir, "backup.key"), filepath.Join(dir, "ledger.key"), filepath.Join(dir, "metadata.key")
	for _, pair := range []struct {
		path  string
		value []byte
	}{{a.backupKey, backupKey}, {a.ledgerKey, key}, {a.sourceJournalKey, key}} {
		if err := os.WriteFile(pair.path, pair.value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 256 << 20}, MaxClaims: 32, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range l.Classes {
		l.Classes[i] = l.Total
	}
	l.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 32 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.Storage.DataDir = data
	cfg.Storage.MetadataFile = filepath.Base(metadata)
	cfg.ResourceBudget = &config.ResourceBudget{BackupWorkspaceDiskBytes: 16 << 20, MetadataStoreDiskBytes: 32 << 20, MetadataJournalDiskBytes: 16 << 20}
	return cfg, a, m, metadata, journal
}

func TestMetadataMigrationCommandUsesRealBackupLockAndPreservesSource(t *testing.T) {
	cfg, a, m, metadata, journal := metadataMigrationCommandFixture(t)
	oldDB, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	oldJournal, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := prepareMetadataMigrationCommand(context.Background(), cfg, a, m, &out); err != nil {
		t.Fatal(err)
	}
	var receipt metadatastate.MigrationReceipt
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Head.FormatVersion != 2 || receipt.Head.Sequence != 0 || receipt.Evidence.InstanceID != a.instanceID || receipt.Evidence.Identity != a.identity {
		t.Fatal(receipt)
	}
	key, err := hkdf.Key(sha256.New, bytes.Repeat([]byte{73}, 32), receipt.Root.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	newJournal := filepath.Join(a.output, "metadata.journal")
	head, err := metadatajournal.ProbeExactContext(context.Background(), newJournal, key, 16<<20)
	if err != nil || head != receipt.Head {
		t.Fatal(head, receipt.Head, err)
	}
	if old, err := metadatajournal.Open(newJournal, key); old != nil || !errors.Is(err, metadatajournal.ErrIncompatibleFormat) {
		t.Fatal("downgrade accepted", err)
	}
	for _, pair := range []struct {
		path  string
		value []byte
	}{{metadata, oldDB}, {journal, oldJournal}} {
		got, err := os.ReadFile(pair.path)
		if err != nil || !bytes.Equal(got, pair.value) {
			t.Fatal("command modified live source", pair.path, err)
		}
	}
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal("command did not release actual guard", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataMigrationCommandRefusesActiveDirectoryAndForeignCandidate(t *testing.T) {
	cfg, a, m, _, _ := metadataMigrationCommandFixture(t)
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot().Used
	if err := prepareMetadataMigrationCommand(context.Background(), cfg, a, m, &bytes.Buffer{}); !errors.Is(err, lock.ErrAlreadyLocked) {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(a.output, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(a.output, "foreign")
	if err := os.WriteFile(sentinel, []byte("foreign owner"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareMetadataMigrationCommand(context.Background(), cfg, a, m, &bytes.Buffer{}); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "foreign owner" || m.Snapshot().Used != before {
		t.Fatal("refusal modified foreign candidate or retained fee", err, m.Snapshot())
	}
}

func TestMetadataMigrationCommandKeyFilesAndFlagsRefuseAmbiguousInputs(t *testing.T) {
	if err := runMetadataCommand([]string{"migrate", "--identity", "00"}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing authority accepted")
	}
	if err := runMetadataCommand([]string{"downgrade"}, &bytes.Buffer{}); err == nil {
		t.Fatal("downgrade command exists")
	}
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, bytes.Repeat([]byte{7}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if key, err := metadataMigrationKeyFile(link); err == nil || key != nil {
		t.Fatal("symlink key accepted", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if key, err := metadataMigrationKeyFile(path); err == nil || key != nil {
		t.Fatal("public key accepted", err)
	}
}
