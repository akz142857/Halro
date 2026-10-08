package authenticatedstate

import (
	"context"
	"errors"
	"testing"
)

func TestStoreSealRootReferenceExportsActualNonEmptyAndEmptyRoots(t *testing.T) {
	for _, count := range []int{0, 5} {
		t.Run(map[int]string{0: "empty", 5: "nonempty"}[count], func(t *testing.T) {
			s, _ := testStore(t, count)
			wantSchema := SealRootSchema{Kind: SealRootState, Component: SealComponentLedger, IndexID: 1, Codec: 1}
			got, err := s.SealRootReference(context.Background(), wantSchema)
			if err != nil {
				t.Fatal(err)
			}
			if got.Component != wantSchema.Component || got.IndexID != wantSchema.IndexID || got.Codec != wantSchema.Codec || got.Rows != uint64(count) || got.Digest == ([32]byte{}) {
				t.Fatalf("seal root summary = %#v", got)
			}
			if count == 0 {
				if !got.Empty || got.FileID != ([16]byte{}) || got.Offset != 0 || got.Length != 0 {
					t.Fatalf("empty store did not produce canonical explicit empty root: %#v", got)
				}
			} else if got.Empty || got.FileID != s.current.content.id || got.Offset != s.current.content.root.offset || got.Length != s.current.content.root.length || got.Digest != s.current.content.root.digest {
				t.Fatalf("seal ref differs from actual committed page reference: %#v", got)
			}
			if _, err := s.SealRootReference(context.Background(), SealRootSchema{Kind: SealRootState, Component: SealComponentLedger, Codec: 1}); !errors.Is(err, ErrLimit) {
				t.Fatalf("invalid schema entry error = %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStoreSealRootReferenceRejectsCancellationAndClosedStore(t *testing.T) {
	s, _ := testStore(t, 2)
	schema := SealRootSchema{Kind: SealRootState, Component: SealComponentLedger, IndexID: 1, Codec: 1}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := s.SealRootReference(ctx, schema); !errors.Is(err, context.Canceled) || got != (SealRoot{}) {
		t.Fatalf("cancelled root reference = %#v, %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SealRootReference(context.Background(), schema); !errors.Is(err, ErrClosed) || got != (SealRoot{}) {
		t.Fatalf("closed root reference = %#v, %v", got, err)
	}
}
