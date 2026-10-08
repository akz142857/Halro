package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPackedAuthenticatedLedgerBuildMatchesCompleteAndPermittedNamespaces(t *testing.T) {
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
			out := filepath.Join(t.TempDir(), "packed")
			p, err := BuildPackedAuthenticatedLedgerProjection(ctx, path, testChainKey, out, report, o)
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
				t.Fatal("temporary reducer survived successful build", err)
			}
			if got := o.Budget.Snapshot().Classes[resourcebudget.IO].RAM; got != 0 {
				t.Fatal("scratch/mapping/source workspace leaked", got)
			}
			identity, err := p.store.RootIdentity(ctx)
			if err != nil || identity.Revision != 1 {
				t.Fatal("initial tree retained per-event COW roots", identity, err)
			}
		})
	}
}

func TestPackedAuthenticatedLedgerBuildRetainsActualRenamedScratchOwnerAndFees(t *testing.T) {
	ctx := context.Background()
	path, report, _ := authenticatedLedgerNativeFixture(t)
	o := authenticatedLedgerTestOptions(t)
	out := filepath.Join(t.TempDir(), "packed")
	p, err := buildPackedAuthenticatedLedgerProjection(ctx, path, testChainKey, out, report, o, func(scratch *LedgerProjection) error {
		if err := os.Rename(out+".reducer.db", out+".saved"); err != nil {
			return err
		}
		return os.WriteFile(out+".reducer.db", []byte("foreign"), 0600)
	})
	var debt *packedLedgerBuildDebt
	if p != nil || !errors.As(err, &debt) || !errors.Is(err, ownedfile.ErrRetirement) || debt.scratch == nil || debt.packed == nil {
		t.Fatal("actual source retirement lost owner/debt", p, err)
	}
	if got := o.Budget.Snapshot().Classes[resourcebudget.IO].RAM; got == 0 {
		t.Fatal("mapping/source credit returned before owned bytes retired")
	}
	if raw, err := os.ReadFile(out + ".reducer.db"); err != nil || string(raw) != "foreign" {
		t.Fatal("foreign path changed", string(raw), err)
	}
	// The fixture owns the replacement and returns the ORIGINAL inode so the
	// preserved creation receipt can actually retire it; no new receipt is minted.
	if err := os.Remove(out + ".reducer.db"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(out+".saved", out+".reducer.db"); err != nil {
		t.Fatal(err)
	}
	if err := debt.scratch.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := debt.packed.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(debt.disk.Release(), debt.work.Release()); err != nil {
		t.Fatal(err)
	}
	if got := o.Budget.Snapshot().Used; got != (resourcebudget.Cost{}) {
		t.Fatal("real owner retirement retained stale fees", got)
	}
}

func TestPackedAuthenticatedLedgerBuildPreservesForeignScratchAndRejectsWrongNativeHead(t *testing.T) {
	ctx := context.Background()
	path, report, _ := authenticatedLedgerNativeFixture(t)
	o := authenticatedLedgerTestOptions(t)
	out := filepath.Join(t.TempDir(), "packed")
	if err := os.WriteFile(out+".reducer.db", []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if p, err := BuildPackedAuthenticatedLedgerProjection(ctx, path, testChainKey, out, report, o); err == nil || p != nil {
		t.Fatal("foreign scratch reused", p, err)
	}
	if raw, err := os.ReadFile(out + ".reducer.db"); err != nil || string(raw) != "foreign" {
		t.Fatal("foreign scratch changed", string(raw), err)
	}
	if got := o.Budget.Snapshot().Used; got != (resourcebudget.Cost{}) {
		t.Fatal("refused scratch leaked claims", got)
	}
	out = filepath.Join(t.TempDir(), "packed")
	report.Head.Sequence++
	if p, err := BuildPackedAuthenticatedLedgerProjection(ctx, path, testChainKey, out, report, o); err == nil || p != nil {
		t.Fatal("unselected native head accepted", p, err)
	}
	if got := o.Budget.Snapshot().Used; got != (resourcebudget.Cost{}) {
		t.Fatal("wrong head leaked claims", got)
	}
}
