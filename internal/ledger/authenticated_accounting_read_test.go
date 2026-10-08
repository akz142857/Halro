package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestAuthenticatedLedgerAccountingReadsMatchNativeReference(t *testing.T) {
	ctx := context.Background()
	path, report, _ := authenticatedLedgerNativeFixture(t)
	p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "auth"), report, authenticatedLedgerTestOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	r, err := BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "ref.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Discard()
	for _, project := range []string{"prj_1", "other"} {
		a, ae := p.ReadOpenWorkUnitCount(ctx, project)
		b, be := r.ReadOpenWorkUnitCount(ctx, project)
		if ae != nil || be != nil || a != b {
			t.Fatal("open count", a, ae, b, be)
		}
		for _, id := range []string{"wku_1", "missing", ""} {
			for _, at := range []time.Time{time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 3, 11, 0, 0, 0, time.UTC).Add(-time.Nanosecond), time.Date(2026, 10, 3, 11, 0, 0, 0, time.UTC), time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)} {
				a, ae := p.ReadRunCapacity(ctx, project, id, at)
				b, be := r.ReadRunCapacity(ctx, project, id, at)
				if ae != nil || be != nil || a != b {
					t.Fatal("run capacity", at, a, ae, b, be)
				}
			}
			if id != "" {
				for _, include := range []bool{false, true} {
					comparePoint(t, func() (WorkUnitAccountingView, bool, error) {
						return p.ReadWorkUnitAccounting(ctx, project, id, include)
					}, func() (WorkUnitAccountingView, bool, error) {
						return r.ReadWorkUnitAccounting(ctx, project, id, include)
					})
				}
			}
		}
	}
	for _, now := range []time.Time{time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)} {
		a, ae := p.ReadPendingLeaseStats(ctx, now)
		b, be := r.ReadPendingLeaseStats(ctx, now)
		if ae != nil || be != nil || a != b {
			t.Fatal("pending stats", a, ae, b, be)
		}
	}
	view, found, err := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true)
	if err != nil || !found || len(view.Runs) != domain.MaxRunsPerWorkUnit {
		t.Fatal(found, len(view.Runs), err)
	}
	view.Runs[0].ID = "changed"
	again, _, err := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true)
	if err != nil || again.Runs[0].ID == "changed" {
		t.Fatal("returned Run alias", err)
	}
}

func TestAuthenticatedLedgerAccountingInflightAndExpiryAtEachActualCut(t *testing.T) {
	p, native, records := authenticatedAdvanceFixture(t, 0)
	ctx := context.Background()
	events := projectionTestLifecycle(1, true)
	// Acceptance before lease, then finalized request with unresolved attempt,
	// then late settlement. Never infer Inflight solely from active requests.
	for _, phase := range []int{-1, 1, 2, 4, 3} {
		if phase >= 0 {
			r, _, err := native.AppendRecordWithReceipt(ctx, events[phase])
			if err != nil {
				t.Fatal(err)
			}
			records = append(records, r)
			if _, err := p.AdvanceFromNative(ctx, native, nativeRecordPrefix(r)); err != nil {
				t.Fatal(err)
			}
		}
		a, found, err := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true)
		if err != nil || !found || a.Inflight != (phase != 3) {
			t.Fatal("inflight cut", phase, a, found, err)
		}
		report, partial, err := InspectReplayAuthenticated(native.path, testChainKey, nil)
		if err != nil || partial {
			t.Fatal(err, partial)
		}
		r, err := BuildLedgerProjection(ctx, native.path, testChainKey, filepath.Join(t.TempDir(), "ref.db"), report)
		if err != nil {
			t.Fatal(err)
		}
		want, wf, we := r.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true)
		if we != nil || !wf || !reflect.DeepEqual(a, want) {
			t.Fatal("torn accounting cut", phase, a, want, we)
		}
		got, ge := p.ReadPendingLeaseStats(ctx, events[1].OccurredAt.Add(time.Hour))
		expected, ee := r.ReadPendingLeaseStats(ctx, events[1].OccurredAt.Add(time.Hour))
		if ge != nil || ee != nil || got != expected {
			t.Fatal(phase, got, ge, expected, ee)
		}
		if err := r.Discard(); err != nil {
			t.Fatal(err)
		}
	}
	closeRun := projectionTestAttribution()[1]
	closeRun.Kind = EventRunClosed
	closeRun.EventID = "close-run"
	closeRun.Operation = "close-run"
	closeRun.CloseReason = "complete"
	r, _, err := native.AppendRecordWithReceipt(ctx, closeRun)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AdvanceFromNative(ctx, native, nativeRecordPrefix(r)); err != nil {
		t.Fatal(err)
	}
	capacity, err := p.ReadRunCapacity(ctx, "prj_1", "wku_1", events[1].OccurredAt)
	if err != nil || capacity != (RunCapacity{WorkUnitRuns: 1}) {
		t.Fatal("closed Run freed lifetime capacity", capacity, err)
	}
	stats, err := p.ReadPendingLeaseStats(ctx, time.Now())
	if err != nil || stats.Count != 0 || stats.OldestAge != 0 {
		t.Fatal("settled pending retained", stats, err)
	}
}

func TestAuthenticatedLedgerAccountingIndexFaultsDoNotExposePartialResults(t *testing.T) {
	for _, kind := range []string{"open-index", "active-index", "run-index", "run-source", "request-index", "pending-index", "pending-time", "pending-count", "missing-count"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			p, _, _, _ := recoveryViewFixture(t)
			var read func() error
			switch kind {
			case "open-index":
				mutatePoint(t, p, "open_work_units_by_project", projectionTuple("prj_1", "wku_1"), []byte("missing"))
				read = func() error {
					n, e := p.ReadOpenWorkUnitCount(ctx, "prj_1")
					if n != 0 {
						t.Error("partial count")
					}
					return e
				}
			case "active-index":
				run, _, _ := p.ReadRun(ctx, "run_1")
				mutatePoint(t, p, "active_runs_by_project", projectionActiveRunKey(run), []byte("missing"))
				read = func() error {
					v, e := p.ReadRunCapacity(ctx, "prj_1", "wku_1", run.CreatedAt)
					if v != (RunCapacity{}) {
						t.Error("partial capacity")
					}
					return e
				}
			case "run-index":
				mutatePoint(t, p, "runs_by_work_unit", projectionTuple("wku_1", "run_1"), []byte("missing"))
				read = func() error {
					v, f, e := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true)
					if f || len(v.Runs) != 0 {
						t.Error("partial runs")
					}
					return e
				}
			case "run-source":
				mutatePoint(t, p, "runs", projectionIDKey("run_1"), nil)
				read = func() error {
					_, f, e := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true)
					if f {
						t.Error("dangling run index")
					}
					return e
				}
			case "request-index":
				mutatePoint(t, p, "active_requests_by_work_unit", firstAccountingIndexKey(t, p, "active_requests_by_work_unit", projectionTuple("wku_1")), []byte("missing"))
				read = func() error {
					_, f, e := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", false)
					if f {
						t.Error("dangling active index")
					}
					return e
				}
			case "pending-index":
				mutatePoint(t, p, "pending_by_work_unit", firstAccountingIndexKey(t, p, "pending_by_work_unit", projectionTuple("wku_1")), []byte("missing"))
				read = func() error {
					_, f, e := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", false)
					if f {
						t.Error("dangling pending index")
					}
					return e
				}
			case "pending-time":
				mutatePoint(t, p, "pending_by_time", firstAccountingIndexKey(t, p, "pending_by_time", nil), []byte("missing"))
				read = func() error {
					v, e := p.ReadPendingLeaseStats(ctx, time.Now())
					if v != (PendingLeaseStatistics{}) {
						t.Error("partial stats")
					}
					return e
				}
			case "pending-count", "missing-count":
				var value any = int64(-1)
				if kind == "missing-count" {
					value = nil
				}
				mutatePoint(t, p, "meta", []byte("pending_count"), value)
				read = func() error {
					v, e := p.ReadPendingLeaseStats(ctx, time.Now())
					if v != (PendingLeaseStatistics{}) {
						t.Error("partial stats")
					}
					return e
				}
			}
			if err := read(); !errors.Is(err, ErrProjectionCorrupt) {
				t.Fatal(kind, err)
			}
			if _, _, err := p.ReadLease(ctx, "missing"); !errors.Is(err, ErrProjectionUnavailable) {
				t.Fatal("fault did not retire owner", err)
			}
		})
	}
}

func TestAuthenticatedLedgerAccountingJobQuotaAndCancelAreRetryable(t *testing.T) {
	p, _, _, _ := recoveryViewFixture(t)
	ctx := context.Background()
	before := p.budget.Snapshot().Used
	fee, err := p.budget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 140 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, f, e := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true); f || !errors.Is(e, resourcebudget.ErrExhausted) {
		t.Fatal(f, e)
	}
	if err := fee.Release(); err != nil {
		t.Fatal(err)
	}
	if _, f, e := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true); e != nil || !f {
		t.Fatal(f, e)
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if v, e := p.ReadPendingLeaseStats(c, time.Now()); v != (PendingLeaseStatistics{}) || !errors.Is(e, context.Canceled) {
		t.Fatal(v, e)
	}
	if n, e := p.ReadOpenWorkUnitCount(c, "prj_1"); n != 0 || !errors.Is(e, context.Canceled) {
		t.Fatal(n, e)
	}
	if v, e := p.ReadRunCapacity(c, "prj_1", "wku_1", time.Now()); v != (RunCapacity{}) || !errors.Is(e, context.Canceled) {
		t.Fatal(v, e)
	}
	if used := p.budget.Snapshot().Used; used != before {
		t.Fatal("fees retained", used, before)
	}
	p.mu.Lock()
	_, f, e := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true)
	p.mu.Unlock()
	if f || !errors.Is(e, authenticatedstate.ErrBusy) {
		t.Fatal("queued read", f, e)
	}
}

func firstAccountingIndexKey(t *testing.T, p *AuthenticatedLedgerProjection, bucket string, prefix []byte) []byte {
	t.Helper()
	var key []byte
	err := authenticatedPointJob(context.Background(), p, func(c context.Context, pin *authenticatedstate.Snapshot, _ ReplayAnchor) error {
		return authenticatedIndexWalk(c, pin, bucket, prefix, nil, func(k, raw []byte) (bool, error) { key = append([]byte{}, k...); return true, nil })
	})
	if err != nil || key == nil {
		t.Fatal("fixture first index unavailable", bucket, err)
	}
	return key
}
