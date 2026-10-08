package usage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDiskExportStepPreservingCatalogStreamsLegacyRootAndKeepsEntrySchema(t *testing.T) {
	f := diskExportFixture(t, 7)
	e, err := NewExporterWithOptions(filepath.Join(t.TempDir(), "archive"), Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	early := f.a.Snapshot()
	early.Attempts = early.Attempts[:2]
	m, err := e.Export(early)
	if err != nil {
		t.Fatal(err)
	}
	m.SchemaVersion = 7
	for i := range m.Files {
		var rows []parquetAttempt
		for _, attempt := range early.Attempts {
			if attempt.Sequence >= m.Files[i].MinSequence && attempt.Sequence <= m.Files[i].MaxSequence {
				rows = append(rows, narrowToSchema(toParquetAttempt(attempt), 7))
			}
		}
		path := filepath.Join(e.root, m.Files[i].Path)
		if err := writeNDJSONAtomic(context.Background(), path, rows, nil); err != nil {
			t.Fatal(err)
		}
		m.Files[i].SHA256, err = fileSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		m.Files[i].SchemaVersion = 0
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// Two bounded unknown scalar values make the whole legacy document larger
	// than a paged root cap; per-entry schemas still inherit the old root.
	raw = append(raw[:len(raw)-1], []byte(`,"padding_a":"`+strings.Repeat("a", 32<<10)+`","padding_b":"`+strings.Repeat("b", 32<<10)+`"}`)...)
	root := filepath.Join(e.root, "manifest.json")
	if len(raw) <= archiveManifestValueLimit {
		t.Fatal("fixture failed to exceed fixed-root cap")
	}
	if err := os.WriteFile(root, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before := verificationArchiveTree(t, e.root)
	limits := DefaultDiskExportStepLimits()
	limits.Source.Rows = 2
	var steps int
	for {
		cut, err := e.ExportDiskStepPreservingCatalog(context.Background(), f.p, limits)
		if err != nil || cut.Rows > 2 {
			t.Fatal(cut, err)
		}
		steps++
		if cut.ThroughSequence == cut.Head.Sequence {
			break
		}
		if steps > 4 {
			t.Fatal("prefix did not progress")
		}
	}
	if steps != 3 {
		t.Fatal(steps)
	}
	position, err := e.ReadArchivePosition(context.Background())
	if err != nil || position.CatalogVersion != 0 || position.SchemaVersion != 8 || !position.CompletePrefix || position.LastSequence != f.a.Watermark().Sequence {
		t.Fatal(position, err)
	}
	now, err := e.LoadManifest() // Resident fixture assertion only.
	if err != nil || len(now.Files) <= len(m.Files) {
		t.Fatal(now, err)
	}
	for i, old := range m.Files {
		if now.Files[i].Path != old.Path || now.Files[i].SchemaVersion != 7 || now.Files[i].SHA256 != old.SHA256 {
			t.Fatal("root upgrade fabricated an old entry schema", now.Files[i], old)
		}
	}
	for path, old := range before {
		if path == root {
			continue
		}
		actual := verificationArchiveTree(t, e.root)[path]
		if old.digest != actual.digest || !os.SameFile(old.info, actual.info) {
			t.Fatal("streaming root rewrote an immutable old partition", path)
		}
	}
	verified, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
	if err != nil || verified.SourceRows != 7 || verified.ArchiveRows != 7 {
		t.Fatal(verified, err)
	}
	// The same compatibility entry keeps an explicitly migrated paged root.
	if _, err := e.MigrateArchiveCatalog(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(root)
	if err != nil {
		t.Fatal(err)
	}
	cut, err := e.ExportDiskStepPreservingCatalog(context.Background(), f.p, limits)
	after, readErr := os.ReadFile(root)
	if err != nil || readErr != nil || cut.Rows != 0 || !reflect.DeepEqual(bytes, after) {
		t.Fatal("compatibility entry downgraded a paged root", cut, err, readErr)
	}
}

func TestDiskExportStepPreservingCatalogRootWriteAndWorkLimitsKeepOldRoot(t *testing.T) {
	for _, dimension := range []string{"write", "work"} {
		t.Run(dimension, func(t *testing.T) {
			f := diskExportFixture(t, 3)
			e, err := NewExporterWithOptions(filepath.Join(t.TempDir(), "archive"), Options{Format: FormatNDJSON})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Export(f.a.Snapshot()); err != nil {
				t.Fatal(err)
			}
			before := verificationArchiveTree(t, e.root)
			limits := DefaultDiskExportStepLimits()
			if dimension == "write" {
				limits.WriteBytes = 1
			} else {
				limits.WorkBytes = 1
			}
			cut, err := e.ExportDiskStepPreservingCatalog(context.Background(), f.p, limits)
			if !errors.Is(err, ErrDiskUsageLimit) || cut != (DiskArchiveCut{}) {
				t.Fatal("root publication bypassed actual job limits", cut, err)
			}
			assertVerificationArchivePreserved(t, before, verificationArchiveTree(t, e.root))
		})
	}
}
