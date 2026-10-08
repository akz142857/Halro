package authenticatedstate

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestSealCandidateCurrentContextUsesOnlyOriginalPrepaidWorkspace(t *testing.T) {
	ctx := context.Background()
	budget := testBudget(t)
	reservation := sealPublisherReservation(t, budget)
	defer reservation.Release()
	dir := t.TempDir()
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	want := sealCandidateFixture()
	p, err := OpenSealCandidatePublisher(dir, guard, make([]byte, 32), sealHardFor(want), budget, reservation)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
		if err := p.Retire(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := p.Publish(ctx, want); err != nil {
		t.Fatal(err)
	}
	work, err := budget.TryAcquireCompletableWork(resourcebudget.Active, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: candidateSealMaxBytes}, []resourcebudget.WorkspaceRequirement{{Class: resourcebudget.IO, RAM: candidateSealMaxBytes}})
	if err != nil {
		t.Fatal(err)
	}
	defer work.Active.Release()
	defer work.Close()
	if err := budget.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	if _, found, err := p.Current(); !errors.Is(err, resourcebudget.ErrPressure) || found {
		t.Fatal("ordinary read borrowed terminal resources", found, err)
	}
	if _, found, err := p.CurrentContext(work.Bind(ctx)); !errors.Is(err, resourcebudget.ErrPressure) || found {
		t.Fatal("inactive continuation lent its workspace", found, err)
	}
	before := budget.Snapshot()
	if err := resourcebudget.WithCompletionWorkspace(work.Bind(ctx), func(ctx context.Context) error {
		got, found, err := p.CurrentContext(ctx)
		if err == nil && (!found || got != want) {
			t.Fatal("owned resource loan changed actual selection", got, found)
		}
		if err != nil {
			return err
		}
		expected := sealEligibilityFor(want)
		got, found, err = p.SelectContext(ctx, expected)
		if err != nil || !found || got != want {
			t.Fatal("original eligibility rejected actual selected seal", found, err)
		}
		for _, mismatch := range []string{"role", "incarnation", "cut"} {
			wrong := expected
			switch mismatch {
			case "role":
				wrong.Role = SealReplica
			case "incarnation":
				wrong.Incarnation[0] ^= 1
			case "cut":
				wrong.Positions[0].Sequence++
			}
			if got, found, err := p.SelectContext(ctx, wrong); !errors.Is(err, ErrSealIneligible) || found || got != (CandidateSeal{}) {
				t.Fatal("resource loan manufactured eligibility", mismatch, found, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal("original owned continuation could not read current selection", err)
	}
	after := budget.Snapshot()
	if after.Used != before.Used || after.NormalClaims != before.NormalClaims || after.RecoveryClaims != before.RecoveryClaims {
		t.Fatal("selection read acquired new resources", before, after)
	}
}
