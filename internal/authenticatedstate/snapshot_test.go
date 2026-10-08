package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func testBudget(t testing.TB) *resourcebudget.Manager {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 256 << 20}, MaxClaims: 16, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	m, e := resourcebudget.New(limits)
	if e != nil {
		t.Fatal(e)
	}
	if e = m.SetPressure(resourcebudget.Healthy); e != nil {
		t.Fatal(e)
	}
	return m
}
func testOptions(t testing.TB) Options {
	return Options{Context: sha256.Sum256([]byte("independent-test-scope-and-native-cut")), Key: bytes.Repeat([]byte{0x38}, 32), MaxDiskBytes: 64 << 20, Budget: testBudget(t)}
}
func testBuild(t testing.TB, o Options, count int) (*Snapshot, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.pack")
	i := 0
	s, e := Build(context.Background(), path, o, func() (Row, bool, error) {
		if i == count {
			return Row{}, false, nil
		}
		k := []byte(fmt.Sprintf("key_%06d", i))
		v := []byte(fmt.Sprintf("value_%06d", i))
		i++
		return Row{k, v}, true, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := s.Discard(); e != nil {
			t.Error(e)
		}
	})
	return s, path
}
func TestSnapshotReopenGetAndCompleteRange(t *testing.T) {
	for _, count := range []int{0, 1, 3, 4, 5, 15, 16, 17, 65, 1025} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			o := testOptions(t)
			owner, path := testBuild(t, o, count)
			extent := owner.current.extent
			if e := owner.Close(); e != nil {
				t.Fatal(e)
			}
			stats := o.Budget.Snapshot()
			if stats.Used.RAM != resourcebudget.ClaimRAMBytes || stats.Used.Disk != int64(extent) {
				t.Fatal("Close refunded persistent bytes or retained RAM", stats)
			}
			s, e := Open(context.Background(), path, o)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			for i := 0; i < count; i++ {
				v, ok, e := s.Get(context.Background(), []byte(fmt.Sprintf("key_%06d", i)))
				if e != nil || !ok || string(v) != fmt.Sprintf("value_%06d", i) {
					t.Fatal(i, ok, string(v), e)
				}
			}
			if _, ok, e := s.Get(context.Background(), []byte("nonexistent")); e != nil || ok {
				t.Fatal(ok, e)
			}
			cursor := ""
			seen := 0
			for {
				p, e := s.Range(context.Background(), RangeOptions{Cursor: cursor, MaxRows: 13, MaxBytes: 512, MaxPages: 256})
				if e != nil {
					t.Fatal(e)
				}
				for _, r := range p.Rows {
					if string(r.Key) != fmt.Sprintf("key_%06d", seen) {
						t.Fatal("missing/unordered", seen, string(r.Key))
					}
					seen++
				}
				if p.Done {
					if p.Cursor != "" {
						t.Fatal("EOF cursor")
					}
					break
				}
				if p.Cursor == "" || len(p.Rows) == 0 {
					t.Fatal("no progress")
				}
				cursor = p.Cursor
			}
			if seen != count {
				t.Fatal(seen, count)
			}
		})
	}
}
func TestSnapshotWideKeysValuesAndEmptyValue(t *testing.T) {
	o := testOptions(t)
	path := filepath.Join(t.TempDir(), "wide.pack")
	rows := []Row{{bytes.Repeat([]byte{1}, MaxKeyBytes), bytes.Repeat([]byte{3}, MaxValueBytes)}, {bytes.Repeat([]byte{2}, MaxKeyBytes), nil}}
	i := 0
	s, e := Build(context.Background(), path, o, func() (Row, bool, error) {
		if i == len(rows) {
			return Row{}, false, nil
		}
		r := rows[i]
		i++
		return r, true, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Discard()
	for _, r := range rows {
		v, ok, e := s.Get(context.Background(), r.Key)
		if e != nil || !ok || !bytes.Equal(v, r.Value) {
			t.Fatal(ok, len(v), e)
		}
	}
	p, e := s.Range(context.Background(), RangeOptions{MaxRows: 64, MaxBytes: 8 << 20, MaxPages: 16})
	if e != nil || !p.Done || len(p.Rows) != 2 {
		t.Fatal(len(p.Rows), e)
	}
	if p, e = s.Range(context.Background(), RangeOptions{MaxRows: 64, MaxBytes: 1, MaxPages: 16}); !errors.Is(e, ErrLimit) || len(p.Rows) != 0 {
		t.Fatal(p, e)
	}
}
func TestSnapshotCursorBindsRootAndRange(t *testing.T) {
	o := testOptions(t)
	s, _ := testBuild(t, o, 12)
	other, _ := testBuild(t, o, 12)
	ctx := context.Background()
	options := RangeOptions{Lo: []byte("key_000002"), Hi: []byte("key_000009"), MaxRows: 2, MaxBytes: 1024, MaxPages: 64}
	page, e := s.Range(ctx, options)
	if e != nil || page.Done || len(page.Rows) != 2 {
		t.Fatal(page, e)
	}
	options.Cursor = page.Cursor
	bad := options
	bad.Hi = []byte("key_000010")
	if _, e = s.Range(ctx, bad); !errors.Is(e, ErrCursor) {
		t.Fatal(e)
	}
	if _, e = other.Range(ctx, options); !errors.Is(e, ErrCursor) {
		t.Fatal(e)
	}
	bad = options
	bad.Cursor = options.Cursor[:len(options.Cursor)-1] + "!"
	if _, e = s.Range(ctx, bad); !errors.Is(e, ErrCursor) {
		t.Fatal(e)
	}
	var keys []string
	keys = append(keys, string(page.Rows[0].Key), string(page.Rows[1].Key))
	for {
		page, e = s.Range(ctx, options)
		if e != nil {
			t.Fatal(e)
		}
		for _, r := range page.Rows {
			keys = append(keys, string(r.Key))
		}
		if page.Done {
			break
		}
		options.Cursor = page.Cursor
	}
	if !reflect.DeepEqual(keys, []string{"key_000002", "key_000003", "key_000004", "key_000005", "key_000006", "key_000007", "key_000008"}) {
		t.Fatal(keys)
	}
}
func firstLeaf(t testing.TB, s *Snapshot, key []byte) node {
	t.Helper()
	q := query{context.Background(), s, 0, 128}
	d := descriptor{count: s.current.count, height: s.current.height, ref: s.current.root}
	for {
		n, e := q.load(d)
		if e != nil {
			t.Fatal(e)
		}
		if n.height == 1 {
			return n
		}
		found := false
		for _, c := range n.children {
			if bytes.Compare(key, c.first) >= 0 && bytes.Compare(key, c.last) <= 0 {
				d = c
				found = true
				break
			}
		}
		if !found {
			t.Fatal("test key absent")
		}
	}
}
func TestSnapshotCorruptionNeverReturnsPartialOrFalseAbsence(t *testing.T) {
	for _, kind := range []string{"value", "leaf", "root", "truncated", "header", "replacement"} {
		t.Run(kind, func(t *testing.T) {
			o := testOptions(t)
			s, path := testBuild(t, o, 20)
			key := []byte("key_000005")
			leaf := firstLeaf(t, s, key)
			file, e := os.OpenFile(path, os.O_RDWR, 0)
			if e != nil {
				t.Fatal(e)
			}
			defer file.Close()
			switch kind {
			case "value":
				_, e = file.WriteAt([]byte{0xff}, int64(leaf.value.offset+1))
			case "leaf":
				_, e = file.WriteAt([]byte{0xff}, int64(leaf.value.offset+uint64(leaf.value.length)))
			case "root":
				_, e = file.WriteAt([]byte{0xff}, int64(s.current.root.offset))
			case "truncated":
				e = file.Truncate(int64(s.current.extent) - 1)
			case "header":
				_, e = file.WriteAt([]byte{0xff}, 8)
			case "replacement":
				e = os.Rename(path, path+".original")
				if e == nil {
					e = os.WriteFile(path, []byte("foreign"), 0o600)
				}
				t.Cleanup(func() { _ = os.Remove(path); _ = os.Rename(path+".original", path) })
			}
			if e != nil {
				t.Fatal(e)
			}
			p, e := s.Range(context.Background(), RangeOptions{MaxRows: 64, MaxBytes: 8192, MaxPages: 256})
			if !errors.Is(e, ErrCorrupt) || len(p.Rows) != 0 || p.Done || p.Cursor != "" {
				t.Fatal("partial business page", p, e)
			}
			if v, ok, e := s.Get(context.Background(), key); !errors.Is(e, ErrCorrupt) || ok || v != nil {
				t.Fatal("false absence or stale value", ok, v, e)
			}
		})
	}
}
func TestSnapshotCancelsAndOwnsCleanupWithoutDeletingReplacement(t *testing.T) {
	for _, kind := range []string{"cancel", "source", "quota", "order", "replace"} {
		t.Run(kind, func(t *testing.T) {
			o := testOptions(t)
			if kind == "quota" {
				o.MaxDiskBytes = headerBytes + 4
			}
			path := filepath.Join(t.TempDir(), "snapshot.pack")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			injected := errors.New("source failure")
			s, e := Build(ctx, path, o, func() (Row, bool, error) {
				calls++
				if calls == 2 {
					switch kind {
					case "cancel":
						cancel()
					case "source":
						return Row{}, false, injected
					case "replace":
						if e := os.Rename(path, path+".owned"); e != nil {
							t.Fatal(e)
						}
						if e := os.WriteFile(path, []byte("foreign"), 0o600); e != nil {
							t.Fatal(e)
						}
						cancel()
					}
				}
				if calls > 3 {
					return Row{}, false, nil
				}
				key := []byte(fmt.Sprintf("key_%d", calls))
				if kind == "order" {
					key = []byte("same")
				}
				return Row{key, []byte("v")}, true, nil
			})
			if e == nil || s != nil {
				t.Fatal("failed build returned owner", s, e)
			}
			stats := o.Budget.Snapshot()
			if kind == "replace" {
				if !errors.Is(e, ownedfile.ErrReplaced) {
					t.Fatal(e)
				}
				b, e := os.ReadFile(path)
				if e != nil || string(b) != "foreign" {
					t.Fatal(string(b), e)
				}
				if stats.Used.Disk != o.MaxDiskBytes || stats.Used.RAM != resourcebudget.ClaimRAMBytes {
					t.Fatal("lost persistent charge", stats)
				}
			} else {
				if _, e := os.Stat(path); !errors.Is(e, os.ErrNotExist) {
					t.Fatal(e)
				}
				if stats.Used != (resourcebudget.Cost{}) || stats.Owners != 0 {
					t.Fatal("leaked permit", stats)
				}
			}
		})
	}
}
func TestSnapshotCommitmentRejectsEveryChangedByteAndIndependentContext(t *testing.T) {
	o := testOptions(t)
	s, path := testBuild(t, o, 5)
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	header := raw[:headerBytes]
	for i := range header {
		changed := bytes.Clone(header)
		changed[i] ^= 1
		if c, e := decodeCommitment(changed, o.Key, o.Context); e == nil || c != (commitment{}) {
			t.Fatal(i, c, e)
		}
	}
	for n := 0; n < headerBytes; n++ {
		if _, e := decodeCommitment(header[:n], o.Key, o.Context); e == nil {
			t.Fatal(n)
		}
	}
	different := o
	different.Context[0] ^= 1
	if x, e := Open(context.Background(), path, different); e == nil || x != nil {
		t.Fatal(x, e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
}
func TestSnapshotLimitsCancellationAndCloseJoin(t *testing.T) {
	o := testOptions(t)
	s, _ := testBuild(t, o, 20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, e := s.Get(ctx, []byte("key_000000")); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if p, e := s.Range(context.Background(), RangeOptions{MaxRows: 64, MaxBytes: 8192, MaxPages: 1}); !errors.Is(e, ErrLimit) || len(p.Rows) != 0 {
		t.Fatal(p, e)
	}
	if v, ok, e := s.Get(context.Background(), []byte("key_000000")); e != nil || !ok || string(v) != "value_000000" {
		t.Fatal(ok, e)
	}
	s.mu.Lock()
	if _, _, e := s.Get(context.Background(), []byte("key_000000")); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() { close(entered); done <- s.Close() }()
	<-entered
	select {
	case e := <-done:
		t.Fatal("Close did not join active reader", e)
	case <-time.After(20 * time.Millisecond):
	}
	s.mu.Unlock()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.Get(context.Background(), []byte("key_000000")); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
}
func TestSnapshotConcurrentReadersAndClose(t *testing.T) {
	o := testOptions(t)
	s, _ := testBuild(t, o, 100)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _, e := s.Get(context.Background(), []byte("key_000001"))
				if e != nil && !errors.Is(e, ErrBusy) && !errors.Is(e, ErrClosed) {
					t.Error(e)
				}
			}
		}()
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	wg.Wait()
}
func FuzzSnapshotNodeDecoder(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("HSN\x01"))
	f.Fuzz(func(t *testing.T, b []byte) {
		n, e := decodeNode(b, 1<<20)
		if e == nil {
			raw, e := encodeNode(n, 1<<20)
			if e != nil || !bytes.Equal(raw, b) {
				t.Fatal("noncanonical", e)
			}
		}
	})
}

func TestSnapshotPressureDoesNotOpenNewIOOrRefundExistingDisk(t *testing.T) {
	o := testOptions(t)
	s, _ := testBuild(t, o, 3)
	before := o.Budget.Snapshot().Used
	if e := o.Budget.SetPressure(resourcebudget.Pressured); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.Get(context.Background(), []byte("key_000000")); !errors.Is(e, resourcebudget.ErrPressure) {
		t.Fatal(e)
	}
	output := filepath.Join(t.TempDir(), "refused.pack")
	if owner, e := Build(context.Background(), output, o, func() (Row, bool, error) { t.Fatal("source read before admission"); return Row{}, false, nil }); !errors.Is(e, resourcebudget.ErrPressure) || owner != nil {
		t.Fatal(owner, e)
	}
	if _, e := os.Stat(output); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
	if used := o.Budget.Snapshot().Used; used != before {
		t.Fatal("refunded existing ownership", before, used)
	}
	if e := o.Budget.SetPressure(resourcebudget.Healthy); e != nil {
		t.Fatal(e)
	}
	if _, found, e := s.Get(context.Background(), []byte("key_000000")); e != nil || !found {
		t.Fatal(found, e)
	}
}

func TestSnapshotSignedChildMetadataMustMatchActualPage(t *testing.T) {
	for _, field := range []string{"count", "bounds", "height"} {
		t.Run(field, func(t *testing.T) {
			o := testOptions(t)
			s, path := testBuild(t, o, 20)
			q := query{context.Background(), s, 0, 32}
			root, e := q.load(descriptor{count: s.current.count, height: s.current.height, ref: s.current.root})
			if e != nil {
				t.Fatal(e)
			}
			switch field {
			case "count":
				root.children[0].count++
				root.count++
			case "bounds":
				root.children[0].first = []byte("key_00000/")
			case "height":
				root.children[len(root.children)-1].height++
			}
			raw, e := encodeNode(root, s.current.extent)
			if e != nil {
				t.Fatal(e)
			}
			if len(raw) != int(s.current.root.length) {
				t.Fatal("fixture must preserve length")
			}
			file, e := os.OpenFile(path, os.O_RDWR, 0)
			if e != nil {
				t.Fatal(e)
			}
			defer file.Close()
			if _, e = file.WriteAt(raw, int64(s.current.root.offset)); e != nil {
				t.Fatal(e)
			}
			s.current.root.digest = sha256.Sum256(raw)
			s.current.count = root.count
			header, e := s.current.encode(o.Key)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = file.WriteAt(header, 0); e != nil {
				t.Fatal(e)
			}
			s.headerDigest = sha256.Sum256(header)
			if p, e := s.Range(context.Background(), RangeOptions{MaxRows: 64, MaxBytes: 8192, MaxPages: 256}); !errors.Is(e, ErrCorrupt) || len(p.Rows) != 0 {
				t.Fatal("signed mismatch accepted", p, e)
			}
		})
	}
}
