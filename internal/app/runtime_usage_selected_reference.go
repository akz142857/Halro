package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/usage"
	"os"
	"path/filepath"
)

type selectedUsageReferenceRetirement struct {
	Cause error
	Owner *usage.DiskProjection
}

func (e *selectedUsageReferenceRetirement) Error() string { return e.Cause.Error() }
func (e *selectedUsageReferenceRetirement) Unwrap() error { return e.Cause }

// Build a complete reference using the real selected Ledger owner and native
// source. The four fixed unpublished names are never opened as saved authority.
// An interrupted/foreign slot stays opaque and charged to actual inventory.
func (r *Runtime) buildSelectedUsageWindowReference(ctx context.Context, lease *ledger.MaintenanceLease) (*usage.DiskProjection, error) {
	if r.selectedState == nil || r.config.ResourceBudget == nil || r.resourcePressure.logicalBudget() == nil {
		return nil, usage.ErrArchiveWindowProof
	}
	base := memoryStateDirectory(r.config)
	var root, scratch string
	for slot := 0; slot < 4; slot++ {
		candidate := filepath.Join(base, fmt.Sprintf("window-reference-%d", slot))
		candidateScratch := candidate + "-scratch"
		available := true
		for _, path := range []string{candidate, candidateScratch} {
			if err := validateRuntimeDerivedPath(ctx, r.config, path); err != nil {
				return nil, err
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				if err != nil {
					return nil, err
				}
				available = false
			}
		}
		if available {
			root, scratch = candidate, candidateScratch
			break
		}
	}
	if root == "" {
		return nil, errors.New("selected Usage reference opaque slots exhausted")
	}
	b, limits := r.selectedState.bootstrap, r.config.ResourceBudget
	p, _, err := usage.BuildPackedSelectedUsageReference(ctx, usage.AuthenticatedUsageOptions{
		Native: r.ledger, Owner: r.selectedState.generation.Owners.Ledger, Identity: b.Evidence.Identity, UsageKey: b.LedgerKey,
		Budget: r.resourcePressure.logicalBudget(), RootDirectory: root, RootMaxDiskBytes: limits.UsageProjectionDiskBytes,
		RootMaxAppendBytes: min(limits.UsageProjectionDiskBytes/2, 16<<20), ScratchPath: scratch, ScratchMaxDiskBytes: limits.UsageProjectionDiskBytes,
	}, lease)
	return p, err
}

func retireSelectedUsageWindowReference(p *usage.DiskProjection) error {
	if err := p.Discard(); err != nil {
		return &selectedUsageReferenceRetirement{Cause: errors.Join(ownedfile.ErrRetirement, err), Owner: p}
	}
	return nil
}

// The complete source factory requires this actual selected owner, its joined
// historical audit, the same Manager and the real native maintenance lease.
// It lends no normal replay permission and the callback cannot escape its pin.
func (r *Runtime) verifySelectedUsageNativeReference(ctx context.Context, lease *ledger.MaintenanceLease, head ledger.Watermark, hash [32]byte) error {
	if r.selectedState == nil || r.ledger == nil || r.resourcePressure == nil {
		return usage.ErrArchiveWindowProof
	}
	owner := r.selectedState.generation.Owners.Ledger
	ref, err := owner.SealReference(ctx)
	if err != nil {
		return err
	}
	return owner.WithSelectedCompleteUsageSource(ctx, r.ledger, lease, r.resourcePressure.logicalBudget(), ref.Scope, func(ctx context.Context, source ledger.AuthenticatedUsageSource) error {
		actual := source.NativeHead()
		if actual.Head != head || actual.Hash != hash || source.Reference().Anchor != actual {
			return usage.ErrCollectorHead
		}
		verified, err := source.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, nil)
		if err != nil || verified != head {
			return errors.Join(usage.ErrCollectorHead, err)
		}
		return source.Check(ctx)
	})
}
