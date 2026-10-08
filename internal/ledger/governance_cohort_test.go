package ledger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func cohortTestQuery() GovernanceCohortQuery {
	return GovernanceCohortQuery{ProjectID: "prj_1", DefinitionID: "odef_cohort", DefinitionVersion: 1,
		CohortStart: "2026-09-01", CohortEnd: "2026-10-03", Limits: ProjectionPageLimits{Rows: 1, Bytes: 4096}}
}

func cohortTestAttribution() []Event {
	events := projectionTestAttribution()
	events[0].OutcomeDefinitions = []domain.OutcomeDefinitionRef{{ID: "odef_cohort", Version: 1}}
	return events
}

func collectCohort(ctx context.Context, reader AccountingCohortReader, q GovernanceCohortQuery) ([]GovernanceCohortRow, Watermark, error) {
	rows := []GovernanceCohortRow{}
	head, err := reader.VisitGovernanceCohort(ctx, q, func(row GovernanceCohortRow) error {
		rows = append(rows, row)
		return nil
	})
	return rows, head, err
}

func TestGovernanceCohortMatchesFullSnapshotAtEveryCut(t *testing.T) {
	p, s := projectionTestStore(t), NewState()
	q, ctx := cohortTestQuery(), context.Background()
	events := cohortTestAttribution()
	first := projectionTestLifecycle(1, true)
	first[3].CostEstimated = true
	// A different EventID for the same accepted identity must not double count.
	acceptedAlias := first[0]
	acceptedAlias.EventID = "accepted-alias"
	events = append(events, first[:3]...)
	events = append(events, acceptedAlias, first[3], first[3], first[4], first[4])
	unknown := projectionTestLifecycle(2, true)
	unknown[1].LeaseMode = LeaseModeUnknownAllowed
	unknown[1].ReservationMicrosUSD = nil
	unknown[3].CommittedMicrosUSD = nil
	snapshot := domain.NewUnknownPriceSnapshot(unknown[0].OccurredAt)
	unknown[1].PriceSnapshot = &snapshot
	unknown[1].UnknownPolicyEvidence = &domain.UnknownPricePolicyEvidence{PolicyVersion: "v1", ProjectID: "prj_1", TokenGuardStatus: "disabled", ReasonCode: "cost_governance_disabled", InstanceExplicitOptIn: true, CostGovernanceDisabled: true}
	unknown[1].TokenGuardPricingViewDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	unknown[3].LeaseMode = LeaseModeUnknownAllowed
	unknown[3].PriceSnapshot = &snapshot
	events = append(events, unknown...)
	closed := governanceEvent("closed-cohort", EventWorkUnitClosed, time.Now())
	closed.WorkUnitID = "wku_1"
	events = append(events, closed)
	for i, e := range events {
		r := projectionTestRecord(uint64(i+1), e)
		if err := s.Apply(r); err != nil {
			t.Fatal(i, err)
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal(i, err)
		}
		snapshot, err := s.GovernanceSnapshot(ctx, GovernanceSnapshotOptions{ProjectID: q.ProjectID,
			DefinitionID: q.DefinitionID, DefinitionVersion: q.DefinitionVersion, CohortStart: q.CohortStart, CohortEnd: q.CohortEnd, IncludeAttempts: true})
		if err != nil {
			t.Fatal(err)
		}
		want := []GovernanceCohortRow{}
		for _, w := range snapshot.WorkUnits {
			row := GovernanceCohortRow{WorkUnitID: w.ID, Status: w.Status, Inflight: snapshot.InflightWorkUnit[w.ID]}
			for _, a := range snapshot.SettledAttempts {
				if a.Settlement.WorkUnitID != w.ID {
					continue
				}
				if a.CostKnown {
					row.KnownCostMicrosUSD += a.CostMicrosUSD
					if a.Settlement.CostEstimated {
						row.EstimatedCostMicrosUSD += a.CostMicrosUSD
					}
				} else {
					row.UnknownAttempts++
				}
			}
			want = append(want, row)
		}
		for _, reader := range []AccountingCohortReader{s, p} {
			got, head, err := collectCohort(ctx, reader, q)
			if err != nil || head != snapshot.Watermark || !reflect.DeepEqual(got, want) {
				t.Fatalf("cut=%d %T got=%#v want=%#v head=%#v err=%v", i+1, reader, got, want, head, err)
			}
		}
	}
}

func TestGovernanceCohortFinalizedPendingAndLateSettlementRemainDistinct(t *testing.T) {
	p := projectionTestStore(t)
	ctx, q := context.Background(), cohortTestQuery()
	seq := projectionApplyEvents(t, p, cohortTestAttribution(), 0)
	life := projectionTestLifecycle(9, true)
	seq = projectionApplyEvents(t, p, life[:3], seq)
	seq = projectionApplyEvents(t, p, life[4:], seq)
	alias := life[4]
	alias.EventID = "finalized-alias"
	seq = projectionApplyEvents(t, p, []Event{alias}, seq)
	closed := governanceEvent("closed-pending", EventWorkUnitClosed, time.Now())
	closed.WorkUnitID = "wku_1"
	seq = projectionApplyEvents(t, p, []Event{closed}, seq)
	rows, head, err := collectCohort(ctx, p, q)
	if err != nil || head.Sequence != seq || len(rows) != 1 || !rows[0].Inflight || rows[0].Status != domain.WorkUnitClosed || rows[0].KnownCostMicrosUSD != 0 {
		t.Fatal("finalized hid unresolved attempt", rows, head, err)
	}
	seq = projectionApplyEvents(t, p, life[3:4], seq)
	seq = projectionApplyEvents(t, p, life[3:4], seq)
	rows, head, err = collectCohort(ctx, p, q)
	if err != nil || head.Sequence != seq || len(rows) != 1 || rows[0].Inflight || rows[0].KnownCostMicrosUSD != 9 {
		t.Fatal("late/duplicate settlement corrupted maturity/cost", rows, head, err)
	}
}

func TestGovernanceCohortPagesKeepCutAcrossConcurrentWritesWithoutOpenReader(t *testing.T) {
	p, s := projectionTestStore(t), NewState()
	ctx, q := context.Background(), cohortTestQuery()
	seq := uint64(0)
	apply := func(e Event, legacy bool) {
		t.Helper()
		seq++
		r := projectionTestRecord(seq, e)
		if legacy {
			if err := s.Apply(r); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	unit := func(id string) Event {
		e := cohortTestAttribution()[0]
		e.EventID, e.Operation, e.WorkUnitID = "create-"+id, "create-"+id, id
		return e
	}
	for _, id := range []string{"wku_1", "wku_2", "wku_3"} {
		apply(unit(id), true)
	}
	run := cohortTestAttribution()[1]
	run.WorkUnitID = "wku_2"
	apply(run, true)
	want, expectedHead, err := collectCohort(ctx, s, q)
	if err != nil {
		t.Fatal(err)
	}
	var got []GovernanceCohortRow
	head, err := p.VisitGovernanceCohort(ctx, q, func(row GovernanceCohortRow) error {
		if p.db.Stats().OpenTxN != 0 {
			return errors.New("visitor retained a DB reader")
		}
		got = append(got, row)
		if len(got) == 1 {
			for _, e := range projectionTestLifecycle(7, true) {
				e.WorkUnitID = "wku_2"
				apply(e, false)
			}
			closed := governanceEvent("close-second", EventWorkUnitClosed, time.Now())
			closed.WorkUnitID = "wku_2"
			apply(closed, false)
			// New units on both sides of the page cursor belong to a later cut.
			apply(unit("wku_0"), false)
			apply(unit("wku_4"), false)
		}
		return nil
	})
	if err != nil || head != expectedHead || !reflect.DeepEqual(got, want) {
		t.Fatal("mixed accounting prefixes", got, want, head, expectedHead, err)
	}
	nowRows, nowHead, err := collectCohort(ctx, p, q)
	if err != nil || nowHead.Sequence != seq || len(nowRows) != 5 || nowRows[2].Status != domain.WorkUnitClosed || nowRows[2].KnownCostMicrosUSD != 7 {
		t.Fatal("later query did not observe newer cut", nowRows, nowHead, err)
	}
}

func TestGovernanceCohortFiltersBeforeLimitAndContinuesEmptyPages(t *testing.T) {
	p, s := projectionTestStore(t), NewState()
	q, ctx := cohortTestQuery(), context.Background()
	q.MaxWorkUnits = 1
	for i := 0; i < 12; i++ {
		e := cohortTestAttribution()[0]
		e.EventID, e.Operation, e.WorkUnitID = fmt.Sprintf("create-%d", i), fmt.Sprintf("create-%d", i), fmt.Sprintf("wku_%02d", i)
		if i < 5 {
			e.PeriodID = "2026-08-01"
		} else if i < 11 {
			e.OutcomeDefinitions[0].Version = 2
		}
		r := projectionTestRecord(uint64(i+1), e)
		if err := s.Apply(r); err != nil {
			t.Fatal(err)
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, reader := range []AccountingCohortReader{s, p} {
		rows, _, err := collectCohort(ctx, reader, q)
		if err != nil || len(rows) != 1 || rows[0].WorkUnitID != "wku_11" {
			t.Fatal(rows, err)
		}
	}
	e := cohortTestAttribution()[0]
	e.EventID, e.Operation, e.WorkUnitID = "extra", "extra", "wku_12"
	r := projectionTestRecord(13, e)
	if err := s.Apply(r); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, reader := range []AccountingCohortReader{s, p} {
		_, head, err := collectCohort(ctx, reader, q)
		if !errors.Is(err, ErrGovernanceCohortLimit) || head != (Watermark{}) {
			t.Fatal("silent eligible truncation", head, err)
		}
	}
	q.MaxWorkUnits, q.Limits.Bytes = 100, 1
	if _, head, err := collectCohort(ctx, p, q); !errors.Is(err, ErrProjectionPageLimit) || head != (Watermark{}) {
		t.Fatal("oversized row not rejected", head, err)
	}
}

func TestGovernanceCohortQueryOverflowDoesNotPoisonDurableApply(t *testing.T) {
	p, s := projectionTestStore(t), NewState()
	q, ctx := cohortTestQuery(), context.Background()
	events := cohortTestAttribution()
	events[1].RunBudgetMicrosUSD = math.MaxInt64
	first := projectionTestLifecycle(1, true)
	first[1].ReservationMicrosUSD, first[3].CommittedMicrosUSD = MicrosUSD(math.MaxInt64), MicrosUSD(math.MaxInt64)
	events = append(events, first...)
	otherRun := cohortTestAttribution()[1]
	otherRun.EventID, otherRun.Operation, otherRun.RunID = "second-run", "second-run", "run_2"
	events = append(events, otherRun)
	second := projectionTestLifecycle(2, true)
	for i := range second {
		second[i].RunID = "run_2"
	}
	second[3].CommittedMicrosUSD = MicrosUSD(1)
	events = append(events, second...)
	for i, e := range events {
		r := projectionTestRecord(uint64(i+1), e)
		if err := s.Apply(r); err != nil {
			t.Fatal("legitimate cross-period/cross-run native fact rejected", i, err)
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal("query aggregate poisoned Apply", i, err)
		}
	}
	for _, reader := range []AccountingCohortReader{s, p} {
		_, head, err := collectCohort(ctx, reader, q)
		if !errors.Is(err, ErrGovernanceSummaryOverflow) || head != (Watermark{}) {
			t.Fatal(head, err)
		}
	}
	if head, _, err := p.ReadPosition(ctx); err != nil || head.Sequence != uint64(len(events)) {
		t.Fatal("overflow changed durable head", head, err)
	}
}

func TestGovernanceCohortFailuresDiscardHeadAndBatchRollsBack(t *testing.T) {
	ctx, q := context.Background(), cohortTestQuery()
	for _, mode := range []string{"missing_identity_bucket", "missing_version_bucket", "missing_history", "bad_identity", "bad_version", "wrong_sequence", "bad_counters", "estimated_exceeds_known"} {
		t.Run(mode, func(t *testing.T) {
			p := projectionTestStore(t)
			projectionApplyEvents(t, p, cohortTestAttribution(), 0)
			if err := p.db.Update(func(tx *bbolt.Tx) error {
				key := cohortVersionKey("wku_1", 1)
				switch mode {
				case "missing_identity_bucket":
					return tx.DeleteBucket([]byte("cohort_units"))
				case "missing_version_bucket":
					return tx.DeleteBucket([]byte("cohort_versions"))
				case "missing_history":
					return tx.Bucket([]byte("cohort_versions")).Delete(key)
				case "bad_identity":
					identity, _, _ := projectionGet[cohortIdentity](tx, "cohort_units", projectionListKey("prj_1", "wku_1"))
					identity.WorkUnit.ProjectID = "other"
					return projectionPut(tx, "cohort_units", projectionListKey("prj_1", "wku_1"), identity)
				case "bad_version":
					return tx.Bucket([]byte("cohort_versions")).Put(key, []byte("null"))
				}
				v, _, _ := projectionGet[cohortVersion](tx, "cohort_versions", key)
				switch mode {
				case "wrong_sequence":
					v.Sequence++
				case "bad_counters":
					v.PendingAttempts = 100
				case "estimated_exceeds_known":
					v.Estimated.Lo = 1
				}
				return projectionPut(tx, "cohort_versions", key, v)
			}); err != nil {
				t.Fatal(err)
			}
			if _, head, err := collectCohort(ctx, p, q); !errors.Is(err, ErrProjectionCorrupt) || head != (Watermark{}) {
				t.Fatal(head, err)
			}
		})
	}
	p := projectionTestStore(t)
	seq := projectionApplyEvents(t, p, cohortTestAttribution(), 0)
	life := projectionTestLifecycle(1, true)
	if err := p.ApplyBatch(ctx, []Record{projectionTestRecord(seq+1, life[0]), projectionTestRecord(seq+2, life[2])}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	rows, head, err := collectCohort(ctx, p, q)
	if err != nil || len(rows) != 1 || rows[0].Inflight || head.Sequence != seq {
		t.Fatal("rolled-back version remained visible", rows, head, err)
	}
	injected := errors.New("visitor failure")
	if head, err := p.VisitGovernanceCohort(ctx, q, func(GovernanceCohortRow) error { return injected }); !errors.Is(err, injected) || head != (Watermark{}) {
		t.Fatal(head, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	if head, err := p.VisitGovernanceCohort(canceled, q, func(GovernanceCohortRow) error { cancel(); return nil }); !errors.Is(err, context.Canceled) || head != (Watermark{}) {
		t.Fatal(head, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, head, err := collectCohort(ctx, p, q); !errors.Is(err, ErrProjectionUnavailable) || head != (Watermark{}) {
		t.Fatal(head, err)
	}
}

func TestGovernanceCohortMidQueryOwnerChangeCannotPublishPartialCut(t *testing.T) {
	ctx, q := context.Background(), cohortTestQuery()
	for _, mode := range []string{"close", "generation", "cancel", "corrupt_next_page"} {
		t.Run(mode, func(t *testing.T) {
			p := projectionTestStore(t)
			seq := projectionApplyEvents(t, p, cohortTestAttribution(), 0)
			other := cohortTestAttribution()[0]
			other.EventID, other.Operation, other.WorkUnitID = "other", "other", "wku_2"
			projectionApplyEvents(t, p, []Event{other}, seq)
			queryCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			visits := 0
			head, err := p.VisitGovernanceCohort(queryCtx, q, func(GovernanceCohortRow) error {
				visits++
				if visits != 1 {
					t.Error("visitor continued after owner failure")
					return nil
				}
				switch mode {
				case "close":
					return p.Close()
				case "generation":
					p.mu.Lock()
					p.generation[0]++
					p.mu.Unlock()
				case "cancel":
					cancel()
				case "corrupt_next_page":
					return p.db.Update(func(tx *bbolt.Tx) error {
						return tx.Bucket([]byte("cohort_versions")).Delete(cohortVersionKey("wku_2", seq+1))
					})
				}
				return nil
			})
			if err == nil || head != (Watermark{}) || visits != 1 {
				t.Fatal("partial query returned authoritative head", head, visits, err)
			}
		})
	}
}

func TestGovernanceCohortMissingBucketsFailEmptyReadAndApplyWithoutPanic(t *testing.T) {
	for _, bucket := range []string{"cohort_units", "cohort_versions"} {
		t.Run(bucket, func(t *testing.T) {
			p := projectionTestStore(t)
			ctx := context.Background()
			if err := p.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket([]byte(bucket)) }); err != nil {
				t.Fatal(err)
			}
			if _, head, err := collectCohort(ctx, p, cohortTestQuery()); !errors.Is(err, ErrProjectionCorrupt) || head != (Watermark{}) {
				t.Fatal(head, err)
			}
			if err := p.Apply(ctx, projectionTestRecord(1, cohortTestAttribution()[0])); !errors.Is(err, ErrProjectionCorrupt) {
				t.Fatal(err)
			}
			if head, _, err := p.ReadPosition(ctx); err != nil || head != (Watermark{}) {
				t.Fatal("failed Apply advanced prefix", head, err)
			}
		})
	}
}

func BenchmarkGovernanceCohortHistory(b *testing.B) {
	for _, count := range []int{1, 4096} {
		for _, backend := range []string{"state", "projection"} {
			b.Run(fmt.Sprintf("%s/%d", backend, count), func(b *testing.B) {
				ctx, q := context.Background(), cohortTestQuery()
				p, s := projectionTestStore(b), NewState()
				seq := uint64(0)
				var batch []Record
				flush := func() {
					if len(batch) != 0 {
						if err := p.ApplyBatch(ctx, batch); err != nil {
							b.Fatal(err)
						}
						clear(batch)
						batch = batch[:0]
					}
				}
				apply := func(e Event) {
					seq++
					r := projectionTestRecord(seq, e)
					if err := s.Apply(r); err != nil {
						b.Fatal(err)
					}
					batch = append(batch, r)
					if len(batch) == projectionBatchRows {
						flush()
					}
				}
				for _, e := range cohortTestAttribution() {
					apply(e)
				}
				wantCost := int64(0)
				for i := 1; i <= count; i++ {
					for _, e := range projectionTestLifecycle(i, true) {
						apply(e)
					}
					wantCost += int64(i % 80)
				}
				flush()
				var reader AccountingCohortReader = s
				if backend == "projection" {
					reader = p
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					seen := 0
					head, err := reader.VisitGovernanceCohort(ctx, q, func(row GovernanceCohortRow) error {
						seen++
						if row.KnownCostMicrosUSD != wantCost || row.Inflight {
							return errors.New("history summary mismatch")
						}
						return nil
					})
					if err != nil || head.Sequence != seq || seen != 1 {
						b.Fatal(head, seen, err)
					}
				}
			})
		}
	}
}
