package metadatastate

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/store/lock"
)

// OpenAuthorizedRotationRoot accepts a historical source only from canonical
// authenticated authorization saved before the target existed. It has no
// business admission, writable COW owner, native append or torn-tail repair.
// The actual independently selected successor must already supersede its root.
func OpenAuthorizedRotationRoot(ctx context.Context, ledgerKey []byte, output, journalPath string, o OpenOptions, sourceSealBytes []byte, sourceAuthority, targetAuthority authenticatedstate.SealEligibility, hard authenticatedstate.SealHardLimits, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock) (*Baseline, ReopenReceipt, error) {
	if publisher == nil || guard == nil || o.SavedHead != o.NativeHead || o.SavedHead != o.PermittedHead {
		return nil, ReopenReceipt{}, ErrSource
	}
	if err := publisher.ValidateOwner(guard, o.Budget); err != nil {
		return nil, ReopenReceipt{}, err
	}
	source, err := authenticatedstate.DecodeSealCandidate(sourceSealBytes, ledgerKey, hard)
	if err != nil {
		return nil, ReopenReceipt{}, err
	}
	if err := authenticatedstate.ValidateSealEligibility(source, sourceAuthority); err != nil {
		return nil, ReopenReceipt{}, err
	}
	if source.Instance != o.Identity || source.Role != authenticatedstate.SealStandalone || source.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return nil, ReopenReceipt{}, ErrSource
	}
	selected, found, err := publisher.Select(targetAuthority)
	if err != nil || !found || selected.Generation != source.Generation || selected.Revision <= source.Revision || selected.Instance != source.Instance || selected.Incarnation != source.Incarnation {
		return nil, ReopenReceipt{}, errors.Join(ErrSource, err)
	}
	var historical authenticatedstate.SealRoot
	for _, root := range source.Roots[:source.RootCount] {
		if root.Component == authenticatedstate.SealComponentMetadata && root.Kind == authenticatedstate.SealRootState && root.IndexID == AuthenticatedMetadataSealIndexID && root.Codec == AuthenticatedMetadataSealCodec {
			historical = root
		}
	}
	if historical.State.Scope != o.SavedRoot.Scope || historical.State.Cut != o.SavedRoot.Cut || historical.State.Digest != o.SavedRoot.Digest || historical.State.PackID != o.SavedRoot.ID || historical.State.Revision != o.SavedRoot.Revision || historical.Rows != o.SavedRoot.Rows {
		return nil, ReopenReceipt{}, ErrSource
	}
	var successor authenticatedstate.SealRoot
	for _, root := range selected.Roots[:selected.RootCount] {
		if root == historical {
			return nil, ReopenReceipt{}, ErrSource
		}
		if root.Component == authenticatedstate.SealComponentMetadata && root.Kind == authenticatedstate.SealRootState && root.IndexID == AuthenticatedMetadataSealIndexID && root.Codec == AuthenticatedMetadataSealCodec {
			successor = root
		}
	}
	if successor.State.Scope != historical.State.Scope || successor.State.PackID == historical.State.PackID {
		return nil, ReopenReceipt{}, ErrSource
	}
	var sourcePosition authenticatedstate.SealPosition
	for _, position := range source.Positions[:source.PositionCount] {
		if position.Component == authenticatedstate.SealComponentMetadata {
			sourcePosition = position
		}
	}
	if sourcePosition.Epoch != o.SavedHead.Epoch || sourcePosition.Revision != o.SavedHead.FileRevision || sourcePosition.Sequence != o.SavedHead.Sequence || sourcePosition.Offset != uint64(o.SavedHead.Offset) || sourcePosition.Hash != o.SavedHead.Hash || sourcePosition.MetadataBaseline != o.SavedHead.Baseline.Cut {
		return nil, ReopenReceipt{}, ErrSource
	}
	var successorPosition authenticatedstate.SealPosition
	for _, position := range selected.Positions[:selected.PositionCount] {
		if position.Component == authenticatedstate.SealComponentMetadata {
			successorPosition = position
		}
	}
	if successorPosition.Epoch != sourcePosition.Epoch+1 || successorPosition.MetadataBaseline == sourcePosition.MetadataBaseline {
		return nil, ReopenReceipt{}, ErrSource
	}
	o.SelectedSealRoot, o.selectedNativeRoot, o.rotationSource = &historical, true, true
	return openFromRoot(ctx, ledgerKey, output, journalPath, o, nil)
}
