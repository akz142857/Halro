package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
)

func TestArchiveDateWindowRequiresFreshPublishedComparisonAndKeepsRetainedRows(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e := dateActivationFixture(t, format)
			ctx := context.Background()
			finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
			if e.windowProof != nil {
				t.Fatal("candidate comparison authorized window retention")
			}
			if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); err != nil {
				t.Fatal(err)
			}
			if _, err := e.PruneVerifiedWindowStep(ctx, f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) {
				t.Fatal("activation was a fresh published comparison", err)
			}
			finishPublishedDateVerification(t, e, f.p)
			if e.windowProof == nil || e.windowProof.position.Retention.RetainFromDate != archiveDateCutoff.Format("2006-01-02") {
				t.Fatal("published comparison did not restore scoped window permission")
			}
			before, err := f.p.ReadStats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rootPath := filepath.Join(e.root, "manifest.json")
			root, err := os.ReadFile(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			drainVerifiedWindow(t, f, e)
			after, err := f.p.ReadStats(ctx)
			if err != nil || after.Window.Attempts != 2 || after.Window.Summaries != 1 || after.Totals != before.Totals || after.Metrics != before.Metrics {
				t.Fatal("date scope changed cumulative facts or removed retained rows", before, after, err)
			}
			actual, err := os.ReadFile(rootPath)
			if err != nil || string(root) != string(actual) {
				t.Fatal("window mutation changed date coverage", err)
			}
			// The old full-source comparison no longer matches this trimmed
			// query window. It cannot authorize physical old-data reclamation.
			if _, err := e.ReclaimRetainedDateStep(ctx, f.p, archiveDateCutoff, DefaultDateReclaimStepLimits()); !errors.Is(err, ErrArchiveDateProof) {
				t.Fatal("trimmed query source reused full-history deletion proof", err)
			}
		})
	}
}

func TestArchiveDateWindowPublicationExtendsLateRowsBeyondOriginalBaseline(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f := newDiskFailureFixture(t, windowRequest("first", false, false))
			e, err := NewExporterWithOptions(t.TempDir(), datePolicyTestOptions(f, format))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close() })
			ctx := context.Background()
			if _, err := e.ExportDisk(ctx, f.p, DefaultDiskExportLimits()); err != nil {
				t.Fatal(err)
			}
			finishDateCandidate(t, e, archiveDateCutoff)
			finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
			activation, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff)
			if err != nil {
				t.Fatal(err)
			}
			finishPublishedDateVerification(t, e, f.p)
			drainVerifiedWindow(t, f, e)
			for _, event := range windowRequest("late-old", false, false) {
				appendDiskDetailEvent(t, f, event)
			}
			drainVerifiedWindow(t, f, e)
			unexported, err := f.p.ReadStats(ctx)
			if err != nil || unexported.Window.Attempts != 2 || unexported.Window.Summaries != 1 {
				t.Fatal("late unexported rows were treated as old-policy omissions", unexported, err)
			}
			if _, err := e.ExportDiskStep(ctx, f.p, DefaultDiskExportStepLimits()); err != nil {
				t.Fatal("retained publication could not extend window permission", err)
			}
			position, err := e.ReadArchivePosition(ctx)
			if err != nil || position.Retention != activation.Current.Retention || position.LastSequence != f.a.Watermark().Sequence || e.windowProof == nil {
				t.Fatal("append changed original policy baseline or lost permission", position, err)
			}
			drainVerifiedWindow(t, f, e)
			after, err := f.p.ReadStats(ctx)
			if err != nil || after.Window.Attempts != 0 || after.Window.Summaries != 0 || after.Totals.Attempts != 4 {
				t.Fatal("late exported rows did not drain", after, err)
			}
			// A reopened exporter does not inherit authority from that root.
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			e, err = NewExporterWithOptions(e.root, datePolicyTestOptions(f, format))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.PruneVerifiedWindowStep(ctx, f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) {
				t.Fatal("reopen inherited a saved window permission", err)
			}
		})
	}
}

func TestArchiveDateWindowFailedComparisonDoesNotMintPermission(t *testing.T) {
	for _, fault := range []string{"wrong-key", "missing-required-file", "cancel", "late-save-error"} {
		t.Run(fault, func(t *testing.T) {
			f, e := dateActivationFixture(t, FormatNDJSON)
			ctx := context.Background()
			finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
			if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); err != nil {
				t.Fatal(err)
			}
			if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
				t.Fatal(err)
			}
			callCtx := ctx
			var beforeSave func() error
			switch fault {
			case "wrong-key":
				e.datePolicyKey[0] ^= 1
			case "missing-required-file":
				files, err := e.ReadArchiveFiles(ctx, "", 1)
				if err != nil || len(files.Files) != 1 {
					t.Fatal(files, err)
				}
				if err := os.Remove(filepath.Join(e.root, filepath.FromSlash(files.Files[0].Path))); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				c, cancel := context.WithCancel(ctx)
				cancel()
				callCtx = c
			case "late-save-error":
				beforeSave = func() error { return errors.New("actual checkpoint barrier failed") }
			}
			limits := DefaultDateCatalogVerifyLimits()
			limits.Files, limits.Source.Rows, limits.Source.PageRows = 16, 128, 128
			_, err := e.verifyDateCatalogStep(callCtx, f.p, archiveDateCutoff, limits, true, beforeSave)
			if err == nil || e.windowProof != nil || e.windowPending != nil {
				t.Fatal("failed comparison minted permission", fault, err)
			}
			if _, err := e.PruneVerifiedWindowStep(ctx, f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) {
				t.Fatal("failed comparison authorized window removal", err)
			}
		})
	}
}

// A fresh query DB replays actual bytes from the SAME native/selected Ledger.
// It never restores a saved window floor or grants trust to the previous DB.
func rebuildDateWindowProjection(t *testing.T, f *diskFailureFixture) *DiskProjection {
	t.Helper()
	d, p, err := NewDiskLedgerDerivative(f.owner, f.p.db.Path()+".rebuilt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); _ = p.Close() })
	if _, err := f.native.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, d.Apply); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestArchiveDateWindowRebuildMustRecompareAndReapplyWindow(t *testing.T) {
	f, e := dateActivationFixture(t, FormatNDJSON)
	ctx := context.Background()
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); err != nil {
		t.Fatal(err)
	}
	finishPublishedDateVerification(t, e, f.p)
	drainVerifiedWindow(t, f, e)
	f.p = rebuildDateWindowProjection(t, f)
	if _, err := e.PruneVerifiedWindowStep(ctx, f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) {
		t.Fatal("different generation reused previous permission", err)
	}
	if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
		t.Fatal(err)
	}
	finishPublishedDateVerification(t, e, f.p)
	drainVerifiedWindow(t, f, e)
	stats, err := f.p.ReadStats(ctx)
	if err != nil || stats.Window.Attempts != 2 || stats.Window.Summaries != 1 || stats.Totals.Attempts != 4 {
		t.Fatal("fresh native rebuild did not restore the configured window", stats, err)
	}
	if _, err := e.VerifyPublishedDateCatalogStep(ctx, f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits()); !errors.Is(err, ErrDiskUsageCursor) {
		t.Fatal("trimmed query DB pretended to contain the full historical source", err)
	}
}
