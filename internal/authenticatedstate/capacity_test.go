package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// A pack without one worst-case transaction of room refuses the update before
// appending anything. The store stays healthy: its rows, root and extent are
// unchanged, and after a common cut and repack it accepts updates again.
func TestStoreRefusesUpdateWithoutAppendRoomAndRecoversAfterRepack(t *testing.T) {
	// Updates stop one transaction short of the ceiling. With that bound at half
	// the ceiling, a full pack never reaches the half-full benefit test, and
	// must still qualify for reclamation.
	for _, size := range []struct{ disk, append int64 }{{64 << 10, 16 << 10}, {64 << 10, 32 << 10}} {
		t.Run(fmt.Sprint(size.disk, "/", size.append), func(t *testing.T) {
			refuseWithoutAppendRoomAndRecover(t, newRepackFixtureSized(t, size.disk, size.append))
		})
	}
}

func refuseWithoutAppendRoomAndRecover(t *testing.T, f *repackFixture) {
	ctx := context.Background()
	put := func() error {
		value := bytes.Repeat([]byte{byte(f.store.physicalRevision)}, 2048)
		_, err := f.store.Update(ctx, nextCut(int(f.store.physicalRevision)), func(tx *Transaction) error { return tx.Put([]byte("key_000001"), value) })
		return err
	}
	var err error
	for n := 0; err == nil; n++ {
		if n > 1000 {
			t.Fatal("pack never reported its capacity")
		}
		err = put()
	}
	if !errors.Is(err, ErrCapacity) {
		t.Fatal("full pack refused with", err)
	}
	if uint64(f.store.ceiling)-f.store.physicalExtent >= uint64(f.store.appendLimit) {
		t.Fatal("refused with a whole transaction of room", f.store.ceiling, f.store.physicalExtent)
	}
	want := storeRows(t, f.store)
	extent, current := f.store.physicalExtent, f.store.current
	if err := put(); !errors.Is(err, ErrCapacity) {
		t.Fatal("repeated refusal", err)
	}
	if f.store.physicalExtent != extent || f.store.current != current || f.store.fault() != nil || f.store.NativeFrameRecoveryDebt() != nil {
		t.Fatal("refused update changed or poisoned the store", f.store.fault(), f.store.NativeFrameRecoveryDebt())
	}
	if got := storeRows(t, f.store); !reflect.DeepEqual(got, want) {
		t.Fatal("refused update changed rows")
	}
	publish := func() {
		t.Helper()
		f.seal.Revision++
		f.seal.Roots[0], err = f.store.SealRootReference(ctx, f.schema)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.publisher.Publish(ctx, f.seal); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
	if err != nil || candidate == nil {
		t.Fatal("repack of a full pack", err)
	}
	publish()
	if err := candidate.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := put(); err != nil {
		t.Fatal("update after reclamation", err)
	}
}

// Admission stops writes while a pack still has room for one update, which can
// be below half the ceiling when that bound is large. Repack must qualify once
// fewer than two updates fit, or the pack keeps its obsolete pages and room
// never returns; with ample room the half-full benefit test is unchanged.
func TestStoreRepackQualifiesWhenAppendRoomIsLow(t *testing.T) {
	for _, c := range []struct {
		append    int64
		qualifies bool
	}{{16 << 10, false}, {32 << 10, true}} {
		t.Run(fmt.Sprint(c.append), func(t *testing.T) {
			f := newRepackFixtureSized(t, 64<<10, c.append)
			ctx := context.Background()
			for i := 0; i < 2; i++ {
				value := bytes.Repeat([]byte{byte(i)}, 2048)
				if _, err := f.store.Update(ctx, nextCut(int(f.store.physicalRevision)), func(tx *Transaction) error { return tx.Put([]byte("key_000001"), value) }); err != nil {
					t.Fatal(err)
				}
			}
			if f.store.physicalExtent >= uint64(f.store.ceiling/2) {
				t.Fatal("fixture reached the half-full test", f.store.physicalExtent, f.store.ceiling)
			}
			var err error
			f.seal.Revision++
			if f.seal.Roots[0], err = f.store.SealRootReference(ctx, f.schema); err != nil {
				t.Fatal(err)
			}
			if _, err := f.publisher.Publish(ctx, f.seal); err != nil {
				t.Fatal(err)
			}
			candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
			if err != nil || (candidate != nil) != c.qualifies {
				t.Fatal("repack qualification", candidate != nil, err)
			}
			if candidate != nil {
				if err := candidate.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// Admission polls PackUsage from every request while another request's Apply
// updates the store, and update only tries the store lock. A poll must never
// make an update fail busy, and it reports the extent each update, repack and
// close leaves behind.
func TestPackUsageNeverHoldsTheStoreLockAgainstAnUpdate(t *testing.T) {
	f := newRepackFixtureSized(t, 2<<20, 16<<10)
	ctx := context.Background()
	check := func(open bool) {
		t.Helper()
		used, usable, ok := f.store.PackUsage()
		if ok != open || open && (used != f.store.physicalExtent || usable != uint64(f.store.ceiling-f.store.appendLimit)) {
			t.Fatal("pack usage", used, usable, ok, f.store.physicalExtent)
		}
	}
	check(true)
	stop, polled := make(chan struct{}), make(chan int)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				polled <- n
				return
			default:
				f.store.PackUsage()
				n++
			}
		}
	}()
	var err error
	for i := 0; err == nil && f.store.physicalExtent < uint64(f.store.ceiling/2); i++ {
		if i > 2000 {
			err = errors.New("pack never reached half full")
			break
		}
		value := bytes.Repeat([]byte{byte(i)}, 512)
		_, err = f.store.Update(ctx, nextCut(int(f.store.physicalRevision)), func(tx *Transaction) error { return tx.Put([]byte("key_000001"), value) })
	}
	close(stop)
	if n := <-polled; err != nil || n == 0 {
		t.Fatal("updates beside a pack poll:", err, n)
	}
	check(true)
	publish := func() {
		t.Helper()
		f.seal.Revision++
		if f.seal.Roots[0], err = f.store.SealRootReference(ctx, f.schema); err != nil {
			t.Fatal(err)
		}
		if _, err := f.publisher.Publish(ctx, f.seal); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	before := f.store.physicalExtent
	candidate, err := f.store.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
	if err != nil || candidate == nil {
		t.Fatal("repack", err)
	}
	if check(true); f.store.physicalExtent >= before {
		t.Fatal("repack reclaimed nothing", before, f.store.physicalExtent)
	}
	publish()
	if err := candidate.Commit(); err != nil {
		t.Fatal(err)
	}
	check(true)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	check(false)
}
