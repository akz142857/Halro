package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFixedScratchPackedUsageAllNamespacesMatchOrdinary(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		late   bool
		prefix int
	}{{"ordinary", false, 0}, {"late", true, 0}, {"excluded-tail", true, 3}, {"empty", false, -1}} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newTypedUsageFixtureAt(t, scenario.late, scenario.prefix)
			o := f.options
			o.ScratchMaxDiskBytes = 32 << 20
			o.ScratchPath = filepath.Join(t.TempDir(), "fixed.reducer")
			before := o.Budget.Snapshot().Used
			packed, ref, err := buildFixedPackedTypedUsageProjection(context.Background(), o, f.lease, false)
			if err != nil {
				t.Fatal(err)
			}
			defer packed.Discard()
			ordinary, err := CreateDiskProjection(filepath.Join(t.TempDir(), "ordinary.db"), f.owner)
			if err != nil {
				t.Fatal(err)
			}
			defer ordinary.Discard()
			for _, r := range f.records {
				if r.Sequence > ref.Ledger.Anchor.Head.Sequence {
					continue
				}
				if err := ordinary.ApplyContext(context.Background(), r); err != nil {
					t.Fatal(err)
				}
			}
			if err := alignPackedUsageBoundary(context.Background(), ordinary, ref.Ledger.Anchor); err != nil {
				t.Fatal(err)
			}
			compareTypedUsageRows(t, packed, ordinary)
			if _, err := os.Lstat(o.ScratchPath); !os.IsNotExist(err) {
				t.Fatal("actual scratch survived", err)
			}
			if err := packed.Discard(); err != nil {
				t.Fatal(err)
			}
			if after := o.Budget.Snapshot().Used; after != before {
				t.Fatal("fees survived actual retirement", before, after)
			}
		})
	}
}
