package usage

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
)

func TestPackedSelectedUsageReferenceRejectsFreshOwnerBeforeCreatingFiles(t *testing.T) {
	f := newTypedUsageFixtureAt(t, false, 0)
	before := f.options.Budget.Snapshot().Used
	p, _, err := BuildPackedSelectedUsageReference(context.Background(), f.options, f.lease)
	if p != nil || !errors.Is(err, ledger.ErrProjectionSource) {
		t.Fatal("fresh owner obtained selected complete source", p, err)
	}
	for _, path := range []string{f.options.RootDirectory, f.options.ScratchPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("unqualified factory created candidate", path, err)
		}
	}
	if after := f.options.Budget.Snapshot().Used; after != before {
		t.Fatal("refused reference fees survived", before, after)
	}
}
