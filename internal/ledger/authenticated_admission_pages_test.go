package ledger

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func authenticatedAdmissionFixture(t *testing.T, count int, wide bool) (*AuthenticatedLedgerProjection, *Log, *LedgerProjection, *State) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "native.wal")
	native := openChained(t, path, nil)
	state := NewState()
	now := time.Date(2026, 10, 2, 11, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		wu := governanceEvent(fmt.Sprint("unit-", i), EventWorkUnitCreated, now)
		wu.WorkUnitID = fmt.Sprintf("wku_%03d", i)
		wu.OutcomeDefinitions = []domain.OutcomeDefinitionRef{{ID: "odef_cohort", Version: 1}}
		if count < 10 && i == count-1 {
			wu.ProjectID = "prj_other"
		}
		if wide {
			wu.KeyID = strings.Repeat("k", 900<<10)
		}
		r, _, err := native.AppendRecordWithReceipt(ctx, wu)
		if err != nil {
			t.Fatal(err)
		}
		if err := state.Apply(r); err != nil {
			t.Fatal(err)
		}
		run := governanceEvent(fmt.Sprint("run-", i), EventRunCreated, now)
		run.WorkUnitID, run.ProjectID, run.RunID = wu.WorkUnitID, wu.ProjectID, fmt.Sprintf("run_%03d", i)
		run.RunBudgetMicrosUSD, run.RunExpiresAt = 1000, now.Add(time.Hour)
		r, _, err = native.AppendRecordWithReceipt(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		if err := state.Apply(r); err != nil {
			t.Fatal(err)
		}
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(err, partial)
	}
	options := authenticatedLedgerTestOptions(t)
	options.MaxDiskBytes = 128 << 20
	p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "auth"), report, options)
	if err != nil {
		t.Fatal(err)
	}
	r, err := BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "ref.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := p.Discard(); e != nil {
			t.Error(e)
		}
		if e := r.Discard(); e != nil {
			t.Error(e)
		}
		if e := native.Close(); e != nil {
			t.Error(e)
		}
		if used := options.Budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
			t.Error("fee leak", used)
		}
	})
	return p, native, r, state
}

func TestAuthenticatedLedgerAdmissionListsAndMaturityMatchFreshReference(t *testing.T) {
	p, _, r, _ := authenticatedAdmissionFixture(t, 6, false)
	ctx := context.Background()
	for _, project := range []string{"", "prj_1", "prj_other", "missing"} {
		for _, status := range []domain.WorkUnitStatus{"", domain.WorkUnitOpen, domain.WorkUnitClosed} {
			q := WorkUnitListQuery{ProjectID: project, Status: status, GovernanceListOptions: GovernanceListOptions{Limits: ProjectionPageLimits{Rows: 2, Bytes: 1 << 20}}}
			for n := 0; n < 10; n++ {
				a, ae := p.ReadWorkUnitPage(ctx, q)
				b, be := r.ReadWorkUnitPage(ctx, q)
				if ae != nil || be != nil || !reflect.DeepEqual(a, b) {
					t.Fatal("work unit page", q, a, ae, b, be)
				}
				if a.NextID == "" {
					break
				}
				q.After = a.NextID
				if n == 9 {
					t.Fatal("pagination did not end")
				}
			}
		}
		for _, unit := range []string{"", "wku_000", "missing"} {
			for _, status := range []domain.RunStatus{"", domain.RunActive, domain.RunExpired, domain.RunClosed} {
				q := RunListQuery{ProjectID: project, WorkUnitID: unit, Status: status, At: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), GovernanceListOptions: GovernanceListOptions{Limits: ProjectionPageLimits{Rows: 2, Bytes: 1 << 20}}}
				for n := 0; n < 10; n++ {
					a, ae := p.ReadRunPage(ctx, q)
					b, be := r.ReadRunPage(ctx, q)
					if ae != nil || be != nil || !reflect.DeepEqual(a, b) {
						t.Fatal("run page", q, a, ae, b, be)
					}
					if a.NextID == "" {
						break
					}
					q.After = a.NextID
					if n == 9 {
						t.Fatal("run pagination did not end")
					}
				}
			}
		}
	}
	refs := []WorkUnitReference{{"prj_1", "wku_000"}, {"other", "wku_000"}, {"prj_1", "wku_missing"}, {"prj_1", "wku_000"}, {"prj_other", "wku_005"}}
	a, ae := p.ReadWorkUnitMaturityPage(ctx, refs)
	b, be := r.ReadWorkUnitMaturityPage(ctx, refs)
	if ae != nil || be != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("maturity", a, ae, b, be)
	}
	if a, err := p.ReadWorkUnitMaturityPage(ctx, nil); err != nil || a.Items == nil || len(a.Items) != 0 {
		t.Fatal(a, err)
	}
}

func TestAuthenticatedLedgerAdmissionSparseAndWideLimitsRemainRetryable(t *testing.T) {
	t.Run("sparse", func(t *testing.T) {
		p, _, r, _ := authenticatedAdmissionFixture(t, 100, false)
		ctx := context.Background()
		q := WorkUnitListQuery{ProjectID: "prj_1", Status: domain.WorkUnitClosed, GovernanceListOptions: GovernanceListOptions{Limits: ProjectionPageLimits{Rows: 1, Bytes: 1 << 20}}}
		a, ae := p.ReadWorkUnitPage(ctx, q)
		b, be := r.ReadWorkUnitPage(ctx, q)
		if ae != nil || be != nil || !reflect.DeepEqual(a, b) || len(a.Items) != 0 || a.NextID == "" || a.Scanned != 68 {
			t.Fatal("sparse continuation", a, ae, b, be)
		}
		q.After = a.NextID
		a, ae = p.ReadWorkUnitPage(ctx, q)
		if ae != nil || a.NextID != "" || a.Scanned != 32 {
			t.Fatal("sparse end", a, ae)
		}
	})
	t.Run("wide", func(t *testing.T) {
		p, _, r, _ := authenticatedAdmissionFixture(t, 2, true)
		ctx := context.Background()
		refs := make([]WorkUnitReference, 200)
		for i := range refs {
			refs[i] = WorkUnitReference{"prj_1", "wku_000"}
		}
		a, ae := p.ReadWorkUnitMaturityPage(ctx, refs)
		b, be := r.ReadWorkUnitMaturityPage(ctx, refs)
		if ae != nil || be != nil || !reflect.DeepEqual(a, b) {
			t.Fatal("wide duplicate decode budget", ae, be)
		}
		if v, e := p.ReadWorkUnitMaturityPage(ctx, []WorkUnitReference{{"prj_1", "wku_000"}, {"prj_other", "wku_001"}}); !errors.Is(e, ErrProjectionPageLimit) || v.Watermark != (Watermark{}) || len(v.Items) != 0 {
			t.Fatal("wide page did not fail atomically", v, e)
		}
		q := WorkUnitListQuery{GovernanceListOptions: GovernanceListOptions{Limits: ProjectionPageLimits{Rows: 1, Bytes: 1}}}
		if v, e := p.ReadWorkUnitPage(ctx, q); !errors.Is(e, ErrProjectionPageLimit) || len(v.Items) != 0 {
			t.Fatal(v, e)
		}
		q.Limits.Bytes = 1 << 20
		if v, e := p.ReadWorkUnitPage(ctx, q); e != nil || len(v.Items) != 1 {
			t.Fatal("page retry poisoned", v, e)
		}
	})
}

func TestAuthenticatedLedgerAdmissionCohortFixedCutAllowsVisitorAdvance(t *testing.T) {
	p, native, r, state := authenticatedAdmissionFixture(t, 4, false)
	ctx := context.Background()
	q := cohortTestQuery()
	want, wantHead, err := collectCohort(ctx, r, q)
	if err != nil || len(want) != 3 {
		t.Fatal(want, wantHead, err)
	}
	var got []GovernanceCohortRow
	advanced := false
	head, err := p.VisitGovernanceCohort(ctx, q, func(row GovernanceCohortRow) error {
		got = append(got, row)
		if _, err := p.ReadBalance(ctx, BalanceKey{ProjectID: "prj_1"}); err != nil {
			return err
		}
		if !advanced {
			advanced = true
			e := projectionTestLifecycle(1, true)[0]
			e.WorkUnitID, e.RunID = "wku_000", "run_000"
			e.OccurredAt = time.Date(2026, 10, 2, 11, 30, 0, 0, time.UTC)
			r, _, err := native.AppendRecordWithReceipt(ctx, e)
			if err != nil {
				return err
			}
			if err := state.Apply(r); err != nil {
				return err
			}
			if _, err := p.AdvanceFromNative(ctx, native, nativeRecordPrefix(r)); err != nil {
				return err
			}
			if err := p.WithRecoveryView(ctx, func(AuthenticatedRecoveryView) error { t.Error("nested callback admitted"); return nil }); !errors.Is(err, authenticatedstate.ErrBusy) {
				return fmt.Errorf("nested callback: %w", err)
			}
		}
		return nil
	})
	if err != nil || head != wantHead || !reflect.DeepEqual(got, want) {
		t.Fatal("mixed visitor cut", got, want, head, wantHead, err)
	}
	current, _, err := p.ReadPosition(ctx)
	if err != nil || current.Sequence != head.Sequence+1 {
		t.Fatal(current, err)
	}
	page, err := p.ReadWorkUnitMaturityPage(ctx, []WorkUnitReference{{"prj_1", "wku_000"}})
	if err != nil || !page.Items[0].Inflight || page.Watermark != current {
		t.Fatal("new cut maturity", page, err)
	}
	// Actual known/estimated and unknown settlements, followed by close, must
	// match an independent State oracle, not only the shared Bolt reducer.
	var last Record
	for i := 1; i <= 2; i++ {
		events := projectionTestLifecycle(i, true)
		snapshot := domain.NewUnknownPriceSnapshot(time.Date(2026, 10, 2, 11, 30, 0, 0, time.UTC))
		for j := range events {
			e := &events[j]
			e.WorkUnitID, e.RunID = "wku_000", "run_000"
			e.OccurredAt = time.Date(2026, 10, 2, 11, 30, 0, 0, time.UTC)
			if i == 1 && e.Kind == EventAttemptSettled {
				e.CostEstimated = true
			}
			if i == 2 && e.Kind == EventReservationCreated {
				e.LeaseMode = LeaseModeUnknownAllowed
				e.ReservationMicrosUSD = nil
				e.PriceSnapshot = &snapshot
				e.UnknownPolicyEvidence = &domain.UnknownPricePolicyEvidence{PolicyVersion: "v1", ProjectID: "prj_1", TokenGuardStatus: "disabled", ReasonCode: "cost_governance_disabled", InstanceExplicitOptIn: true, CostGovernanceDisabled: true}
				e.TokenGuardPricingViewDigest = "sha256:" + strings.Repeat("a", 64)
			}
			if i == 2 && e.Kind == EventAttemptSettled {
				e.LeaseMode = LeaseModeUnknownAllowed
				e.CommittedMicrosUSD = nil
				e.PriceSnapshot = &snapshot
			}
			record, _, err := native.AppendRecordWithReceipt(ctx, *e)
			if err != nil {
				t.Fatal(err)
			}
			if err := state.Apply(record); err != nil {
				t.Fatal(err)
			}
			last = record
		}
	}
	closed := governanceEvent("cohort-close", EventWorkUnitClosed, time.Date(2026, 10, 2, 11, 40, 0, 0, time.UTC))
	closed.WorkUnitID = "wku_000"
	last, _, err = native.AppendRecordWithReceipt(ctx, closed)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Apply(last); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AdvanceFromNative(ctx, native, nativeRecordPrefix(last)); err != nil {
		t.Fatal(err)
	}
	gotRows, gotHead, gotErr := collectCohort(ctx, p, q)
	wantRows, wantHead, wantErr := collectCohort(ctx, state, q)
	if gotErr != nil || wantErr != nil || gotHead != wantHead || !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatal("cost/status cohort differs", gotRows, gotErr, wantRows, wantErr)
	}
	if gotRows[0].KnownCostMicrosUSD != 1 || gotRows[0].EstimatedCostMicrosUSD != 1 || gotRows[0].UnknownAttempts != 1 || gotRows[0].Inflight || gotRows[0].Status != domain.WorkUnitClosed {
		t.Fatal("cohort semantic fields", gotRows)
	}
}

func TestAuthenticatedLedgerAdmissionCohortCloseRetainsVisitorFee(t *testing.T) {
	for _, mode := range []string{"self-close", "external-close", "cancel", "panic"} {
		t.Run(mode, func(t *testing.T) {
			p, _, _, _ := authenticatedAdmissionFixture(t, 2, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			before := p.budget.Snapshot().Used
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				defer func() {
					if v := recover(); v != nil {
						if mode != "panic" {
							done <- fmt.Errorf("unexpected panic: %v", v)
						} else {
							done <- nil
						}
					}
				}()
				head, err := p.VisitGovernanceCohort(ctx, cohortTestQuery(), func(GovernanceCohortRow) error {
					if mode == "self-close" {
						if err := p.Close(); err != nil {
							return err
						}
					}
					close(entered)
					<-release
					if mode == "panic" {
						panic("visitor panic")
					}
					return nil
				})
				if mode == "panic" {
					done <- errors.New("panic swallowed")
					return
				}
				if err == nil || head != (Watermark{}) {
					done <- errors.New("closed/canceled scope published head")
					return
				}
				done <- nil
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("visitor blocked/deadlocked")
			}
			if mode == "external-close" {
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
			} else if mode == "cancel" {
				cancel()
			}
			if used := p.budget.Snapshot().Used.RAM; used < AuthenticatedRecoveryRAMBytes {
				t.Fatal("visitor refunded before return", used)
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("scope did not retire")
			}
			if mode == "self-close" || mode == "external-close" {
				// The visitor fee exits; the creator's fixed persistent cleanup
				// receipt remains until physical Discard.
				if ram := p.budget.Snapshot().Used.RAM; ram != 4096+resourcebudget.ClaimRAMBytes {
					t.Fatal("visitor fee not retired", ram)
				}
			} else if used := p.budget.Snapshot().Used; used != before {
				t.Fatal("callback fee leak", used, before)
			}
			if mode == "cancel" {
				if _, _, err := p.ReadPosition(context.Background()); err != nil {
					t.Fatal("cancel poisoned owner", err)
				}
			}
		})
	}
}

func TestAuthenticatedLedgerAdmissionSelectedFaultsAndCohortLimits(t *testing.T) {
	for _, kind := range []string{"list-id", "list-source", "maturity-version", "cohort-identity", "cohort-version"} {
		t.Run(kind, func(t *testing.T) {
			p, _, _, _ := authenticatedAdmissionFixture(t, 2, false)
			ctx := context.Background()
			var read func() error
			switch kind {
			case "list-id":
				mutatePoint(t, p, "work_units_list_project", projectionListKey("prj_1", "wku_000"), []byte("wrong"))
				read = func() error {
					v, e := p.ReadWorkUnitPage(ctx, WorkUnitListQuery{ProjectID: "prj_1", GovernanceListOptions: GovernanceListOptions{Limits: ProjectionPageLimits{Rows: 1, Bytes: 4096}}})
					if len(v.Items) != 0 || v.NextID != "" {
						t.Error("partial list")
					}
					return e
				}
			case "list-source":
				mutatePoint(t, p, "runs", projectionIDKey("run_000"), nil)
				read = func() error {
					v, e := p.ReadRunPage(ctx, RunListQuery{ProjectID: "prj_1", GovernanceListOptions: GovernanceListOptions{Limits: ProjectionPageLimits{Rows: 1, Bytes: 4096}}})
					if len(v.Items) != 0 {
						t.Error("dangling run source")
					}
					return e
				}
			case "maturity-version", "cohort-version":
				mutatePoint(t, p, "cohort_versions", cohortVersionKey("wku_000", 1), nil)
				if kind == "maturity-version" {
					read = func() error {
						v, e := p.ReadWorkUnitMaturityPage(ctx, []WorkUnitReference{{"prj_1", "wku_000"}})
						if len(v.Items) != 0 {
							t.Error("partial maturity")
						}
						return e
					}
				} else {
					read = func() error {
						_, h, e := collectCohort(ctx, p, cohortTestQuery())
						if h != (Watermark{}) {
							t.Error("partial cohort authoritative")
						}
						return e
					}
				}
			case "cohort-identity":
				mutatePoint(t, p, "cohort_units", projectionListKey("prj_1", "wku_000"), cohortIdentity{})
				read = func() error {
					_, h, e := collectCohort(ctx, p, cohortTestQuery())
					if h != (Watermark{}) {
						t.Error("bad identity authoritative")
					}
					return e
				}
			}
			if err := read(); !errors.Is(err, ErrProjectionCorrupt) {
				t.Fatal(kind, err)
			}
			if _, _, err := p.ReadPosition(ctx); !errors.Is(err, ErrProjectionUnavailable) {
				t.Fatal("fault candidate alive", err)
			}
		})
	}
	p, _, _, _ := authenticatedAdmissionFixture(t, 4, false)
	q := cohortTestQuery()
	q.MaxWorkUnits = 1
	if _, h, e := collectCohort(context.Background(), p, q); !errors.Is(e, ErrGovernanceCohortLimit) || h != (Watermark{}) {
		t.Fatal("cohort truncation", h, e)
	}
	q.MaxWorkUnits = 100
	q.Limits.Bytes = 1
	if _, h, e := collectCohort(context.Background(), p, q); !errors.Is(e, ErrProjectionPageLimit) || h != (Watermark{}) {
		t.Fatal(h, e)
	}
	q.Limits.Bytes = 4096
	if rows, _, e := collectCohort(context.Background(), p, q); e != nil || len(rows) != 3 {
		t.Fatal("limit poisoned candidate", rows, e)
	}
}
