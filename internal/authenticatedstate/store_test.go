package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"math/bits"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func storeOptions(t testing.TB) StoreOptions {
	o := testOptions(t)
	return StoreOptions{Scope: o.Context, ExpectedCut: sha256.Sum256([]byte("native-genesis")), Key: o.Key, MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: 8 << 20, Budget: o.Budget}
}
func nextCut(i int) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("already-durable-native-cut-%d", i)))
}

func TestStoreDiscardMissingNamePreservesWholePersistentCreditAndOtherFiles(t *testing.T) {
	for _, name := range []string{"directory", "pages", "active.root", ".halro.lock"} {
		t.Run(name, func(t *testing.T) {
			s, o := testStore(t, 3)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			before := o.Budget.Snapshot().Used
			path := s.dir
			if name != "directory" {
				path = filepath.Join(s.dir, name)
			}
			moved := filepath.Join(filepath.Dir(s.dir), name+".moved")
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			for n := 0; n < 2; n++ {
				if err := s.Discard(); !errors.Is(err, os.ErrNotExist) || o.Budget.Snapshot().Used != before {
					t.Fatal("missing name retired/refunded live state", err, before, o.Budget.Snapshot())
				}
			}
			if name != "directory" {
				for _, other := range []string{"pages", "active.root", ".halro.lock"} {
					if other != name {
						if _, err := os.Stat(filepath.Join(s.dir, other)); err != nil {
							t.Fatal("failed preflight removed another captured file", err)
						}
					}
				}
			}
			if err := os.Rename(moved, path); err != nil {
				t.Fatal(err)
			}
			if err := s.Discard(); err != nil || o.Budget.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("restored receipt could not physically retire", err, o.Budget.Snapshot())
			}
		})
	}
}
func testStore(t testing.TB, count int) (*Store, StoreOptions) {
	s, o := testStoreWithoutCleanup(t, count)
	t.Cleanup(func() {
		if e := s.Discard(); e != nil {
			t.Error(e)
		}
	})
	return s, o
}

// Retirement-failure tests deliberately leave a charged physical debt. They
// must not register a cleanup that requires successful retirement.
func testStoreWithoutCleanup(t testing.TB, count int) (*Store, StoreOptions) {
	t.Helper()
	o := storeOptions(t)
	dir := filepath.Join(t.TempDir(), "state")
	i := 0
	s, e := CreateStore(context.Background(), dir, o, func() (Row, bool, error) {
		if i == count {
			return Row{}, false, nil
		}
		r := Row{[]byte(fmt.Sprintf("key_%06d", i)), []byte(fmt.Sprintf("value_%06d", i))}
		i++
		return r, true, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	o.DiskReservation = s.control
	return s, o
}
func storeRows(t testing.TB, s *Store) []Row {
	t.Helper()
	pin, e := s.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer pin.Close()
	var rows []Row
	cursor := ""
	for {
		page, e := pin.Range(context.Background(), RangeOptions{Cursor: cursor, MaxRows: 64, MaxBytes: 8 << 20, MaxPages: 4096})
		if e != nil {
			t.Fatal(e)
		}
		rows = append(rows, page.Rows...)
		if page.Done {
			return rows
		}
		cursor = page.Cursor
	}
}
func TestStoreAtomicCOWReadsNewPrivateValuesAndPreservesOldPin(t *testing.T) {
	s, o := testStore(t, 65)
	old, e := s.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer old.Close()
	initial := s.current
	var escaped *Transaction
	receipt, e := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		escaped = tx
		v, found, e := tx.Get([]byte("key_000005"))
		if e != nil || !found || string(v) != "value_000005" {
			return fmt.Errorf("old value %q %v %w", v, found, e)
		}
		if e = tx.Put([]byte("key_000005"), []byte("replacement")); e != nil {
			return e
		}
		v, found, e = tx.Get([]byte("key_000005"))
		if e != nil || !found || string(v) != "replacement" {
			return fmt.Errorf("private new value %q %v %w", v, found, e)
		}
		if e = tx.Delete([]byte("key_000006")); e != nil {
			return e
		}
		if e = tx.Put([]byte("added"), []byte("new")); e != nil {
			return e
		}
		return nil
	})
	if e != nil || receipt.Cut != nextCut(1) || receipt.Rows != 65 || receipt.Revision != 2 || receipt.AppendedBytes <= 0 || receipt.AppendedBytes >= 32<<10 {
		t.Fatal(receipt, e)
	}
	if _, _, e = escaped.Get([]byte("key_000005")); !errors.Is(e, ErrClosed) {
		t.Fatal("escaped transaction remained active", e)
	}
	v, found, e := old.Get(context.Background(), []byte("key_000005"))
	if e != nil || !found || string(v) != "value_000005" {
		t.Fatal("old root changed", found, string(v), e)
	}
	if _, found, e = old.Get(context.Background(), []byte("added")); e != nil || found {
		t.Fatal("old root imported new fact", found, e)
	}
	now, e := s.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer now.Close()
	if _, found, e = now.Get(context.Background(), []byte("key_000006")); e != nil || found {
		t.Fatal("deleted key not absent", found, e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	o.ExpectedCut = nextCut(1)
	reopened, e := OpenStore(context.Background(), s.dir, o)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	rows := storeRows(t, reopened)
	if len(rows) != 65 || string(rows[0].Key) != "added" {
		t.Fatal(len(rows))
	}
	if s.current.content.root == initial.content.root {
		t.Fatal("root did not change")
	}
}
func TestStoreCOWDeleteShrinkAndReinsertAcrossHeightChanges(t *testing.T) {
	s, _ := testStore(t, 100)
	ctx := context.Background()
	cut := 0
	for cycle := 0; cycle < 3; cycle++ {
		for start := 0; start < 100; start += 25 {
			cut++
			_, e := s.Update(ctx, nextCut(cut), func(tx *Transaction) error {
				for i := start; i < start+25; i++ {
					if e := tx.Delete([]byte(fmt.Sprintf("key_%06d", i))); e != nil {
						return e
					}
				}
				return nil
			})
			if e != nil {
				t.Fatal(cycle, start, e)
			}
		}
		if rows := storeRows(t, s); len(rows) != 0 || s.current.content.height != 0 {
			t.Fatal("empty root", len(rows), s.current.content.height)
		}
		for start := 0; start < 100; start += 25 {
			cut++
			_, e := s.Update(ctx, nextCut(cut), func(tx *Transaction) error {
				for i := start; i < start+25; i++ {
					if e := tx.Put([]byte(fmt.Sprintf("key_%06d", i)), []byte(fmt.Sprintf("round_%d", cycle))); e != nil {
						return e
					}
				}
				return nil
			})
			if e != nil {
				t.Fatal(cycle, start, e)
			}
		}
		rows := storeRows(t, s)
		if len(rows) != 100 || int(s.current.content.height) > 1+bits.Len(uint(len(rows))) {
			t.Fatal("unbounded churn height", len(rows), s.current.content.height)
		}
		for i, r := range rows {
			if string(r.Key) != fmt.Sprintf("key_%06d", i) || string(r.Value) != fmt.Sprintf("round_%d", cycle) {
				t.Fatal(i, r)
			}
		}
	}
}
func TestStoreCOWUpdateCostDoesNotScanHistory(t *testing.T) {
	for _, count := range []int{100, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, _ := testStore(t, count)
			receipt, e := s.Update(context.Background(), nextCut(count), func(tx *Transaction) error { return tx.Put([]byte("key_000050"), []byte("changed")) })
			if e != nil || receipt.ReadPages > 16 || receipt.WrittenNodes > 16 || receipt.AppendedBytes > 16<<10 {
				t.Fatal("history-scaled mutation", receipt, e)
			}
			t.Logf("rows=%d reads=%d nodes=%d appended=%d", count, receipt.ReadPages, receipt.WrittenNodes, receipt.AppendedBytes)
		})
	}
}
func TestStoreIgnoredMethodErrorAndCallbackFailureCannotPublishPrefix(t *testing.T) {
	for _, kind := range []string{"ignored-limit", "callback", "cancel", "quota", "panic"} {
		t.Run(kind, func(t *testing.T) {
			s, o := testStore(t, 20)
			before := s.current
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "quota" {
				s.appendLimit = 1
			}
			apply := func(tx *Transaction) error {
				if kind == "panic" {
					panic("injected callback")
				}
				if e := tx.Put([]byte("key_000001"), []byte("must-not-publish")); e != nil {
					return e
				}
				switch kind {
				case "ignored-limit":
					_ = tx.Put(bytes.Repeat([]byte{'x'}, MaxKeyBytes+1), []byte("bad"))
				case "callback":
					return errors.New("caller error")
				case "cancel":
					cancel()
				}
				return nil
			}
			var receipt UpdateReceipt
			var err error
			if kind == "panic" {
				func() {
					defer func() {
						if recover() == nil {
							t.Fatal("panic swallowed")
						}
					}()
					receipt, err = s.Update(ctx, nextCut(1), apply)
				}()
			} else {
				receipt, err = s.Update(ctx, nextCut(1), apply)
				if err == nil {
					t.Fatal("failed batch succeeded")
				}
			}
			if receipt != (UpdateReceipt{}) || s.current != before {
				t.Fatal("partial batch visible", receipt)
			}
			if e := s.Close(); e != nil {
				t.Fatal(e)
			}
			reopened, e := OpenStore(context.Background(), s.dir, o)
			if e != nil {
				t.Fatal(e)
			}
			defer reopened.Close()
			rows := storeRows(t, reopened)
			if len(rows) != 20 || string(rows[1].Value) != "value_000001" {
				t.Fatal("unpublished content recovered", rows[1])
			}
			info, e := os.Stat(filepath.Join(s.dir, "pages"))
			if e != nil || uint64(info.Size()) != before.content.extent {
				t.Fatal("orphan tail not trimmed", e)
			}
		})
	}
}
func TestStoreFaultPhaseReopenChoosesWholeOldOrNewRoot(t *testing.T) {
	phases := []string{"pages-written", "pages-synced", "root-written", "root-synced", "root-renamed", "directory-synced"}
	for index, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			s, o := testStore(t, 20)
			injected := errors.New("publication fault")
			s.hook = func(p string) error {
				if p == phase {
					return injected
				}
				return nil
			}
			receipt, e := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
				if e := tx.Put([]byte("key_000001"), []byte("new")); e != nil {
					return e
				}
				return tx.Delete([]byte("key_000002"))
			})
			if !errors.Is(e, injected) || receipt != (UpdateReceipt{}) {
				t.Fatal(receipt, e)
			}
			if _, e = s.Snapshot(context.Background()); !errors.Is(e, ErrRecovery) {
				t.Fatal("fault ignored", e)
			}
			if e = s.Close(); e != nil {
				t.Fatal(e)
			}
			expected := o.ExpectedCut
			if index >= 4 {
				expected = nextCut(1)
			}
			o.ExpectedCut = expected
			reopened, e := OpenStore(context.Background(), s.dir, o)
			if e != nil {
				t.Fatal(e)
			}
			defer reopened.Close()
			rows := storeRows(t, reopened)
			if index < 4 {
				if len(rows) != 20 || string(rows[1].Value) != "value_000001" {
					t.Fatal("partial old root")
				}
			} else {
				if len(rows) != 19 || string(rows[1].Value) != "new" || string(rows[2].Key) != "key_000003" {
					t.Fatal("partial new root")
				}
			}
		})
	}
}
func TestStoreCorruptOldValueCannotBeDeletedOrResigned(t *testing.T) {
	s, _ := testStore(t, 20)
	pin, e := s.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	leaf := firstLeaf(t, pin, []byte("key_000005"))
	defer pin.Close()
	file, e := os.OpenFile(filepath.Join(s.dir, "pages"), os.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer file.Close()
	if _, e = file.WriteAt([]byte{0xff}, int64(leaf.value.offset+1)); e != nil {
		t.Fatal(e)
	}
	before := s.current
	receipt, e := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error { return tx.Delete([]byte("key_000005")) })
	if !errors.Is(e, ErrCorrupt) || receipt != (UpdateReceipt{}) || s.current != before {
		t.Fatal("old corruption blessed", receipt, e)
	}
	if _, _, e = pin.Get(context.Background(), []byte("key_000001")); !errors.Is(e, ErrRecovery) {
		t.Fatal("other pin bypassed global store fault", e)
	}
}
func TestStorePinSlotsCloseJoinAndNestedPinClose(t *testing.T) {
	s, _ := testStore(t, 20)
	var pins [maxStorePins]*Snapshot
	for i := range pins {
		p, e := s.Snapshot(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		pins[i] = p
	}
	if _, e := s.Snapshot(context.Background()); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	if _, e := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		if e := pins[0].Close(); e != nil {
			return e
		}
		return tx.Put([]byte("new"), []byte("v"))
	}); e != nil {
		t.Fatal("pin Close nested in callback deadlocked/failed", e)
	}
	replacement, e := s.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	pins[1].mu.Lock()
	done := make(chan error, 2)
	go func() { done <- s.Close() }()
	go func() { done <- s.Close() }()
	select {
	case e := <-done:
		t.Fatal("Close skipped blocked reader", e)
	case <-time.After(20 * time.Millisecond):
	}
	pins[1].mu.Unlock()
	for range 2 {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	if _, _, e := replacement.Get(context.Background(), []byte("new")); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
}
func TestStoreTransactionRangeBindsPrivateRootAndRejectsIgnoredCursorFailure(t *testing.T) {
	s, _ := testStore(t, 20)
	_, e := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		options := RangeOptions{MaxRows: 2, MaxBytes: 1024, MaxPages: 64}
		page, e := tx.Range(options)
		if e != nil {
			return e
		}
		if page.Done || len(page.Rows) != 2 {
			return errors.New("no page")
		}
		options.Cursor = page.Cursor
		if e := tx.Put([]byte("key_000001"), []byte("changed")); e != nil {
			return e
		}
		_, _ = tx.Range(options)
		return nil
	})
	if !errors.Is(e, ErrCursor) {
		t.Fatal("private-root cursor inherited", e)
	}
}
func TestStoreCOWMixedChangesMatchIndependentMap(t *testing.T) {
	s, _ := testStore(t, 0)
	expected := map[string]string{}
	ctx := context.Background()
	for batch := 1; batch <= 30; batch++ {
		_, e := s.Update(ctx, nextCut(batch), func(tx *Transaction) error {
			for j := 0; j < 16; j++ {
				id := (batch*37 + j*19) % 201
				k := fmt.Sprintf("k_%04d", id)
				if (batch+j)%4 == 0 {
					if e := tx.Delete([]byte(k)); e != nil {
						return e
					}
					delete(expected, k)
				} else {
					v := fmt.Sprintf("v_%d_%d", batch, j)
					if e := tx.Put([]byte(k), []byte(v)); e != nil {
						return e
					}
					expected[k] = v
				}
			}
			return nil
		})
		if e != nil {
			t.Fatal(batch, e)
		}
		rows := storeRows(t, s)
		actual := map[string]string{}
		for _, r := range rows {
			actual[string(r.Key)] = string(r.Value)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatal("mixed COW lost/added content", batch, len(actual), len(expected))
		}
	}
}
func TestStoreSnapshotsReadWhileAppendingAndClose(t *testing.T) {
	s, _ := testStore(t, 65)
	pin, e := s.Snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			v, found, e := pin.Get(context.Background(), []byte("key_000001"))
			if e != nil || !found || string(v) != "value_000001" {
				t.Error(found, string(v), e)
				return
			}
		}
	}()
	for i := 1; i <= 20; i++ {
		if _, e = s.Update(context.Background(), nextCut(i), func(tx *Transaction) error { return tx.Put([]byte("key_000001"), []byte(fmt.Sprint(i))) }); e != nil {
			t.Fatal(i, e)
		}
	}
	wg.Wait()
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestStoreReopenRequiresPersistentReservationAndDiscardExcludesWriters(t *testing.T) {
	s, o := testStore(t, 20)
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	bad := o
	bad.DiskReservation = resourcebudget.Permit{}
	if reopened, e := OpenStore(context.Background(), s.dir, bad); !errors.Is(e, resourcebudget.ErrInvalid) || reopened != nil {
		t.Fatal("uncharged persistent open accepted", reopened, e)
	}
	bad = o
	bad.Budget = testBudget(t)
	if reopened, e := OpenStore(context.Background(), s.dir, bad); !errors.Is(e, resourcebudget.ErrInvalid) || reopened != nil {
		t.Fatal("other manager's credit accepted", reopened, e)
	}
	credit, e := s.DiskReservation()
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if credit != (resourcebudget.Permit{}) {
			_ = credit.Release()
		}
	}()
	o.DiskReservation = credit
	before := o.Budget.Snapshot().Used.Disk
	reopened, e := OpenStore(context.Background(), s.dir, o)
	if e != nil {
		t.Fatal(e)
	}
	if used := o.Budget.Snapshot().Used.Disk; used != before || used != o.MaxDiskBytes {
		t.Fatal("persistent reservation lost/duplicated", before, used)
	}
	if e := s.Discard(); !errors.Is(e, lock.ErrAlreadyLocked) {
		t.Fatal("discard ignored another writer", e)
	}
	if _, e := os.Stat(filepath.Join(s.dir, "pages")); e != nil {
		t.Fatal("live bytes removed", e)
	}
	if e = reopened.Close(); e != nil {
		t.Fatal(e)
	}
	if e = s.Discard(); e != nil {
		t.Fatal(e)
	}
	if used := o.Budget.Snapshot().Used.Disk; used != o.MaxDiskBytes {
		t.Fatal("external reservation silently refunded", used)
	}
	if e = credit.Release(); e != nil {
		t.Fatal(e)
	}
	credit = resourcebudget.Permit{}
	if used := o.Budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
		t.Fatal("retired persistence leaked", used)
	}
}

func TestStoreCursorBindsNativeCutEvenWhenContentsDoNotChange(t *testing.T) {
	s, _ := testStore(t, 20)
	var cursor string
	if _, e := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error {
		page, e := tx.Range(RangeOptions{MaxRows: 2, MaxBytes: 1024, MaxPages: 64})
		cursor = page.Cursor
		return e
	}); e != nil {
		t.Fatal(e)
	}
	receipt, e := s.Update(context.Background(), nextCut(2), func(tx *Transaction) error {
		_, _ = tx.Range(RangeOptions{Cursor: cursor, MaxRows: 2, MaxBytes: 1024, MaxPages: 64})
		return nil
	})
	if !errors.Is(e, ErrCursor) || receipt != (UpdateReceipt{}) || s.current.cut != nextCut(1) {
		t.Fatal("same contents inherited other native cut cursor", receipt, e)
	}
}

func TestStoreForeignPendingAndChangedActiveRootArePreserved(t *testing.T) {
	for _, kind := range []string{"pending", "root"} {
		t.Run(kind, func(t *testing.T) {
			s, o := testStore(t, 20)
			if e := s.Close(); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(s.dir, "root.pending")
			if kind == "root" {
				path = filepath.Join(s.dir, "active.root")
			}
			original, e := os.ReadFile(filepath.Join(s.dir, "active.root"))
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(path, []byte("foreign-or-truncated"), 0o600); e != nil {
				t.Fatal(e)
			}
			if reopened, e := OpenStore(context.Background(), s.dir, o); e == nil || reopened != nil {
				t.Fatal("malformed control accepted", reopened, e)
			}
			b, e := os.ReadFile(path)
			if e != nil || string(b) != "foreign-or-truncated" {
				t.Fatal("foreign control deleted", string(b), e)
			}
			if kind == "pending" {
				if e = os.Remove(path); e != nil {
					t.Fatal(e)
				}
			} else {
				if e = os.WriteFile(path, original, 0o600); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

func TestStoreSignedRootCannotMixAnotherPack(t *testing.T) {
	s, o := testStore(t, 20)
	other, _ := testStore(t, 20)
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	if e := other.Close(); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(s.dir, "active.root")
	original, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	foreign, e := os.ReadFile(filepath.Join(other.dir, "active.root"))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, foreign, 0o600); e != nil {
		t.Fatal(e)
	}
	if reopened, e := OpenStore(context.Background(), s.dir, o); !errors.Is(e, ErrCorrupt) || reopened != nil {
		t.Fatal("valid MAC from another pack accepted", reopened, e)
	}
	if e = os.WriteFile(path, original, 0o600); e != nil {
		t.Fatal(e)
	}
}
func FuzzCOWRootDecoder(f *testing.F) {
	scope := sha256.Sum256([]byte("fuzz-scope"))
	cut := nextCut(1)
	key := bytes.Repeat([]byte{0x38}, 32)
	var id [16]byte
	id[0] = 1
	r := cowRoot{cut: cut, revision: 1, content: commitment{context: scope, id: id, extent: headerBytes, root: ref{digest: emptyDigest(scope)}}}
	raw, e := r.encode(key)
	if e != nil {
		f.Fatal(e)
	}
	f.Add(raw)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		r, e := decodeCOWRoot(b, key, scope, cut)
		if e == nil {
			canonical, e := r.encode(key)
			if e != nil || !bytes.Equal(canonical, b) {
				t.Fatal("noncanonical root", e)
			}
		} else if r != (cowRoot{}) {
			t.Fatal("partial root on failure")
		}
	})
}
