package ledger

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestFixedScratchPackedLedgerCompletePrefixAndGenesisMatchOrdinary(t *testing.T) {
	ctx := context.Background()
	path, report, records := authenticatedLedgerNativeFixture(t)
	for _, mode := range []string{"complete", "prefix", "genesis"} {
		t.Run(mode, func(t *testing.T) {
			o := authenticatedLedgerTestOptions(t)
			if mode == "prefix" {
				r := records[len(records)/2]
				o.Prefix = &ProjectionPrefix{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash}
			}
			if mode == "genesis" {
				o.Prefix = &ProjectionPrefix{}
			}
			out := filepath.Join(t.TempDir(), "fixed")
			p, err := buildFixedPackedAuthenticatedLedgerProjection(ctx, path, testChainKey, out, report, o)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Discard()
			var reference *LedgerProjection
			if o.Prefix == nil {
				reference, err = BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "reference.db"), report)
			} else {
				reference, err = BuildLedgerProjectionPrefix(ctx, path, testChainKey, filepath.Join(t.TempDir(), "reference.db"), report, *o.Prefix)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer reference.Discard()
			compareAuthenticatedLedgerRows(t, p, reference)
			if _, err := os.Lstat(out + ".reducer.db"); !os.IsNotExist(err) {
				t.Fatal("actual scratch survived", err)
			}
			if got := o.Budget.Snapshot().Classes[resourcebudget.IO].RAM; got != 0 {
				t.Fatal("scratch workspace survived known retirement", got)
			}
		})
	}
}
