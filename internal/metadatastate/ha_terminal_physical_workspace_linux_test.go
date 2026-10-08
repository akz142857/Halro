//go:build linux

package metadatastate

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// The source proof borrows prepaid RAM, while its new physical control owner
// must acquire a separate persistent RAM+Disk claim. Saturating real claim slots
// makes an accidental ordinary source read or borrowed mixed claim observable.
func TestHAMetadataPhysicalEnableBorrowedWorkspacePaysPersistentControl(t *testing.T) {
	for _, replica := range []bool{false, true} {
		name := "Primary"
		if replica {
			name = "Replica"
		}
		t.Run(name, func(t *testing.T) {
			b, _ := haPhysicalSelectedFixture(t, replica)
			ctx := context.Background()
			profile := []resourcebudget.WorkspaceRequirement{
				{Class: resourcebudget.IO, RAM: DeltaRAMBytes},
				{Class: resourcebudget.IO, RAM: metadatajournal.FrozenReadRAMBytes},
				{Class: resourcebudget.IO, RAM: 128 << 10},
				{Class: resourcebudget.IO, RAM: 4096},
				{Class: resourcebudget.IO, RAM: 4096},
				{Class: resourcebudget.Background, RAM: 4096},
			}
			var ram int64
			for _, r := range profile {
				ram += r.RAM
			}
			pool, err := b.budget.TryAcquireCompletableWork(resourcebudget.IO, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: ram}, profile)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			var held []resourcebudget.Permit
			defer func() {
				for _, p := range held {
					if err := p.Release(); err != nil {
						t.Error(err)
					}
				}
			}()
			for {
				p, err := b.budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{RAM: 1})
				if errors.Is(err, resourcebudget.ErrExhausted) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, p)
			}
			if len(held) == 0 {
				t.Fatal("fixture has no spare claim")
			}
			before := b.budget.Snapshot()
			if err := pool.WithWorkspace(ctx, b.EnableHAPhysicalTerminals); !errors.Is(err, resourcebudget.ErrExhausted) || errors.Is(err, resourcebudget.ErrInvalid) {
				t.Fatal("persistent fee must refuse without a real claim", err)
			}
			if b.physicalEnabled || b.budget.Snapshot().Used != before.Used || b.journal.Head() != b.position {
				t.Fatal("refused enable changed acceptance, native, or paid fees")
			}
			last := len(held) - 1
			if err := held[last].Release(); err != nil {
				t.Fatal(err)
			}
			held = held[:last]
			before = b.budget.Snapshot()
			if err := pool.WithWorkspace(ctx, b.EnableHAPhysicalTerminals); err != nil {
				t.Fatal("source proof must use its original RAM pool", err)
			}
			after := b.budget.Snapshot()
			if !b.physicalEnabled || after.NormalClaims != before.NormalClaims+1 || after.Used.Disk <= before.Used.Disk || after.Owners != before.Owners+1 {
				t.Fatal("control fee was not independently retained or source loans escaped", before, after)
			}
			// Requalification borrows the same pool without creating another
			// persistent control owner when every normal claim is occupied.
			if err := pool.WithWorkspace(ctx, b.EnableHAPhysicalTerminals); err != nil {
				t.Fatal("original physical owner requalification", err)
			}
			if got := b.budget.Snapshot(); got.Used != after.Used || got.NormalClaims != after.NormalClaims || got.Owners != after.Owners {
				t.Fatal("requalification allocated or leaked", after, got)
			}
		})
	}
}
