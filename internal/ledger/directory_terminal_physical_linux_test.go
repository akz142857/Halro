//go:build linux

package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
)

func TestSelectedDirectoryPhysicalFreshGatePoolAndKnownClose(t *testing.T) {
	var fs unix.Statfs_t
	if err := unix.Statfs(os.TempDir(), &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC {
		t.Skip("persistent physical terminal recovery requires an ext4 TMPDIR fixture")
	}
	ctx := context.Background()
	// This explicit physical/checkpoint profile includes all prepaid tickets;
	// the ordinary 16-claim fixture cannot fund seven simultaneous root freezes.
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 512 << 20, Disk: 256 << 20}, MaxClaims: 64, MaxOwners: 16, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 64 * resourcebudget.ClaimRAMBytes}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	manager, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	f := newSelectedStartupGenerationsFixture(t, false, false, 0, manager)
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	capacity := &physicalFixtureCapacity{}
	native, _, err := OpenWithSelectedSeal(ctx, f.path, NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1, ReserveAppendDisk: capacity.reserve}, f.o)
	if err != nil {
		t.Fatal(err)
	}
	f.native = native
	if err := native.AuditSelectedStartup(ctx, f.o.DirectoryOptions.Budget); err != nil {
		t.Fatal(err)
	}
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	head, hash, _ := native.ChainHead()
	lo := f.ledgerOptions
	lo.Prefix = &ProjectionPrefix{Head: head, Hash: hash}
	so := SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority}
	owner, _, err := OpenSelectedAuthenticatedLedgerProjection(ctx, native, f.ledgerDirectory, lo, so)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
		if err := native.Close(); err != nil {
			t.Error(err)
		}
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
		if err := f.ledgerOwner.AdoptClosedSuccessor(owner); err != nil {
			t.Error(err)
		}
	}()
	fee, err := f.o.DirectoryOptions.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer fee.Release()
	seal, found, err := f.o.Publisher.Current()
	if err != nil || !found {
		t.Fatal(found, err)
	}
	raw, err := json.Marshal(seal)
	if err != nil {
		t.Fatal(err)
	}
	selection := sha256.Sum256(raw)
	if err := owner.EnableQualifiedPhysicalTerminals(ctx, native, 64<<20, fee, capacity, selection); err != nil {
		t.Fatal(err)
	}
	// A real selected native factory/source loan opens the Directory. Non-nil
	// recovery requests defer generic repair; no receipt still clears that gate
	// through the actual Restore boundary rather than granting a fake target.
	so.PhysicalRecovery = &PhysicalStartupRecoveryOptions{}
	ref := AuthenticatedDirectoryReference{Schema: authenticatedDirectorySchema, Root: seal.Roots[1], Position: seal.Positions[0]}
	d, err := OpenSelectedAuthenticatedDirectory(ctx, native, ref, f.o.Directory, f.o.DirectoryOptions, so)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
		if err := f.creator.AdoptClosedSuccessor(d); err != nil {
			t.Error(err)
		}
	}()
	if err := d.EnablePhysicalCatchup(ctx, native, owner, selection); err != nil {
		t.Fatal(err)
	}
	if err := native.BindPhysicalSelectionDigestContext(ctx, selection); err != nil {
		t.Fatal("qualify unchanged actual common selection", err)
	}
	if d.physical == nil || d.physical.window.Count() != directoryPhysicalCatchupSlots {
		t.Fatal("missing actual catch-up candidates")
	}
	original := d.physical.window
	for i := 0; i < 2; i++ {
		if err := d.EnsurePhysicalCatchup(ctx, native, owner); err != nil {
			t.Fatal(err)
		}
		if d.physical.window != original {
			t.Fatal("admission grew pool rather than reusing actual prepared targets")
		}
	}
	if err := d.InstallPhysicalCatchupCoverage(ctx, native, owner); err != nil {
		t.Fatal(err)
	}
	if err := d.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		if err := d.RetirePhysicalCatchupAfterCommon(ctx, native, owner, lease, f.o.Publisher); err != nil {
			return err
		}
		return d.BindPhysicalCommonSelection(ctx, native, owner, selection)
	}); err != nil {
		t.Fatal("retire actual common and retain original controls", err)
	}
	if !d.physical.closedQualified || d.physical.window != nil {
		t.Fatal("actual complete common did not qualify the close boundary")
	}
	// The actual ordinary Roll preflight must renew the two captured targets
	// before it could owe a changed descriptor; no request Reserve is borrowed.
	native.mu.Lock()
	rollPrepareErr := native.preparePhysicalDerivedLocked(ctx, PhysicalDerivedRoll, 1)
	native.mu.Unlock()
	if rollPrepareErr != nil || d.physical.window == nil || d.physical.window == original || d.physical.remaining != directoryPhysicalCatchupSlots || d.physical.closedQualified {
		t.Fatal("ordinary Roll did not prepare its actual finite directory vector", rollPrepareErr)
	}
	if err := d.WithPhysicalCatchupWorkspace(ctx, func(ctx context.Context) error {
		if err := d.RetirePhysicalCatchupAfterCommon(ctx, native, owner, lease, f.o.Publisher); err != nil {
			return err
		}
		return d.BindPhysicalCommonSelection(ctx, native, owner, selection)
	}); err != nil {
		t.Fatal("unchanged actual common did not join Roll preparation", err)
	}
	// Use the real original native pre-acceptance lane after publication. Its
	// callback must renew targets before any source fact can owe derived work.
	next, err := native.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal("next actual native admission after common publication", err)
	}
	if d.physical.closedQualified || d.physical.window == nil || d.physical.window == original || d.physical.remaining != directoryPhysicalCatchupSlots {
		t.Fatal("new admission reused a retired pool or retained stale close proof")
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal("completed selected NoDebt Close", err)
	}
	if d.physical.window != nil || d.physical.remaining != 0 {
		t.Fatal("known retired pool still active")
	}
}
