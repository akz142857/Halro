package budget

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func finishExportFacts(ctx context.Context, v ledger.GovernanceExportView) error {
	o := ledger.GovernanceListOptions{Limits: ledger.ProjectionPageLimits{Rows: 1, Bytes: ledger.GovernanceListMaxBytes}}
	for {
		page, err := v.ReadWorkUnitPage(ctx, o)
		if err != nil {
			return err
		}
		if page.NextID == "" {
			break
		}
		o.After = page.NextID
	}
	o.After = ""
	for {
		page, err := v.ReadRunPage(ctx, o)
		if err != nil {
			return err
		}
		if page.NextID == "" {
			break
		}
		o.After = page.NextID
	}
	return v.Complete(ctx)
}

func TestGovernanceExportManagerRealOwnerKeepsFixedCut(t *testing.T) {
	m, p, _ := newProjectedManagerFixture(t, ledger.Options{MaxBatch: 8, FlushInterval: time.Millisecond})
	ctx := context.Background()
	w, _, err := m.CreateWorkUnit(ctx, "prj_1", "key_1", 10, testGovernanceIntent("export-unit"))
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := m.CreateRun(ctx, "prj_1", "key_1", w.ID, 100, time.Hour, 10, testGovernanceIntent("export-run"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := m.BeginRequestAttributed(ctx, "prj_1", "key_1", "req_export", "model", w.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.CloseWorkUnit(ctx, "prj_1", "key_1", w.ID, testGovernanceIntent("export-close")); err != nil {
		t.Fatal(err)
	}
	if err := m.WithGovernanceExport(ctx, func(v ledger.GovernanceExportView) error {
		cut := v.Head()
		if err := m.Finalize(ctx, request, "success"); err != nil {
			return err
		}
		page, err := v.ReadMaturityPage(ctx, []ledger.WorkUnitReference{{ProjectID: "prj_1", WorkUnitID: w.ID}})
		if err != nil {
			return err
		}
		if !page.Items[0].Inflight || page.Items[0].Status != domain.WorkUnitClosed || page.Watermark != cut {
			t.Fatal("native append changed export cut", page)
		}
		live, err := p.ReadWorkUnitMaturityPage(ctx, []ledger.WorkUnitReference{{ProjectID: "prj_1", WorkUnitID: w.ID}})
		if err != nil || live.Items[0].Inflight || live.Watermark.Sequence <= cut.Sequence {
			t.Fatal(live, err)
		}
		return finishExportFacts(ctx, v)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGovernanceExportManagerRetireJoinsPublicationAfterComplete(t *testing.T) {
	m, p, _ := newProjectedManagerFixture(t, ledger.Options{})
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	done := make(chan error, 1)
	var saved ledger.GovernanceExportView
	go func() {
		done <- m.WithGovernanceExport(ctx, func(v ledger.GovernanceExportView) error {
			saved = v
			if err := finishExportFacts(ctx, v); err != nil {
				close(entered)
				return err
			}
			close(entered)
			<-release // Simulates external manifest publication.
			return nil
		})
	}()
	<-entered
	retireWithDeadline(t, m)
	m.operationMu.Lock()
	retained := m.owner != nil && m.activeOperations == 1
	m.operationMu.Unlock()
	if !retained {
		t.Fatal("Complete released owner before publication")
	}
	if err := m.WithGovernanceExport(ctx, func(ledger.GovernanceExportView) error { t.Error("retired callback ran"); return nil }); !errors.Is(err, ErrAccountingRetired) {
		t.Fatal(err)
	}
	if _, _, err := p.ReadPosition(ctx); err != nil {
		t.Fatal("retirement closed native projection", err)
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertRetiredOwnerReleased(t, m)
	if err := saved.Complete(ctx); !errors.Is(err, ledger.ErrGovernanceExportClosed) {
		t.Fatal(err)
	}
}

func TestGovernanceExportManagerRejectsStateFallbackAndReleasesFailedCall(t *testing.T) {
	m, _, closeLog := newTestManager(t)
	defer closeLog()
	if err := m.WithGovernanceExport(context.Background(), func(ledger.GovernanceExportView) error { t.Error("State export callback ran"); return nil }); !errors.Is(err, ledger.ErrGovernanceExportBackend) {
		t.Fatal(err)
	}
	assertRetiredOwnerReleased(t, m)
	m, _, _ = newProjectedManagerFixture(t, ledger.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.WithGovernanceExport(ctx, func(v ledger.GovernanceExportView) error {
		cancel()
		if err := finishExportFacts(context.Background(), v); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	assertRetiredOwnerReleased(t, m)
}
