//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

// Design §6.1: the stop line must come before the terminal reserve is
// encroached, and accepted terminals still complete from the reserve. The
// physical reserve is allocated before admission, so the dangerous window is
// the sampling delay: the filesystem is already at ENOSPC while the pressure
// state still reads Healthy. Requests are accepted on an HA pair, their prefix
// confirmed, then a foreign file fills the isolated ext4 without any pressure
// transition. A new admission must be refused without disturbing the accepted
// windows; with one request accepted, a control admission first proves the
// logical budget still has room, so the refusal is the physical one. An
// online checkpoint (the background writer) may run or refuse but cannot take
// the reserve; every accepted request must then finalize, be confirmed and be
// applied by the Replica. Once the fill is removed admission resumes; both
// members then finish and close at a fresh ENOSPC and leave no owned temporary.
func TestHAPhysicalReserveHoldsAtUnobservedENOSPC(t *testing.T) {
	t.Run("one-accepted", func(t *testing.T) { exerciseHAPhysicalReserveAtUnobservedENOSPC(t, 1) })
	t.Run("two-accepted", func(t *testing.T) { exerciseHAPhysicalReserveAtUnobservedENOSPC(t, 2) })
}

func exerciseHAPhysicalReserveAtUnobservedENOSPC(t *testing.T, inFlight int) {
	root := os.Getenv("HALRO_EXT4_ROOT")
	if root == "" {
		t.Skip("requires explicitly isolated ext4 runner")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatal("actual ext4 fixture", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	profile := ledger.HASelectedCompletionWorkspaceRequirements()
	var completionRAM int64
	for _, slot := range profile {
		completionRAM += slot.RAM
	}
	// The fixed profile holds one accepted request: its normal RAM fits one
	// completion workspace, and the Primary's 80 MiB projections fit one
	// window's worst-case COW debt above an ordinary append. Room for a
	// second is added here, with the Replica's projection ceilings; this is
	// not a capacity claim. Two windows then exhaust the logical Metadata
	// terminal budget, so only one accepted request leaves room for a control.
	owners := startHAPhysicalBusinessPair(t, ctx, func(_ int, limits *config.ResourceBudget) {
		limits.TotalRAMBytes += completionRAM
		limits.CompletionRAMBytes += completionRAM
		limits.LedgerProjectionDiskBytes = 576 << 20
		limits.UsageProjectionDiskBytes = 576 << 20
	})
	primary, replica := owners[0], owners[1]
	c := primary.reopened.Owners.Components
	type accepted struct {
		ctx     context.Context
		retire  func() error
		work    *resourcebudget.CompletableWork
		request budget.Request
	}
	var open []accepted
	for i := range inFlight {
		admitted, retire, err := primary.physicalTerminalAdmission(ctx, 2)
		if err != nil {
			t.Fatal("physical admission before the fill", i, err)
		}
		work, err := c.Budget.TryAcquireCompletableWork(resourcebudget.Active, resourcebudget.Cost{RAM: 4096}, resourcebudget.Cost{RAM: completionRAM}, profile)
		if err != nil {
			t.Fatalf("pre-admitted request completion RAM %d: %v %+v", i, err, c.Budget.Snapshot())
		}
		request, err := primary.business.accounting.BeginRequestDetailed(work.Bind(admitted), "prj_physical", "key_physical", fmt.Sprintf("reserve-accepted-%d", i), "model")
		if err != nil {
			t.Fatal("RequestAccepted", i, err)
		}
		open = append(open, accepted{admitted, retire, work, request})
	}
	if inFlight == 1 {
		control, closeControl, err := primary.physicalTerminalAdmission(ctx, 2)
		if err != nil {
			t.Fatal("control admission before the fill", err)
		}
		_ = control
		if err := closeControl(); err != nil {
			t.Fatal("control window retirement", err)
		}
	}
	waitHAPhysicalApplied(t, ctx, primary, replica)
	fill := filepath.Join(root, "foreign-reserve-fill")
	t.Cleanup(func() { _ = os.Remove(fill) })
	if err := exhaustHAPhysicalFixture(fill, haPhysicalFixtureFillBound); err != nil {
		t.Fatal(err)
	}
	for i, f := range owners {
		// The sampler has not observed the fill yet.
		if p := f.reopened.Owners.Components.Budget.Snapshot().Pressure; p != resourcebudget.Healthy {
			t.Fatal("fixture pressure is not the unobserved state", i, p)
		}
	}
	if _, closeNew, err := primary.physicalTerminalAdmission(ctx, 2); err == nil {
		_ = closeNew()
		t.Fatal("new physical admission succeeded at actual ENOSPC before the stop line was observed")
	} else {
		if inFlight == 1 && errors.Is(err, resourcebudget.ErrExhausted) && !errors.Is(err, syscall.ENOSPC) {
			t.Fatal("refusal after a successful control is the logical budget, not the physical stop", err)
		}
		t.Log("new admission at unobserved ENOSPC refused:", inFlight, err)
	}
	for i, f := range owners {
		checkpointCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		err := f.checkpointHAPhysicalOnline(checkpointCtx)
		stop()
		t.Log("online checkpoint with accepted requests at ENOSPC", i, err)
		if f.recovery.Load() {
			t.Fatal("background checkpoint at ENOSPC forced recovery on an accepted reserve", i, err)
		}
	}
	// A refused allocation may roll back blocks it briefly held. Take those
	// too, so the terminals run with no free block at all.
	refill := filepath.Join(root, "foreign-reserve-refill")
	t.Cleanup(func() { _ = os.Remove(refill) })
	if err := exhaustHAPhysicalFixture(refill, haPhysicalFixtureFillBound); err != nil {
		t.Fatal(err)
	}
	if err := unix.Statfs(root, &fs); err != nil || fs.Bavail != 0 {
		t.Fatal("terminals would not run at ENOSPC", fs.Bavail, err)
	}
	for i, a := range open {
		if err := a.work.WithWorkspace(a.ctx, func(ctx context.Context) error {
			return primary.business.accounting.Finalize(ctx, a.request, "success")
		}); err != nil {
			t.Fatal("accepted terminal at ENOSPC", i, err)
		}
		if err := errors.Join(a.work.Close(), a.work.Active.Release()); err != nil {
			t.Fatal("accepted request RAM retirement", i, err)
		}
	}
	waitHAPhysicalApplied(t, ctx, primary, replica)
	for i, a := range open {
		if _, found, err := replica.reopened.Owners.Components.Ledger.ReadFinalization(ctx, a.request.RequestID); err != nil || !found {
			t.Fatal("Replica did not apply the accepted terminal", i, found, err)
		}
		if err := a.retire(); err != nil {
			t.Fatal("accepted completion owners", i, err)
		}
	}
	// Neither refusal may outlive the shortage: once space returns, the same
	// members admit again.
	if err := errors.Join(os.Remove(fill), os.Remove(refill)); err != nil {
		t.Fatal(err)
	}
	if _, closeAgain, err := primary.physicalTerminalAdmission(ctx, 2); err != nil {
		t.Fatal("admission after space returned", err)
	} else if err := closeAgain(); err != nil {
		t.Fatal(err)
	}
	for i, f := range owners {
		// Retirement may return owned blocks; refill so neither member borrows
		// capacity the other released.
		fill := filepath.Join(root, fmt.Sprintf("foreign-reserve-finish-fill-%d", i))
		t.Cleanup(func() { _ = os.Remove(fill) })
		if err := exhaustHAPhysicalFixture(fill, haPhysicalFixtureFillBound); err != nil {
			t.Fatal(err)
		}
		if err := f.finishHAPhysicalBusiness(ctx); err != nil {
			t.Fatal("confirmed terminal whole common at ENOSPC", i, err)
		}
		if err := f.closeHAPhysicalNative(ctx); err != nil {
			t.Fatal("physical native close at ENOSPC", i, err)
		}
		if err := f.Close(); err != nil {
			t.Fatal("whole close at ENOSPC", i, err)
		}
		if left := haPhysicalOwnedTemporaries(t, f.cfg.Storage.DataDir); len(left) != 0 {
			t.Fatal("owned temporaries survived", i, left)
		}
	}
}

// waitHAPhysicalApplied waits until the Primary's durable Ordering prefix is
// confirmed and applied by the Replica.
func waitHAPhysicalApplied(t *testing.T, ctx context.Context, primary, replica *haSelectedRuntimeFoundation) {
	t.Helper()
	through := primary.replication.publisher.Snapshot().DurableIndex
	if err := primary.replication.coordinator.WaitConfirmed(ctx, through); err != nil {
		t.Fatal("durable prefix confirmation", err)
	}
	for replica.replication.publisher.Snapshot().AppliedIndex < through {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
