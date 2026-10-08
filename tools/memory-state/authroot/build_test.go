package authroot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func sortedFixture(n int) func() (entry, bool, error) {
	i := 0
	var key [16]byte
	var value [64]byte
	return func() (entry, bool, error) {
		if i == n {
			return entry{}, false, nil
		}
		// Reuse callback buffers deliberately: bulkBuild must take ownership before
		// another callback overwrites them. Padded keys preserve numeric ordering.
		text := fmt.Sprintf("k%012d", i)
		copy(key[:], text)
		for j := range value {
			value[j] = byte(i % 251)
		}
		i++
		return entry{key[:len(text)], value[:]}, true, nil
	}
}

func TestBulkBuildBoundariesAndStreamingOwnership(t *testing.T) {
	for _, n := range []int{0, 1, 8, 9, 63, 64, 65, 511, 512, 513, 4097} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			tree, dir, key := newDiskFixture(t)
			if err := tree.bulkBuild(context.Background(), sortedFixture(n)); err != nil {
				t.Fatal(err)
			}
			r, err := tree.snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if r.root.count != uint64(n) || r.root.sequence != uint64(n) || r.root.revision != 2 {
				t.Fatalf("root %+v", r.root)
			}
			var after []byte
			expected := 0
			var digest [32]byte
			for {
				result, err := r.scan(context.Background(), []byte("a"), []byte("z"), after, 32, 8192)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range result.rows {
					if string(e.key) != fmt.Sprintf("k%012d", expected) || !bytes.Equal(e.value, bytes.Repeat([]byte{byte(expected % 251)}, 64)) {
						t.Fatalf("wrong row %d", expected)
					}
					expected++
					digest = mutationHash(digest, uint64(expected), mutation{key: e.key, value: e.value})
				}
				if result.done {
					break
				}
				after = result.after
			}
			if expected != n || digest != r.root.factHash {
				t.Fatal("stream count/chain differs")
			}
			tree.close()
			reopened, err := openDiskTree(dir, key, 7, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.close()
			if reopened.current != r.root {
				t.Fatal("reopen root differs")
			}
			if err := reopened.bulkBuild(context.Background(), sortedFixture(1)); !errors.Is(err, errCorrupt) {
				t.Fatalf("rebuild live tree allowed: %v", err)
			}
		})
	}
}

func TestBulkBuildBadInputNeverPublishesPartialRoot(t *testing.T) {
	for _, mode := range []string{"duplicate", "out-of-order", "source-error", "oversized", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			tree, dir, key := newDiskFixture(t)
			before := tree.current
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := sortedFixture(20)
			i := 0
			iterator := func() (entry, bool, error) {
				e, ok, err := source()
				i++
				if i == 13 {
					switch mode {
					case "duplicate":
						e.key = []byte("k000000000011")
					case "out-of-order":
						e.key = []byte("a")
					case "source-error":
						return entry{}, false, io.ErrUnexpectedEOF
					case "oversized":
						e.value = make([]byte, maxValueBytes+1)
					case "cancel":
						cancel()
					}
				}
				return e, ok, err
			}
			if err := tree.bulkBuild(ctx, iterator); err == nil {
				t.Fatal("bad stream accepted")
			}
			if tree.current != before {
				t.Fatal("partial root visible")
			}
			if _, err := tree.snapshot(); err == nil {
				t.Fatal("failed build not poisoned")
			}
			tree.close()
			reopened, err := openDiskTree(dir, key, 7, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.close()
			if reopened.current != before || reopened.nextOffset <= packHeaderBytes {
				t.Fatal("partial tail advanced root or vanished")
			}
		})
	}
}

func TestBatchContentsAndSyntheticPositionPublishTogether(t *testing.T) {
	for _, phase := range []string{"success", "pages-written", "pages-synced", "root-written", "root-synced", "root-renamed", "directory-synced"} {
		t.Run(phase, func(t *testing.T) {
			tree, dir, key := newDiskFixture(t)
			if err := tree.bulkBuild(context.Background(), sortedFixture(65)); err != nil {
				t.Fatal(err)
			}
			old, err := tree.snapshot()
			if err != nil {
				t.Fatal(err)
			}
			before := old.root
			input := make([]mutation, 32)
			digest := before.factHash
			for i := range input {
				input[i] = mutation{key: []byte(fmt.Sprintf("k%012d", i)), value: []byte("batch")}
				digest = mutationHash(digest, before.sequence+uint64(i)+1, input[i])
			}
			if phase != "success" {
				tree.hook = func(at string) error {
					if at == phase {
						return errInjected
					}
					return nil
				}
			}
			changed, err := tree.applyBatch(context.Background(), input)
			if phase == "success" {
				if err != nil || !changed {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, errInjected) || changed {
					t.Fatalf("missed fault %v %v", changed, err)
				}
			}
			// An existing immutable reader still sees its exact original input root.
			original, found, err := old.get(context.Background(), input[0].key)
			if err != nil || !found || !bytes.Equal(original, make([]byte, 64)) {
				t.Fatal("old root changed")
			}
			if phase != "success" && tree.current != before {
				t.Fatal("failed publish visible")
			}
			tree.close()
			reopened, err := openDiskTree(dir, key, 7, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.close()
			next, err := reopened.snapshot()
			if err != nil {
				t.Fatal(err)
			}
			published := phase == "success" || phase == "root-renamed" || phase == "directory-synced"
			if published {
				if next.root.sequence != before.sequence+32 || next.root.factHash != digest || next.root.revision != before.revision+1 {
					t.Fatal("position not atomic with contents")
				}
			} else if next.root != before {
				t.Fatal("failed stage advanced position")
			}
			for i, m := range input {
				v, found, err := next.get(context.Background(), m.key)
				if err != nil || !found {
					t.Fatal(err)
				}
				expected := bytes.Repeat([]byte{byte(i % 251)}, 64)
				if published {
					expected = []byte("batch")
				}
				if !bytes.Equal(v, expected) {
					t.Fatalf("partial batch at %d", i)
				}
			}
		})
	}
}

func TestBatchLimitsRejectBeforeMutationAndNoopsAdvancePosition(t *testing.T) {
	tree, dir, _ := newDiskFixture(t)
	before := tree.current
	size := tree.nextOffset
	for _, input := range [][]mutation{nil, make([]mutation, maxBatchRows+1), {{key: []byte("k"), value: make([]byte, maxValueBytes+1)}}, {{key: []byte("k"), value: []byte("v"), remove: true}}} {
		if _, err := tree.applyBatch(context.Background(), input); !errors.Is(err, errLimit) {
			t.Fatal(err)
		}
		if tree.current != before || tree.nextOffset != size || tree.poisoned {
			t.Fatal("invalid batch touched state")
		}
	}
	input := make([]mutation, 32)
	for i := range input {
		input[i] = mutation{key: bytes.Repeat([]byte{'k'}, maxKeyBytes), value: make([]byte, maxValueBytes)}
	}
	if _, err := tree.applyBatch(context.Background(), input); !errors.Is(err, errLimit) {
		t.Fatal("byte budget ignored")
	}
	if changed, err := tree.applyBatch(context.Background(), []mutation{{key: []byte("absent"), remove: true}}); err != nil || changed {
		t.Fatalf("no-op: %v %v", changed, err)
	}
	if tree.current.sequence != 1 || tree.current.count != 0 || tree.current.factHash == ([32]byte{}) || tree.nextOffset != size {
		t.Fatal("no-op position not committed with empty root")
	}
	info, err := os.Stat(filepath.Join(dir, "root"))
	if err != nil || info.Size() != rootBytes {
		t.Fatal("root framing changed")
	}
}

func TestSecondBatchPartialQuotaFailurePreservesFirstBatch(t *testing.T) {
	tree, dir, key := newDiskFixture(t)
	ctx := context.Background()
	first := []mutation{{key: []byte("a"), value: []byte("first-a")}, {key: []byte("b"), value: []byte("first-b")}}
	if _, err := tree.applyBatch(ctx, first); err != nil {
		t.Fatal(err)
	}
	before := tree.current
	onePage, err := encodePage(page{scope: 7, kind: leafKind, count: 2, entries: []entry{{[]byte("a"), []byte("second-a")}, {[]byte("b"), []byte("first-b")}}})
	if err != nil {
		t.Fatal(err)
	}
	tree.maxBytes = tree.nextOffset + int64(len(onePage)) // first mutation fits, second must fail
	second := []mutation{{key: []byte("a"), value: []byte("second-a")}, {key: []byte("b"), value: []byte("second-b")}}
	if _, err := tree.applyBatch(ctx, second); !errors.Is(err, errLimit) {
		t.Fatalf("partial quota failure not reached: %v", err)
	}
	if tree.current != before {
		t.Fatal("partial second batch advanced contents/position")
	}
	tree.close()
	reopened, err := openDiskTree(dir, key, 7, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if reopened.current != before {
		t.Fatal("reopen lost first committed batch")
	}
	r, err := reopened.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range first {
		v, found, err := r.get(ctx, m.key)
		if err != nil || !found || !bytes.Equal(v, m.value) {
			t.Fatalf("first batch altered: %q %v %v", v, found, err)
		}
	}
}
