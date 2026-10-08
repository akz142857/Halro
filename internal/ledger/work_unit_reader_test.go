package ledger

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func TestWorkUnitAccountingMatchesStateAtEachLifecycleCut(t *testing.T) {
	p := projectionTestStore(t)
	s := NewState()
	ctx := context.Background()
	sequence := uint64(0)
	events := append(projectionTestAttribution(), projectionTestLifecycle(1, true)...)
	for _, e := range events {
		sequence++
		r := projectionTestRecord(sequence, e)
		if err := s.Apply(r); err != nil {
			t.Fatal(err)
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
		for _, withRuns := range []bool{false, true} {
			got, found, err := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", withRuns)
			want, stateFound, stateErr := s.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", withRuns)
			if err != nil || stateErr != nil || found != stateFound || !reflect.DeepEqual(got, want) {
				t.Fatalf("cut %d runs=%v: disk=%#v state=%#v errors=%v/%v", sequence, withRuns, got, want, err, stateErr)
			}
		}
	}
	for _, reader := range []AccountingWorkUnitReader{s, p} {
		for _, scope := range [][2]string{{"other", "wku_1"}, {"prj_1", "missing"}} {
			v, found, err := reader.ReadWorkUnitAccounting(ctx, scope[0], scope[1], true)
			if err != nil || found || v.WorkUnit.ID != "" || len(v.Runs) != 0 || v.Inflight {
				t.Fatal("missing/cross-project leaked data", v, found, err)
			}
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, found, err := reader.ReadWorkUnitAccounting(cancelled, "prj_1", "wku_1", true); !errors.Is(err, context.Canceled) || found {
			t.Fatal(found, err)
		}
	}
}

func TestWorkUnitAccountingConcurrentApplyShowsWholeRecordCut(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	seq := projectionApplyEvents(t, p, projectionTestAttribution(), 0)
	done := make(chan error, 1)
	go func() {
		for i := 1; i <= 40; i++ {
			for _, e := range projectionTestLifecycle(i, true) {
				seq++
				if err := p.Apply(ctx, projectionTestRecord(seq, e)); err != nil {
					done <- err
					return
				}
			}
		}
		done <- nil
	}()
	verify := func() error {
		v, found, err := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true)
		if err != nil {
			return err
		}
		phase := (v.Watermark.Sequence - 2) % 5
		if !found || len(v.Runs) != 1 || v.Inflight != (phase != 0) || (v.Runs[0].ReservedMicrosUSD > 0) != (phase == 2 || phase == 3) {
			return fmt.Errorf("torn accounting record cut: %#v", v)
		}
		return nil
	}
	var readErr error
	for {
		if err := verify(); err != nil && readErr == nil {
			readErr = err
		}
		select {
		case err := <-done:
			if err != nil || readErr != nil {
				t.Fatal(err, readErr)
			}
			if err := verify(); err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
	}
}

func TestWorkUnitAccountingRetainsFinalizedRequestPendingAttemptAndAtomicIndex(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	seq := projectionApplyEvents(t, p, projectionTestAttribution(), 0)
	events := projectionTestLifecycle(2, true)
	seq = projectionApplyEvents(t, p, events[:3], seq)
	seq = projectionApplyEvents(t, p, events[4:], seq)
	v, found, err := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", false)
	if err != nil || !found || !v.Inflight || v.Watermark.Sequence != seq {
		t.Fatal("finalized request hid pending attempt", v, found, err)
	}
	seq = projectionApplyEvents(t, p, events[3:4], seq)
	v, found, err = p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true)
	if err != nil || !found || v.Inflight || len(v.Runs) != 1 || v.Watermark.Sequence != seq {
		t.Fatal("late settlement did not clear activity", v, found, err)
	}
	// New accepted-request index must roll back with a later invalid event.
	next := projectionTestLifecycle(3, true)
	if err := p.ApplyBatch(ctx, []Record{projectionTestRecord(seq+1, next[0]), projectionTestRecord(seq+2, next[2])}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	v, found, err = p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", false)
	if err != nil || !found || v.Inflight || v.Watermark.Sequence != seq {
		t.Fatal("rollback left activity index or advanced cut", v, found, err)
	}
	seq = projectionApplyEvents(t, p, events[4:], seq)
	seq = projectionApplyEvents(t, p, events[3:4], seq)
	v, found, err = p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", false)
	if err != nil || !found || v.Inflight || v.Watermark.Sequence != seq {
		t.Fatal("duplicate resurrected activity", v, found, err)
	}
}

func TestWorkUnitAccountingRejectsCorruptIndexesAndUnavailableOwner(t *testing.T) {
	for _, mode := range []string{"missing_active_bucket", "dangling_active", "wrong_scope_active", "dangling_pending", "wrong_scope_pending", "bad_run", "bad_position", "invalid_work_unit_status", "too_many_runs"} {
		t.Run(mode, func(t *testing.T) {
			p := projectionTestStore(t)
			ctx := context.Background()
			seq := projectionApplyEvents(t, p, projectionTestAttribution(), 0)
			events := projectionTestLifecycle(1, true)
			projectionApplyEvents(t, p, events[:2], seq)
			if err := p.db.Update(func(tx *bbolt.Tx) error {
				switch mode {
				case "missing_active_bucket":
					return tx.DeleteBucket([]byte("active_requests_by_work_unit"))
				case "dangling_active":
					return tx.Bucket([]byte("requests")).Delete(projectionIDKey(events[0].RequestID))
				case "wrong_scope_active":
					r, _, err := projectionGet[ProjectedRequest](tx, "requests", projectionIDKey(events[0].RequestID))
					if err != nil {
						return err
					}
					r.Accepted.Event.ProjectID = "other"
					return projectionPut(tx, "requests", projectionIDKey(events[0].RequestID), r)
				case "dangling_pending":
					return tx.Bucket([]byte("pending")).Delete(projectionIDKey(events[1].AttemptID))
				case "wrong_scope_pending":
					r, _, err := projectionGet[projectionReservation](tx, "pending", projectionIDKey(events[1].AttemptID))
					if err != nil {
						return err
					}
					r.Evidence.Event.WorkUnitID = "other"
					return projectionPut(tx, "pending", projectionIDKey(events[1].AttemptID), r)
				case "bad_run":
					return tx.Bucket([]byte("runs")).Delete(projectionIDKey("run_1"))
				case "bad_position":
					return tx.Bucket([]byte("meta")).Delete([]byte("position"))
				case "invalid_work_unit_status":
					wu, _, err := projectionGet[domain.WorkUnit](tx, "work_units", projectionIDKey("wku_1"))
					if err != nil {
						return err
					}
					wu.Status = ""
					return projectionPut(tx, "work_units", projectionIDKey(wu.ID), wu)
				case "too_many_runs":
					run, _, err := projectionGet[domain.Run](tx, "runs", projectionIDKey("run_1"))
					if err != nil {
						return err
					}
					for i := 0; i < domain.MaxRunsPerWorkUnit; i++ {
						run.ID = fmt.Sprintf("run_extra_%d", i)
						if err := projectionPut(tx, "runs", projectionIDKey(run.ID), run); err != nil {
							return err
						}
						if err := tx.Bucket([]byte("runs_by_work_unit")).Put(projectionTuple(run.WorkUnitID, run.ID), []byte(run.ID)); err != nil {
							return err
						}
					}
					return nil
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if v, found, err := p.ReadWorkUnitAccounting(ctx, "prj_1", "wku_1", true); !errors.Is(err, ErrProjectionCorrupt) || found || v.WorkUnit.ID != "" {
				t.Fatal("corruption became a final or absent view", v, found, err)
			}
		})
	}
	p := projectionTestStore(t)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, found, err := p.ReadWorkUnitAccounting(context.Background(), "prj_1", "wku_1", true); !errors.Is(err, ErrProjectionUnavailable) || found {
		t.Fatal(found, err)
	}
}
