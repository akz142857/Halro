package budget

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func TestLegacyGovernanceExportLoanUsesSelectedStateAndJoinsPublication(t *testing.T) {
	m, state, closeLog := newTestManager(t)
	defer closeLog()
	ctx := context.Background()
	w, _, err := m.CreateWorkUnit(ctx, "prj_1", "key_1", 10, testGovernanceIntent("legacy-export"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WithLegacyGovernanceSnapshot(ctx, func(ctx context.Context, snapshot ledger.GovernanceSnapshot) error {
		if snapshot.Watermark != state.Watermark() || len(snapshot.WorkUnits) != 1 || snapshot.WorkUnits[0].ID != w.ID {
			t.Fatal("selected owner changed", snapshot.Watermark)
		}
		deadline, stop := context.WithTimeout(ctx, 10*time.Millisecond)
		defer stop()
		if err := m.Retire(deadline); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("retire skipped publication=%v", err)
		}
		if err := m.WithLegacyGovernanceSnapshot(context.Background(), func(context.Context, ledger.GovernanceSnapshot) error {
			t.Fatal("retired legacy callback entered")
			return nil
		}); !errors.Is(err, ErrAccountingRetired) {
			t.Fatalf("legacy owner reopened=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	p, _, _ := newProjectedManagerFixture(t, ledger.Options{})
	if err := p.WithLegacyGovernanceSnapshot(ctx, func(context.Context, ledger.GovernanceSnapshot) error {
		t.Fatal("disk owner silently borrowed State")
		return nil
	}); !errors.Is(err, ledger.ErrGovernanceExportBackend) {
		t.Fatalf("projection fallback=%v", err)
	}
}
