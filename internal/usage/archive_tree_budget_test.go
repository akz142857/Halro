package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInventoryArchiveTreeExactLimitAndRejectsUnknownObjects(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "data"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if size, err := inventoryArchiveTree(context.Background(), root, 5); err != nil || size != 5 {
		t.Fatal(size, err)
	}
	if _, err := inventoryArchiveTree(context.Background(), root, 4); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("cap+1 tree was accepted", err)
	}
	if err := os.Symlink(filepath.Join(root, "nested", "data"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := inventoryArchiveTree(context.Background(), root, 5); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("symlink was treated as owned archive data", err)
	}
}

func TestExporterArchiveDiskCeilingRejectsOverLimitBeforePublication(t *testing.T) {
	f := diskExportFixture(t, 3)
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	e, err := NewExporterWithOptions(root, Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ExportDiskStep(context.Background(), f.p, DefaultDiskExportStepLimits()); err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	used, err := inventoryArchiveTree(context.Background(), root, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	for _, event := range diskFailureRequest("archive_ceiling_next", "success", 1, false, true) {
		appendDiskDetailEvent(t, f, event)
	}
	e, err = NewExporterWithOptions(root, Options{Format: FormatNDJSON, ArchiveDiskBytes: used})
	if err != nil {
		t.Fatal("exact-cap startup rejected", err)
	}
	cut, err := e.ExportDiskStep(context.Background(), f.p, DefaultDiskExportStepLimits())
	if !errors.Is(err, ErrDiskUsageLimit) || cut != (DiskArchiveCut{}) {
		t.Fatal("over-cap publication was accepted", cut, err)
	}
	manifestAfter, readErr := os.ReadFile(filepath.Join(root, "manifest.json"))
	if readErr != nil || string(manifestAfter) != string(manifestBefore) {
		t.Fatal("rejected export changed authoritative archive root", readErr)
	}
	if _, err := NewExporterWithOptions(root, Options{Format: FormatNDJSON, ArchiveDiskBytes: used - 1}); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("over-cap startup was accepted", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExporterArchiveDiskCeilingCountsParquetColumnScratch(t *testing.T) {
	f := diskExportFixture(t, 2)
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	e, err := NewExporterWithOptions(root, Options{Format: FormatParquet, ArchiveDiskBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	cut, err := e.ExportDiskStep(context.Background(), f.p, DefaultDiskExportStepLimits())
	if !errors.Is(err, ErrDiskUsageLimit) || cut != (DiskArchiveCut{}) {
		t.Fatal("Parquet scratch escaped the tree ceiling", cut, err)
	}
	if _, err := os.Stat(filepath.Join(root, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Parquet scratch limit published a root", err)
	}
	if _, err := inventoryArchiveTree(context.Background(), root, 1); err != nil {
		t.Fatal("failed Parquet publication left unaccounted bytes", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveTreeCeilingCoversDateCatalogCandidatePublication(t *testing.T) {
	e := dateBuildCatalogFixture(t, 80)
	manifestPath := filepath.Join(e.root, "manifest.json")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	used, err := inventoryArchiveTree(context.Background(), e.root, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	root := e.root
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = NewExporterWithOptions(root, Options{ArchiveDiskBytes: used})
	if err != nil {
		t.Fatal("exact-cap archive did not reopen", err)
	}
	out, err := e.PrepareRetainedDateCatalogStep(context.Background(), archiveDateCutoff, DefaultDateCatalogBuildLimits())
	if !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("date catalog candidate crossed archive ceiling", out, err)
	}
	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected date candidate changed the published root", readErr)
	}
	if total, err := inventoryArchiveTree(context.Background(), root, used); err != nil || total != used {
		t.Fatal("rejected date candidate left bytes beyond the ceiling", total, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveVerifyIndexTreeBudgetStopsAppendAtExactLimit(t *testing.T) {
	root := t.TempDir()
	const ceiling = archiveVerifyNodeBytes
	tree, err := newArchiveTreeBudget(context.Background(), root, ceiling)
	if err != nil {
		t.Fatal(err)
	}
	index, err := newArchiveVerifyIndexWithTree(filepath.Join(root, "index.bin"), 1<<20, tree)
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte("row"))
	if err := index.write(context.Background(), uint64(index.size), archiveVerifyNode{bit: 256, key: key}, &archiveVerifyReadBudget{remaining: 1 << 20}); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("index append exceeded exact tree cap", err)
	}
	info, err := index.file.Stat()
	if err != nil || info.Size() != archiveVerifyNodeBytes {
		t.Fatal("rejected append changed index size", info, err)
	}
	if err := index.close(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveDiskCeilingAlsoCoversLegacySnapshotExport(t *testing.T) {
	f := diskExportFixture(t, 2)
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	e, err := NewExporterWithOptions(root, Options{Format: FormatNDJSON, ArchiveDiskBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Export(f.a.Snapshot()); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("legacy snapshot export bypassed the archive tree ceiling", err)
	}
	if _, err := os.Stat(filepath.Join(root, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused legacy export published a manifest", err)
	}
	if total, err := inventoryArchiveTree(context.Background(), root, 1); err != nil || total != 0 {
		t.Fatal("refused legacy export left archive bytes", total, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveDiskCeilingCoversLegacyRetentionRootPublication(t *testing.T) {
	f := diskExportFixture(t, 3)
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	e, err := NewExporterWithOptions(root, Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Export(f.a.Snapshot()); err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	used, err := inventoryArchiveTree(context.Background(), root, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = NewExporterWithOptions(root, Options{Format: FormatNDJSON, ArchiveDiskBytes: used})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PruneBefore(time.Now().AddDate(1, 0, 0)); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("retention root publication crossed the ceiling", err)
	}
	manifestAfter, readErr := os.ReadFile(filepath.Join(root, "manifest.json"))
	if readErr != nil || !bytes.Equal(manifestBefore, manifestAfter) {
		t.Fatal("refused retention changed the published root", readErr)
	}
	if total, err := inventoryArchiveTree(context.Background(), root, used); err != nil || total != used {
		t.Fatal("refused retention left unaccounted archive bytes", total, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
}
