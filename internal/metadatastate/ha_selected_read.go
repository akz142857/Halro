package metadatastate

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/store/lock"
)

// OpenHASelectedReadRoot opens the actual common root, including derived rows
// at sequence zero, using read-only descriptors. A complete authenticated native tail may lie
// beyond SavedHead; it is inspected without reducing or crediting it. The independent HA caller
// must hold its Member/Ordering qualification through this factory. It permits
// neither semantic writes nor adoption of a candidate as role authority.
func OpenHASelectedReadRoot(ctx context.Context, key []byte, output, journalPath string, o OpenOptions, binding metadatajournal.BoundReplicationAuthority, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, authority authenticatedstate.SealEligibility) (*Baseline, ReopenReceipt, error) {
	return openHASelectedReadRoot(ctx, key, output, journalPath, o, binding, publisher, guard, authority)
}

// OpenHASelectedReadRootFromSource borrows the independently qualified actual
// maintenance writer's exclusion. Its whole native freeze and component lock
// stay owned until selection completes; original Close joins every borrowed FD.
func OpenHASelectedReadRootFromSource(ctx context.Context, key []byte, output, journalPath string, o OpenOptions, binding metadatajournal.BoundReplicationAuthority, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, authority authenticatedstate.SealEligibility, source *Baseline) (owner *Baseline, receipt ReopenReceipt, resultErr error) {
	if source == nil || source.output != output || source.journalPath != journalPath {
		return nil, receipt, ErrSource
	}
	resultErr = source.WithFrozenSealReference(ctx, o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, func(ctx context.Context, _ AuthenticatedMetadataSealReference, head metadatajournal.Head) error {
		if head != o.NativeHead {
			return ErrSource
		}
		o.haReadSource = source
		var err error
		owner, receipt, err = openHASelectedReadRoot(ctx, key, output, journalPath, o, binding, publisher, guard, authority)
		return err
	})
	if resultErr != nil && owner != nil {
		cleanup := owner.Close()
		resultErr = errors.Join(resultErr, cleanup)
		receipt = ReopenReceipt{}
		if cleanup != nil {
			resultErr = &BaselineOpenFailure{Owner: owner, Cause: resultErr}
		} else {
			owner = nil
		}
	}
	return owner, receipt, resultErr
}
func openHASelectedReadRoot(ctx context.Context, key []byte, output, journalPath string, o OpenOptions, binding metadatajournal.BoundReplicationAuthority, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, authority authenticatedstate.SealEligibility) (*Baseline, ReopenReceipt, error) {
	if publisher == nil || guard == nil || authority.Instance != o.Identity || authority.Coverage != authenticatedstate.SealCoverageAuthenticated || (authority.Role != authenticatedstate.SealPrimary && authority.Role != authenticatedstate.SealReplica) || o.SavedHead != o.PermittedHead || o.NativeHead.Baseline != binding.Baseline || o.NativeHead.FileRevision != binding.FileRevision || o.MaxJournalBytes != binding.MaxFileBytes {
		return nil, ReopenReceipt{}, ErrSource
	}
	if err := binding.Validate(); err != nil {
		return nil, ReopenReceipt{}, err
	}
	if err := publisher.ValidateOwner(guard, o.Budget); err != nil {
		return nil, ReopenReceipt{}, err
	}
	selected, found, err := publisher.Select(authority)
	if err != nil || !found {
		return nil, ReopenReceipt{}, errors.Join(ErrSource, err)
	}
	var root authenticatedstate.SealRoot
	for _, candidate := range selected.Roots[:selected.RootCount] {
		if candidate.Component == authenticatedstate.SealComponentMetadata && candidate.Kind == authenticatedstate.SealRootState && candidate.IndexID == AuthenticatedMetadataSealIndexID && candidate.Codec == AuthenticatedMetadataSealCodec {
			root = candidate
		}
	}
	if root.State.Scope != baselineScope(o.BaselineOptions) || root.Empty || root.Rows == 0 {
		return nil, ReopenReceipt{}, ErrSource
	}
	if authority.Positions[1] != metadataSealPosition(o.SavedHead) {
		return nil, ReopenReceipt{}, ErrSource
	}
	o.SelectedSealRoot = &root
	o.SavedRoot = authenticatedstate.RootIdentity{Scope: root.State.Scope, Cut: root.State.Cut, Digest: root.State.Digest, ID: root.State.PackID, Revision: root.State.Revision, Rows: root.Rows}
	o.selectedNativeRoot, o.haSelectedRead = true, true
	owner, receipt, err := openFromRoot(ctx, key, output, journalPath, o, nil)
	if err != nil {
		return owner, receipt, err
	}
	if err = owner.VerifyHAMaintenanceRoot(ctx); err == nil {
		actual, found, selectErr := publisher.Select(authority)
		if selectErr != nil || !found || actual != selected {
			err = errors.Join(ErrSource, selectErr)
		}
	}
	if err != nil {
		cleanup := owner.Close()
		if cleanup != nil {
			return owner, ReopenReceipt{}, &BaselineOpenFailure{Owner: owner, Cause: errors.Join(err, cleanup)}
		}
		return nil, ReopenReceipt{}, err
	}
	return owner, receipt, nil
}
