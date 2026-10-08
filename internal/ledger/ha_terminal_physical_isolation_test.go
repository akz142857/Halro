package ledger

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// These are source isolation tests, not evidence of genuine Member/Ordering
// qualification. The actual HA factory integration owns the positive proof.
func TestHAPhysicalEntrancesRejectActualStandaloneBeforeAllocating(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	ctx := context.Background()
	before := f.o.DirectoryOptions.Budget.Snapshot().Used
	projection := f.ledgerOwner
	selection := [32]byte{1}
	lease := &MaintenanceLease{}
	if err := projection.EnableHAQualifiedPhysicalTerminals(ctx, f.native, lease, 1<<20, f.publisherCredit, haPhysicalRefusedCapacity{}, selection); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("Standalone owner obtained HA physical authority", err)
	}
	if err := f.creator.EnableHAPhysicalCatchup(ctx, f.native, projection, lease, selection); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("Standalone directory obtained HA preparation", err)
	}
	if err := f.creator.InstallHAPhysicalCatchupCoverage(ctx, f.native, projection, lease); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("Standalone directory installed HA callback", err)
	}
	if err := qualifyHAPhysicalStartupRestoreLocked(ctx, f.native, selection, f.ledgerOptions.SavedAnchor); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("Standalone constructor minted HA restore permission", err)
	}
	if err := f.native.WithHASelectedPhysicalStartupOwner(ctx, f.o.DirectoryOptions.Budget, SelectedLedgerProjectionOpenOptions{}, f.o.DirectoryOptions.Identity, func(context.Context, SelectedHAPhysicalStartupSource) error {
		t.Fatal("Standalone startup loan reached HA physical visitor")
		return nil
	}); !errors.Is(err, ErrProjectionSource) {
		t.Fatal(err)
	}
	if f.native.physical.Load() != nil || f.creator.physical != nil || f.o.DirectoryOptions.Budget.Snapshot().Used != before {
		t.Fatal("refused HA entrance changed original owners or fees")
	}
}

func TestHAPhysicalNoDebtAfterCommonRequiresOriginalFullCutScope(t *testing.T) {
	q := &haPhysicalUsageSource{key: haSelectedLiveScopeKey{}}
	if err := q.VerifyHAPhysicalNoDebtAfterCommon(context.Background()); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("single-source live prefix became common publication authority", err)
	}
	// A candidate object with a full-scope key still has no actual source loan.
	q.key = haSelectedStartupScopeKey{}
	if err := q.VerifyHAPhysicalNoDebtAfterCommon(context.Background()); !errors.Is(err, ErrProjectionSource) && !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("unowned full-scope object granted no-debt permission", err)
	}
}

// Any call would prove mutation before source qualification.
type haPhysicalRefusedCapacity struct{}

func (haPhysicalRefusedCapacity) ReservePhysicalRange(int64) error { panic("unqualified allocation") }
func (haPhysicalRefusedCapacity) ConsumePhysicalRange(int64) error { panic("unqualified consumption") }
func (haPhysicalRefusedCapacity) RetirePhysicalRange(int64) error  { panic("unqualified retirement") }

func TestHAPhysicalRecoveryCandidateMustMatchActualAuthenticatedNative(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	ctx := context.Background()
	saved := f.ledgerOptions.SavedAnchor
	f.native.startupSaved = saved // only the independent native-boundary helper is tested here
	f.native.mu.Lock()
	defer f.native.mu.Unlock()
	if err := verifyHAPhysicalRecoveryAnchorLocked(ctx, f.native, saved, saved); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"hash", "epoch", "offset", "generation"} {
		bad := saved
		switch change {
		case "hash":
			bad.Hash[0] ^= 1
		case "epoch":
			bad.Epoch++
		case "offset":
			bad.Head.Offset--
		case "generation":
			bad.Head.Generation++
		}
		if err := verifyHAPhysicalRecoveryAnchorLocked(ctx, f.native, bad, saved); !errors.Is(err, ErrProjectionSource) {
			t.Fatal("candidate field granted authenticated native prefix", change, err)
		}
	}
}
