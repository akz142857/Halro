//go:build linux

package authenticatedstate

import (
	"context"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestPhysicalSealCycleWorkspaceRenewalAtActualENOSPC(t *testing.T) {
	dir := t.TempDir()
	requireSealPhysicalExt4(t, dir)
	m, disk := sealCycleBudget(t)
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	selected := sealCandidateFixture()
	p, err := OpenSealCandidatePublisher(dir, guard, sealCycleKey(), sealHardFor(selected), m, disk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), selected); err != nil {
		t.Fatal(err)
	}
	if err := p.EnablePhysicalSealPreparation(context.Background(), selected, disk); err != nil {
		t.Fatal(err)
	}
	original := p.physicalTargetWork
	pool, err := m.TryAcquireCompletableWork(resourcebudget.IO, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: 128 << 10}, []resourcebudget.WorkspaceRequirement{{Class: resourcebudget.IO, RAM: 128 << 10}})
	if err != nil {
		t.Fatal(err)
	}
	filler := exhaustSealPhysicalExt4(t, dir)
	if err := m.SetPressure(resourcebudget.Pressured); err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot()
	for i := 0; i < 4; i++ {
		selected.Revision++
		err := pool.WithWorkspace(context.Background(), func(ctx context.Context) error { _, err := p.Publish(ctx, selected); return err })
		if err != nil {
			t.Fatal("original permanent target control escaped checkpoint loan", i, err)
		}
		if p.physicalTargetWork != original {
			t.Fatal("renewal replaced original permanent claim")
		}
		if got := m.Snapshot(); got.Used != before.Used || got.NormalClaims != before.NormalClaims {
			t.Fatal("renewal re-admitted memory", before, got)
		}
		if fs := requireSealPhysicalExt4(t, dir); fs.Bavail != 0 {
			t.Fatal("publication released target space", fs.Bavail)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := original.Cost(); err == nil {
		t.Fatal("actual target exit failed to retire permanent fee")
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.Active.Release(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filler); err != nil {
		t.Fatal(err)
	}
}
