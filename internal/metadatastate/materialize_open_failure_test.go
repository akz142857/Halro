package metadatastate

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Board §40.11/§40.13: bbolt.Open can fail after the image file exists, as it
// did at real ENOSPC while writing its first pages. bbolt has then closed the
// file, so the failure is an ordinary one: no image, no fee, and nothing that
// would make a backup keep its staging directory as retirement debt.
func TestMetadataImageOpenFailureAfterCreateIsNotRetirement(t *testing.T) {
	b, o, _ := linkedDeltaCandidate(t)
	ctx := context.Background()
	selected, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credit, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	saved := materializeCreate
	t.Cleanup(func() { materializeCreate = saved })
	// A read-only handle makes bbolt's first page write fail inside Open.
	materializeCreate = func(path string, flags int, mode os.FileMode) (*os.File, *ownedfile.Owner, error) {
		file, owner, err := saved(path, flags, mode)
		if err != nil {
			return nil, nil, err
		}
		if err := file.Close(); err != nil {
			return nil, nil, err
		}
		readOnly, err := os.Open(path)
		return readOnly, owner, err
	}
	output := privateSource(t)
	before := o.Budget.Snapshot().Used
	image, err := b.MaterializeMetadata(ctx, output, selected, 16<<20, credit)
	if err == nil || image != nil {
		t.Fatal("image built from a failed Open", err)
	}
	if errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("closed handle reported as uncertain retirement", err)
	}
	if got := o.Budget.Snapshot().Used; got != before {
		t.Fatal("failed Open kept the image fee", before, got)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("failed Open left the image file", err)
	}
}
