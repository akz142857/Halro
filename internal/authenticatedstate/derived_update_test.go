package authenticatedstate

import (
	"context"
	"errors"
	"testing"
)

func TestDerivedUpdateKeepsNativeCutAndPreservesPinnedPreviousContent(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t, 3)
	before, err := s.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	barriers := 0
	receipt, err := s.UpdateDerived(ctx, before.Cut, func(tx *Transaction) error {
		return tx.Delete([]byte("key_000001"))
	}, func() error { barriers++; return nil })
	if err != nil || barriers != 1 || receipt.Cut != before.Cut || receipt.Rows != before.Rows-1 {
		t.Fatal(receipt, barriers, err)
	}
	after, err := s.RootIdentity(ctx)
	if err != nil || after.Cut != before.Cut || after.Revision != before.Revision+1 || after.Digest == before.Digest {
		t.Fatal(before, after, err)
	}
	if value, found, err := pin.Get(ctx, []byte("key_000001")); err != nil || !found || string(value) != "value_000001" {
		t.Fatal("derived publication mutated old pin", value, found, err)
	}
	current, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if value, found, err := current.Get(ctx, []byte("key_000001")); err != nil || found || value != nil {
		t.Fatal(value, found, err)
	}
}

func TestDerivedUpdateCannotAdvanceCutOrPublishRejectedPermission(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t, 3)
	before, err := s.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	apply := func(tx *Transaction) error { called = true; return tx.Delete([]byte("key_000001")) }
	if _, err := s.UpdateDerived(ctx, nextCut(1), apply, func() error { return nil }); !errors.Is(err, ErrLimit) || called {
		t.Fatal("maintenance advanced native cut", called, err)
	}
	if _, err := s.UpdateDerived(ctx, before.Cut, apply, nil); !errors.Is(err, ErrLimit) || called {
		t.Fatal("maintenance omitted permission barrier", called, err)
	}
	rejected := errors.New("archive publication is no longer independently qualified")
	if _, err := s.UpdateDerived(ctx, before.Cut, apply, func() error { return rejected }); !errors.Is(err, rejected) || !called {
		t.Fatal(called, err)
	}
	if s.current.cut != before.Cut || s.current.revision != before.Revision || s.current.content.count != before.Rows {
		t.Fatal("rejected permission published private candidate")
	}
	if _, err := s.Snapshot(ctx); !errors.Is(err, ErrRecovery) {
		t.Fatal("partially written rejected owner remained reusable", err)
	}
}
