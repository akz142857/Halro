package usage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/scratchstate"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestHotScratchBatchEvictionCursorWideValuesAndReturnedOwnership(t *testing.T) {
	if unsafe.Sizeof(scratchstate.RowBuffer{}) > scratchstate.RowBufferRAMBytes {
		t.Fatal("unpaid workspace")
	}
	f := newTypedUsageFixtureAt(t, false, -1)
	o := f.options
	o.ScratchPath = filepath.Join(t.TempDir(), "hot")
	o.ScratchMaxDiskBytes = 32 << 20
	before := o.Budget.Snapshot().Used
	p, err := createFixedUsageScratch(context.Background(), o, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	want := map[string][]byte{}
	err = p.engine.Update(func(tx *usageTx) error {
		tx.hotScratch = p.engine.hotScratch
		b := tx.Bucket([]byte("dashboard_group"))
		for i := 0; i < 512+9; i++ {
			key := []byte(fmt.Sprintf("key_%03d", i))
			v := []byte(fmt.Sprintf("value_%03d", i))
			want[string(key)] = v
			if err := b.Put(key, v); err != nil {
				return err
			}
			got := b.Get(key)
			if !bytes.Equal(got, v) {
				return ErrDiskUsageCorrupt
			}
			got[0] = 'X'
			if !bytes.Equal(b.Get(key), v) {
				return ErrDiskUsageCorrupt
			}
		}
		// A cursor must see all pending keys and exact ordering, even when created
		// before subsequent point writes.
		cur := b.Cursor()
		if err := b.Put([]byte("key_000"), []byte{}); err != nil {
			return err
		}
		want["key_000"] = []byte{}
		k, v := cur.Seek([]byte("key_000"))
		if string(k) != "key_000" || v == nil || len(v) != 0 {
			return ErrDiskUsageCorrupt
		}
		if err := b.Delete([]byte("key_001")); err != nil {
			return err
		}
		delete(want, "key_001")
		k, _ = cur.Next()
		if string(k) != "key_002" {
			return ErrDiskUsageCorrupt
		}
		large := bytes.Repeat([]byte{'w'}, 4<<10+1)
		if err := b.Put([]byte("wide"), large); err != nil {
			return err
		}
		if !bytes.Equal(b.Get([]byte("wide")), large) {
			return ErrDiskUsageCorrupt
		}
		if err := b.Put([]byte("wide"), []byte("small")); err != nil {
			return err
		}
		want["wide"] = []byte("small")
		if err := b.Delete([]byte("absent")); err != nil {
			return err
		}
		return tx.failure
	})
	if err != nil {
		t.Fatal(err)
	}
	err = p.engine.View(func(tx *usageTx) error {
		b := tx.Bucket([]byte("dashboard_group"))
		c := b.Cursor()
		rows := 0
		for k, v := c.First(); k != nil; k, v = c.Next() {
			rows++
			w, ok := want[string(k)]
			if !ok || !bytes.Equal(w, v) {
				return ErrDiskUsageCorrupt
			}
		}
		if rows != len(want) {
			return ErrDiskUsageCorrupt
		}
		return tx.failure
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if got := o.Budget.Snapshot().Used; got != before {
		t.Fatal("workspace survived known discard", before, got)
	}
	if p.scratchWork != (resourcebudget.Permit{}) || p.engine.hotScratch != nil {
		t.Fatal("pool survived actual Close")
	}
}

func TestHotScratchBatchRejectsChangedAuthenticatedOriginalAndPoisons(t *testing.T) {
	f := newTypedUsageFixtureAt(t, false, -1)
	o := f.options
	o.ScratchPath = filepath.Join(t.TempDir(), "hot")
	o.ScratchMaxDiskBytes = 32 << 20
	p, err := createFixedUsageScratch(context.Background(), o, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	key := []byte("original")
	err = p.engine.Update(func(tx *usageTx) error { return tx.Bucket([]byte("dashboard_group")).Put(key, []byte("before")) })
	if err != nil {
		t.Fatal(err)
	}
	err = p.engine.Update(func(tx *usageTx) error {
		tx.hotScratch = p.engine.hotScratch
		b := tx.Bucket([]byte("dashboard_group"))
		if string(b.Get(key)) != "before" {
			return ErrDiskUsageCorrupt
		}
		if err := b.Put(key, []byte("intent")); err != nil {
			return err
		}
		k, err := typedUsageKey(b.name, key)
		if err != nil {
			return err
		}
		// A separate actual authenticated mutation bypasses the intent facade;
		// the original-content recheck must still refuse the candidate.
		return tx.scratch.Put(k, []byte("changed underneath"))
	})
	if !errors.Is(err, scratchstate.ErrCorrupt) {
		t.Fatal("changed original accepted", err)
	}
	if err := p.engine.View(func(*usageTx) error { t.Fatal("poisoned candidate exported"); return nil }); !errors.Is(err, scratchstate.ErrPoisoned) {
		t.Fatal(err)
	}
}

func TestHotScratchBatchCreditRefusalRetiresOriginalScratch(t *testing.T) {
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 128 << 20, Disk: 128 << 20}, MaxClaims: 16, MaxOwners: 8, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	limits.Classes[resourcebudget.IO] = resourcebudget.Cost{RAM: scratchstate.WorkspaceRAMBytes}
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot().Used
	dir := filepath.Join(t.TempDir(), "private")
	p, err := createFixedUsageScratch(context.Background(), AuthenticatedUsageOptions{Budget: m, ScratchPath: dir, ScratchMaxDiskBytes: 8 << 20}, nil)
	if err == nil || p == nil || p.engine.scratch == nil || p.engine.hotScratch != nil || p.scratchWork != (resourcebudget.Permit{}) {
		t.Fatal("unpaid workspace allocated or owner lost", err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if got := m.Snapshot().Used; got != before {
		t.Fatal("credit refusal leaked owner fees", before, got)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("original scratch survived known discard", err)
	}
}

// Block only the source recheck, while the actual cold replay owns p.mu.
type hotScratchBlockedReader struct {
	*ledger.AuthenticatedLedgerProjection
	reads            int
	entered, release chan struct{}
}

func (r *hotScratchBlockedReader) ReadPosition(ctx context.Context) (ledger.Watermark, [32]byte, error) {
	r.reads++
	if r.reads == 2 {
		close(r.entered)
		<-r.release
	}
	return r.AuthenticatedLedgerProjection.ReadPosition(ctx)
}
func TestHotScratchBatchCloseJoinsReplayBeforeWorkspaceRelease(t *testing.T) {
	f := newTypedUsageFixtureAt(t, false, 0)
	o := f.options
	o.ScratchPath = filepath.Join(t.TempDir(), "hot")
	o.ScratchMaxDiskBytes = 32 << 20
	before := o.Budget.Snapshot().Used
	source := &hotScratchBlockedReader{AuthenticatedLedgerProjection: f.owner, entered: make(chan struct{}), release: make(chan struct{})}
	p, err := createFixedUsageScratch(context.Background(), o, source)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(source.release) }) }
	defer release()
	held := o.Budget.Snapshot().Used
	applied := make(chan error, 1)
	go func() {
		p.mu.Lock()
		err := p.applyReplayBatchWithFillLocked(context.Background(), f.records, true)
		p.mu.Unlock()
		applied <- err
	}()
	select {
	case <-source.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("replay never held source")
	}
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for !p.retired.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Close did not withdraw")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatal("Close outran real replay", err)
	default:
	}
	if got := o.Budget.Snapshot().Used; got != held {
		t.Fatal("live replay credit refunded", held, got)
	}
	release()
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if p.engine.hotScratch != nil || p.scratchWork != (resourcebudget.Permit{}) {
		t.Fatal("known Close retained pool")
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if got := o.Budget.Snapshot().Used; got != before {
		t.Fatal("fees survived joined close", before, got)
	}
}
