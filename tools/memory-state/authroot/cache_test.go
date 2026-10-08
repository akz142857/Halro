package authroot

import (
	"context"
	"errors"
	"os"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestPackCacheAlignmentThresholdAndCounters(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "pack")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	page := int64(os.Getpagesize())
	if err := f.Truncate(3*page + 17); err != nil {
		t.Fatal(err)
	}
	var ranges [][2]int64
	c := &packCache{quantum: 2 * page, drop: func(got *os.File, off, n int64) error {
		if got != f || n == 0 || n%page != 0 || off%page != 0 {
			t.Fatal("wrong FD/range")
		}
		ranges = append(ranges, [2]int64{off, n})
		return nil
	}}
	if called, err := c.discard(f, 0, page-1); err != nil || called {
		t.Fatal(err)
	}
	if err := c.afterWrite(f, page, false); err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 0 {
		t.Fatal("premature drop")
	}
	if err := c.afterWrite(f, 2*page+17, false); err != nil {
		t.Fatal(err)
	}
	if err := c.afterWrite(f, 3*page+17, true); err != nil {
		t.Fatal(err)
	}
	if err := c.afterRead(f, 2*page+3, int(page)); err != nil {
		t.Fatal(err)
	}
	if err := c.afterRead(f, 2*page+3, int(page)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ranges, [][2]int64{{0, 2 * page}, {2 * page, page}, {2 * page, page}}) {
		t.Fatal(ranges)
	}
	want := packCacheStats{WriteSyncs: 1, WriteDrops: 2, ReadDrops: 1, AdvisedBytes: uint64(4 * page), ReadBytes: uint64(2 * page)}
	if got := c.stats(); got != want {
		t.Fatalf("stats %+v want %+v", got, want)
	}
	// A failed sync must prevent advice. force=true is only used after caller sync.
	f.Close()
	if err := c.afterWrite(f, 6*page, false); err == nil {
		t.Fatal("closed-file sync accepted")
	}
	if len(ranges) != 3 || c.stats().Errors != 1 {
		t.Fatal("advice ran after failed sync")
	}
}

func TestPackCacheReadTracksEarlierWindowsAndFixedCapacity(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "pack")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	page := int64(os.Getpagesize())
	if err := f.Truncate(40 * page); err != nil {
		t.Fatal(err)
	}
	var ranges [][2]int64
	drop := func(_ *os.File, start, length int64) error {
		if start%page != 0 || length != 2*page {
			t.Fatal("unbounded/unaligned range")
		}
		ranges = append(ranges, [2]int64{start, length})
		return nil
	}
	c := &packCache{quantum: 2 * page, drop: drop}
	if err := c.afterRead(f, 0, int(page)); err != nil {
		t.Fatal(err)
	}
	if err := c.afterRead(f, 4*page, int(page)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ranges, [][2]int64{{0, 2 * page}, {4 * page, 2 * page}}) {
		t.Fatal("earlier window omitted", ranges)
	}
	ranges = nil
	c = &packCache{quantum: 2 * page, drop: drop}
	for i := 0; i < 17; i++ {
		if err := c.afterRead(f, int64(i)*2*page, 1); err != nil {
			t.Fatal(err)
		}
	}
	if len(ranges) != 16 || c.readWindowCount != 1 || c.readWindows[0] != 32*page || c.stats().ReadDrops != 16 {
		t.Fatal("fixed range list did not flush", ranges, c.stats())
	}
}

func TestPackCacheFailureCannotPublishOrProveAbsence(t *testing.T) {
	tree, dir, key := newDiskFixture(t)
	if err := tree.bulkBuild(context.Background(), sortedFixture(513)); err != nil {
		t.Fatal(err)
	}
	before := tree.current
	c := &packCache{quantum: 64 << 20, drop: func(*os.File, int64, int64) error { return errInjected }}
	tree.cache.Store(c)
	if _, err := tree.update(context.Background(), []byte("k000000000001"), []byte("changed"), false); !errors.Is(err, errInjected) {
		t.Fatalf("write: %v", err)
	}
	if tree.current != before || !tree.poisoned || c.stats().Errors != 1 {
		t.Fatal("hint failure published or was hidden")
	}
	tree.close()
	reopened, err := openDiskTree(dir, key, 7, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if reopened.current != before {
		t.Fatal("failed write changed durable root")
	}
	pageSize := int64(os.Getpagesize())
	// An unreferenced aligned tail lets advice execute even for the last root
	// page on platforms with a larger OS page. It cannot change any root proof.
	if err := reopened.pack.Truncate((reopened.nextOffset + pageSize - 1) / pageSize * pageSize); err != nil {
		t.Fatal(err)
	}
	r, err := reopened.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	reopened.cache.Store(&packCache{quantum: int64(os.Getpagesize()), drop: func(*os.File, int64, int64) error { return errInjected }})
	for _, k := range []string{"k000000000001", "missing"} {
		reopened.cache.Load().readBytes.Store(uint64(os.Getpagesize() - 1))
		v, found, err := r.get(context.Background(), []byte(k))
		if err == nil || found || v != nil {
			t.Fatalf("failed read became an answer: %q %v %v", v, found, err)
		}
	}
	reopened.cache.Load().readBytes.Store(uint64(os.Getpagesize() - 1))
	page, err := r.scan(context.Background(), []byte("a"), []byte("z"), nil, 32, 8192)
	if err == nil || len(page.rows) != 0 {
		t.Fatal("failed scan returned partial rows")
	}
}

func TestPackCacheCloseWaitsForOwnedAdvice(t *testing.T) {
	tree, _, _ := newDiskFixture(t)
	if err := tree.bulkBuild(context.Background(), sortedFixture(513)); err != nil {
		t.Fatal(err)
	}
	pageSize := int64(os.Getpagesize())
	if err := tree.pack.Truncate((tree.nextOffset + pageSize - 1) / pageSize * pageSize); err != nil {
		t.Fatal(err)
	}
	r, err := tree.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	tree.cache.Store(&packCache{quantum: int64(os.Getpagesize()), drop: func(f *os.File, _, _ int64) error {
		close(entered)
		<-release
		_, err := f.Stat()
		return err
	}})
	tree.cache.Load().readBytes.Store(uint64(os.Getpagesize() - 1))
	readDone := make(chan error, 1)
	// One direct read avoids multiple callbacks after the test releases its gate.
	go func() {
		var buf [maxPageBytes]byte
		_, err := tree.readPage(r.root.ref, buf[:r.root.ref.length])
		readDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not enter advice")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- tree.close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("FD closed during advice: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.get(context.Background(), []byte("k000000000001")); err == nil {
		t.Fatal("closed reader answered")
	}
}

func TestPackCacheConcurrentReadersAndWriter(t *testing.T) {
	tree, _, _ := newDiskFixture(t)
	if err := tree.bulkBuild(context.Background(), sortedFixture(513)); err != nil {
		t.Fatal(err)
	}
	old, err := tree.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	tree.cache.Store(&packCache{quantum: int64(os.Getpagesize()), drop: func(*os.File, int64, int64) error { return nil }})
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				v, found, err := old.get(context.Background(), []byte("k000000000001"))
				if err != nil || !found || len(v) != 64 || v[0] != 1 {
					errs <- errors.New("old root changed during advice/write")
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 32; j++ {
			if _, err := tree.update(context.Background(), []byte("k000000000001"), []byte("changed"), false); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	stats := tree.cache.Load().stats()
	if stats.ReadDrops == 0 || stats.WriteDrops == 0 || stats.Errors != 0 {
		t.Fatalf("advice unexercised: %+v", stats)
	}
}

func TestPackCachePlatformAndConfiguration(t *testing.T) {
	tree, _, _ := newDiskFixture(t)
	page := int64(os.Getpagesize())
	for _, q := range []int64{0, -page, page - 1, page + 1} {
		if !errors.Is(tree.configureCache(q), errLimit) {
			t.Fatal("bad quantum accepted")
		}
	}
	err := tree.configureCache(page)
	if runtime.GOOS != "linux" {
		if !errors.Is(err, errCacheUnsupported) || tree.cache.Load() != nil {
			t.Fatalf("unsupported platform: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.configureCache(page); err == nil {
		t.Fatal("policy replaced")
	}
	if err := tree.bulkBuild(context.Background(), sortedFixture(513)); err != nil {
		t.Fatal(err)
	}
	r, err := tree.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(allDiskRows(t, r)) != 513 {
		t.Fatal("advice changed scan")
	}
}
