package metadatastate

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// This fixture exercises actual legacy Metadata transactions and native fmt1,
// then a verified Metadata backup and fmt2 migration. Its placeholder Ledger/
// Audit archive members are not a whole-Runtime acceptance claim.
func actualLegacyMetadataArchive(t *testing.T, dir string, alter ...func(*backup.CreateOptions)) (*backup.VerifiedArchive, string) {
	t.Helper()
	legacyDir := filepath.Join(dir, "legacy")
	if err := os.Mkdir(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(legacyDir, "halro.db")
	s, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachMetadataJournal(metadataTestKey(), "legacy-format1", 16<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedInstanceID("ins_baseline_test"); err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: "prj_legacy", Name: "Legacy project", Enabled: true, CreatedAt: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
	if _, err := s.PutProject(context.Background(), project, 0, nil); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info()
	if err != nil {
		t.Fatal(err)
	}
	journalPath := s.MetadataJournalPath()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	native, err := metadatajournal.Open(journalPath, metadataTestKey())
	if err != nil {
		t.Fatal(err)
	}
	if head := native.Head(); head.FormatVersion != 1 || head.Sequence != info.MetadataJournalSequence || head.Epoch != info.MetadataJournalEpoch || head.Sequence < 2 {
		t.Fatal("legacy native source disagrees with DB", head, info)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	placeholder := filepath.Join(dir, "metadata-only-native-placeholder")
	if err := os.WriteFile(placeholder, []byte("this fixture does not accept Ledger or Audit"), 0600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{31}, 32)
	archivePath := filepath.Join(dir, "legacy.hmbk")
	create := backup.CreateOptions{OutputPath: archivePath, BackupKey: key, Metadata: info, LedgerFeatureEpoch: 4, MinimumLedgerReaderVersion: "v4", Build: buildinfo.Info{Version: "metadata-migration-test"}, MasterKeyFingerprint: "sha256:" + strings.Repeat("0", 64), PricingStateSHA256: "sha256:" + strings.Repeat("1", 64), PendingIntentSHA256: "sha256:" + strings.Repeat("2", 64), Files: []backup.SourceFile{
		{ArchivePath: "config/config.yaml", LocalPath: placeholder},
		{ArchivePath: "data/metadata.db", LocalPath: path},
		{ArchivePath: "data/metadata.journal", LocalPath: journalPath},
		{ArchivePath: "data/ledger/ledger.wal", LocalPath: placeholder},
		{ArchivePath: "data/audit/audit.log", LocalPath: placeholder},
	}}
	for _, mutate := range alter {
		mutate(&create)
	}
	_, err = backup.CreatePaged(create)
	if err != nil {
		t.Fatal(err)
	}
	h, err := backup.OpenVerified(context.Background(), archivePath, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	return h, journalPath
}

type metadataMigrationSelection struct {
	Evidence BaselineEvidence
	Root     authenticatedstate.RootIdentity
	Saved    metadatajournal.Head
	Selected AuthenticatedMetadataSealReference
}

func TestMetadataMigrationExitAfterActualNativeSyncHelper(t *testing.T) {
	dir := os.Getenv("HALRO_METADATA_MIGRATION_EXIT_DIR")
	if dir == "" {
		t.Skip("subprocess-only")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	archive, legacyPath := actualLegacyMetadataArchive(t, dir)
	o := baselineOptions(t)
	b, err := BuildFromBackup(context.Background(), archive, metadataTestKey(), filepath.Join(dir, "source.db"), filepath.Join(dir, "root"), o)
	if err != nil {
		t.Fatal(err)
	}
	epoch := archive.Summary().Metadata.MetadataJournalEpoch + 1
	if _, err := b.StartNewEpoch(context.Background(), metadataTestKey(), filepath.Join(dir, "metadata.journal"), epoch, 16<<20); err != nil {
		t.Fatal(err)
	}
	selected, err := b.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.store.RootIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	selection := metadataMigrationSelection{Evidence: b.Evidence(), Root: root, Saved: b.position, Selected: selected}
	raw, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "independent-test-selection.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	legacy, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old-native-before"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(domain.Project{ID: "prj_after_migration", Name: "Durable tail", Enabled: true, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.appendFrame(context.Background(), []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("prj_after_migration"), Value: value}}, func(phase string) {
		if phase == "native-durable" {
			os.Exit(23)
		}
	})
	t.Fatalf("did not exit at actual durable boundary: %v", err)
}

func TestMetadataMigrationActualFormat1BackupResumesAfterNativeSyncProcessExit(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMetadataMigrationExitAfterActualNativeSyncHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HALRO_METADATA_MIGRATION_EXIT_DIR="+dir)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("publisher exit=%v output=%s", err, output)
	}
	var selected metadataMigrationSelection
	raw, err := os.ReadFile(filepath.Join(dir, "independent-test-selection.json"))
	if err != nil || json.Unmarshal(raw, &selected) != nil {
		t.Fatal(err)
	}
	key, err := hkdf.Key(sha256.New, metadataTestKey(), selected.Root.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	journalPath := filepath.Join(dir, "metadata.journal")
	// The test harness supplies an independently accepted saved root and
	// Standalone policy permits the actual authenticated durable tail. Reading
	// this test-selection JSON alone grants no application-level selection.
	native, err := metadatajournal.OpenBoundContext(context.Background(), journalPath, key, selected.Saved.Baseline, selected.Saved.FileRevision, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	head := native.Head()
	if head.Sequence != 1 || head.Epoch != selected.Saved.Epoch {
		t.Fatal(head)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if legacy, err := metadatajournal.Open(journalPath, key); err == nil || legacy != nil {
		t.Fatal("old reader accepted format2", err)
	}
	after, err := os.ReadFile(journalPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("incompatible reader repaired new file", err)
	}
	o := baselineOptions(t)
	rc, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Release()
	jc, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer jc.Release()
	options := OpenOptions{BaselineOptions: o, Evidence: selected.Evidence, SavedRoot: selected.Root, SelectedSealRoot: &selected.Selected.Root, SavedHead: selected.Saved, NativeHead: head, PermittedHead: head, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: 16 << 20}
	next, receipt, err := OpenFromRoot(context.Background(), metadataTestKey(), filepath.Join(dir, "root"), journalPath, options)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if receipt.AppliedFrames != 1 || receipt.From != selected.Saved || receipt.To != head {
		t.Fatal(receipt)
	}
	ref, err := next.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backend, err := next.StandaloneFacade(context.Background(), o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, ref)
	if err != nil {
		t.Fatal(err)
	}
	s, err := boltstore.NewFacadeStore(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"prj_legacy", "prj_after_migration"} {
		p, err := s.GetProject(context.Background(), id)
		if err != nil || p.ID != id || p.Revision != 1 {
			t.Fatal(p, err)
		}
	}
	if _, err := s.PutProject(context.Background(), domain.Project{ID: "prj_resume", Name: "New after recovery", Enabled: true}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if position, err := s.ReadMetadataAppliedPosition(context.Background()); err != nil || position.Sequence != 2 {
		t.Fatal(position, err)
	}
	oldBefore, err := os.ReadFile(filepath.Join(dir, "old-native-before"))
	if err != nil {
		t.Fatal(err)
	}
	oldAfter, err := os.ReadFile(filepath.Join(dir, "legacy", boltstore.MetadataJournalFileName))
	if err != nil || !bytes.Equal(oldBefore, oldAfter) {
		t.Fatal("migration erased/rewrote old epoch", err)
	}
}
