package resourcebudget

import (
	"context"
	"errors"
	"testing"
)

func TestPhysicalCandidateWorkspaceCannotEscapeScopedLoan(t *testing.T) {
	m := fixtureManager(t)
	for _, tc := range []struct {
		name  string
		class Class
		cost  Cost
	}{{"wrong-class", IO, Cost{RAM: 4096}}, {"wrong-cost", Background, Cost{RAM: 4097}}, {"independent", Background, Cost{RAM: 4096}}} {
		t.Run(tc.name, func(t *testing.T) {
			work, err := m.TryAcquire(tc.class, tc.cost)
			if err != nil {
				t.Fatal(err)
			}
			c := &PhysicalCandidate{manager: m, work: work, ready: true}
			before := m.Snapshot()
			retained, err := c.RetainIndependentWorkspace(m)
			if tc.name != "independent" {
				if !errors.Is(err, ErrInvalid) {
					t.Fatal("unqualified workspace accepted", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := m.Snapshot(); got.Used != before.Used || got.NormalClaims != before.NormalClaims || got.Owners != before.Owners+1 {
					t.Fatal("re-admitted retained workspace", before, got)
				}
				if err := retained.Release(); err != nil {
					t.Fatal(err)
				}
			}
			if err := work.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
	w, err := m.TryAcquireCompletableWork(Active, Cost{RAM: 1}, Cost{RAM: 4096}, []WorkspaceRequirement{{Background, 4096}})
	if err != nil {
		t.Fatal(err)
	}
	err = w.WithWorkspace(context.Background(), func(ctx context.Context) error {
		p, err := m.TryAcquireContext(ctx, Background, Cost{RAM: 4096})
		if err != nil {
			return err
		}
		defer p.Release()
		c := &PhysicalCandidate{manager: m, work: p, ready: true}
		if _, err := c.RetainIndependentWorkspace(m); !errors.Is(err, ErrInvalid) {
			t.Fatal("scoped child turned into permanent owner", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal("scoped child did not join", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Active.Release(); err != nil {
		t.Fatal(err)
	}
	zeroUsage(t, m)
}
