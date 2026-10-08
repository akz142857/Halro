package usage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/akz142857/Halro/internal/ledger"
)

// All dependencies come from an actually authenticated WAL and its fresh
// projection. Only the observer and explicit attribution fault are injected.
type replayEvidenceReader struct {
	*ledger.LedgerProjection
	positionReads int
	prefetched    func()
	fault         func(*ledger.Record)
}

func (r *replayEvidenceReader) ReadPosition(ctx context.Context) (ledger.Watermark, [32]byte, error) {
	r.positionReads++
	if r.positionReads == 2 && r.prefetched != nil {
		r.prefetched() // Every prepared dependency is still live before Update.
	}
	return r.LedgerProjection.ReadPosition(ctx)
}

func (r *replayEvidenceReader) ReadFinalization(ctx context.Context, id string) (ledger.Record, bool, error) {
	f, found, err := r.LedgerProjection.ReadFinalization(ctx, id)
	if err == nil && found && r.fault != nil {
		r.fault(&f)
	}
	return f, found, err
}

func wideFinalizationReplayFixture(t *testing.T) (*ledger.LedgerProjection, []ledger.Record) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	key := bytes.Repeat([]byte{0x72}, 32)
	path := filepath.Join(root, "native.wal")
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	const requests = 96
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for step, kind := range []ledger.EventKind{ledger.EventRequestAccepted, ledger.EventRequestFinalized} {
		for i := 0; i < requests; i++ {
			e := ledger.Event{EventID: fmt.Sprintf("accepted_%03d", i), Kind: kind, RequestID: fmt.Sprintf("request_%03d", i), ProjectID: "project", PeriodID: "2026-10-07", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, PeriodStartMicros: now.Truncate(24 * time.Hour).UnixMicro(), PeriodEndMicros: now.Truncate(24 * time.Hour).Add(24 * time.Hour).UnixMicro(), OccurredAt: now.Add(time.Duration(step) * time.Second), Outcome: "success"}
			if step == 1 {
				// Legal native payload: EventID is nonempty and the complete
				// event remains below Ledger's 1MiB authenticated frame ceiling.
				e.EventID = strings.Repeat("x", 900<<10) + fmt.Sprint(i)
			}
			if _, err := native.Append(ctx, e); err != nil {
				t.Fatal(step, i, err)
			}
		}
	}
	var records []ledger.Record
	if _, err := native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(r ledger.Record) error {
		if r.Event.Kind == ledger.EventRequestAccepted {
			records = append(records, r)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	owner, err := ledger.BuildLedgerProjection(ctx, path, key, filepath.Join(root, "fresh-ledger.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Discard() })
	return owner, records
}

func TestDiskUsageReplayWideFutureFinalizationRetainedHeapIsBounded(t *testing.T) {
	owner, records := wideFinalizationReplayFixture(t)
	ctx := context.Background()
	for _, packed := range []bool{false, true} {
		t.Run(fmt.Sprintf("packed=%t", packed), func(t *testing.T) {
			reader := &replayEvidenceReader{LedgerProjection: owner}
			p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), reader)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Discard()
			runtime.GC()
			var before, prepared runtime.MemStats
			runtime.ReadMemStats(&before)
			observed := false
			reader.prefetched = func() {
				runtime.GC()
				runtime.ReadMemStats(&prepared)
				observed = true
			}
			if packed {
				p.mu.Lock()
				err = p.applyReplayBatchWithFillLocked(ctx, records, true)
				p.mu.Unlock()
			} else {
				err = p.ApplyReplayBatchContext(ctx, records)
			}
			if err != nil || !observed {
				t.Fatal("real selected dependencies were not applied", observed, err)
			}
			// Old prefetch retains >84MiB from 96 independent JSON decodes;
			// the fixed allowance covers prepared structs and temporary overhead.
			if prepared.HeapAlloc > before.HeapAlloc+16<<20 {
				t.Fatalf("short input retained wide future payloads: baseline=%d prepared=%d", before.HeapAlloc, prepared.HeapAlloc)
			}
			t.Logf("accepted=%d wide_dependency_bytes=%d retained_heap_delta=%d", len(records), len(records)*(900<<10), int64(prepared.HeapAlloc)-int64(before.HeapAlloc))
			stats, err := p.ReadStats(ctx)
			if err != nil || stats.Metrics.ActiveRequests != uint64(len(records)) || stats.Watermark.Sequence != records[len(records)-1].Sequence || stats.Totals.Requests != 0 {
				t.Fatal("future finalization suppressed accepted activity", stats, err)
			}
			final, found, err := owner.ReadFinalization(ctx, records[0].Event.RequestID)
			if err != nil || !found || len(final.Event.EventID) < 900<<10 {
				t.Fatal("wide authoritative fact changed", found, err)
			}
			if err := p.ApplyReplayBatchContext(ctx, []ledger.Record{final}); err != nil {
				t.Fatal(err)
			}
			summary, found, err := p.ReadRequest(ctx, records[0].Event.RequestID)
			if err != nil || !found || summary.Sequence != final.Sequence || summary.Outcome != "success" {
				t.Fatal("actual terminal payload/order changed", summary, found, err)
			}
		})
	}
}

func TestDiskUsageReplayCompactedEvidencePreservesAtomicFailure(t *testing.T) {
	owner, records := wideFinalizationReplayFixture(t)
	ctx := context.Background()
	for _, packed := range []bool{false, true} {
		t.Run(fmt.Sprintf("packed=%t", packed), func(t *testing.T) {
			reader := &replayEvidenceReader{LedgerProjection: owner}
			p, err := CreateDiskProjection(filepath.Join(t.TempDir(), "usage.db"), reader)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Discard()
			apply := func() error {
				if !packed {
					return p.ApplyReplayBatchContext(ctx, records)
				}
				p.mu.Lock()
				defer p.mu.Unlock()
				return p.applyReplayBatchWithFillLocked(ctx, records, true)
			}
			initial, err := p.ReadStats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, fault := range []func(*ledger.Record){
				func(r *ledger.Record) { r.Event.ProjectID = "other-project" },
				func(r *ledger.Record) { r.Event.RequestID = "other-request" },
				func(r *ledger.Record) { r.Event.PeriodTimezoneVersion++ },
				func(r *ledger.Record) { r.Event.WorkUnitID, r.Event.RunID = "other-work", "other-run" },
				func(r *ledger.Record) { r.Sequence = 0 },
			} {
				reader.fault = func(r *ledger.Record) {
					if r.Event.RequestID == records[len(records)-1].Event.RequestID {
						fault(r)
					}
				}
				if err := apply(); err == nil {
					t.Fatal("compaction hid inconsistent terminal evidence")
				}
				after, err := p.ReadStats(ctx)
				if err != nil || after != initial {
					t.Fatal("failed dependency page committed", after, initial, err)
				}
			}
			reader.fault = nil
			// Fail in the final reducer AFTER earlier rows have actually written
			// in this transaction, proving rollback rather than prefetch refusal.
			lastID := records[len(records)-1].Event.RequestID
			if err := p.engine.Update(func(tx *usageTx) error {
				return diskUsagePut(tx, "active", diskUsageID(lastID), RequestSummary{RequestID: lastID, Sequence: 1})
			}); err != nil {
				t.Fatal(err)
			}
			if err := apply(); !errors.Is(err, ErrDiskUsageCorrupt) {
				t.Fatal("late reducer corruption was hidden", err)
			}
			after, err := p.ReadStats(ctx)
			if err != nil || after != initial {
				t.Fatal("late reducer failure partially committed", after, initial, err)
			}
			if err := p.engine.View(func(tx *usageTx) error {
				_, found, err := diskUsageGet[RequestSummary](tx, "active", diskUsageID(records[0].Event.RequestID))
				if found {
					return errors.New("first reducer write survived rollback")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := p.engine.Update(func(tx *usageTx) error { return tx.Bucket([]byte("active")).Delete(diskUsageID(lastID)) }); err != nil {
				t.Fatal(err)
			}
			if err := apply(); err != nil {
				t.Fatal("clean retry did not apply the same facts", err)
			}
		})
	}
}

func TestDiskUsageFinalizationEvidenceBorrowsValidatedInputStrings(t *testing.T) {
	input := ledger.Record{Event: ledger.Event{RequestID: strings.Clone("request"), ProjectID: strings.Clone("project"), PeriodID: strings.Clone("period"), WorkUnitID: strings.Clone("work"), RunID: strings.Clone("run"), PeriodTimezoneVersion: 7}}
	e := diskUsageEvidence{finalFound: true, final: ledger.Record{Sequence: 12, Event: ledger.Event{Kind: ledger.EventRequestFinalized, RequestID: strings.Clone(input.Event.RequestID), ProjectID: strings.Clone(input.Event.ProjectID), PeriodID: strings.Clone(input.Event.PeriodID), WorkUnitID: strings.Clone(input.Event.WorkUnitID), RunID: strings.Clone(input.Event.RunID), PeriodTimezoneVersion: 7, EventID: strings.Repeat("x", 900<<10)}}}
	if err := e.compactFinalization(input); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{e.final.Event.RequestID, input.Event.RequestID}, {e.final.Event.ProjectID, input.Event.ProjectID}, {e.final.Event.PeriodID, input.Event.PeriodID}, {e.final.Event.WorkUnitID, input.Event.WorkUnitID}, {e.final.Event.RunID, input.Event.RunID}} {
		if unsafe.StringData(pair[0]) != unsafe.StringData(pair[1]) {
			t.Fatal("dependency retained its decoded attribution copy")
		}
	}
	if e.final.Event.EventID != "" || e.final.Sequence != 12 {
		t.Fatal("payload retained or order discarded")
	}
}
