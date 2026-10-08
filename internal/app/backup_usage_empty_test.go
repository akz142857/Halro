package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

func TestBackupUnpublishedUsageAuthenticatesNativeWithoutReducers(t *testing.T) {
	for _, fault := range []string{"empty", "wrong-key", "wrong-head", "orphan", "symlink", "canceled"} {
		t.Run(fault, func(t *testing.T) {
			cfg := usageReferenceBudgetFixture(t)
			ctx, pressure, err := commandBudgetContext(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			log, store, key := openSealingLedger(t, cfg)
			defer log.Close()
			defer store.Close()
			defer clear(key)
			expected, partial, err := ledger.InspectReplayAuthenticated(cfg.LedgerPath(), key, nil)
			if err != nil || partial {
				t.Fatal(expected, partial, err)
			}
			root, parent := t.TempDir(), t.TempDir()
			t.Setenv("TMPDIR", parent)
			actualKey := key
			switch fault {
			case "wrong-key":
				actualKey = bytes.Repeat([]byte{0x98}, 32)
			case "wrong-head":
				expected.Head.Sequence++
			case "orphan":
				if err := os.WriteFile(filepath.Join(root, "orphan.ndjson"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				root = alias
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			position, err := withBackupUsageReference(ctx, cfg, root, cfg.LedgerPath(), actualKey, expected, cfg.ResourceBudget.UsageArchiveDiskBytes, pressure, func(*usage.Exporter, usage.ArchivePosition) error {
				t.Fatal("unpublished archive supplied work authority")
				return nil
			})
			if fault == "empty" && err != nil || fault != "empty" && err == nil || position != (usage.ArchivePosition{}) {
				t.Fatal(position, err)
			}
			if fault == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if s := pressure.logicalBudget().Snapshot(); s.Used != (resourcebudget.Cost{}) {
				t.Fatal("reference/archive fee survived known retirement", s)
			}
			entries, readErr := os.ReadDir(parent)
			if readErr != nil || len(entries) != 0 {
				t.Fatal("unpublished backup allocated reducers", entries, readErr)
			}
			if fault == "orphan" {
				raw, err := os.ReadFile(filepath.Join(root, "orphan.ndjson"))
				if err != nil || string(raw) != "preserve" {
					t.Fatal("orphan changed", err)
				}
			}
		})
	}
}
