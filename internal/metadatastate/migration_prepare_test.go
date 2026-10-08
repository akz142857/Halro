package metadatastate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestMetadataMigrationPreparationUsesExactFormat1BackupAndActualLock(t *testing.T) {
	dir := t.TempDir()
	archive, source := actualLegacyMetadataArchive(t, dir)
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(filepath.Dir(source))
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	workspace := filepath.Join(dir, "migration")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	o := baselineOptions(t)
	p, err := PrepareFormat2FromBackup(context.Background(), archive, metadataTestKey(), metadataTestKey(), filepath.Dir(source), workspace, guard, o, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
	}()
	if p.Receipt.Version != 1 || p.Receipt.Head.FormatVersion != 2 || p.Receipt.Head.Sequence != 0 || p.Receipt.Head.Epoch != archive.Summary().Metadata.MetadataJournalEpoch+1 || p.Receipt.Reference.Root.State.Digest != p.Receipt.Root.Digest {
		t.Fatal(p.Receipt)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("migration touched legacy native", err)
	}
	if l, err := metadatajournal.Open(filepath.Join(workspace, "metadata.journal"), metadataTestKey()); err == nil || l != nil {
		t.Fatal("old reader accepted candidate", err)
	}
}

func TestMetadataMigrationPreparationRejectsForeignGuardAndWrongSourceKeyBeforeCandidate(t *testing.T) {
	dir := t.TempDir()
	archive, source := actualLegacyMetadataArchive(t, dir)
	guard, err := lock.Acquire(filepath.Dir(source))
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	for _, wrongGuard := range []bool{false, true} {
		workspace := t.TempDir()
		if err := os.Chmod(workspace, 0700); err != nil {
			t.Fatal(err)
		}
		o := baselineOptions(t)
		dataDir, key := filepath.Dir(source), bytes.Repeat([]byte{44}, 32)
		if wrongGuard {
			dataDir, key = workspace, metadataTestKey()
		}
		before := o.Budget.Snapshot().Used
		p, err := PrepareFormat2FromBackup(context.Background(), archive, metadataTestKey(), key, dataDir, workspace, guard, o, 16<<20)
		if err == nil || p != nil || wrongGuard && !errors.Is(err, ErrSource) || !wrongGuard && !errors.Is(err, metadatajournal.ErrCorrupt) {
			t.Fatal(p, err)
		}
		if before != o.Budget.Snapshot().Used {
			t.Fatal("refusal leaked fees")
		}
		if _, err := os.Stat(filepath.Join(workspace, "metadata-root")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refusal created candidate", err)
		}
	}
}

func TestMetadataMigrationPreparationRejectsAuthenticatedBackupWithWrongAppliedCut(t *testing.T) {
	dir := t.TempDir()
	archive, source := actualLegacyMetadataArchive(t, dir, func(o *backup.CreateOptions) { o.Metadata.MetadataJournalSequence++ })
	guard, err := lock.Acquire(filepath.Dir(source))
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	o := baselineOptions(t)
	before := o.Budget.Snapshot().Used
	p, err := PrepareFormat2FromBackup(context.Background(), archive, metadataTestKey(), metadataTestKey(), filepath.Dir(source), workspace, guard, o, 16<<20)
	if p != nil || !errors.Is(err, ErrSource) || before != o.Budget.Snapshot().Used {
		t.Fatal("accepted authenticated but mismatched native cut", p, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "metadata-root")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cut mismatch constructed candidate", err)
	}
}
