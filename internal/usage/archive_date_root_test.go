package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixture publication only. The candidate API itself cannot activate a root.
func publishDateRootFixture(t *testing.T, e *Exporter, out DateCatalogBuildProgress) {
	t.Helper()
	position, err := readArchiveManifest(context.Background(), bytes.NewReader(out.RootJSON), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.withArchiveReclamation(context.Background(), func(advance func() error) (err error) {
		old, err := openArchiveManifest(e.root)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, old.file.Close()) }()
		if err := advance(); err != nil {
			return err
		}
		return e.publishArchiveCatalogRootWithRetention(context.Background(), old, false, position.LastSequence, false, position.CatalogHead, position.Retention, &archiveWork{remaining: 1 << 20}, nil)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveDateRootPreservesPolicyAcrossLateOldAppendAndNoop(t *testing.T) {
	for _, format := range []string{FormatParquet, FormatNDJSON} {
		t.Run(format, func(t *testing.T) {
			f, e := dateArchiveFixture(t, format, false)
			out := finishDateCandidate(t, e, archiveDateCutoff)
			publishDateRootFixture(t, e, out)
			position, err := e.ReadArchivePosition(context.Background())
			if err != nil || position.SchemaVersion != 10 || position.CompletePrefix || position.Retention.PruneThroughSequence != position.LastSequence {
				t.Fatal(position, err)
			}
			report, err := e.VerifyDiskArchiveRetainedDates(context.Background(), f.p, archiveDateCutoff, DefaultArchiveVerifyLimits())
			if err != nil || report.RequiredRows != 2 || report.ArchiveRows != 2 || report.ExpiredOmittedRows != 4 || e.windowProof != nil {
				t.Fatal(report, err)
			}
			for _, event := range windowRequest("late_again", false, false) {
				appendDiskDetailEvent(t, f, event)
			}
			if _, err := e.ExportDiskStep(context.Background(), f.p, DefaultDiskExportStepLimits()); err != nil {
				t.Fatal(err)
			}
			after, err := e.ReadArchivePosition(context.Background())
			if err != nil || after.Retention != position.Retention || after.SchemaVersion != 10 || after.CompletePrefix || after.LastSequence <= position.LastSequence {
				t.Fatal(after, err)
			}
			report, err = e.VerifyDiskArchiveRetainedDates(context.Background(), f.p, archiveDateCutoff, DefaultArchiveVerifyLimits())
			if err != nil || report.SourceRows != 8 || report.ArchiveRows != 4 || report.RequiredRows != 2 || report.ExpiredOmittedRows != 4 || e.windowProof != nil || e.windowPending != nil {
				t.Fatal(report, err)
			}
			path := filepath.Join(e.root, "manifest.json")
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.ExportDiskStep(context.Background(), f.p, DefaultDiskExportStepLimits()); err != nil {
				t.Fatal(err)
			}
			now, err := os.Stat(path)
			if err != nil || !os.SameFile(before, now) || before.ModTime() != now.ModTime() {
				t.Fatal("noop rewrote date coverage", err)
			}
			reopened, err := NewExporter(e.root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.VerifyArchiveFileChecksums(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.LoadManifest(); !errors.Is(err, ErrArchivePagedCatalog) {
				t.Fatal(err)
			}
		})
	}
}

func TestArchiveDateRootFullRowsStillCannotMintWindowAuthority(t *testing.T) {
	f, e := dateArchiveFixture(t, FormatNDJSON, false)
	cutoff := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	out := finishDateCandidate(t, e, cutoff)
	publishDateRootFixture(t, e, out)
	report, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
	if err != nil || report.ArchiveRows != 6 || e.windowProof != nil || e.windowPending != nil {
		t.Fatal(report, err)
	}
	if _, err := e.PruneVerifiedWindowStep(context.Background(), f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) {
		t.Fatal(err)
	}
}

func TestArchiveDateRootMalformedPolicyAndExpiredBaselineEntriesRefuse(t *testing.T) {
	valid, _, err := archiveCatalogRootBytes(100, false, ArchivePageRef{}, ArchiveDateRetention{Version: 1, RetainFromDate: "2026-10-05", PruneThroughSequence: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{
		bytes.Replace(valid, []byte(`"schema_version":10`), []byte(`"schema_version":9`), 1),
		bytes.Replace(valid, []byte(`"complete_prefix":false`), []byte(`"complete_prefix":true`), 1),
		bytes.Replace(valid, []byte(`"version":1,"retain_from_date"`), []byte(`"version":2,"retain_from_date"`), 1),
		bytes.Replace(valid, []byte(`"2026-10-05"`), []byte(`"2026-1-5"`), 1),
		bytes.Replace(valid, []byte(`"prune_through_sequence":100`), []byte(`"prune_through_sequence":101`), 1),
		bytes.Replace(valid, []byte(`"prune_through_sequence":100`), []byte(`"prune_through_sequence":null`), 1),
		bytes.Replace(valid, []byte(`"prune_through_sequence":100`), []byte(`"prune_through_sequence":100,"prune_through_sequence":0`), 1),
		bytes.Replace(valid, []byte(`"prune_through_sequence":100`), []byte(`"prune_through_sequence":100,"extra":true`), 1),
		[]byte(`{"schema_version":10,"last_sequence":100,"complete_prefix":false,"files":null,"catalog":{"version":1,"head":{}}}`),
	} {
		if position, err := readArchiveManifest(context.Background(), bytes.NewReader(raw), nil); err == nil || position != (ArchivePosition{}) {
			t.Fatal("malformed retention accepted", string(raw), position, err)
		}
	}
	e := dateBuildCatalogFixture(t, 2)
	position, err := e.ReadArchivePosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Retention metadata alone must not hide a listed expired baseline row.
	raw, _, err := archiveCatalogRootBytes(position.LastSequence, false, position.CatalogHead, ArchiveDateRetention{Version: 1, RetainFromDate: "2026-10-05", PruneThroughSequence: position.LastSequence})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.root, "manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.ValidateArchiveCatalog(context.Background()); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal(err)
	}
	if page, err := e.ReadArchiveFiles(context.Background(), "", 64); !errors.Is(err, ErrDiskUsageCorrupt) || page.LastSequence != 0 {
		t.Fatal(page, err)
	}
	if _, err := NewExporter(e.root); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal(err)
	}
	// Preserve the old format's rejection; neither a legacy date field nor an
	// unknown nullable field may silently select retention semantics.
	var root map[string]any
	if err := json.Unmarshal(valid, &root); err != nil {
		t.Fatal(err)
	}
	root["schema_version"] = 9
	root["retention"] = nil
	raw, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readArchiveManifest(context.Background(), strings.NewReader(string(raw)), nil); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal(err)
	}
}
