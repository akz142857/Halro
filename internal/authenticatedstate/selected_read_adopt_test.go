package authenticatedstate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAdoptClosedSelectedReadRootPreservesPhysicalHistoryAndUsesNewRevision(t *testing.T) {
	ctx := context.Background()
	s, o := testStore(t, 3)
	saved := selectedReadBorrowOptions(t, s, o)
	if _, err := s.Update(ctx, nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("unselected"), []byte("physical suffix")) }); err != nil {
		t.Fatal(err)
	}
	physical, err := s.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(s.dir, "active.root")
	before, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	extent, revision := s.physicalExtent, s.physicalRevision
	child, err := s.BorrowSelectedReadStore(ctx, saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := adoptTestGet(ctx, child, []byte("unselected")); err != nil || found {
		t.Fatal("historical child adopted suffix", found, err)
	}
	schema := *saved.ExpectedSealSchema
	root := *saved.ExpectedSealRoot
	if err := s.AdoptClosedSelectedReadRoot(ctx, child, root, schema, saved.ExpectedCut); !errors.Is(err, ErrBusy) {
		t.Fatal("actual open child not joined", err)
	}
	if err := s.RetainBorrowedReadCleanup(child, child.Close); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.AdoptClosedSelectedReadRoot(ctx, child, root, schema, saved.ExpectedCut); !errors.Is(err, ErrBusy) {
		t.Fatal("Store-only Close substituted for component join", err)
	}
	if err := s.JoinBorrowedReadComponent(child); err != nil {
		t.Fatal(err)
	}
	wrong := root
	wrong.State.Digest[0] ^= 1
	if err := s.AdoptClosedSelectedReadRoot(ctx, child, wrong, schema, saved.ExpectedCut); err == nil {
		t.Fatal("foreign root accepted")
	}
	if err := s.AdoptClosedSelectedReadRoot(ctx, child, root, schema, saved.ExpectedCut); err != nil {
		t.Fatal(err)
	}
	actual, err := s.RootIdentity(ctx)
	if err != nil || actual != *saved.ExpectedRoot {
		t.Fatal(actual, err)
	}
	after, err := os.ReadFile(rootPath)
	if err != nil || !bytes.Equal(before, after) || s.physicalExtent != extent || s.physicalRevision != revision {
		t.Fatal("logical handoff rewrote/truncated physical root", err)
	}
	if _, err := s.Update(ctx, nextCut(2), func(tx *Transaction) error { return tx.Put([]byte("allowed"), []byte("authenticated next fact")) }); err != nil {
		t.Fatal(err)
	}
	next, err := s.RootIdentity(ctx)
	if err != nil || next.Revision != physical.Revision+1 || s.physicalExtent <= extent {
		t.Fatal(next, physical, extent, err)
	}
	if _, found, err := adoptTestGet(ctx, s, []byte("unselected")); err != nil || found {
		t.Fatal("unselected rows survived logical replay", found, err)
	}
	if v, found, err := adoptTestGet(ctx, s, []byte("allowed")); err != nil || !found || string(v) != "authenticated next fact" {
		t.Fatal(v, found, err)
	}
}

func adoptTestGet(ctx context.Context, s *Store, key []byte) (value []byte, found bool, resultErr error) {
	pin, err := s.Snapshot(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	return pin.Get(ctx, key)
}
