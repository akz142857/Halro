package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/usage"
)

func TestUsageMaintenanceExportsSelectedDiskWithoutAggregate(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	r.stopBackgroundWork()
	seedLedgerActivity(t, r, "disk_export_native")
	installDiskUsageQueries(t, r)
	r.exportUsageParquet()
	manifest, err := r.usageExporter.ReadArchivePosition(context.Background())
	stats, statsErr := r.usage.disk.ReadStats(context.Background())
	if err != nil || statsErr != nil || !manifest.CompletePrefix || manifest.LastSequence != stats.Watermark.Sequence || manifest.Files == 0 {
		t.Fatal(manifest, err, statsErr)
	}
	if r.usage.Aggregate != nil {
		t.Fatal("export borrowed resident history")
	}
	if err := r.usageExporter.VerifyArchiveFileChecksums(context.Background()); err != nil {
		t.Fatal(err)
	}
	verified, verifyErr := r.usageExporter.VerifyDiskArchive(context.Background(), r.usage.disk, usage.DefaultArchiveVerifyLimits())
	if verifyErr != nil || verified.ArchiveRows == 0 || verified.ArchiveRows != verified.SourceRows || verified.ThroughSequence != manifest.LastSequence || verified.SourceHead != stats.Watermark {
		t.Fatal("selected physical archive did not reconcile", verified, verifyErr)
	}
	if _, err := r.usageExporter.LoadManifest(); !errors.Is(err, usage.ErrArchivePagedCatalog) {
		t.Fatal("legacy reader accepted paged catalog", err)
	}
	// A fresh derivative/export is not a checkpoint or a compaction permission.
	if through, ok := r.ledgerArchivedThrough(); ok || through != 0 {
		t.Fatal(through, ok)
	}
	before, err := os.ReadFile(filepath.Join(r.config.UsagePath(), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.pruneUsageWindow()
	r.pruneUsageArchive()
	after, err := os.ReadFile(filepath.Join(r.config.UsagePath(), "manifest.json"))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("unfinished disk retention changed archive", err)
	}
	// Source retirement fails closed and preserves the published prefix.
	if err := r.usage.disk.Close(); err != nil {
		t.Fatal(err)
	}
	r.exportUsageParquet()
	position, err := r.usageExporter.ReadArchivePosition(context.Background())
	if err != nil || position.LastSequence != manifest.LastSequence {
		t.Fatal(position, err)
	}
}

func TestUsageMaintenanceReplicaDoesNotExportDiskNativeTail(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	r.stopBackgroundWork()
	installDiskUsageQueries(t, r)
	before, err := r.usageExporter.ReadArchivePosition(context.Background())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	seedLedgerActivity(t, r, "unapplied_replica_export_tail")
	r.replication = &replicationRuntime{role: replication.RoleReplica}
	defer func() { r.replication = nil }()
	r.exportUsageParquet()
	after, afterErr := r.usageExporter.ReadArchivePosition(context.Background())
	if !reflect.DeepEqual(before, after) || os.IsNotExist(err) != os.IsNotExist(afterErr) {
		t.Fatal("Replica exported durable tail", before, after, err, afterErr)
	}
	if _, err := r.usageExporter.ExportDisk(context.Background(), nil, usage.DefaultDiskExportLimits()); err == nil {
		t.Fatal("missing owner accepted")
	}
}

func TestStandaloneStartupAcceptsPagedArchiveAndReleasesOwners(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	cfg, logger := r.config, r.logger
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	exporter, err := usage.NewExporter(cfg.UsagePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.MigrateArchiveCatalog(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(cfg.UsagePath(), "manifest.json")
	before, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		opened, err := Open(context.Background(), cfg, logger)
		if err != nil {
			t.Fatal("default disk startup refused authenticated paged archive", err)
		}
		if opened.usage.Aggregate != nil || opened.usage.disk == nil {
			opened.Close()
			t.Fatal("default startup retained Aggregate")
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(rootPath)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("startup rewrote paged archive", err)
	}
	if err := exporter.VerifyArchiveFileChecksums(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUsageMaintenanceDiskFiniteStepsAdvanceAndFinishBacklog(t *testing.T) {
	r, _ := summaryRuntime(t, 0)
	r.stopBackgroundWork()
	for i := 0; i < 259; i++ {
		request, err := r.accounting.BeginRequestDetailed(context.Background(), "project_1", "key_1", fmt.Sprintf("maintenance_step_%d", i), "chat")
		if err != nil {
			t.Fatal(err)
		}
		// The small billOneRequest fixture uses a 1000-micro daily cap. This backlog
		// fixture must supply its own policy; production admission is unchanged.
		attempt, err := r.accounting.ReserveAttemptDetailed(context.Background(), request, 100_000_000, 100, budget.AttemptMetadata{RouteID: "route_1", ProviderID: "provider_1", ProviderModel: "model_1", AttemptNumber: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.MarkStarted(context.Background(), attempt); err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Settle(context.Background(), attempt, budget.Settlement{CommittedMicrosUSD: 90, ProviderInputTokens: 7, ProviderOutputTokens: 3, Outcome: "success", LatencyMillis: 12}); err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Finalize(context.Background(), request, "success"); err != nil {
			t.Fatal(err)
		}
	}
	installDiskUsageQueries(t, r)
	stats, err := r.usage.disk.ReadStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.exportUsageParquet()
	first, err := r.usageExporter.ReadArchivePosition(context.Background())
	if err != nil || !first.CompletePrefix || first.LastSequence == 0 || first.LastSequence >= stats.Watermark.Sequence {
		t.Fatal("one maintenance job credited entire backlog", first, err)
	}
	verified, err := r.usageExporter.VerifyDiskArchive(context.Background(), r.usage.disk, usage.DefaultArchiveVerifyLimits())
	if err != nil || verified.ArchiveRows != 256 || verified.SourceHead != stats.Watermark || verified.ThroughSequence != first.LastSequence {
		t.Fatal(verified, err)
	}
	r.exportUsageParquet()
	final, err := r.usageExporter.ReadArchivePosition(context.Background())
	if err != nil || final.LastSequence != stats.Watermark.Sequence || !final.CompletePrefix {
		t.Fatal(final, err)
	}
	verified, err = r.usageExporter.VerifyDiskArchive(context.Background(), r.usage.disk, usage.DefaultArchiveVerifyLimits())
	if err != nil || verified.ArchiveRows != 259 || verified.SourceRows != 259 || r.usage.Aggregate != nil {
		t.Fatal(verified, err)
	}
	if through, ok := r.ledgerArchivedThrough(); ok || through != 0 {
		t.Fatal("finite steps authorized native trim", through, ok)
	}
}
