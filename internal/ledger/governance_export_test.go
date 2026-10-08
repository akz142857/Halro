package ledger

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func collectExportFacts(ctx context.Context, v GovernanceExportView, rows int) ([]GovernanceExportWorkUnit, []GovernanceExportRun, error) {
	units, runs := []GovernanceExportWorkUnit{}, []GovernanceExportRun{}
	o := GovernanceListOptions{Limits: ProjectionPageLimits{Rows: rows, Bytes: GovernanceListMaxBytes}}
	for {
		page, err := v.ReadWorkUnitPage(ctx, o)
		if err != nil {
			return nil, nil, err
		}
		if page.Scanned > rows || page.Bytes > GovernanceListMaxBytes {
			return nil, nil, errors.New("unbounded unit page")
		}
		units = append(units, page.Items...)
		if page.NextID == "" {
			break
		}
		o.After = page.NextID
	}
	o.After = ""
	for {
		page, err := v.ReadRunPage(ctx, o)
		if err != nil {
			return nil, nil, err
		}
		if page.Scanned > rows || page.Bytes > GovernanceListMaxBytes {
			return nil, nil, errors.New("unbounded run page")
		}
		runs = append(runs, page.Items...)
		if page.NextID == "" {
			break
		}
		o.After = page.NextID
	}
	return units, runs, nil
}

func TestGovernanceExportMatchesSnapshotAcrossLifecycleAndAliases(t *testing.T) {
	p, s := projectionTestStore(t), NewState()
	ctx := context.Background()
	events := cohortTestAttribution()
	alias := events[0]
	alias.EventID = "unit-create-alias"
	events = append(events, alias)
	life := projectionTestLifecycle(9, true)
	// Legacy State is the comparison oracle for its supported normal order;
	// finalized-before-settlement is covered by the fixed-cut test below.
	events = append(events, life...)
	closed := governanceEvent("export-close-unit", EventWorkUnitClosed, life[0].OccurredAt.Add(time.Hour))
	closed.WorkUnitID = "wku_1"
	events = append(events, closed)
	alias = closed
	alias.EventID = "unit-close-alias"
	events = append(events, alias, life[3], life[3])
	closed = governanceEvent("export-close-run", EventRunClosed, life[0].OccurredAt.Add(2*time.Hour))
	closed.WorkUnitID, closed.RunID, closed.CloseReason = "wku_1", "run_1", "done"
	events = append(events, closed)
	for i, e := range events {
		r := projectionTestRecord(uint64(i+1), e)
		if err := s.Apply(r); err != nil {
			t.Fatal(i, err)
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal(i, err)
		}
		want, err := s.GovernanceSnapshot(ctx, GovernanceSnapshotOptions{IncludeRuns: true})
		if err != nil {
			t.Fatal(err)
		}
		wantUnits, wantRuns := []GovernanceExportWorkUnit{}, []GovernanceExportRun{}
		for _, w := range want.WorkUnits {
			wantUnits = append(wantUnits, GovernanceExportWorkUnit{w.ID, w.ProjectID, w.Status, w.CreatedAt, w.ClosedAt, w.OutcomeDefinitions})
		}
		for _, r := range want.Runs {
			wantRuns = append(wantRuns, GovernanceExportRun{r.ID, r.ProjectID, r.WorkUnitID, r.BudgetMicrosUSD, r.Status, r.CreatedAt, r.ExpiresAt, r.ClosedAt})
		}
		err = p.WithGovernanceExport(ctx, func(v GovernanceExportView) error {
			units, runs, err := collectExportFacts(ctx, v, 1)
			if err != nil {
				return err
			}
			if v.Head() != want.Watermark || !reflect.DeepEqual(units, wantUnits) || !reflect.DeepEqual(runs, wantRuns) {
				t.Fatalf("cut %d: got %#v %#v want %#v %#v", i+1, units, runs, wantUnits, wantRuns)
			}
			refs := []WorkUnitReference{{"prj_1", "wku_1"}, {"prj_1", "wku_1"}, {"other", "wku_1"}, {"prj_1", "wku_absent"}}
			page, err := v.ReadMaturityPage(ctx, refs)
			if err != nil {
				return err
			}
			if page.Watermark != want.Watermark || len(page.Items) != 4 || !page.Items[0].Found || page.Items[0].Status != want.WorkUnits[0].Status || page.Items[0].Inflight != want.InflightWorkUnit["wku_1"] || page.Items[0] != page.Items[1] || page.Items[2].Found || page.Items[3].Found {
				t.Fatal("maturity mismatch", page, want)
			}
			return v.Complete(ctx)
		})
		if err != nil {
			t.Fatal(i, err)
		}
	}
}

func TestGovernanceExportFixedCutSurvivesLaterWritesWithoutOpenReader(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	seq := projectionApplyEvents(t, p, cohortTestAttribution(), 0)
	life := projectionTestLifecycle(9, true)
	seq = projectionApplyEvents(t, p, life[:3], seq)
	seq = projectionApplyEvents(t, p, life[4:], seq)
	cut := seq
	var saved *projectionExportView
	err := p.WithGovernanceExport(ctx, func(view GovernanceExportView) error {
		saved = view.(*projectionExportView)
		if p.db.Stats().OpenTxN != 0 {
			t.Fatal("capture held a reader")
		}
		// Mutate after capture, including both lifecycle closes and late settlement.
		seq = projectionApplyEvents(t, p, life[3:4], seq)
		wuClose := governanceEvent("export-late-wu-close", EventWorkUnitClosed, time.Now())
		wuClose.WorkUnitID = "wku_1"
		runClose := governanceEvent("export-late-run-close", EventRunClosed, time.Now())
		runClose.WorkUnitID, runClose.RunID, runClose.CloseReason = "wku_1", "run_1", "done"
		seq = projectionApplyEvents(t, p, []Event{wuClose, runClose}, seq)
		for i := 0; i < 30; i++ {
			e := cohortTestAttribution()[0]
			e.EventID, e.Operation, e.WorkUnitID = fmt.Sprintf("future-%02d", i), fmt.Sprintf("future-%02d", i), fmt.Sprintf("wku_0%02d", i)
			seq = projectionApplyEvents(t, p, []Event{e}, seq)
		}
		units, runs, err := collectExportFacts(ctx, view, 1)
		if err != nil {
			return err
		}
		if len(units) != 1 || len(runs) != 1 || units[0].Status != domain.WorkUnitOpen || units[0].ClosedAt != nil || runs[0].Status != domain.RunActive || runs[0].ClosedAt != nil || view.Head().Sequence != cut {
			t.Fatal("later close leaked into cut", units, runs, view.Head())
		}
		page, err := view.ReadMaturityPage(ctx, []WorkUnitReference{{"prj_1", "wku_1"}, {"prj_1", "wku_000"}})
		if err != nil {
			return err
		}
		if !page.Items[0].Inflight || page.Items[1].Found {
			t.Fatal("late settlement/future creation leaked", page)
		}
		if p.db.Stats().OpenTxN != 0 {
			t.Fatal("page retained a reader")
		}
		if err := view.Complete(ctx); err != nil {
			return err
		}
		if saved.p != nil || saved.operationCtx != nil || saved.pendingMaturity != nil {
			t.Fatal("sealed view retained owner")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if page, err := saved.ReadMaturityPage(ctx, nil); !errors.Is(err, ErrGovernanceExportClosed) || page.Watermark != (Watermark{}) {
		t.Fatal(page, err)
	}
	live, err := p.ReadWorkUnitMaturityPage(ctx, []WorkUnitReference{{"prj_1", "wku_1"}})
	if err != nil || live.Items[0].Inflight || live.Items[0].Status != domain.WorkUnitClosed {
		t.Fatal(live, err)
	}
}

func TestGovernanceExportCompletionRequiresFactsAndOperationContext(t *testing.T) {
	for _, mode := range []string{"omit-complete", "incomplete", "cancel-root", "cancel-read", "callback-error", "generation", "same-head-hash", "closed-owner"} {
		t.Run(mode, func(t *testing.T) {
			p := projectionTestStore(t)
			projectionApplyEvents(t, p, cohortTestAttribution(), 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var saved *projectionExportView
			err := p.WithGovernanceExport(ctx, func(view GovernanceExportView) error {
				saved = view.(*projectionExportView)
				if mode == "callback-error" {
					return errors.New("sink failed")
				}
				if mode == "incomplete" {
					return view.Complete(context.Background())
				}
				if mode == "cancel-root" {
					cancel()
				}
				readCtx := context.Background()
				if mode == "cancel-read" {
					c, stop := context.WithCancel(readCtx)
					stop()
					readCtx = c
				}
				if mode == "generation" {
					p.mu.Lock()
					p.generation[0]++
					p.mu.Unlock()
				}
				if mode == "same-head-hash" {
					if err := p.db.Update(func(tx *bbolt.Tx) error {
						pos, _, err := projectionGet[projectionPosition](tx, "meta", []byte("position"))
						if err != nil {
							return err
						}
						pos.Hash[0]++
						return projectionPut(tx, "meta", []byte("position"), pos)
					}); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "closed-owner" {
					if err := p.Close(); err != nil {
						t.Fatal(err)
					}
				}
				units, runs, readErr := collectExportFacts(readCtx, view, 1)
				if mode != "omit-complete" && (readErr == nil || units != nil || runs != nil) {
					t.Fatal("failed read returned facts", units, runs, readErr)
				}
				if mode == "omit-complete" {
					return nil
				}
				// Ignoring the source failure cannot regain permission with a fresh ctx.
				if err := view.Complete(context.Background()); err == nil {
					t.Fatal("failed read permitted completion")
				}
				return nil
			})
			if err == nil {
				t.Fatal("unsealed/failed source succeeded")
			}
			if mode == "cancel-root" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if saved.p != nil || saved.operationCtx != nil || saved.pendingMaturity != nil {
				t.Fatal("callback exit retained references")
			}
		})
	}
}

func TestGovernanceExportLimitsRetryCoverageAndDedup(t *testing.T) {
	for _, mode := range []string{"retry", "ignore", "unrelated", "invalid-refs"} {
		t.Run(mode, func(t *testing.T) {
			p := projectionTestStore(t)
			ctx := context.Background()
			refs := []WorkUnitReference{}
			for i := 0; i < 10; i++ {
				e := cohortTestAttribution()[0]
				e.EventID, e.Operation, e.WorkUnitID = fmt.Sprintf("wide-%d", i), fmt.Sprintf("wide-%d", i), fmt.Sprintf("wku_wide%d", i)
				e.KeyID = strings.Repeat("k", 900<<10) // Legal native payload under 1MiB.
				projectionApplyEvents(t, p, []Event{e}, uint64(i))
				refs = append(refs, WorkUnitReference{"prj_1", e.WorkUnitID})
			}
			err := p.WithGovernanceExport(ctx, func(v GovernanceExportView) error {
				if _, _, err := collectExportFacts(ctx, v, 1); err != nil {
					return err
				}
				duplicates := make([]WorkUnitReference, 200)
				for i := range duplicates {
					duplicates[i] = refs[0]
				}
				if page, err := v.ReadMaturityPage(ctx, duplicates); err != nil || len(page.Items) != 200 {
					t.Fatal("dedup did not bound source", len(page.Items), err)
				}
				if mode == "invalid-refs" {
					if _, err := v.ReadMaturityPage(ctx, make([]WorkUnitReference, 201)); !errors.Is(err, ErrProjectionPageLimit) {
						t.Fatal(err)
					}
					if err := v.Complete(ctx); err == nil {
						t.Fatal("ignored invalid query")
					}
					return nil
				}
				page, err := v.ReadMaturityPage(ctx, refs)
				if !errors.Is(err, ErrProjectionPageLimit) || page.Watermark != (Watermark{}) || len(page.Items) != 0 {
					t.Fatal("wide batch leaked partial result", page, err)
				}
				if mode == "retry" {
					for _, chunk := range [][]WorkUnitReference{refs[:5], refs[5:]} {
						if page, err := v.ReadMaturityPage(ctx, chunk); err != nil || len(page.Items) != 5 {
							t.Fatal(page, err)
						}
					}
					return v.Complete(ctx)
				}
				if mode == "unrelated" {
					if _, err := v.ReadMaturityPage(ctx, []WorkUnitReference{{"prj_1", "wku_absent"}}); !errors.Is(err, ErrGovernanceExportIncomplete) {
						t.Fatal(err)
					}
				}
				if err := v.Complete(ctx); err == nil {
					t.Fatal("ignored overflow completed")
				}
				return nil
			})
			if (mode == "retry") != (err == nil) {
				t.Fatal(mode, err)
			}
		})
	}
}

func TestGovernanceExportCorruptionNeverProducesFacts(t *testing.T) {
	for _, fault := range []string{"unit-index", "run-index", "unit-position", "run-position", "unit-raw", "run-raw", "position-width", "counter-width", "counter-missing", "missing-bucket", "status"} {
		t.Run(fault, func(t *testing.T) {
			p := projectionTestStore(t)
			ctx := context.Background()
			projectionApplyEvents(t, p, cohortTestAttribution(), 0)
			if err := p.db.Update(func(tx *bbolt.Tx) error {
				switch fault {
				case "unit-index":
					return tx.Bucket([]byte("work_units_list")).Delete([]byte("wku_1"))
				case "run-index":
					return tx.Bucket([]byte("runs_list")).Delete([]byte("run_1"))
				case "unit-position":
					return tx.Bucket([]byte("export_work_units")).Delete([]byte("wku_1"))
				case "run-position":
					return tx.Bucket([]byte("export_runs")).Delete([]byte("run_1"))
				case "unit-raw":
					return tx.Bucket([]byte("work_units")).Put(projectionIDKey("wku_1"), []byte("{"))
				case "run-raw":
					return tx.Bucket([]byte("runs")).Put(projectionIDKey("run_1"), []byte("{"))
				case "position-width":
					return tx.Bucket([]byte("export_work_units")).Put([]byte("wku_1"), make([]byte, 257))
				case "counter-width":
					return tx.Bucket([]byte("meta")).Put([]byte("export_work_unit_count"), make([]byte, 21))
				case "counter-missing":
					return tx.Bucket([]byte("meta")).Delete([]byte("export_run_count"))
				case "missing-bucket":
					return tx.DeleteBucket([]byte("export_work_units"))
				case "status":
					return projectionPut(tx, "export_work_units", []byte("wku_1"), governanceExportPosition{ID: "wku_1", Created: 1, Closed: 2})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			err := p.WithGovernanceExport(ctx, func(v GovernanceExportView) error {
				_, _, err := collectExportFacts(ctx, v, 1)
				if !errors.Is(err, ErrProjectionCorrupt) {
					t.Fatal("fault was not detected", fault, err)
				}
				if err := v.Complete(ctx); err == nil {
					t.Fatal("corrupt facts completed")
				}
				return nil
			})
			if !errors.Is(err, ErrProjectionCorrupt) {
				t.Fatal(fault, err)
			}
		})
	}
}

func TestGovernanceExportBatchRollbackAndSequentialPages(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	e := cohortTestAttribution()[0]
	bad := cohortTestAttribution()[1]
	bad.WorkUnitID = "wku_absent"
	if err := p.ApplyBatch(ctx, []Record{projectionTestRecord(1, e), projectionTestRecord(2, bad)}); err == nil {
		t.Fatal("bad batch committed")
	}
	if err := p.WithGovernanceExport(ctx, func(v GovernanceExportView) error {
		units, runs, err := collectExportFacts(ctx, v, 1)
		if err != nil {
			return err
		}
		if len(units) != 0 || len(runs) != 0 || v.Head().Sequence != 0 {
			t.Fatal("rollback left export state", units, runs, v.Head())
		}
		return v.Complete(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	projectionApplyEvents(t, p, cohortTestAttribution(), 0)
	if err := p.WithGovernanceExport(ctx, func(v GovernanceExportView) error {
		o := GovernanceListOptions{Limits: ProjectionPageLimits{Rows: 1, Bytes: 1}}
		if _, err := v.ReadWorkUnitPage(ctx, o); !errors.Is(err, ErrProjectionPageLimit) {
			t.Fatal(err)
		}
		o.Limits.Bytes = GovernanceListMaxBytes
		o.After = "wku_1"
		if _, err := v.ReadWorkUnitPage(ctx, o); !errors.Is(err, ErrProjectionPageLimit) {
			t.Fatal("skipped unread prefix", err)
		}
		if _, _, err := collectExportFacts(ctx, v, 1); err != nil {
			return err
		}
		return v.Complete(ctx)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGovernanceExportConcurrentMaturityAndAppend(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	seq := projectionApplyEvents(t, p, cohortTestAttribution(), 0)
	life := projectionTestLifecycle(11, true)
	seq = projectionApplyEvents(t, p, life[:3], seq)
	if err := p.WithGovernanceExport(ctx, func(v GovernanceExportView) error {
		if _, _, err := collectExportFacts(ctx, v, 1); err != nil {
			return err
		}
		var wg sync.WaitGroup
		errs := make(chan error, 21)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 5; j++ {
					page, err := v.ReadMaturityPage(ctx, []WorkUnitReference{{"prj_1", "wku_1"}})
					if err != nil {
						errs <- err
						return
					}
					if !page.Items[0].Inflight {
						errs <- errors.New("maturity advanced beyond cut")
						return
					}
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, e := range life[3:] {
				seq++
				if err := p.Apply(ctx, projectionTestRecord(seq, e)); err != nil {
					errs <- err
					return
				}
			}
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				return err
			}
		}
		return v.Complete(ctx)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGovernanceExportClosedPendingMaturityStaysAtCapturedCut(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	seq := projectionApplyEvents(t, p, cohortTestAttribution(), 0)
	life := projectionTestLifecycle(12, true)
	seq = projectionApplyEvents(t, p, life[:3], seq)
	seq = projectionApplyEvents(t, p, life[4:], seq)
	closed := governanceEvent("closed-pending-export", EventWorkUnitClosed, time.Now())
	closed.WorkUnitID = "wku_1"
	seq = projectionApplyEvents(t, p, []Event{closed}, seq)
	if err := p.WithGovernanceExport(ctx, func(v GovernanceExportView) error {
		seq = projectionApplyEvents(t, p, life[3:4], seq)
		units, _, err := collectExportFacts(ctx, v, 1)
		if err != nil {
			return err
		}
		page, err := v.ReadMaturityPage(ctx, []WorkUnitReference{{"prj_1", "wku_1"}})
		if err != nil {
			return err
		}
		if len(units) != 1 || units[0].Status != domain.WorkUnitClosed || units[0].ClosedAt == nil || !page.Items[0].Inflight || page.Items[0].Status != domain.WorkUnitClosed {
			t.Fatal("finalized pending matured at old cut", units, page)
		}
		return v.Complete(ctx)
	}); err != nil {
		t.Fatal(err)
	}
}
