package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var archiveDateCutoff = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

func dateArchiveFixture(t *testing.T, format string, legacy bool) (*diskFailureFixture, *Exporter) {
	t.Helper()
	events := windowRequest("old", false, false)
	events = append(events, windowRequest("retained", true, true)...)
	events = append(events, windowRequest("late_old", false, false)...)
	f := newDiskFailureFixture(t, events)
	e, err := NewExporterWithOptions(t.TempDir(), Options{Format: format})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.exportDisk(context.Background(), f.p, DefaultDiskExportLimits(), false, legacy); err != nil {
		t.Fatal(err)
	}
	return f, e
}

// Small test fixtures only. Production date coverage retains no manifest list.
func rewriteDateArchive(t *testing.T, e *Exporter, keep func(ManifestFile) bool, mutate func(*ManifestFile)) {
	t.Helper()
	ctx := context.Background()
	old, err := openArchiveManifest(e.root)
	if err != nil {
		t.Fatal(err)
	}
	defer old.file.Close()
	position, _, err := old.walk(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var entries []ManifestFile
	add := func(entry ManifestFile) error {
		if entry.SchemaVersion == 0 {
			entry.SchemaVersion = position.SchemaVersion
		}
		if keep != nil && !keep(entry) {
			return nil
		}
		if mutate != nil {
			mutate(&entry)
		}
		entries = append(entries, entry)
		return nil
	}
	if position.CatalogVersion == 0 {
		if _, _, err := old.walk(ctx, add); err != nil {
			t.Fatal(err)
		}
	} else {
		for cursor := ""; ; {
			page, err := e.ReadArchiveFiles(ctx, cursor, 64)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range page.Files {
				if err := add(entry); err != nil {
					t.Fatal(err)
				}
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	if position.CatalogVersion == 0 {
		if err := e.commitManifest(Manifest{SchemaVersion: position.SchemaVersion, LastSequence: position.LastSequence, CompletePrefix: true, Files: entries}); err != nil {
			t.Fatal(err)
		}
		return
	}
	work := &archiveWork{remaining: 1 << 20}
	builder := archiveCatalogBuilder{exporter: e, ctx: ctx, work: work}
	for _, entry := range entries {
		if err := builder.add(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := builder.flush(); err != nil {
		t.Fatal(err)
	}
	if err := e.publishArchiveCatalogRoot(ctx, old, false, position.LastSequence, true, builder.head, work); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveDateCoverageInterleavedNativePrefixAndBothFormats(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy_%t", format, legacy), func(t *testing.T) {
				f, e := dateArchiveFixture(t, format, legacy)
				rewriteDateArchive(t, e, func(entry ManifestFile) bool { return entry.Date >= "2026-10-05" }, nil)
				before := verificationArchiveTree(t, e.root)
				report, err := e.VerifyDiskArchiveRetainedDates(context.Background(), f.p, archiveDateCutoff, DefaultArchiveVerifyLimits())
				if err != nil || report.SourceRows != 6 || report.ArchiveRows != 2 || report.RequiredRows != 2 || report.MatchedRows != 2 || report.ExpiredOmittedRows != 4 || report.RetainFromDate != "2026-10-05" || report.AfterSequence != 0 || report.ThroughSequence != f.a.Watermark().Sequence {
					t.Fatal(report, err)
				}
				assertVerificationArchivePreserved(t, before, verificationArchiveTree(t, e.root))
				if e.windowProof != nil || e.windowPending != nil {
					t.Fatal("date scope minted full-history authority")
				}
				if out, err := e.PruneVerifiedWindowStep(context.Background(), f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) || out != (DiskWindowProgress{}) {
					t.Fatal(out, err)
				}
				if report, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits()); !errors.Is(err, ErrDiskUsageCorrupt) || report != (ArchiveVerification{}) {
					t.Fatal("inferred sequence floor", report, err)
				}
			})
		}
	}
}

func TestArchiveDateCoverageCannotTrustRootPolicyOrMissingRetainedRows(t *testing.T) {
	for _, kind := range []string{"retained_descriptor", "all_descriptors", "root_policy", "retained_file"} {
		t.Run(kind, func(t *testing.T) {
			f, e := dateArchiveFixture(t, FormatNDJSON, false)
			switch kind {
			case "retained_descriptor", "all_descriptors", "root_policy":
				rewriteDateArchive(t, e, func(entry ManifestFile) bool { return kind != "all_descriptors" && entry.Date < "2026-10-05" }, nil)
				if kind == "root_policy" {
					path := filepath.Join(e.root, "manifest.json")
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var root map[string]any
					if err := json.Unmarshal(raw, &root); err != nil {
						t.Fatal(err)
					}
					root["retain_from_date"] = "2099-01-01"
					raw, err = json.Marshal(root)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, raw, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			case "retained_file":
				page, err := e.ReadArchiveFiles(context.Background(), "", 64)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range page.Files {
					if entry.Date == "2026-10-05" {
						if err := os.Remove(filepath.Join(e.root, entry.Path)); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if report, err := e.VerifyDiskArchiveRetainedDates(context.Background(), f.p, archiveDateCutoff, DefaultArchiveVerifyLimits()); err == nil || report != (ArchiveDateVerification{}) {
				t.Fatal(report, err)
			}
		})
	}
}

func TestArchivePartitionDateMustMatchCanonicalPathAndNativeRows(t *testing.T) {
	for _, kind := range []string{"empty", "noncanonical", "invalid_calendar", "zero_year", "path_mismatch", "normalized_escape", "forged_date_and_path"} {
		t.Run(kind, func(t *testing.T) {
			f, e := dateArchiveFixture(t, FormatNDJSON, true)
			rewriteDateArchive(t, e, nil, func(entry *ManifestFile) {
				switch kind {
				case "empty":
					entry.Date = ""
				case "noncanonical":
					entry.Date = "2026-2-1"
				case "invalid_calendar":
					entry.Date = "2026-02-30"
				case "zero_year":
					entry.Date = "0000-01-01"
				case "path_mismatch":
					entry.Date = "2000-01-01"
				case "normalized_escape":
					entry.Path = "date=" + entry.Date + "/../" + entry.Path
				case "forged_date_and_path":
					actual := filepath.Join(e.root, entry.Path)
					entry.Date = "2000-01-01"
					entry.Path = "date=2000-01-01/" + filepath.Base(entry.Path)
					if err := os.MkdirAll(filepath.Dir(filepath.Join(e.root, entry.Path)), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(actual, filepath.Join(e.root, entry.Path)); err != nil {
						t.Fatal(err)
					}
				}
			})
			if report, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits()); !errors.Is(err, ErrDiskUsageCorrupt) || report != (ArchiveVerification{}) {
				t.Fatal(report, err)
			}
			if report, err := e.VerifyDiskArchiveRetainedDates(context.Background(), f.p, archiveDateCutoff, DefaultArchiveVerifyLimits()); !errors.Is(err, ErrDiskUsageCorrupt) || report != (ArchiveDateVerification{}) {
				t.Fatal(report, err)
			}
		})
	}
}

func TestArchiveDateCoverageListedExpiredRowsStillRequireExactNativeMatch(t *testing.T) {
	for _, kind := range []string{"content", "extra", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			f := diskExportFixture(t, 2)
			e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
				t.Fatal(err)
			}
			var rows []parquetAttempt
			for _, a := range f.a.Snapshot().Attempts {
				rows = append(rows, toParquetAttempt(a))
			}
			switch kind {
			case "content":
				rows[0].ProviderModel = "forged"
			case "extra":
				rows[0].EventID = "not_native"
			case "duplicate":
				rows[1].EventID = rows[0].EventID
			}
			rewriteTestVerificationArchive(t, e, rows, nil)
			if report, err := e.VerifyDiskArchiveRetainedDates(context.Background(), f.p, archiveDateCutoff, DefaultArchiveVerifyLimits()); !errors.Is(err, ErrDiskUsageCorrupt) || report != (ArchiveDateVerification{}) {
				t.Fatal(report, err)
			}
		})
	}
}

func TestArchiveDateCoverageCompleteArchiveDoesNotGrantWindowAndAllExpiredScope(t *testing.T) {
	f, e := dateArchiveFixture(t, FormatNDJSON, false)
	report, err := e.VerifyDiskArchiveRetainedDates(context.Background(), f.p, archiveDateCutoff, DefaultArchiveVerifyLimits())
	if err != nil || report.SourceRows != 6 || report.MatchedRows != 6 || report.RequiredRows != 2 || report.ExpiredOmittedRows != 0 || e.windowProof != nil {
		t.Fatal(report, err)
	}
	rewriteDateArchive(t, e, func(ManifestFile) bool { return false }, nil)
	future := archiveDateCutoff.AddDate(0, 0, 1)
	report, err = e.VerifyDiskArchiveRetainedDates(context.Background(), f.p, future, DefaultArchiveVerifyLimits())
	if err != nil || report.SourceRows != 6 || report.ArchiveRows != 0 || report.MatchedRows != 0 || report.RequiredRows != 0 || report.ExpiredOmittedRows != 6 {
		t.Fatal(report, err)
	}
	// The request's UTC day, not its displayed local day, determines the policy.
	local := time.Date(2026, 10, 6, 0, 0, 0, 0, time.FixedZone("UTC+14", 14*3600))
	if report, err := e.VerifyDiskArchiveRetainedDates(context.Background(), f.p, local, DefaultArchiveVerifyLimits()); !errors.Is(err, ErrDiskUsageCorrupt) || report != (ArchiveDateVerification{}) {
		t.Fatal(report, err)
	}
}

func TestArchiveDateCoverageCancellationLimitsAndInvalidPolicyWithdrawReport(t *testing.T) {
	f, e := dateArchiveFixture(t, FormatNDJSON, false)
	for _, kind := range []string{"cancel", "source_rows", "archive_rows", "read_bytes", "zero_date", "out_of_range"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			limits := DefaultArchiveVerifyLimits()
			date := archiveDateCutoff
			switch kind {
			case "cancel":
				cancel()
			case "source_rows":
				limits.Source.Rows = 1
			case "archive_rows":
				limits.Rows = 1
			case "read_bytes":
				limits.ReadBytes = 1
			case "zero_date":
				date = time.Time{}
			case "out_of_range":
				date = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			report, err := e.VerifyDiskArchiveRetainedDates(ctx, f.p, date, limits)
			if err == nil || report != (ArchiveDateVerification{}) {
				t.Fatal(kind, report, err)
			}
			leftovers, err := filepath.Glob(filepath.Join(e.root, ".archive-verify-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatal(leftovers, err)
			}
		})
	}
}

func TestArchivePartitionPublisherRefusesMixedOrInvalidDates(t *testing.T) {
	f, e := dateArchiveFixture(t, FormatNDJSON, false)
	attempts := f.a.Snapshot().Attempts
	before := verificationArchiveTree(t, e.root)
	for _, date := range []string{"2026-10-03", "2026-2-1", "0000-01-01"} {
		entry, err := e.publishDiskPartition(context.Background(), date, []AttemptEvent{attempts[0], attempts[2]}, 16<<20, &archiveWork{remaining: 1 << 20}, &archiveWork{remaining: 1 << 20})
		if !errors.Is(err, ErrDiskUsageCorrupt) || entry != (ManifestFile{}) {
			t.Fatal(entry, err)
		}
	}
	assertVerificationArchivePreserved(t, before, verificationArchiveTree(t, e.root))
}
