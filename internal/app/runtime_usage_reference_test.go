package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

func TestUsageMaintenanceReferenceReconstructsTrimmedQueryHistory(t *testing.T) {
	for _, format := range []string{usage.FormatNDJSON, usage.FormatParquet} {
		t.Run(format, func(t *testing.T) {
			r, _ := summaryRuntime(t, 0)
			now := time.Now().UTC()
			seedRetentionUsage(t, r, 2, now.AddDate(0, 0, -60))
			billOneRequest(t, r, "reference-current")
			p := installDiskUsageQueries(t, r)
			e := bindStartupUsageArchive(t, r, format)
			ctx := context.Background()
			if _, err := e.ExportDiskStep(ctx, p, usage.DefaultDiskExportStepLimits()); err != nil {
				t.Fatal(err)
			}
			before, err := p.ReadStats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			out := restoreStartupUsageWindow(t, r, now)
			if out.Window.Attempts != 1 || out.Window.TrimmedAttempts != 2 {
				t.Fatal(out)
			}
			parent := t.TempDir()
			t.Setenv("TMPDIR", parent)
			dateCutoff := now.AddDate(0, 0, -45)
			for round := 0; round < 2; round++ {
				err := r.withUsageMaintenanceReference(ctx, func(ctx context.Context, reference *usage.DiskProjection, archive *usage.Exporter) error {
					paths, globErr := filepath.Glob(filepath.Join(parent, "halro-usage-maintenance-*"))
					if globErr != nil || len(paths) != 1 {
						t.Fatal("private job scratch", paths, globErr)
					}
					stats, err := reference.ReadStats(ctx)
					if err != nil || stats.Window.Attempts != 3 || stats.Window.AttemptAfter != 0 || stats.Totals != before.Totals || stats.Watermark != before.Watermark || reference == p || archive != e {
						t.Fatal("reference reused trimmed history", stats, err)
					}
					// A native roll and new live suffix may follow the frozen
					// private cut without changing this job's historical rows.
					if _, err := r.ledger.Roll(); err != nil {
						return err
					}
					if round == 0 {
						for {
							out, err := archive.PrepareRetainedDateCatalogStep(ctx, dateCutoff, usage.DefaultDateCatalogBuildLimits())
							if err != nil {
								return err
							}
							if out.Phase == "ready" {
								break
							}
						}
						for {
							out, err := archive.VerifyRetainedDateCatalogStep(ctx, reference, dateCutoff, usage.DefaultDateCatalogVerifyLimits())
							if err != nil {
								return err
							}
							if out.Phase == "ready" {
								break
							}
						}
						if _, err := archive.ActivateRetainedDateCatalog(ctx, reference, dateCutoff); err != nil {
							return err
						}
						for {
							if _, _, _, err := prepareUsageDateReclaim(ctx, reference, archive, dateCutoff); err != nil {
								return err
							}
							out, err := archive.ReclaimRetainedDateStep(ctx, reference, dateCutoff, usage.DefaultDateReclaimStepLimits())
							if err != nil {
								return err
							}
							if out.Phase == "ready" {
								break
							}
						}
						for {
							if _, _, _, err := prepareUsageDateReclaim(ctx, reference, archive, dateCutoff); err != nil {
								return err
							}
							out, err := archive.ReclaimRetainedDatePagesStep(ctx, reference, dateCutoff, usage.DefaultDatePageReclaimLimits())
							if err != nil {
								return err
							}
							if out.Phase == "ready" {
								break
							}
						}
						if _, _, _, err := prepareUsageDateReclaim(ctx, reference, archive, dateCutoff); err != nil {
							return err
						}
						completed, err := archive.CompleteRetainedDateObligation(ctx, reference, dateCutoff)
						if err != nil || !completed.OriginalChainComplete {
							return errors.Join(err, errors.New("reference date obligation did not complete"))
						}
						return nil
					}
					if err := archive.ResetPublishedDateCatalogVerification(ctx); err != nil {
						return err
					}
					for {
						out, err := archive.VerifyPublishedDateCatalogStep(ctx, reference, dateCutoff, usage.DefaultDateCatalogVerifyLimits())
						if err != nil {
							return err
						}
						if out.Phase == "ready" {
							return nil
						}
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				paths, err := filepath.Glob(filepath.Join(parent, "halro-usage-maintenance-*"))
				if err != nil || len(paths) != 0 {
					t.Fatal("completed job retained private scratch", paths, err)
				}
			}
			query, err := p.ReadStats(ctx)
			if err != nil || query.Window.Attempts != 1 || query.Totals != before.Totals {
				t.Fatal("maintenance changed query owner", query, err)
			}
			// The helper did not retire the business generation or take a
			// maintenance lease: genuine new accounting remains available.
			billOneRequest(t, r, "reference-after-maintenance")
		})
	}
}

func TestUsageMaintenanceReferenceCloseCancelsAndJoinsRealCallback(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	installDiskUsageQueries(t, r)
	bindStartupUsageArchive(t, r, usage.FormatNDJSON)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	jobDone, closeDone := make(chan error, 1), make(chan error, 1)
	go func() {
		jobDone <- r.withUsageMaintenanceReference(context.Background(), func(ctx context.Context, p *usage.DiskProjection, _ *usage.Exporter) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			if _, err := p.ReadStats(context.Background()); err != nil {
				return err
			}
			_, err := r.ledger.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, nil)
			return err
		})
	}()
	<-entered
	busy := r.withUsageMaintenanceReference(context.Background(), func(context.Context, *usage.DiskProjection, *usage.Exporter) error {
		t.Fatal("second maintenance admitted")
		return nil
	})
	if !errors.Is(busy, usage.ErrDiskUsageBusy) {
		t.Fatal(busy)
	}
	go func() { closeDone <- r.Close() }()
	<-canceled
	select {
	case err := <-closeDone:
		t.Fatal("Close passed active callback", err)
	default:
	}
	if r.usage.owner.nativeKey == ([32]byte{}) {
		t.Fatal("Close cleared borrowed key before callback exit")
	}
	close(release)
	if err := <-jobDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := r.withUsageMaintenanceReference(context.Background(), func(context.Context, *usage.DiskProjection, *usage.Exporter) error {
		t.Fatal("closed owner admitted")
		return nil
	}); err == nil {
		t.Fatal("closed owner remained active")
	}
	if r.usage.owner.nativeKey != ([32]byte{}) {
		t.Fatal("native key not withdrawn")
	}
}

func TestUsageMaintenanceReferenceCanceledBeforeScratch(t *testing.T) {
	r, _ := summaryRuntime(t, 0)
	installDiskUsageQueries(t, r)
	bindStartupUsageArchive(t, r, usage.FormatNDJSON)
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := r.withUsageMaintenanceReference(ctx, func(context.Context, *usage.DiskProjection, *usage.Exporter) error {
		t.Fatal("canceled work invoked")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(parent, "halro-usage-maintenance-*"))
	if err != nil || len(paths) != 0 {
		t.Fatal(paths, err)
	}
}

func TestUsageMaintenanceReferenceFirstRunEmptyHistory(t *testing.T) {
	r, _ := summaryRuntime(t, 0)
	installDiskUsageQueries(t, r)
	bindStartupUsageArchive(t, r, usage.FormatNDJSON)
	err := r.withUsageMaintenanceReference(context.Background(), func(ctx context.Context, p *usage.DiskProjection, _ *usage.Exporter) error {
		stats, err := p.ReadStats(ctx)
		if err != nil || stats.Window.Attempts != 0 || stats.Window.Summaries != 0 || stats.Watermark.Sequence != 0 {
			t.Fatal(stats, err)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
