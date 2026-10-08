//go:build linux

package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

type physicalFixtureCapacity struct {
	mu               sync.Mutex
	admitted, unused int64
}

func (c *physicalFixtureCapacity) reserve(bytes int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes <= 0 || bytes > (64<<20)-c.admitted-c.unused {
		return resourcebudget.ErrExhausted
	}
	c.admitted += bytes
	return nil
}
func (c *physicalFixtureCapacity) ReservePhysicalRange(bytes int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes <= 0 || bytes > (64<<20)-c.admitted-c.unused {
		return resourcebudget.ErrExhausted
	}
	c.unused += bytes
	return nil
}
func (c *physicalFixtureCapacity) ConsumePhysicalRange(bytes int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes <= 0 || bytes > c.unused {
		return resourcebudget.ErrInvalid
	}
	c.unused -= bytes
	c.admitted += bytes
	return nil
}
func (c *physicalFixtureCapacity) RetirePhysicalRange(bytes int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes <= 0 || bytes > c.unused {
		return resourcebudget.ErrInvalid
	}
	c.unused -= bytes
	return nil
}

// This is a fresh native/COW owner fixture, not HA or migrated Runtime authority.
// The separate app fixture exercises the actual selected Runtime caller.
func physicalNativeFixture(t *testing.T) (*AuthenticatedLedgerProjection, *Log, resourcebudget.Permit) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	native := openChained(t, path, nil)
	for _, e := range projectionTestLifecycle(1, false)[:3] {
		if _, err := native.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(err, partial)
	}
	o := authenticatedLedgerTestOptions(t)
	p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "auth"), report, o)
	if err != nil {
		t.Fatal(err)
	}
	fee, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	capacity := &physicalFixtureCapacity{}
	native, err = OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1, ReserveAppendDisk: capacity.reserve})
	if err != nil {
		t.Fatal(err)
	}
	p.selectedNative.Store(native)
	if err := p.EnablePhysicalTerminals(ctx, native, 64<<20, fee, capacity); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close(); _ = p.Close() })
	return p, native, fee
}

func appendPhysicalFixture(p *AuthenticatedLedgerProjection, native *Log, ctx context.Context, event Event) error {
	ctx = WithPhysicalAppendKind(ctx, event.Kind)
	return p.WithPreparedPhysicalAppend(ctx, func(ctx context.Context) error {
		return p.WithNativeApplyWorkspace(ctx, func(ctx context.Context) error {
			_, _, err := native.AppendApplyRecordWithReceipt(ctx, event, func(ctx context.Context, r Record) error {
				_, err := p.AdvanceOneFromNative(ctx, native, nativeRecordPrefix(r))
				return err
			})
			return err
		})
	})
}

func TestPhysicalTerminalNativePartialWriteRetainsDebtAndOwner(t *testing.T) {
	p, native, fee := physicalNativeFixture(t)
	ctx := context.Background()
	w, err := native.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	before := native.offset
	beforeUsed := p.budget.Snapshot().Used
	native.mu.Lock()
	native.durability = &faultDurability{file: native.file, writeErr: syscall.ENOSPC, partial: true}
	native.mu.Unlock()
	err = resourcebudget.WithCompletionWorkspace(w.Bind(ctx), func(ctx context.Context) error {
		return appendPhysicalFixture(p, native, ctx, projectionTestLifecycle(1, false)[3])
	})
	if !errors.Is(err, syscall.ENOSPC) || native.status.Load() != AccountingRecoveryRequired || native.PhysicalTerminalDebt() != 1 {
		t.Fatal("partial native write was treated as definite refusal", err, native.status.Load(), native.PhysicalTerminalDebt())
	}
	actual, err := os.Stat(native.path)
	if err != nil || actual.Size() <= before || native.offset != before {
		t.Fatal("fixture did not write real partial bytes", actual, err)
	}
	for i := 0; i < 2; i++ {
		if err := w.Close(); !errors.Is(err, authenticatedstate.ErrRecovery) {
			t.Fatal("unknown child Close was not sticky", err)
		}
	}
	if err := appendPhysicalFixture(p, native, ctx, projectionTestLifecycle(2, false)[0]); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("partial owner admitted next fact", err)
	}
	if err := native.Close(); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("Close lost partial debt", err)
	}
	if !native.physical.Load().feeHeld || native.PhysicalTerminalDebt() != 1 || p.budget.Snapshot().Used.Disk != beforeUsed.Disk {
		t.Fatal("uncertain actual Close rebated persistent ownership", p.budget.Snapshot())
	}
	if cost, err := fee.Cost(); err != nil || cost.Disk != 64<<20 {
		t.Fatal(cost, err)
	}
}

func TestPhysicalTerminalNativeForeignReplacementPreservesFees(t *testing.T) {
	p, native, fee := physicalNativeFixture(t)
	ctx := context.Background()
	original := native.path + ".original"
	if err := os.Rename(native.path, original); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign native must remain")
	if err := os.WriteFile(native.path, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	before := p.budget.Snapshot().Used
	if _, err := native.ReservePhysicalTerminals(ctx, 1); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("foreign inode acquired allocation authority", err)
	}
	if err := native.Close(); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("foreign exit became successful owner transfer", err)
	}
	if !native.physical.Load().feeHeld || p.budget.Snapshot().Used.Disk != before.Disk {
		t.Fatal("foreign owner lost credit")
	}
	if cost, err := fee.Cost(); err != nil || cost.Disk != 64<<20 {
		t.Fatal(cost, err)
	}
	if got, err := os.ReadFile(native.path); err != nil || string(got) != string(foreign) {
		t.Fatal("foreign source mutated", err)
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatal("captured original discarded", err)
	}
}

func TestPhysicalTerminalRollFailureRetainsFeeAndStopsAdmission(t *testing.T) {
	p, native, _ := physicalNativeFixture(t)
	ctx := context.Background()
	w, err := native.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	before := p.budget.Snapshot().Used
	failure := errors.New("actual successor selected before previous Close")
	previous := native.file
	native.maintenancePhase = func(phase string) error {
		if phase == "roll-before-previous-close" {
			return failure
		}
		return nil
	}
	if _, err := native.Roll(); !errors.Is(err, failure) {
		t.Fatal("Roll failure was not joined", err)
	}
	if _, err := previous.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("failed Roll did not join previous descriptor", err)
	}
	if _, err := native.ReservePhysicalTerminals(ctx, 1); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("failed Roll manufactured successor authority", err)
	}
	if err := native.Close(); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("failed handoff was released by Close", err)
	}
	if !native.physical.Load().feeHeld || p.budget.Snapshot().Used.Disk != before.Disk {
		t.Fatal("failed handoff returned fee", p.budget.Snapshot())
	}
}

func TestPhysicalTerminalRollTransfersExactOwnerAndBudget(t *testing.T) {
	p, native, fee := physicalNativeFixture(t)
	ctx := context.Background()
	w, err := native.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	previous := native.file
	before, err := previous.Stat()
	if err != nil {
		t.Fatal(err)
	}
	oldCeiling := native.physical.Load().ceiling
	roll, err := native.Roll()
	if err != nil || !roll.Rolled {
		t.Fatal("actual Roll failed", roll, err)
	}
	if _, err := previous.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("previous descriptor did not actually exit", err)
	}
	var sealed unix.Stat_t
	if err := unix.Stat(filepath.Join(native.directory, roll.Sealed.File), &sealed); err != nil || sealed.Blocks*512 > (before.Size()+4095)/4096*4096 {
		t.Fatal("old native unused allocated tail was stranded", sealed, err)
	}
	after, err := native.file.Stat()
	o := native.physical.Load()
	if err != nil || os.SameFile(before, after) || !os.SameFile(o.file, after) || o.generation != native.generation || o.ceiling != oldCeiling-before.Size() || o.debt != 0 || o.active != 0 {
		t.Fatal("Roll changed qualification or reset consumed growth allowance", after, err, o)
	}
	w, err = native.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal("new successor cannot own physical terminal", err)
	}
	err = resourcebudget.WithCompletionWorkspace(w.Bind(ctx), func(ctx context.Context) error {
		return appendPhysicalFixture(p, native, ctx, projectionTestLifecycle(1, false)[3])
	})
	if err != nil {
		t.Fatal("actual successor native/COW Apply", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	if o.feeHeld {
		t.Fatal("joined native retained transient permit reference")
	}
	if cost, err := fee.Cost(); err != nil || cost.Disk != 64<<20 {
		t.Fatal("native Close refunded persistent original allowance", cost, err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	// The native namespace is persistent. Only the privately created COW
	// directory has retired; the original native-growth fee must still exist.
	if used := p.budget.Snapshot().Used; used != (resourcebudget.Cost{RAM: resourcebudget.ClaimRAMBytes, Disk: 64 << 20}) {
		t.Fatal("COW retirement refunded persistent native ownership or leaked pool RAM", used)
	}
}
