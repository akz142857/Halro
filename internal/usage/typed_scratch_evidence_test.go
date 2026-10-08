package usage

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/akz142857/Halro/internal/scratchstate"
)

func TestFixedScratchUsageWideNativeDependenciesStayBoundedAndFailurePoisons(t *testing.T) {
	owner, records := wideFinalizationReplayFixture(t)
	ctx := context.Background()
	reader := &replayEvidenceReader{LedgerProjection: owner}
	f := newTypedUsageFixtureAt(t, false, -1)
	o := f.options
	o.ScratchPath = filepath.Join(t.TempDir(), "fixed.reducer")
	o.ScratchMaxDiskBytes = 64 << 20
	p, err := createFixedUsageScratch(ctx, o, reader)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	runtime.GC()
	var before, prepared runtime.MemStats
	runtime.ReadMemStats(&before)
	observed := false
	reader.prefetched = func() { runtime.GC(); runtime.ReadMemStats(&prepared); observed = true }
	p.mu.Lock()
	err = p.applyReplayBatchWithFillLocked(ctx, records, true)
	p.mu.Unlock()
	if err != nil || !observed {
		t.Fatal(observed, err)
	}
	if prepared.HeapAlloc > before.HeapAlloc+16<<20 {
		t.Fatal("private scratch retained wide finalization dependencies", before.HeapAlloc, prepared.HeapAlloc)
	}
	t.Logf("accepted=%d dependency_bytes=%d retained_heap_delta=%d", len(records), len(records)*(900<<10), int64(prepared.HeapAlloc)-int64(before.HeapAlloc))
	// A fault after the first native accepted row has written makes the entire
	// unpublished candidate unusable. It cannot authorize a partly reduced root.
	q, err := createFixedUsageScratch(ctx, AuthenticatedUsageOptions{ScratchPath: filepath.Join(t.TempDir(), "poison"), ScratchMaxDiskBytes: 64 << 20, Budget: o.Budget}, reader)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Discard()
	if err := q.engine.Update(func(tx *usageTx) error {
		return diskUsagePut(tx, "active", diskUsageID(records[len(records)-1].Event.RequestID), RequestSummary{RequestID: records[len(records)-1].Event.RequestID, Sequence: 1})
	}); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	err = q.applyReplayBatchWithFillLocked(ctx, records, true)
	q.mu.Unlock()
	if !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("late actual reducer fault hidden", err)
	}
	if err := q.engine.View(func(*usageTx) error { t.Fatal("failed candidate allowed publication read"); return nil }); !errors.Is(err, scratchstate.ErrPoisoned) {
		t.Fatal(err)
	}
	if err := q.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
}
