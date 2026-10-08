package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type repackFixture struct {
	store            *Store
	options          StoreOptions
	publisher        *SealCandidatePublisher
	guard            *lock.Lock
	schema           SealRootSchema
	seal             CandidateSeal
	reservation      resourcebudget.Permit
	expectedCloseErr error
}

func newRepackFixture(t *testing.T) *repackFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	o := storeOptions(t)
	o.MaxDiskBytes, o.MaxAppendBytes = 64<<10, 16<<10
	i := 0
	creator, err := CreateStore(ctx, filepath.Join(dir, "state"), o, func() (Row, bool, error) {
		if i == 8 {
			return Row{}, false, nil
		}
		r := Row{Key: []byte(fmt.Sprintf("key_%06d", i)), Value: []byte("initial")}
		i++
		return r, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := creator.Close(); err != nil {
		t.Fatal(err)
	}
	o.DiskReservation, err = creator.DiskReservation()
	if err != nil {
		t.Fatal(err)
	}
	schema := SealRootSchema{Kind: SealRootState, Component: SealComponentLedger, IndexID: 1, Codec: 1}
	o.ExpectedRoot, err = func() (*RootIdentity, error) {
		r := RootIdentity{Scope: creator.current.content.context, Cut: creator.current.cut, Digest: creator.rootDigest, ID: creator.current.content.id, Revision: creator.current.revision, Rows: creator.current.content.count}
		return &r, nil
	}()
	s, err := OpenStore(ctx, creator.dir, o)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	seal := sealCandidateFixture()
	seal.Roots[0], err = s.SealRootReference(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenSealCandidatePublisher(dir, guard, o.Key, sealHardFor(seal), o.Budget, sealPublisherReservation(t, o.Budget))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	reservation, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: s.RepackDiskBytes()})
	if err != nil {
		t.Fatal(err)
	}
	f := &repackFixture{store: s, options: o, publisher: p, guard: guard, schema: schema, seal: seal, reservation: reservation}
	t.Cleanup(func() {
		if err := f.store.Close(); err != nil && !errors.Is(err, f.expectedCloseErr) {
			t.Error(err)
		}
		if err := p.Close(); err != nil {
			t.Error(err)
		}
		if err := guard.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func (f *repackFixture) grow(t *testing.T) {
	t.Helper()
	for f.store.physicalExtent < uint64(f.store.ceiling/2) {
		value := bytes.Repeat([]byte{byte(f.store.physicalRevision)}, 2048)
		if _, err := f.store.Update(context.Background(), nextCut(int(f.store.physicalRevision)), func(tx *Transaction) error { return tx.Put([]byte("key_000001"), value) }); err != nil {
			t.Fatal(err)
		}
	}
	f.seal.Revision++
	var err error
	f.seal.Roots[0], err = f.store.SealRootReference(context.Background(), f.schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.publisher.Publish(context.Background(), f.seal); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRepackPublishesReclaimsAndReopensAcrossRepeatedSlots(t *testing.T) {
	f := newRepackFixture(t)
	ctx := context.Background()
	for cycle := 0; cycle < 4; cycle++ {
		f.grow(t)
		want := storeRows(t, f.store)
		oldPath := f.store.slotPath(f.store.packSlot, "pages")
		oldExtent := f.store.physicalExtent
		before := f.options.Budget.Snapshot().Used
		candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
		if err != nil || candidate == nil {
			t.Fatal("prepare", cycle, err)
		}
		if err := candidate.Commit(); !errors.Is(err, ErrSealIneligible) {
			t.Fatal("unselected compact pack committed", err)
		}
		if _, err := os.Stat(oldPath); err != nil {
			t.Fatal("old selected pack removed before common publication", err)
		}
		f.seal.Revision++
		f.seal.Roots[0], err = f.store.SealRootReference(ctx, f.schema)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.publisher.Publish(ctx, f.seal); err != nil {
			t.Fatal(err)
		}
		if err := candidate.Commit(); err != nil {
			t.Fatal("commit", cycle, err)
		}
		if err := candidate.Commit(); err != nil {
			t.Fatal("repeated commit", err)
		}
		if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("obsolete physical pack retained", cycle, err)
		}
		if f.store.physicalExtent >= oldExtent/2 {
			t.Fatal("dead pages not reclaimed", oldExtent, f.store.physicalExtent)
		}
		if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
			t.Fatal("repack changed authenticated rows")
		}
		if after := f.options.Budget.Snapshot().Used; after != before {
			t.Fatal("per-cycle resources accumulated", before, after)
		}
		if err := f.store.Close(); err != nil {
			t.Fatal(err)
		}
		o := f.options
		o.ExpectedRoot = nil
		o.ExpectedCut = f.seal.Roots[0].State.Cut
		o.ExpectedSealRoot, o.ExpectedSealSchema = &f.seal.Roots[0], &f.schema
		f.store, err = OpenStore(ctx, f.store.dir, o)
		if err != nil {
			t.Fatal("selected pack reopen", cycle, err)
		}
		if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
			t.Fatal("reopen lost complete rows")
		}
	}
}

func TestStoreRepackPinRefusalAndCanceledCandidatePreserveSelectedPack(t *testing.T) {
	f := newRepackFixture(t)
	f.grow(t)
	ctx := context.Background()
	before := f.options.Budget.Snapshot().Used
	pin, err := f.store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation); candidate != nil || !errors.Is(err, ErrBusy) {
		t.Fatal("live reader did not exclude reclamation", err)
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	old := f.seal.Roots[0]
	canceled, cancel := context.WithCancel(ctx)
	f.store.hook = func(phase string) error {
		if phase == "repack-pages-synced" {
			cancel()
		}
		return nil
	}
	if candidate, err := f.store.PrepareRepack(canceled, f.publisher, f.guard, f.seal, f.schema, f.reservation); candidate != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled copy published", candidate, err)
	}
	f.store.hook = nil
	if root, err := f.store.SealRootReference(ctx, f.schema); err != nil || root != old {
		t.Fatal("canceled preparation changed selected root", err)
	}
	if after := f.options.Budget.Snapshot().Used; after != before {
		t.Fatal("canceled preparation leaked resources", before, after)
	}
	candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
	if err != nil || candidate == nil {
		t.Fatal("retry", err)
	}
	if err := candidate.Rollback(); err != nil {
		t.Fatal(err)
	}
	if root, err := f.store.SealRootReference(ctx, f.schema); err != nil || root != old {
		t.Fatal("rollback did not restore selected root", err)
	}
}
