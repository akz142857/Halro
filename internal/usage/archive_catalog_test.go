package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func syntheticArchiveEntry(sequence uint64) ManifestFile {
	return ManifestFile{SchemaVersion: 8, Path: fmt.Sprintf("date=2026-10-03/usage-%020d.ndjson", sequence), Date: "2026-10-03", SHA256: strings.Repeat("0", 64), MinSequence: sequence, MaxSequence: sequence, Records: 1, Format: FormatNDJSON}
}
func TestArchiveCatalogTenThousandMigrationAndConstantAppend(t *testing.T) {
	e, err := NewExporter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacy := Manifest{SchemaVersion: 8, LastSequence: 10000}
	for i := uint64(1); i <= 10000; i++ {
		legacy.Files = append(legacy.Files, syntheticArchiveEntry(i))
	}
	if err := e.commitManifest(legacy); err != nil {
		t.Fatal(err)
	}
	position, err := e.MigrateArchiveCatalog(context.Background(), time.Minute)
	if err != nil || position.CatalogVersion != 1 || position.CatalogHead.Pages != 157 || position.Files != 10000 || position.CompletePrefix {
		t.Fatal(position, err)
	}
	info, err := os.Stat(filepath.Join(e.root, "manifest.json"))
	if err != nil || info.Size() > 1024 {
		t.Fatal(info, err)
	}
	headPath := position.CatalogHead.path(e.root)
	oldBytes, err := os.ReadFile(headPath)
	if err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(headPath)
	if err != nil {
		t.Fatal(err)
	}
	old, err := openArchiveManifest(e.root)
	if err != nil {
		t.Fatal(err)
	}
	defer old.file.Close()
	pos, digest, err := old.walk(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := os.CreateTemp(t.TempDir(), "entries-")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	for i := uint64(10001); i <= 10003; i++ {
		if err := json.NewEncoder(stage).Encode(syntheticArchiveEntry(i)); err != nil {
			t.Fatal(err)
		}
	}
	cut := DiskArchiveCut{Head: ledger.Watermark{Sequence: 10005}, AfterSequence: pos.LastSequence, ThroughSequence: 10005}
	if err := e.commitPagedArchive(context.Background(), old, false, pos, digest, stage, 3, cut, &archiveWork{remaining: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	next, err := e.ReadArchivePosition(context.Background())
	if err != nil || next.Files != 10003 || next.CatalogHead.Pages != 158 || !next.CompletePrefix {
		t.Fatal(next, err)
	}
	page, err := e.readCatalogPage(context.Background(), next.CatalogHead)
	if err != nil || page.Previous != position.CatalogHead || len(page.Files) != 3 {
		t.Fatal(page, err)
	}
	unchanged, err := os.ReadFile(headPath)
	if err != nil || !bytes.Equal(oldBytes, unchanged) {
		t.Fatal("append rewrote old page", err)
	}
	now, err := os.Stat(headPath)
	if err != nil || now.ModTime() != oldInfo.ModTime() {
		t.Fatal("append modified old page", err)
	}
	if err := e.ValidateArchiveCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Only root + new page fit this 1 MiB work cap despite the ~3 MiB history.
	// Collecting IDs is fixture-only: production reader holds at most one page.
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := e.ReadArchiveFiles(context.Background(), cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Files) > 7 || page.LastSequence != 10005 {
			t.Fatal(page)
		}
		for _, entry := range page.Files {
			if seen[entry.Path] {
				t.Fatal("duplicate descriptor", entry.Path)
			}
			seen[entry.Path] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 10003 {
		t.Fatal(len(seen))
	}
	for _, call := range []func() error{func() error { _, err := e.LoadManifest(); return err }, func() error { _, err := e.Export(Snapshot{}); return err }, func() error { return e.Verify(nil) }, func() error { _, err := e.PruneBefore(time.Now()); return err }} {
		if err := call(); !errors.Is(err, ErrArchivePagedCatalog) {
			t.Fatal("legacy consumer interpreted paged catalog", err)
		}
	}
}

func TestArchiveCatalogNoopMigrationDoesNotReserveScratch(t *testing.T) {
	e := dateBuildCatalogFixture(t, 16)
	requests := 0
	e.archiveDiskReserve = func(context.Context, int64) (func() error, error) {
		requests++
		return func() error { return nil }, nil
	}
	if _, err := e.MigrateArchiveCatalog(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatal("already-paged catalog reserved migration output", requests)
	}
}

func TestArchiveCatalogCursorFrozenAcrossRealNativeAppendAndScope(t *testing.T) {
	f := diskExportFixture(t, 130)
	e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultDiskExportLimits()
	limits.Source.PageRows = 1
	cut, err := e.ExportDisk(context.Background(), f.p, limits)
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.ReadArchiveFiles(context.Background(), "", 2)
	if err != nil || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	saved := first.NextCursor
	for _, event := range diskFailureRequest("catalog_future", "success", 1, false, true) {
		appendDiskDetailEvent(t, f, event)
	}
	if _, err := e.ExportDisk(context.Background(), f.p, limits); err != nil {
		t.Fatal(err)
	}
	count := len(first.Files)
	cursor := saved
	for {
		page, err := e.ReadArchiveFiles(context.Background(), cursor, 11)
		if err != nil {
			t.Fatal(err)
		}
		if page.LastSequence != cut.Head.Sequence {
			t.Fatal("cursor crossed frozen frontier", page)
		}
		count += len(page.Files)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if count != 130 {
		t.Fatal("old cursor saw future/missing rows", count)
	}
	raw, err := base64.RawURLEncoding.DecodeString(saved)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 1
	if _, err := e.ReadArchiveFiles(context.Background(), base64.RawURLEncoding.EncodeToString(raw), 2); !errors.Is(err, ErrDiskUsageCursor) {
		t.Fatal(err)
	}
	other, err := NewExporter(e.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ReadArchiveFiles(context.Background(), saved, 2); !errors.Is(err, ErrDiskUsageCursor) {
		t.Fatal("cursor escaped owner", err)
	}
	if _, err := e.ReadArchiveFiles(context.Background(), strings.Repeat("x", 100000), 2); !errors.Is(err, ErrDiskUsageCursor) {
		t.Fatal(err)
	}
	position, err := e.ReadArchivePosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rootBefore, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(position.CatalogHead.path(e.root)); err != nil {
		t.Fatal(err)
	}
	receipt, err := e.ExportDisk(context.Background(), f.p, limits)
	if !errors.Is(err, os.ErrNotExist) || receipt != (DiskArchiveCut{}) {
		t.Fatal("idempotent export credited missing head", receipt, err)
	}
	rootAfter, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
	if err != nil || !bytes.Equal(rootBefore, rootAfter) {
		t.Fatal("missing head changed root", err)
	}

}

func TestArchiveCatalogSchemaMigrationAndFailedPublication(t *testing.T) {
	for schema := 3; schema <= 8; schema++ {
		t.Run(fmt.Sprint(schema), func(t *testing.T) {
			e, err := NewExporter(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			entry := syntheticArchiveEntry(1)
			entry.SchemaVersion = 0
			if err := e.commitManifest(Manifest{SchemaVersion: schema, LastSequence: 2, Files: []ManifestFile{entry}}); err != nil {
				t.Fatal(err)
			}
			pos, err := e.MigrateArchiveCatalog(context.Background(), time.Minute)
			if err != nil || pos.SchemaVersion != 9 || pos.LastSequence != 2 || pos.CompletePrefix {
				t.Fatal(pos, err)
			}
			page, err := e.ReadArchiveFiles(context.Background(), "", 1)
			if err != nil || len(page.Files) != 1 || page.Files[0].SchemaVersion != schema {
				t.Fatal(page, err)
			}
		})
	}
	e, err := NewExporter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.commitManifest(Manifest{SchemaVersion: 8, LastSequence: 1, Files: []ManifestFile{syntheticArchiveEntry(1)}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	old, err := openArchiveManifest(e.root)
	if err != nil {
		t.Fatal(err)
	}
	defer old.file.Close()
	pos, digest, err := old.walk(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := os.CreateTemp(t.TempDir(), "stage")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	// An incorrect source/staging receipt may leave a page orphan, never a root.
	if err := e.commitPagedArchive(context.Background(), old, false, pos, digest, stage, 1, DiskArchiveCut{Head: ledger.Watermark{Sequence: 2}, AfterSequence: pos.LastSequence, ThroughSequence: 2}, &archiveWork{remaining: 1 << 20}); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("partial migration activated", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.MigrateArchiveCatalog(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestArchiveCatalogCorruptionMissingPagesAndAllocationBounds(t *testing.T) {
	e, err := NewExporter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	legacy := Manifest{SchemaVersion: 8, LastSequence: 100}
	for i := uint64(1); i <= 100; i++ {
		legacy.Files = append(legacy.Files, syntheticArchiveEntry(i))
	}
	if err := e.commitManifest(legacy); err != nil {
		t.Fatal(err)
	}
	pos, err := e.MigrateArchiveCatalog(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	path := pos.CatalogHead.path(e.root)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutated := append([]byte(nil), original...)
	mutated[len(mutated)/2] ^= 1
	if err := os.WriteFile(path, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.ValidateArchiveCatalog(context.Background()); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal(err)
	}
	if _, err := NewExporter(e.root); err == nil {
		t.Fatal("corrupt catalog accepted at startup")
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewExporter(e.root); err == nil {
		t.Fatal("missing page accepted at startup")
	}
	if err := os.Rename(path+".held", path); err != nil {
		t.Fatal(err)
	}
	oversized := `{"version":1,"previous":{},"files":[` + strings.TrimSuffix(strings.Repeat(`{},`, 65), ",") + `]}`
	if _, err := decodeArchiveCatalogPage([]byte(oversized)); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("array was not capped before struct decode", err)
	}
	// A root pointing to a self-counter page cannot form an unbounded traversal.
	bad := archiveCatalogPage{Version: 1, Previous: pos.CatalogHead, Files: []ManifestFile{syntheticArchiveEntry(1)}}
	raw, _ := json.Marshal(bad)
	hash := sha256.Sum256(raw)
	ref := pos.CatalogHead
	ref.SHA256 = hex.EncodeToString(hash[:])
	ref.Bytes = len(raw)
	if err := os.WriteFile(ref.path(e.root), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.readCatalogPage(context.Background(), ref); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("nondecreasing chain accepted", err)
	}

	rootPath := filepath.Join(e.root, "manifest.json")
	rootBytes, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	largeRoot := append(append([]byte(nil), rootBytes...), bytes.Repeat([]byte(" "), archiveManifestValueLimit)...)
	if err := os.WriteFile(rootPath, largeRoot, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReadArchivePosition(context.Background()); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal("oversized paged root accepted", err)
	}
	if err := os.WriteFile(rootPath, rootBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	root1, _ := e.ReadArchivePosition(context.Background())
	root2, _ := e.ReadArchivePosition(context.Background())
	if !reflect.DeepEqual(root1, root2) {
		t.Fatal("failed read mutated root")
	}
}
