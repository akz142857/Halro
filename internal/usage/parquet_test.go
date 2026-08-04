package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/ledger"
	"github.com/parquet-go/parquet-go"
)

func TestExporterPublishesVerifiableIdempotentPartitions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	firstDay := time.Date(2026, 7, 30, 23, 59, 0, 0, time.UTC)
	snapshot := Snapshot{Attempts: []AttemptEvent{
		{
			EventID: "event_1", RequestID: "request_1", AttemptID: "attempt_1",
			Sequence: 4, AttemptNumber: 1, ProjectID: "project_1", KeyID: "key_1",
			RouteID: "route_1", ProviderID: "provider_1", RequestedModel: "chat",
			ProviderModel: "model_1", ProviderInputTokens: 3, ProviderOutputTokens: 2,
			CostMicrosUSD: ledger.MicrosUSD(7), StartedAt: firstDay.Add(-time.Second),
			CompletedAt: firstDay, Status: "success", HTTPStatus: 200,
			LatencyMillis: 1000,
		},
		{
			EventID: "event_2", RequestID: "request_2", AttemptID: "attempt_2",
			Sequence: 9, AttemptNumber: 2, ProjectID: "project_1", KeyID: "key_1",
			RouteID: "route_2", ProviderID: "provider_2", RequestedModel: "chat",
			ProviderModel: "model_2", ProviderInputTokens: 5, ProviderOutputTokens: 4,
			CostMicrosUSD: ledger.MicrosUSD(11), StartedAt: firstDay.Add(time.Minute),
			CompletedAt: firstDay.Add(time.Minute + time.Second), Status: "error",
			ErrorClass: "upstream_5xx", HTTPStatus: 503, LatencyMillis: 1000,
			RetryCount: 1, FallbackCount: 1,
		},
	}}
	manifest, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.LastSequence != 9 || len(manifest.Files) != 2 {
		t.Fatalf("manifest=%#v", manifest)
	}
	if err := exporter.Verify(&snapshot); err != nil {
		t.Fatal(err)
	}
	report, err := exporter.Reconcile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if report.LedgerRecords != 2 || report.ParquetRecords != 2 ||
		report.Missing != 0 || report.Duplicates != 0 || report.Extra != 0 {
		t.Fatalf("reconciliation=%#v", report)
	}
	repeated, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.Files) != 2 || repeated.LastSequence != manifest.LastSequence {
		t.Fatalf("idempotent export changed manifest: %#v", repeated)
	}
	for _, entry := range manifest.Files {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("unexpected parquet permissions: %o", info.Mode().Perm())
		}
	}
}

func TestExporterReconciliationRejectsMissingAndDuplicateEventIDs(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	t.Run("missing", func(t *testing.T) {
		exporter, err := NewExporter(filepath.Join(t.TempDir(), "usage"))
		if err != nil {
			t.Fatal(err)
		}
		exported := Snapshot{Attempts: []AttemptEvent{
			{EventID: "event_1", RequestID: "r1", AttemptID: "a1", Sequence: 1, ProjectID: "p", StartedAt: now, CompletedAt: now, Status: "success"},
			{EventID: "event_3", RequestID: "r3", AttemptID: "a3", Sequence: 3, ProjectID: "p", StartedAt: now, CompletedAt: now, Status: "success"},
		}}
		if _, err := exporter.Export(exported); err != nil {
			t.Fatal(err)
		}
		ledgerSnapshot := exported
		ledgerSnapshot.Attempts = append(ledgerSnapshot.Attempts,
			AttemptEvent{EventID: "event_2", RequestID: "r2", AttemptID: "a2", Sequence: 2, ProjectID: "p", StartedAt: now, CompletedAt: now, Status: "success"})
		if _, err := exporter.Reconcile(ledgerSnapshot); err == nil {
			t.Fatal("missing Parquet EventID was accepted")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		exporter, err := NewExporter(filepath.Join(t.TempDir(), "usage"))
		if err != nil {
			t.Fatal(err)
		}
		snapshot := Snapshot{Attempts: []AttemptEvent{
			{EventID: "duplicate", RequestID: "r1", AttemptID: "a1", Sequence: 1, ProjectID: "p", StartedAt: now, CompletedAt: now, Status: "success"},
			{EventID: "duplicate", RequestID: "r2", AttemptID: "a2", Sequence: 2, ProjectID: "p", StartedAt: now, CompletedAt: now, Status: "success"},
		}}
		if _, err := exporter.Export(snapshot); err != nil {
			t.Fatal(err)
		}
		if _, err := exporter.Reconcile(snapshot); err == nil {
			t.Fatal("duplicate Parquet EventID was accepted")
		}
	})
}

func TestExporterPublishesAndVerifiesEmptyManifest(t *testing.T) {
	exporter, err := NewExporter(filepath.Join(t.TempDir(), "usage"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := exporter.Export(Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != parquetSchemaVersion || manifest.LastSequence != 0 {
		t.Fatalf("manifest=%#v", manifest)
	}
	if err := exporter.Verify(nil); err != nil {
		t.Fatal(err)
	}
}

func TestExporterDetectsParquetTampering(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
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
	if err := os.WriteFile(path, []byte("not parquet"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exporter.Verify(&snapshot); err == nil {
		t.Fatal("expected tampering to be detected")
	}
}

func TestExporterKeepsSettlementImmutableAndPublishesIndependentAdjustments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	snapshot := Snapshot{Attempts: []AttemptEvent{{EventID: "settle_1", RequestID: "req_1", AttemptID: "att_1", Sequence: 1, ProjectID: "p",
		OriginalCostMicrosUSD: ledger.MicrosUSD(7), FinalCostMicrosUSD: ledger.MicrosUSD(9), CostMicrosUSD: ledger.MicrosUSD(9), CompletedAt: now}},
		Adjustments: []CostAdjustmentEvent{{EventID: "adjust_1", Sequence: 2, RequestID: "req_1", AttemptID: "att_1", ProjectID: "p", Mode: ledger.AdjustmentModeExplicit,
			AdjustmentSequence: 1, IdempotencyKeyDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BaseCostMicrosUSD: 7, NetCostBeforeMicrosUSD: 7, DeltaMicrosUSD: 2, NetCostAfterMicrosUSD: 9,
			ServicePeriodID: "2026-08-04", OriginalCompletedAt: now, PostedPeriodID: "2026-08-05", PostedAt: now.Add(24 * time.Hour), ReasonCode: "invoice_difference", EvidenceDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", CreatedBy: "admin"}}}
	manifest, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].CostMicrosUSD != 7 {
		t.Fatalf("settlement manifest=%#v", manifest)
	}
	adjustments, err := exporter.LoadAdjustmentManifest()
	if err != nil || len(adjustments.Files) != 1 || adjustments.Files[0].DeltaMicrosUSD != 2 || adjustments.LastSequence != 2 {
		t.Fatalf("adjustments=%#v err=%v", adjustments, err)
	}
	if err := exporter.Verify(&snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestExporterDualReadsAndDeterministicallyUpgradesV2Manifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	date := "2026-08-04"
	relative := filepath.Join("date="+date, "usage-v2.parquet")
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writer := parquet.NewGenericWriter[parquetAttemptV2](file)
	row := parquetAttemptV2{SchemaVersion: 2, EventID: "legacy_v2", RequestID: "r", AttemptID: "a", Sequence: 1, ProjectID: "p", ProviderInputTokens: 3, ProviderOutputTokens: 2, CostMicrosUSD: 7, CompletedAtMicros: time.Now().UTC().UnixMicro()}
	if _, err = writer.Write([]parquetAttemptV2{row}); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	checksum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := Manifest{SchemaVersion: 2, LastSequence: 1, Files: []ManifestFile{{Path: filepath.ToSlash(relative), Date: date, SHA256: checksum, MinSequence: 1, MaxSequence: 1, Records: 1, InputTokens: 3, OutputTokens: 2, CostMicrosUSD: 7}}}
	if err = exporter.commitManifest(legacy); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Attempts: []AttemptEvent{{EventID: "legacy_v2", Sequence: 1, AttemptID: "a", ProjectID: "p", ProviderInputTokens: 3, ProviderOutputTokens: 2, CostMicrosUSD: ledger.MicrosUSD(7)}}}
	if err = exporter.Verify(&snapshot); err != nil {
		t.Fatal(err)
	}
	upgraded, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.SchemaVersion != 3 || upgraded.Files[0].SchemaVersion != 2 {
		t.Fatalf("upgraded=%#v", upgraded)
	}
	if err = exporter.Verify(&snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestExporterPrunesOnlyExpiredPartitions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	snapshot := Snapshot{Attempts: []AttemptEvent{
		{EventID: "old", RequestID: "r1", AttemptID: "a1", Sequence: 2,
			ProjectID: "p", StartedAt: old, CompletedAt: old, Status: "success"},
		{EventID: "recent", RequestID: "r2", AttemptID: "a2", Sequence: 5,
			ProjectID: "p", StartedAt: recent, CompletedAt: recent, Status: "success"},
	}}
	manifest, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(root, filepath.FromSlash(manifest.Files[0].Path))
	report, err := exporter.PruneBefore(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if report.FilesRemoved != 1 || report.RowsRemoved != 1 {
		t.Fatalf("report=%#v", report)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expired parquet still exists: %v", err)
	}
	if err := exporter.Verify(&snapshot); err != nil {
		t.Fatal(err)
	}
}
