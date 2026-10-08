package authenticatedstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/store/lock"
)

func selectedReadBorrowOptions(t *testing.T, s *Store, o StoreOptions) StoreOptions {
	t.Helper()
	ctx := context.Background()
	root, err := s.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	schema := SealRootSchema{Kind: SealRootState, Component: SealComponentMetadata, IndexID: 1, Codec: 2}
	ref, err := s.SealRootReference(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	o.ReadOnly = true
	o.ExpectedRoot = &root
	o.ExpectedSealRoot = &ref
	o.ExpectedSealSchema = &schema
	o.DiskReservation = s.control
	return o
}
func TestBorrowSelectedReadStorePreservesActualLockAndJoinsChild(t *testing.T) {
	ctx := context.Background()
	source, o := testStore(t, 3)
	o = selectedReadBorrowOptions(t, source, o)
	before := o.Budget.Snapshot().Used
	wrong := o
	wrong.Scope = [32]byte{7}
	if child, err := source.BorrowSelectedReadStore(ctx, wrong); child != nil || err == nil {
		t.Fatal("foreign source identity borrowed lock", child, err)
	}
	write := o
	write.ReadOnly = false
	if child, err := source.BorrowSelectedReadStore(ctx, write); child != nil || err == nil {
		t.Fatal("writable source borrowed exclusion", child, err)
	}
	child, err := source.BorrowSelectedReadStore(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Update(ctx, nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("deny"), []byte("v")) }); err == nil {
		t.Fatal("actual borrowed descriptor wrote")
	}
	if actual, err := child.RootIdentity(ctx); err != nil || actual != *o.ExpectedRoot {
		t.Fatal(actual, err)
	}
	// Compound component cleanup can own other native FDs/jobs. The original
	// Store cannot unlock until this accepted borrowed component actually joins.
	entered, release := make(chan struct{}), make(chan struct{})
	if err := source.RetainBorrowedReadCleanup(child, func() error { close(entered); <-release; return child.Close() }); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- source.Close() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("actual parent Close did not enter compound cleanup")
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if guard, err := lock.AcquireExistingReadOnly(source.dir); guard != nil || !errors.Is(err, lock.ErrAlreadyLocked) {
		t.Fatal("source unlocked before actual component join", guard, err)
	}
	select {
	case err := <-done:
		t.Fatal("parent Close skipped actual component join", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := child.Snapshot(ctx); !errors.Is(err, ErrClosed) {
		t.Fatal("borrow survived source retirement", err)
	}
	guard, err := lock.AcquireExistingReadOnly(source.dir)
	if err != nil {
		t.Fatal("joined source did not unlock", err)
	}
	if err = guard.Close(); err != nil {
		t.Fatal(err)
	}
	if o.Budget.Snapshot().Used != before {
		t.Fatal("joined child leaked fixed RAM/fees", before, o.Budget.Snapshot().Used)
	}
}
func TestBorrowSelectedReadStoreFixedLoansAndPhysicalChanges(t *testing.T) {
	ctx := context.Background()
	source, o := testStore(t, 3)
	o = selectedReadBorrowOptions(t, source, o)
	for range 4 {
		child, err := source.BorrowSelectedReadStore(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		if err = source.RetainBorrowedReadCleanup(child, child.Close); err != nil {
			t.Fatal(err)
		}
		if err = child.Close(); err != nil {
			t.Fatal(err)
		}
	}
	before := o.Budget.Snapshot().Used
	if child, err := source.BorrowSelectedReadStore(ctx, o); child != nil || err == nil {
		t.Fatal("unbounded lifetime read loans", child, err)
	}
	if o.Budget.Snapshot().Used != before {
		t.Fatal("fixed loan exhaustion leaked fees")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBorrowSelectedReadStoreNeverTracksAChangedPhysicalRoot(t *testing.T) {
	ctx := context.Background()
	source, o := testStore(t, 3)
	o = selectedReadBorrowOptions(t, source, o)
	before := o.Budget.Snapshot().Used
	wrong := o
	root := *o.ExpectedRoot
	root.Digest[0] ^= 1
	wrong.ExpectedRoot = &root
	if child, err := source.BorrowSelectedReadStore(ctx, wrong); child != nil || err == nil {
		t.Fatal("mismatched actual commitment borrowed", child, err)
	}
	if o.Budget.Snapshot().Used != before {
		t.Fatal("failed root qualification leaked descriptor fees")
	}
	child, err := source.BorrowSelectedReadStore(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.RetainBorrowedReadCleanup(child, child.Close); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Update(ctx, nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("changed"), []byte("actual source root")) }); err != nil {
		t.Fatal(err)
	}
	if _, err := child.RootIdentity(ctx); !errors.Is(err, ErrCorrupt) {
		t.Fatal("loan silently adopted new physical root", err)
	}
	if _, err := source.RootIdentity(ctx); err != nil {
		t.Fatal("loan mismatch poisoned actual healthy source", err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBorrowSelectedReadStoreUnknownChildCloseRetainsActualExclusion(t *testing.T) {
	ctx := context.Background()
	source, o := testStoreWithoutCleanup(t, 3)
	o = selectedReadBorrowOptions(t, source, o)
	child, err := source.BorrowSelectedReadStore(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.RetainBorrowedReadCleanup(child, child.Close); err != nil {
		t.Fatal(err)
	}
	if err = child.pack.Close(); err != nil {
		t.Fatal(err)
	} // actual descriptor close uncertainty at the owner boundary
	before := o.Budget.Snapshot().Used
	if err := source.Close(); err == nil {
		t.Fatal("unknown child retirement released actual source")
	}
	if guard, err := lock.AcquireExistingReadOnly(source.dir); guard != nil || !errors.Is(err, lock.ErrAlreadyLocked) {
		t.Fatal("unknown child close released parent flock", guard, err)
	}
	if o.Budget.Snapshot().Used != before {
		t.Fatal("unknown cleanup refunded live/debt permits")
	}
	if child, err := source.BorrowSelectedReadStore(ctx, o); child != nil || err == nil {
		t.Fatal("retired source admitted new loan", child, err)
	}
}
