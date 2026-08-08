package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func TestNewExporterWithOptionsRejectsUnknownFormat(t *testing.T) {
	if _, err := NewExporterWithOptions(filepath.Join(t.TempDir(), "usage"), Options{Format: "csv"}); err == nil {
		t.Fatal("unknown export format was accepted")
	}
}

func TestExporterNDJSONPublishesVerifiableIdempotentPartitions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporterWithOptions(root, Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	snapshot := Snapshot{
		Attempts: []AttemptEvent{
			{
				EventID: "event_1", RequestID: "request_1", AttemptID: "attempt_1", Sequence: 1,
				ProjectID: "project_1", ProviderID: "provider_1", RequestedModel: "chat",
				ProviderInputTokens: 3, ProviderOutputTokens: 2, CostMicrosUSD: ledger.MicrosUSD(7),
				StartedAt: now.Add(-time.Second), CompletedAt: now, Status: "success", HTTPStatus: 200,
			},
		},
	}
	manifest, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Format != FormatNDJSON {
		t.Fatalf("manifest=%#v", manifest)
	}
	path := filepath.Join(root, filepath.FromSlash(manifest.Files[0].Path))
	if filepath.Ext(path) != ".ndjson" {
		t.Fatalf("partition path=%q, want .ndjson extension", path)
	}
	if err := exporter.Verify(&snapshot); err != nil {
		t.Fatal(err)
	}
	report, err := exporter.Reconcile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if report.Missing != 0 || report.Duplicates != 0 || report.Extra != 0 {
		t.Fatalf("reconciliation=%#v", report)
	}
	// Idempotent re-export must not rewrite or duplicate the partition.
	repeated, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.Files) != 1 || repeated.LastSequence != manifest.LastSequence {
		t.Fatalf("idempotent export changed manifest: %#v", repeated)
	}
}

func TestExporterNDJSONDetectsTampering(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporterWithOptions(root, Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Attempts: []AttemptEvent{{
		EventID: "event_1", RequestID: "request_1", AttemptID: "attempt_1",
		Sequence: 1, ProjectID: "project_1", StartedAt: time.Now().UTC(),
		CompletedAt: time.Now().UTC(), Status: "success",
	}}}
	manifest, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(manifest.Files[0].Path))
	if err := os.WriteFile(path, []byte("not ndjson"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exporter.Verify(&snapshot); err == nil {
		t.Fatal("expected tampering to be detected")
	}
}

// A deployment that switches format mid-life keeps its Parquet history
// exactly as written (ADR 0017: "existing bytes are never rewritten") while
// new partitions land in the new format — both must verify and reconcile
// together against one Ledger replay.
func TestExporterMixedParquetAndNDJSONManifestVerifiesTogether(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	parquetExporter, err := NewExporterWithOptions(root, Options{Format: FormatParquet})
	if err != nil {
		t.Fatal(err)
	}
	day1 := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	firstSnapshot := Snapshot{Attempts: []AttemptEvent{{
		EventID: "event_1", RequestID: "request_1", AttemptID: "attempt_1", Sequence: 1,
		ProjectID: "project_1", CostMicrosUSD: ledger.MicrosUSD(5), StartedAt: day1, CompletedAt: day1, Status: "success",
	}}}
	if _, err := parquetExporter.Export(firstSnapshot); err != nil {
		t.Fatal(err)
	}

	ndjsonExporter, err := NewExporterWithOptions(root, Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	fullSnapshot := Snapshot{Attempts: append(append([]AttemptEvent{}, firstSnapshot.Attempts...), AttemptEvent{
		EventID: "event_2", RequestID: "request_2", AttemptID: "attempt_2", Sequence: 2,
		ProjectID: "project_1", CostMicrosUSD: ledger.MicrosUSD(9), StartedAt: day2, CompletedAt: day2, Status: "success",
	})}
	manifest, err := ndjsonExporter.Export(fullSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 2 {
		t.Fatalf("manifest=%#v", manifest)
	}
	formats := map[string]bool{}
	for _, entry := range manifest.Files {
		formats[entry.format()] = true
	}
	if !formats[FormatParquet] || !formats[FormatNDJSON] {
		t.Fatalf("expected one parquet and one ndjson partition: %#v", manifest.Files)
	}
	if err := ndjsonExporter.Verify(&fullSnapshot); err != nil {
		t.Fatal(err)
	}
	report, err := ndjsonExporter.Reconcile(fullSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if report.Missing != 0 || report.Duplicates != 0 || report.Extra != 0 {
		t.Fatalf("mixed-format reconciliation=%#v", report)
	}
}

// A manifest entry written before ADR 0017 introduced Format has no such
// field at all; format() must still treat it as Parquet, and Verify must
// still be able to read the partition it points at.
func TestManifestEntryWithNoFormatFieldStillVerifiesAsParquet(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporterWithOptions(root, Options{Format: FormatParquet})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Attempts: []AttemptEvent{{
		EventID: "event_1", RequestID: "request_1", AttemptID: "attempt_1",
		Sequence: 1, ProjectID: "project_1", StartedAt: time.Now().UTC(),
		CompletedAt: time.Now().UTC(), Status: "success",
	}}}
	manifest, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Files[0].Format != FormatParquet {
		t.Fatalf("fresh export unexpectedly left Format unset: %#v", manifest.Files[0])
	}
	manifest.Files[0].Format = ""
	if err := exporter.commitManifest(manifest); err != nil {
		t.Fatal(err)
	}
	if got := manifest.Files[0].format(); got != FormatParquet {
		t.Fatalf("format() = %q, want %q for an unset Format field", got, FormatParquet)
	}
	reloaded, err := exporter.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Files[0].Format != "" {
		t.Fatalf("Format round-tripped as %q, want empty to stay empty on disk", reloaded.Files[0].Format)
	}
	if err := exporter.Verify(&snapshot); err != nil {
		t.Fatalf("Verify rejected a legacy no-Format manifest entry: %v", err)
	}
}
