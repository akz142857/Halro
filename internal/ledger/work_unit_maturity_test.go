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

func TestWorkUnitMaturityPageMatchesPointReadsAndStateAtEveryCut(t *testing.T) {
	p, s := projectionTestStore(t), NewState()
	ctx := context.Background()
	refs := []WorkUnitReference{{"prj_1", "wku_1"}, {"other", "wku_1"}, {"prj_1", "wku_missing"}, {"prj_1", "wku_1"}}
	events := append(cohortTestAttribution(), projectionTestLifecycle(1, true)...)
	for i, e := range events {
		r := projectionTestRecord(uint64(i+1), e)
		if err := s.Apply(r); err != nil {
			t.Fatal(err)
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
		want, err := s.ReadWorkUnitMaturityPage(ctx, refs)
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.ReadWorkUnitMaturityPage(ctx, refs)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal(got, want, err)
		}
		for j, ref := range refs {
			point, found, err := p.ReadWorkUnitAccounting(ctx, ref.ProjectID, ref.WorkUnitID, false)
			if err != nil || found != got.Items[j].Found || found && (got.Items[j].Status != point.WorkUnit.Status || got.Items[j].Inflight != point.Inflight || point.Watermark != got.Watermark) {
				t.Fatal("maturity differs from coherent point input", got, point, found, err)
			}
		}
	}
	for _, reader := range []AccountingMaturityReader{s, p} {
		page, err := reader.ReadWorkUnitMaturityPage(ctx, nil)
		if err != nil || len(page.Items) != 0 || page.Items == nil || page.Watermark.Sequence != uint64(len(events)) {
			t.Fatal(page, err)
		}
		if page, err := reader.ReadWorkUnitMaturityPage(ctx, make([]WorkUnitReference, 201)); !errors.Is(err, ErrProjectionPageLimit) || page.Watermark != (Watermark{}) {
			t.Fatal(page, err)
		}
	}
}

func TestWorkUnitMaturityPageConcurrentApplyHasOneCut(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	seq := projectionApplyEvents(t, p, cohortTestAttribution(), 0)
	refs := make([]WorkUnitReference, WorkUnitMaturityMaxRows)
	for i := range refs {
		refs[i] = WorkUnitReference{"prj_1", "wku_1"}
	}
	done := make(chan error, 1)
	go func() {
		for i := 1; i <= 25; i++ {
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
	check := func() error {
		page, err := p.ReadWorkUnitMaturityPage(ctx, refs)
		if err != nil {
			return err
		}
		if len(page.Items) != len(refs) {
			return errors.New("missing maturity rows")
		}
		for _, row := range page.Items {
			if !row.Found || row.Status != domain.WorkUnitOpen || row.Inflight != ((page.Watermark.Sequence-2)%5 != 0) {
				return fmt.Errorf("torn maturity cut: %d, %#v", page.Watermark.Sequence, row)
			}
		}
		return nil
	}
	var readErr error
	for {
		if err := check(); err != nil && readErr == nil {
			readErr = err
		}
		select {
		case err := <-done:
			if err != nil || readErr != nil {
				t.Fatal(err, readErr)
			}
			if err := check(); err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
	}
}

func TestWorkUnitMaturityFinalizedPendingDoesNotUseMoneyOrCostRange(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	seq := projectionApplyEvents(t, p, cohortTestAttribution(), 0)
	life := projectionTestLifecycle(2, true)
	seq = projectionApplyEvents(t, p, life[:3], seq)
	seq = projectionApplyEvents(t, p, life[4:], seq)
	refs := []WorkUnitReference{{"prj_1", "wku_1"}}
	page, err := p.ReadWorkUnitMaturityPage(ctx, refs)
	if err != nil || len(page.Items) != 1 || !page.Items[0].Inflight {
		t.Fatal(page, err)
	}
	seq = projectionApplyEvents(t, p, life[3:4], seq)
	page, err = p.ReadWorkUnitMaturityPage(ctx, refs)
	if err != nil || page.Items[0].Inflight || page.Watermark.Sequence != seq {
		t.Fatal(page, err)
	}
	// Valid derived cumulative cost above int64 must not fail a non-financial
	// maturity observation. Existing cohort overflow regression proves Apply.
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		v, err := readCohortVersion(tx, "wku_1", seq)
		if err != nil {
			return err
		}
		v.Known = cohortAmount{Lo: 1 << 63}
		return projectionPut(tx, "cohort_versions", cohortVersionKey("wku_1", seq), v)
	}); err != nil {
		t.Fatal(err)
	}
	page, err = p.ReadWorkUnitMaturityPage(ctx, refs)
	if err != nil || page.Items[0].Inflight || page.Watermark.Sequence != seq {
		t.Fatal(page, err)
	}
}

func TestWorkUnitMaturityCorruptionAndLimitsNeverReturnPartialPage(t *testing.T) {
	ctx := context.Background()
	refs := []WorkUnitReference{{"prj_1", "wku_missing"}, {"prj_1", "wku_1"}}
	for _, mode := range []string{"missing_position", "missing_units", "missing_versions", "invalid_unit", "missing_version", "bad_counter", "status_mismatch", "oversized_source"} {
		t.Run(mode, func(t *testing.T) {
			p := projectionTestStore(t)
			projectionApplyEvents(t, p, cohortTestAttribution(), 0)
			if err := p.db.Update(func(tx *bbolt.Tx) error {
				switch mode {
				case "missing_position":
					return tx.Bucket([]byte("meta")).Delete([]byte("position"))
				case "missing_units":
					return tx.DeleteBucket([]byte("work_units"))
				case "missing_versions":
					return tx.DeleteBucket([]byte("cohort_versions"))
				case "invalid_unit":
					return tx.Bucket([]byte("work_units")).Put(projectionIDKey("wku_1"), []byte("null"))
				case "missing_version":
					return tx.Bucket([]byte("cohort_versions")).Delete(cohortVersionKey("wku_1", 1))
				case "oversized_source":
					return tx.Bucket([]byte("work_units")).Put(projectionIDKey("wku_1"), make([]byte, WorkUnitMaturityMaxBytes+1))
				}
				v, err := readCohortVersion(tx, "wku_1", 1)
				if err != nil {
					return err
				}
				if mode == "bad_counter" {
					v.PendingAttempts = 100
				} else {
					v.Status = domain.WorkUnitClosed
				}
				return projectionPut(tx, "cohort_versions", cohortVersionKey("wku_1", 1), v)
			}); err != nil {
				t.Fatal(err)
			}
			page, err := p.ReadWorkUnitMaturityPage(ctx, refs)
			want := ErrProjectionCorrupt
			if mode == "oversized_source" {
				want = ErrProjectionPageLimit
			}
			if !errors.Is(err, want) || len(page.Items) != 0 || page.Watermark != (Watermark{}) {
				t.Fatal(page, err)
			}
		})
	}
	p := projectionTestStore(t)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if page, err := p.ReadWorkUnitMaturityPage(canceled, refs); !errors.Is(err, context.Canceled) || page.Watermark != (Watermark{}) {
		t.Fatal(page, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if page, err := p.ReadWorkUnitMaturityPage(ctx, refs); !errors.Is(err, ErrProjectionUnavailable) || page.Watermark != (Watermark{}) {
		t.Fatal(page, err)
	}
}
