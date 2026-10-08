package authenticatedstate

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func imageStore(t *testing.T, values [][]byte, alterKey func(int, []byte)) (*Store, SealRootSchema, SealRoot) {
	t.Helper()
	i := 0
	s, err := CreateStore(context.Background(), filepath.Join(t.TempDir(), "image"), storeOptions(t), func() (Row, bool, error) {
		if i == len(values) {
			return Row{}, false, nil
		}
		key := make([]byte, 9)
		key[0] = 1
		binary.BigEndian.PutUint64(key[1:], uint64(i))
		if alterKey != nil {
			alterKey(i, key)
		}
		value := values[i]
		i++
		return Row{Key: key, Value: value}, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Discard(); err != nil {
			t.Error(err)
		}
	})
	schema := SealRootSchema{Kind: SealRootState, Component: SealComponentUsage, IndexID: 1, Codec: 1}
	root, err := s.SealRootReference(context.Background(), schema)
	if err != nil {
		t.Fatal(err)
	}
	return s, schema, root
}

func TestAuthenticatedImageStreamsExactSelectedNamespace(t *testing.T) {
	s, schema, root := imageStore(t, [][]byte{[]byte("abcd"), []byte("efgh"), []byte("ij")}, nil)
	var out bytes.Buffer
	n, err := s.WriteImage(context.Background(), &out, schema, root, 4, 10)
	if err != nil || n != 10 || out.String() != "abcdefghij" {
		t.Fatalf("image = %q/%d: %v", out.String(), n, err)
	}
	before := s.budget.Snapshot().Used
	out.Reset()
	wrong := root
	wrong.Digest[0] ^= 1
	if _, err := s.WriteImage(context.Background(), &out, schema, wrong, 4, 10); !errors.Is(err, ErrRecovery) || out.Len() != 0 {
		t.Fatalf("non-selected root wrote bytes: %v", err)
	}
	if _, err := s.WriteImage(context.Background(), &out, schema, root, 4, 8); !errors.Is(err, ErrLimit) || out.Len() != 0 {
		t.Fatalf("disk preflight failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.WriteImage(ctx, &out, schema, root, 4, 10); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("canceled export wrote bytes: %v", err)
	}
	if after := s.budget.Snapshot().Used; after != before {
		t.Fatalf("refused exports leaked fees: %#v -> %#v", before, after)
	}
}

func TestAuthenticatedImageRejectsIncompleteOrNoncanonicalChunks(t *testing.T) {
	for _, kind := range []string{"missing-ordinal", "short-middle", "empty-final", "wide-final"} {
		t.Run(kind, func(t *testing.T) {
			values := [][]byte{[]byte("abcd"), []byte("ef")}
			var alter func(int, []byte)
			switch kind {
			case "missing-ordinal":
				alter = func(i int, key []byte) {
					if i == 1 {
						binary.BigEndian.PutUint64(key[1:], 2)
					}
				}
			case "short-middle":
				values[0] = []byte("abc")
			case "empty-final":
				values[1] = nil
			case "wide-final":
				values[1] = []byte("efghi")
			}
			s, schema, root := imageStore(t, values, alter)
			_, err := s.WriteImage(context.Background(), io.Discard, schema, root, 4, 10)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("noncanonical image accepted: %v", err)
			}
		})
	}
}

type imageWriterFunc func([]byte) (int, error)

func (f imageWriterFunc) Write(b []byte) (int, error) { return f(b) }

func TestAuthenticatedImageCloseJoinsWriterAndRetainsWorkspace(t *testing.T) {
	s, schema, root := imageStore(t, [][]byte{[]byte("abcd"), []byte("ef")}, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	before := s.budget.Snapshot().Used
	go func() {
		_, err := s.WriteImage(context.Background(), imageWriterFunc(func(b []byte) (int, error) {
			select {
			case <-entered:
			default:
				close(entered)
				<-release
			}
			return len(b), nil
		}), schema, root, 4, 6)
		finished <- err
	}()
	<-entered
	if current := s.budget.Snapshot().Used.RAM; current <= before.RAM {
		t.Fatalf("writer has no workspace: %d <= %d", current, before.RAM)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned while writer held bytes: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	// The persistent owner keeps its claim and fixed namespace-cleanup RAM
	// until its captured files and directory actually retire.
	if current := s.budget.Snapshot().Used.RAM; current != resourcebudget.ClaimRAMBytes+4096 {
		t.Fatalf("joined job retained unexpected RAM: %d", current)
	}
}

func TestAuthenticatedImageWriterErrorsAndBudgetRefusalReleasePins(t *testing.T) {
	s, schema, root := imageStore(t, [][]byte{[]byte("abcd"), []byte("ef")}, nil)
	before := s.budget.Snapshot().Used
	injected := errors.New("destination full")
	for _, writer := range []io.Writer{
		imageWriterFunc(func(b []byte) (int, error) { return 0, injected }),
		imageWriterFunc(func(b []byte) (int, error) { return len(b) - 1, nil }),
	} {
		if _, err := s.WriteImage(context.Background(), writer, schema, root, 4, 6); err == nil {
			t.Fatal("writer failure accepted")
		}
		if after := s.budget.Snapshot().Used; after != before {
			t.Fatalf("writer failure leaked pin/workspace: %#v -> %#v", before, after)
		}
	}
	if err := s.budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := s.WriteImage(context.Background(), &out, schema, root, 4, 6); !errors.Is(err, resourcebudget.ErrPressure) || out.Len() != 0 {
		t.Fatalf("pressure admission = %v, %d bytes", err, out.Len())
	}
	if err := s.budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
}
