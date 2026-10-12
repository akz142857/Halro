package ledger

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

type exportedFacts struct {
	head     Watermark
	units    []GovernanceExportWorkUnit
	runs     []GovernanceExportRun
	maturity WorkUnitMaturityPage
}

func exportFacts(ctx context.Context, reader AccountingExportReader, rows int, refs []WorkUnitReference) (exportedFacts, error) {
	var got exportedFacts
	err := reader.WithGovernanceExport(ctx, func(v GovernanceExportView) error {
		got.head = v.Head()
		var err error
		if got.units, got.runs, err = collectExportFacts(ctx, v, rows); err != nil {
			return err
		}
		if got.maturity, err = v.ReadMaturityPage(ctx, refs); err != nil {
			return err
		}
		return v.Complete(ctx)
	})
	return got, err
}

func TestAuthenticatedGovernanceExportMatchesBoltReference(t *testing.T) {
	p, _, r, _ := authenticatedAdmissionFixture(t, 6, false)
	ctx := context.Background()
	refs := []WorkUnitReference{{"prj_1", "wku_000"}, {"other", "wku_000"}, {"prj_1", "wku_missing"}, {"prj_other", "wku_005"}}
	for _, rows := range []int{1, 2, GovernanceListMaxRows} {
		got, err := exportFacts(ctx, p, rows, refs)
		if err != nil {
			t.Fatal(rows, err)
		}
		want, err := exportFacts(ctx, r, rows, refs)
		if err != nil {
			t.Fatal(rows, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("authenticated export differs from the bbolt reference", rows, got, want)
		}
		if len(got.units) != 6 || len(got.runs) != 6 || !got.maturity.Items[0].Found || got.maturity.Items[1].Found || got.maturity.Items[2].Found || !got.maturity.Items[3].Found {
			t.Fatal("export facts", got)
		}
	}
}

func TestAuthenticatedGovernanceExportFixedCutSurvivesAdvance(t *testing.T) {
	p, native, _, _ := authenticatedAdmissionFixture(t, 4, false)
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 11, 30, 0, 0, time.UTC)
	err := p.WithGovernanceExport(ctx, func(v GovernanceExportView) error {
		cut := v.Head()
		wuClose := governanceEvent("export-late-wu-close", EventWorkUnitClosed, now)
		wuClose.WorkUnitID = "wku_000"
		runClose := governanceEvent("export-late-run-close", EventRunClosed, now)
		runClose.WorkUnitID, runClose.RunID, runClose.CloseReason = "wku_000", "run_000", "done"
		late := governanceEvent("export-late-unit", EventWorkUnitCreated, now)
		late.WorkUnitID = "wku_late"
		late.OutcomeDefinitions = []domain.OutcomeDefinitionRef{{ID: "odef_cohort", Version: 1}}
		for _, e := range []Event{wuClose, runClose, late} {
			record, _, err := native.AppendRecordWithReceipt(ctx, e)
			if err != nil {
				return err
			}
			// The view holds nothing between pages, so the exclusive advance runs.
			if _, err := p.AdvanceFromNative(ctx, native, nativeRecordPrefix(record)); err != nil {
				return fmt.Errorf("advance during export: %w", err)
			}
		}
		units, runs, err := collectExportFacts(ctx, v, 1)
		if err != nil {
			return err
		}
		if len(units) != 4 || len(runs) != 4 || v.Head() != cut {
			t.Fatal("later facts leaked into the cut", units, runs, v.Head(), cut)
		}
		for _, u := range units {
			if u.ID == "wku_000" && (u.Status != domain.WorkUnitOpen || u.ClosedAt != nil) {
				t.Fatal("later close leaked into the cut", u)
			}
		}
		for _, r := range runs {
			if r.ID == "run_000" && (r.Status != domain.RunActive || r.ClosedAt != nil) {
				t.Fatal("later run close leaked into the cut", r)
			}
		}
		page, err := v.ReadMaturityPage(ctx, []WorkUnitReference{{"prj_1", "wku_late"}})
		if err != nil {
			return err
		}
		if page.Items[0].Found || page.Watermark != cut {
			t.Fatal("future creation leaked into the cut", page)
		}
		return v.Complete(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := p.ReadWorkUnitMaturityPage(ctx, []WorkUnitReference{{"prj_1", "wku_000"}, {"prj_1", "wku_late"}})
	if err != nil || live.Items[0].Status != domain.WorkUnitClosed || !live.Items[1].Found {
		t.Fatal("live reads did not advance", live, err)
	}
}

func TestAuthenticatedGovernanceExportWaitsOutHeldOwnerLock(t *testing.T) {
	p, _, _, _ := authenticatedAdmissionFixture(t, 3, false)
	ctx := context.Background()
	err := p.WithGovernanceExport(ctx, func(v GovernanceExportView) error {
		// A writer holding the owner lock for a moment refuses TryRLock with
		// ErrBusy; the export page waits it out instead of poisoning the cut.
		p.mu.Lock()
		released := make(chan struct{})
		go func() {
			time.Sleep(20 * time.Millisecond)
			p.mu.Unlock()
			close(released)
		}()
		units, runs, err := collectExportFacts(ctx, v, 1)
		<-released
		if err != nil {
			return err
		}
		if len(units) != 3 || len(runs) != 3 {
			t.Fatal(units, runs)
		}
		return v.Complete(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
}
