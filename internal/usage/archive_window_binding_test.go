package usage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

func windowBindingFixture(t *testing.T) (*diskFailureFixture, *Exporter, *DiskProjection) {
	t.Helper()
	f := newDiskFailureFixture(t, append(windowRequest("bind-old", false, false), windowRequest("bind-current", true, true)...))
	d, target, err := NewDiskLedgerDerivative(f.owner, filepath.Join(t.TempDir(), "query.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); _ = target.Close() })
	if _, err := f.native.ReplayAuthenticated(context.Background(), ledger.ReplayAnchor{}, d.Apply); err != nil {
		t.Fatal(err)
	}
	e, err := NewExporterWithOptions(t.TempDir(), datePolicyTestOptions(f, FormatNDJSON))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := target.pruneWindowStep(context.Background(), windowTestCutoff, f.a.Watermark().Sequence, DiskWindowLimits{Rows: 128, InputBytes: 8 << 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits()); err != nil {
		t.Fatal(err)
	}
	return f, e, target
}

func TestArchiveWindowBindingNativeAnchorsAndTrimmedTarget(t *testing.T) {
	f, e, target := windowBindingFixture(t)
	ctx := context.Background()
	before, err := target.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Window.AttemptAfter == 0 {
		t.Fatal("target was not trimmed")
	}
	if err := e.BindVerifiedWindow(ctx, f.p, target); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PruneVerifiedWindowStep(ctx, target, windowTestCutoff, DiskWindowLimits{Rows: 128, InputBytes: 8 << 20}); err != nil {
		t.Fatal("bound proof depended on closed private reference", err)
	}
	after, err := target.ReadStats(ctx)
	if err != nil || after.Totals != before.Totals || after.Metrics != before.Metrics {
		t.Fatal(after, err)
	}
	if _, err := e.PruneVerifiedWindowStep(ctx, f.p, windowTestCutoff, windowTestLimits); err == nil {
		t.Fatal("reference retained transferred permission")
	}
}

func TestArchiveWindowBindingRefusesWrongNativeRootAndLifetime(t *testing.T) {
	for _, fault := range []string{"target-hash", "resolver", "reference-trimmed", "root-inode", "no-proof", "closed-target", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			f, e, target := windowBindingFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch fault {
			case "target-hash":
				if err := target.db.Update(func(tx *bbolt.Tx) error {
					state, _, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
					if err != nil {
						return err
					}
					state.ReplayHash[0] ^= 1
					return diskUsagePut(tx, "meta", []byte("state"), state)
				}); err != nil {
					t.Fatal(err)
				}
			case "resolver":
				original := e.datePolicyPrefix
				e.datePolicyPrefix = func(ctx context.Context, head ledger.Watermark) (ledger.ProjectionPrefix, error) {
					prefix, err := original(ctx, head)
					prefix.Hash[0] ^= 1
					return prefix, err
				}
			case "reference-trimmed":
				if _, err := f.p.pruneWindowStep(ctx, windowTestCutoff, f.a.Watermark().Sequence, DiskWindowLimits{Rows: 128, InputBytes: 8 << 20}); err != nil {
					t.Fatal(err)
				}
			case "root-inode":
				path := filepath.Join(e.root, "manifest.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".next", raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".next", path); err != nil {
					t.Fatal(err)
				}
			case "no-proof":
				e.RevokeWindowAuthority()
			case "closed-target":
				if err := target.Close(); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			}
			if err := e.BindVerifiedWindow(ctx, f.p, target); err == nil {
				t.Fatal("invalid binding accepted")
			}
			if e.windowProof != nil || e.windowPending != nil {
				t.Fatal("failed binding kept permission")
			}
			_, err := e.PruneVerifiedWindowStep(context.Background(), target, windowTestCutoff, windowTestLimits)
			if !errors.Is(err, ErrArchiveWindowProof) {
				t.Fatal("failure allowed target trimming", err)
			}
		})
	}
}
