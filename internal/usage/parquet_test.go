package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
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

func TestManifestBelowTheReadableRangeIsRefused(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	// Schema 2 partitions used a different column set and were read through a
	// second struct with its own verify path. Two shapes for one thing is the
	// cost; refusing the older shape outright is what removes it. The refusal
	// has to be explicit rather than a misparse, so that an operator who
	// somehow has one is told why.
	stale := Manifest{SchemaVersion: parquetSchemaMinReadable - 1, LastSequence: 0}
	if err := exporter.commitManifest(stale); err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.Export(Snapshot{}); err == nil {
		t.Fatal("a manifest below the readable range must be refused, not upgraded")
	}
	if err := exporter.Verify(nil); err == nil {
		t.Fatal("verification must refuse a manifest below the readable range")
	}
}

// Every install running the previous release carries a schema 3 manifest, so the
// token-tier bump has to open and upgrade one rather than refuse it. Gating on
// an explicit {2, current} pair would reject exactly the installs being upgraded,
// and the failure would surface as a dead usage pipeline rather than a migration
// error.
func TestExporterUpgradesPreviousSchemaManifestRatherThanRejectingIt(t *testing.T) {
	for previous := parquetSchemaMinReadable; previous < parquetSchemaVersion; previous++ {
		root := filepath.Join(t.TempDir(), "usage")
		exporter, err := NewExporter(root)
		if err != nil {
			t.Fatal(err)
		}
		stale := Manifest{SchemaVersion: previous, LastSequence: 0}
		if err := exporter.commitManifest(stale); err != nil {
			t.Fatal(err)
		}
		snapshot := Snapshot{Attempts: []AttemptEvent{{
			EventID: "tiered", Sequence: 1, AttemptID: "a", ProjectID: "p",
			ProviderInputTokens: 100, ProviderOutputTokens: 10,
			ProviderCachedInputTokens: 80, ProviderCacheWriteInputTokens: 5,
			ProviderReasoningTokens: 4, CostMicrosUSD: ledger.MicrosUSD(7),
		}}}
		upgraded, err := exporter.Export(snapshot)
		if err != nil {
			t.Fatalf("schema %d manifest was rejected instead of upgraded: %v", previous, err)
		}
		if upgraded.SchemaVersion != parquetSchemaVersion {
			t.Fatalf("schema %d manifest upgraded to %d", previous, upgraded.SchemaVersion)
		}
		if err := exporter.Verify(&snapshot); err != nil {
			t.Fatalf("schema %d upgrade did not verify: %v", previous, err)
		}
	}
}

// The tiers are a breakdown of the totals, so a round trip must return them
// intact rather than folding them in or dropping them.
func TestExportedRowsPreserveProviderTokenTiers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Attempts: []AttemptEvent{{
		EventID: "tiered", Sequence: 1, AttemptID: "a", ProjectID: "p",
		ProviderInputTokens: 100, ProviderOutputTokens: 10,
		ProviderCachedInputTokens: 80, ProviderCacheWriteInputTokens: 5,
		ProviderReasoningTokens: 4, CostMicrosUSD: ledger.MicrosUSD(7),
	}}}
	manifest, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 {
		t.Fatalf("manifest files=%d", len(manifest.Files))
	}
	rows, err := readAttemptRows(
		filepath.Join(root, filepath.FromSlash(manifest.Files[0].Path)),
		manifest.Files[0].Format,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	row := rows[0]
	if row.ProviderCachedInputTokens != 80 || row.ProviderCacheWriteInputTokens != 5 || row.ProviderReasoningTokens != 4 {
		t.Fatalf("tiers were not round-tripped: %#v", row)
	}
	// The tiers partition the input total; a reader that summed them onto it
	// would double-count the cached span.
	if row.ProviderInputTokens != 100 {
		t.Fatalf("input total changed to %d", row.ProviderInputTokens)
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

// TestVerifyAcceptsRowsWrittenAtThePreviousSchema is the shape a real install
// has and the shape the fixtures did not: partitions are never rewritten, so
// after a schema bump every row already on disk still carries the version it
// was written under. The upgrade test above starts from an empty manifest and
// then writes fresh rows, so every row it verifies is at the current version —
// it exercises the manifest gate and never the row check. Demanding the
// current version per row turned a bump into a failed verification for exactly
// the installs that have history, while a brand new one passed.
func TestVerifyAcceptsRowsWrittenAtThePreviousSchema(t *testing.T) {
	root := filepath.Join(t.TempDir(), "usage")
	exporter, err := NewExporter(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Attempts: []AttemptEvent{{
		EventID: "event_1", RequestID: "request_1", AttemptID: "attempt_1",
		Sequence: 1, ProjectID: "project_1", StartedAt: time.Now().UTC(),
		CompletedAt: time.Now().UTC(), Status: "success",
		ProviderInputTokens: 100, ProviderOutputTokens: 10,
	}}}
	manifest, err := exporter.Export(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	// Rewrite the partition as the previous schema wrote it: same rows, older
	// version stamp, and no tier columns populated.
	path := filepath.Join(root, filepath.FromSlash(manifest.Files[0].Path))
	rows, err := readAttemptRows(path, manifest.Files[0].format())
	if err != nil {
		t.Fatal(err)
	}
	for index := range rows {
		rows[index].SchemaVersion = parquetSchemaVersion - 1
		rows[index].ProviderCachedInputTokens = 0
		rows[index].ProviderCacheWriteInputTokens = 0
		rows[index].ProviderReasoningTokens = 0
	}
	if err := writeParquetAtomic(path, rows); err != nil {
		t.Fatal(err)
	}
	checksum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Files[0].SHA256 = checksum
	manifest.Files[0].SchemaVersion = parquetSchemaVersion - 1
	if err := exporter.commitManifest(manifest); err != nil {
		t.Fatal(err)
	}

	if err := exporter.Verify(&snapshot); err != nil {
		t.Fatalf("rows written at schema %d must still verify: %v", parquetSchemaVersion-1, err)
	}
}
