package metadatastate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	bbolt "go.etcd.io/bbolt"
)

func TestMetadataImageCompleteBinaryBucketsDeltaAndTrimMatchSelectedRoot(t *testing.T) {
	b, o, _ := linkedDeltaCandidate(t)
	ctx := context.Background()
	if _, err := b.AppendFrame(ctx, []metadatajournal.Op{
		{Kind: metadatajournal.OpCreateBucket, Path: []string{"deployment_price_timeline", "nul\x00/name"}},
		{Kind: metadatajournal.OpPut, Path: []string{"deployment_price_timeline", "nul\x00/name"}, Key: []byte("k\x00/"), Value: []byte{}},
		{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("after_epoch"), Value: []byte("new-value")},
	}); err != nil {
		t.Fatal(err)
	}
	head, err := b.EpochHead()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.TrimAtApplied(ctx, head); err != nil {
		t.Fatal(err)
	}
	selected, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credit, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer credit.Release()
	output := privateSource(t)
	image, err := b.MaterializeMetadata(ctx, output, selected, 16<<20, credit)
	if err != nil {
		t.Fatal(err)
	}
	defer image.Discard()
	db, err := bbolt.Open(output, 0o600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []authenticatedstate.Row
	err = db.View(func(tx *bbolt.Tx) error {
		iterator := newBaselineIterator(ctx, tx)
		for {
			row, found, err := iterator.next()
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			got = append(got, authenticatedstate.Row{Key: bytes.Clone(row.Key), Value: bytes.Clone(row.Value)})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	want := deltaRows(t, b)
	want = want[:len(want)-1] // private provenance is not a bbolt row
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("whole-file content differs: rows %d vs %d", len(want), len(got))
	}
	if err := image.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("candidate survived removal", err)
	}
}

func TestMetadataImageActualReaderCloseDebtBlocksDiscardAndPublication(t *testing.T) {
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
	image, err := b.MaterializeMetadata(ctx, privateSource(t), selected, 16<<20, credit)
	if err != nil {
		t.Fatal(err)
	}
	if err := credit.Release(); err != nil {
		t.Fatal(err)
	}
	before := o.Budget.Snapshot().Used
	_, _, err = image.verifyFileWithOpen(ctx, image.digest, image.size, func(path string) (*os.File, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return file, nil
	})
	if !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("actual reader close debt disappeared", err)
	}
	for n := 0; n < 2; n++ {
		if err := image.Discard(); !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("failed reader licensed inode deletion", err)
		}
		if err := image.Publish(ctx, image.path+".published", nil, nil, selected); !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("unknown reader licensed publication", err)
		}
		if o.Budget.Snapshot().Used != before {
			t.Fatal("reader debt refunded image fee", before, o.Budget.Snapshot())
		}
	}
	if _, err := os.Stat(image.path); err != nil {
		t.Fatal("unknown reader inode removed", err)
	}
}

func TestMetadataImageRefusesWrongRootPressureCeilingAndExistingDestination(t *testing.T) {
	for _, kind := range []string{"root", "cut", "pressure", "no-credit", "ceiling", "existing", "canceled"} {
		t.Run(kind, func(t *testing.T) {
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
			output := privateSource(t)
			limit := int64(16 << 20)
			given := credit
			switch kind {
			case "root":
				selected.Root.Digest[0] ^= 1
			case "cut":
				selected.Position.Revision++
			case "pressure":
				if err := o.Budget.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(err)
				}
			case "no-credit":
				given = resourcebudget.Permit{}
			case "ceiling":
				limit = 8192
			case "existing":
				if err := os.WriteFile(output, []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before := o.Budget.Snapshot().Used
			image, err := b.MaterializeMetadata(ctx, output, selected, limit, given)
			if err == nil || image != nil {
				t.Fatalf("invalid candidate built: %v", err)
			}
			if got := o.Budget.Snapshot().Used; got != before {
				t.Fatalf("failed candidate leaked credit: %+v -> %+v", before, got)
			}
			raw, err := os.ReadFile(output)
			if kind == "existing" {
				if err != nil || string(raw) != "foreign" {
					t.Fatalf("foreign output changed: %q %v", raw, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("failed candidate survived", err)
			}
		})
	}
}

func TestMetadataImageWholeFilePublishRequiresCurrentReceiptAndCapturedDestination(t *testing.T) {
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
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	output, destination := filepath.Join(dir, "prepared.db"), filepath.Join(dir, "metadata.db")
	if err := os.WriteFile(destination, []byte("old-native"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	image, err := b.MaterializeMetadata(ctx, output, selected, 16<<20, credit)
	if err != nil {
		t.Fatal(err)
	}
	wrong := selected
	wrong.Position.Sequence++
	if err := image.Publish(ctx, destination, previous, guard, wrong); !errors.Is(err, ErrSource) {
		t.Fatal("wrong cut published", err)
	}
	// A replacement after capture survives; the prepared inode remains owned.
	old := destination + ".old"
	if err := os.Rename(destination, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := image.Publish(ctx, destination, previous, guard, selected); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, destination); err != nil {
		t.Fatal(err)
	}
	if err := image.Publish(ctx, destination, previous, guard, selected); err != nil {
		t.Fatal(err)
	}
	if err := image.Discard(); !errors.Is(err, ErrSource) {
		t.Fatal("published Metadata could be discarded", err)
	}
	transferred, err := image.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := image.DiskReservation(); err == nil {
		t.Fatal("published fee transferred twice")
	}
	if err := credit.Release(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := image.verifyFile(ctx, image.digest, image.size); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("publication did not rename candidate", err)
	}
	// The test owns its directory and physically retires its published fixture.
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := transferred.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataRowKeyDecoderCanonicalBinaryRoundTripAndRejections(t *testing.T) {
	for _, marker := range []bool{true, false} {
		path := [][]byte{[]byte("deployment_price_timeline"), []byte("n\x00/")}
		key := []byte("k\x00/")
		if marker {
			key = nil
		}
		raw, err := RowKey(path, key, marker)
		if err != nil {
			t.Fatal(err)
		}
		gotPath, gotKey, gotMarker, err := decodeRowKey(raw)
		if err != nil || !reflect.DeepEqual(path, gotPath) || !bytes.Equal(key, gotKey) || gotMarker != marker {
			t.Fatal(gotPath, gotKey, gotMarker, err)
		}
	}
	for _, raw := range [][]byte{nil, {1, 'a', 0, 0, 2}, {1, 'a', 0, 1, 0}, {1, 0, 0, 0}, {1, 'a', 0, 0, 'b', 0}, {1, 'a', 0, 0, 1}} {
		if _, _, _, err := decodeRowKey(raw); err == nil {
			t.Fatal("noncanonical key accepted", raw)
		}
	}
}
