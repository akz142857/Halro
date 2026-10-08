package metadatajournal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestBoundPhysicalNativeActualRoleRevisionAndLoanRevocation(t *testing.T) {
	ctx := context.Background()
	path, l, a := boundReplicationFixture(t)
	guard, err := lock.Acquire(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	m := metadataFrozenBudget(t)
	called := false
	visit := func(q *PhysicalNativeLoan) error { called = true; return nil }
	if err := l.WithBoundPhysicalNative(ctx, m, guard, a, false, visit); !errors.Is(err, authenticatedstate.ErrSealIneligible) || called {
		t.Fatal("unconfigured native granted physical ownership", err)
	}
	if err := l.SetBoundReplicationAdmissionAndAfterDurable(a, func(DurableBatch) (AppendAdmission, error) { return nil, nil }, func(DurableBatch) (uint64, error) { return 1, nil }, func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"revision", "baseline", "role", "guard"} {
		bad, badGuard, replica := a, guard, false
		switch mode {
		case "revision":
			bad.FileRevision++
		case "baseline":
			bad.Baseline.Cut[0] ^= 1
		case "role":
			replica = true
		case "guard":
			badGuard = nil
		}
		if err := l.WithBoundPhysicalNative(ctx, m, badGuard, bad, replica, visit); !errors.Is(err, authenticatedstate.ErrSealIneligible) || called {
			t.Fatal(mode, err)
		}
	}
	var escaped *PhysicalNativeLoan
	before := m.Snapshot().Used
	if err := l.WithBoundPhysicalNative(ctx, m, guard, a, false, func(q *PhysicalNativeLoan) error {
		escaped = q
		_, _, err := q.HeadAt(ctx, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Used != before {
		t.Fatal("joined original scan fee leaked")
	}
	if _, err := escaped.Reference(); !errors.Is(err, authenticatedstate.ErrClosed) {
		t.Fatal("escaped loan revived", err)
	}
	if err := l.WithPhysicalNative(ctx, m, visit); !errors.Is(err, authenticatedstate.ErrSealIneligible) || called {
		t.Fatal("Standalone granted HA loan", err)
	}
	if err := l.WithBoundPhysicalNative(ctx, metadataFrozenBudget(t), guard, a, false, visit); !errors.Is(err, authenticatedstate.ErrSealIneligible) || called {
		t.Fatal("foreign Manager rebound actual owner", err)
	}
}

func TestBoundPhysicalNativeActualReplacementCloseJoinsAndRetainsScanFee(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.journal")
	binding := testBinding()
	l, err := StartBoundEpoch(context.Background(), path, testKey(), EpochHeader{Epoch: binding.Epoch, PreviousEpoch: binding.Epoch - 1, Reason: "bound-physical-debt", Baseline: &binding, FileRevision: 1}, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	a := BoundReplicationAuthority{Baseline: binding, FileRevision: 1, MaxFileBytes: 64 << 20}
	guard, err := lock.Acquire(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if err := l.SetBoundReplicationAdmissionAndAfterDurable(a, func(DurableBatch) (AppendAdmission, error) { return nil, nil }, func(DurableBatch) (uint64, error) { return 1, nil }, func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	m := metadataFrozenBudget(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done, closed := make(chan error, 1), make(chan error, 1)
	go func() {
		done <- l.WithBoundPhysicalNative(context.Background(), m, guard, a, false, func(q *PhysicalNativeLoan) error { close(entered); <-release; return nil })
	}()
	<-entered
	go func() { closed <- l.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close escaped actual callback", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	if err := <-closed; !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if len(l.key) == 0 || l.physical == nil || l.physical.bound.debt == (resourcebudget.Permit{}) || m.Snapshot().Used.RAM < FrozenReadRAMBytes {
		t.Fatal("actual native/key/scan debt owner lost")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "foreign" {
		t.Fatal("foreign altered", err)
	}
}
