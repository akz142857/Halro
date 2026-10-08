package authroot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"syscall"
	"testing"
)

var errInjected = errors.New("injected publication failure")

func newDiskFixture(t *testing.T) (*diskTree, string, []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	key := bytes.Repeat([]byte{0x37}, 32)
	tree, err := createDiskTree(dir, key, 7, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tree.close() })
	return tree, dir, key
}
func allDiskRows(t *testing.T, r reader) map[string]string {
	t.Helper()
	rows := make(map[string]string)
	var after []byte
	for {
		result, err := r.scan(context.Background(), []byte("a"), []byte("z"), after, 7, 1024)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range result.rows {
			if _, exists := rows[string(e.key)]; exists {
				t.Fatal("duplicate")
			}
			rows[string(e.key)] = string(e.value)
		}
		if result.done {
			return rows
		}
		if len(result.rows) == 0 {
			t.Fatal("no progress")
		}
		after = result.after
	}
}

func TestDiskCOWUpdatesDeletesReopenAndOldRoots(t *testing.T) {
	tree, dir, key := newDiskFixture(t)
	ctx := context.Background()
	model := map[string]string{}
	// Seed enough keys to exercise both leaf and branch splits, in random order.
	rng := rand.New(rand.NewSource(47))
	order := rng.Perm(400)
	for _, i := range order {
		k := fmt.Sprintf("k%04d", i)
		v := "v:" + k
		if changed, err := tree.update(ctx, []byte(k), []byte(v), false); err != nil || !changed {
			t.Fatalf("insert %d: %v %v", i, changed, err)
		}
		model[k] = v
	}
	old, err := tree.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	oldModel := allDiskRows(t, old)
	if !reflect.DeepEqual(oldModel, model) {
		t.Fatal("insert scan differs")
	}
	// Exercise overwrites, absence proofs, gap inserts, and removals under churn.
	for i := 0; i < 300; i++ {
		k := fmt.Sprintf("k%04d", rng.Intn(600))
		remove := rng.Intn(3) == 0
		v := fmt.Sprintf("v:%d", i)
		_, err := tree.update(ctx, []byte(k), []byte(v), remove)
		if err != nil {
			t.Fatal(err)
		}
		if remove {
			delete(model, k)
		} else {
			model[k] = v
		}
	}
	current, err := tree.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(allDiskRows(t, current), model) {
		t.Fatal("churn scan differs")
	}
	if !reflect.DeepEqual(allDiskRows(t, old), oldModel) {
		t.Fatal("COW altered old root")
	}
	if err := tree.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDiskTree(dir, key, 7, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	r, err := reopened.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(allDiskRows(t, r), model) {
		t.Fatal("reopen differs")
	}
	keys := make([]string, 0, len(model))
	for k := range model {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if changed, err := reopened.update(ctx, []byte(k), nil, true); err != nil || !changed {
			t.Fatalf("delete %s: %v %v", k, changed, err)
		}
	}
	r, err = reopened.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if r.root.count != 0 || r.root.height != 0 || len(allDiskRows(t, r)) != 0 {
		t.Fatal("not canonical empty after final delete")
	}
	if err := reopened.close(); err != nil {
		t.Fatal(err)
	}
	empty, err := openDiskTree(dir, key, 7, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.close()
	if changed, err := empty.update(ctx, []byte("k-new"), []byte("new"), false); err != nil || !changed {
		t.Fatalf("empty reinsertion %v %v", changed, err)
	}
}

func TestDiskCorruptPathCannotBeResigned(t *testing.T) {
	tree, dir, key := newDiskFixture(t)
	ctx := context.Background()
	if _, err := tree.update(ctx, []byte("k"), []byte("original"), false); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := tree.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	offset := int64(r.root.ref.offset) + int64(r.root.ref.length) - 1
	if _, err := tree.pack.WriteAt([]byte{0}, offset); err != nil {
		t.Fatal(err)
	}
	if changed, err := tree.update(ctx, []byte("k"), []byte("replacement"), false); !errors.Is(err, errCorrupt) || changed {
		t.Fatalf("resigned corruption: %v %v", changed, err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("published changed root from bad input")
	}
	tree.close()
	if reopened, err := openDiskTree(dir, key, 7, 64<<20); err == nil {
		reopened.close()
		t.Fatal("accepted corrupt root page")
	}
}

func TestDiskQuotaAndUnpublishedTailDoNotAdvanceRoot(t *testing.T) {
	tree, dir, key := newDiskFixture(t)
	ctx := context.Background()
	if _, err := tree.update(ctx, []byte("k"), []byte("original"), false); err != nil {
		t.Fatal(err)
	}
	rootBefore := tree.current
	tree.maxBytes = tree.nextOffset + 1 // one-byte slack cannot fit even a single page
	if _, err := tree.update(ctx, []byte("k"), []byte("new"), false); !errors.Is(err, errLimit) {
		t.Fatalf("quota ignored: %v", err)
	}
	if tree.current != rootBefore {
		t.Fatal("quota advanced root")
	}
	if _, err := tree.snapshot(); err == nil {
		t.Fatal("publication failure must poison acquisition")
	}
	tree.close()
	reopened, err := openDiskTree(dir, key, 7, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	reopened.hook = func(phase string) error {
		if phase == "pages-written" {
			return errInjected
		}
		return nil
	}
	if _, err := reopened.update(ctx, []byte("k"), []byte("orphan"), false); !errors.Is(err, errInjected) {
		t.Fatal(err)
	}
	orphanBytes := reopened.nextOffset
	reopened.close()
	again, err := openDiskTree(dir, key, 7, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer again.close()
	if again.current != rootBefore || again.nextOffset != orphanBytes {
		t.Fatal("unpublished tail promoted or uncharged")
	}
	r, err := again.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	v, found, err := r.get(ctx, []byte("k"))
	if err != nil || !found || string(v) != "original" {
		t.Fatalf("tail became visible: %q %v %v", v, found, err)
	}
	again.close()
	if wrong, err := openDiskTree(dir, key, 8, 64<<20); err == nil {
		wrong.close()
		t.Fatal("cross-component open accepted")
	}
	if wrong, err := openDiskTree(dir, bytes.Repeat([]byte{0x38}, 32), 7, 64<<20); err == nil {
		wrong.close()
		t.Fatal("wrong key accepted")
	}
}

func TestDiskPublicationFaultsRecoverWholeOldOrNewRoot(t *testing.T) {
	for _, phase := range []string{"pages-written", "pages-synced", "root-written", "root-synced", "root-renamed", "directory-synced"} {
		t.Run(phase, func(t *testing.T) {
			tree, dir, key := newDiskFixture(t)
			ctx := context.Background()
			if _, err := tree.update(ctx, []byte("k"), []byte("old"), false); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 9; i++ {
				if _, err := tree.update(ctx, []byte(fmt.Sprintf("k%d", i)), []byte("fixed"), false); err != nil {
					t.Fatal(err)
				}
			}
			before := tree.current
			tree.hook = func(at string) error {
				if at == phase {
					return errInjected
				}
				return nil
			}
			if changed, err := tree.update(ctx, []byte("k"), []byte("new"), false); !errors.Is(err, errInjected) || changed {
				t.Fatalf("fault missed %v %v", changed, err)
			}
			if tree.current != before {
				t.Fatal("error made cache visible")
			}
			if _, err := tree.snapshot(); err == nil {
				t.Fatal("not poisoned")
			}
			tree.close()
			reopened, err := openDiskTree(dir, key, 7, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.close()
			r, err := reopened.snapshot()
			if err != nil {
				t.Fatal(err)
			}
			v, found, err := r.get(ctx, []byte("k"))
			if err != nil || !found {
				t.Fatalf("invalid recovered root %q %v %v", v, found, err)
			}
			expected := "old"
			if phase == "root-renamed" || phase == "directory-synced" {
				expected = "new"
			}
			if string(v) != expected {
				t.Fatalf("got %q want %s", v, expected)
			}
			rows := allDiskRows(t, r)
			if len(rows) != 10 || rows["k"] != expected {
				t.Fatal("incomplete recovered multi-page tree")
			}
			for i := 0; i < 9; i++ {
				if rows[fmt.Sprintf("k%d", i)] != "fixed" {
					t.Fatal("unrelated child changed")
				}
			}

		})
	}
}

// Child-process SIGKILL covers process interruption, not disk/controller power
// loss. The phase labels correspond to completed syscall barriers, not mocks.
func TestDiskSIGKILLPublication(t *testing.T) {
	if phase := os.Getenv("HALRO_AUTHROOT_KILL_PHASE"); phase != "" {
		key := bytes.Repeat([]byte{0x37}, 32)
		tree, err := openDiskTree(os.Getenv("HALRO_AUTHROOT_KILL_DIR"), key, 7, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		tree.hook = func(at string) error {
			if at == phase {
				if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				select {}
			}
			return nil
		}
		if _, err := tree.update(context.Background(), []byte("k"), []byte("new"), false); err != nil {
			t.Fatal(err)
		}
		t.Fatal("kill phase not reached")
	}
	for _, phase := range []string{"pages-written", "pages-synced", "root-written", "root-synced", "root-renamed", "directory-synced"} {
		t.Run(phase, func(t *testing.T) {
			tree, dir, key := newDiskFixture(t)
			if _, err := tree.update(context.Background(), []byte("k"), []byte("old"), false); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 9; i++ {
				if _, err := tree.update(context.Background(), []byte(fmt.Sprintf("k%d", i)), []byte("fixed"), false); err != nil {
					t.Fatal(err)
				}
			}
			tree.close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestDiskSIGKILLPublication$", "-test.count=1")
			cmd.Env = append(os.Environ(), "HALRO_AUTHROOT_KILL_PHASE="+phase, "HALRO_AUTHROOT_KILL_DIR="+dir)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("not interrupted: %v %s", err, output)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("unexpected terminal status: %v %s", err, output)
			}
			reopened, err := openDiskTree(dir, key, 7, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.close()
			r, err := reopened.snapshot()
			if err != nil {
				t.Fatal(err)
			}
			v, found, err := r.get(context.Background(), []byte("k"))
			if err != nil || !found {
				t.Fatalf("invalid root after kill: %q %v %v", v, found, err)
			}
			expected := "old"
			if phase == "root-renamed" || phase == "directory-synced" {
				expected = "new"
			}
			if string(v) != expected {
				t.Fatalf("got %q want %q", v, expected)
			}
			rows := allDiskRows(t, r)
			if len(rows) != 10 || rows["k"] != expected {
				t.Fatal("incomplete multi-page tree after kill")
			}
			for i := 0; i < 9; i++ {
				if rows[fmt.Sprintf("k%d", i)] != "fixed" {
					t.Fatal("unrelated child changed after kill")
				}
			}

		})
	}
}

func TestDiskConcurrentSnapshotsDuringCOWPublication(t *testing.T) {
	tree, _, _ := newDiskFixture(t)
	ctx := context.Background()
	if _, err := tree.update(ctx, []byte("k"), []byte("v"), false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				r, err := tree.snapshot()
				if err != nil {
					errs <- err
					return
				}
				v, found, err := r.get(ctx, []byte("k"))
				if err != nil || !found || len(v) == 0 {
					errs <- fmt.Errorf("snapshot answer %q %v %v", v, found, err)
					return
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		if _, err := tree.update(ctx, []byte("k"), []byte(fmt.Sprintf("v%d", i)), false); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestDiskNonemptyCollapseResplitHeightAndReopen(t *testing.T) {
	tree, dir, key := newDiskFixture(t)
	ctx := context.Background()
	for round := 0; round < maxDepth*2; round++ {
		for i := 0; i < 9; i++ {
			if _, err := tree.update(ctx, []byte(fmt.Sprintf("k%d", i)), []byte("v"), false); err != nil {
				t.Fatalf("round %d insert %d: %v", round, i, err)
			}
		}
		r, err := tree.snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if r.root.height != 2 || r.root.count != 9 {
			t.Fatalf("split: height=%d count=%d", r.root.height, r.root.count)
		}
		for i := 1; i < 9; i++ {
			if _, err := tree.update(ctx, []byte(fmt.Sprintf("k%d", i)), nil, true); err != nil {
				t.Fatal(err)
			}
		}
		r, err = tree.snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if r.root.height != 1 || r.root.count != 1 {
			t.Fatalf("collapse: height=%d count=%d", r.root.height, r.root.count)
		}
		if round%4 == 3 {
			if err := tree.close(); err != nil {
				t.Fatal(err)
			}
			tree, err = openDiskTree(dir, key, 7, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			// The cleanup registered by newDiskFixture owns the initial handle only.
			handle := tree
			t.Cleanup(func() { handle.close() })
		}
	}
}

func TestDiskRejectsSecondWriter(t *testing.T) {
	tree, dir, key := newDiskFixture(t)
	if second, err := openDiskTree(dir, key, 7, 64<<20); err == nil {
		second.close()
		t.Fatal("second writer acquired same pack")
	}
	tree.close()
	reopened, err := openDiskTree(dir, key, 7, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if _, err := reopened.update(context.Background(), []byte("k"), []byte("v"), false); err != nil {
		t.Fatal(err)
	}
}
