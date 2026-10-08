package metadatastate

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/store/lock"
)

type HASelectedMetadataPosition struct {
	Epoch, Sequence uint64
	Hash            [32]byte
}
type HASelectedSourceOpenOptions struct {
	OpenOptions
	Publisher               *authenticatedstate.SealCandidatePublisher
	Guard                   *lock.Lock
	WithAuthority           func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedMetadataPosition, metadatajournal.BoundReplicationAuthority) error) error
	PhysicalTerminalReserve bool
	// Supplied by the actual receiver/Member/Ordering constructor. It borrows
	// only an already active confirmed scope and cannot use native head bytes
	// to grant live Replica Applied permission.
	WithConfirmedPhysicalApplyAuthority func(context.Context, *Baseline, metadatajournal.Head, uint64, func(context.Context) error) error
}

// OpenHASelectedSourceRoot opens only the common root independently selected
// under actual Member/Ordering/four-source authority. Complete format2 bytes
// authenticate before only the role-permitted suffix is reduced. It opens no
// legacy metadata DB and does not borrow a freshly replayed maintenance root.
// Actual Primary hooks or Replica sink must be installed separately before a
// business facade can obtain qualification. Unknown Close preserves its owner.
func OpenHASelectedSourceRoot(ctx context.Context, ledgerKey []byte, output, journalPath string, o HASelectedSourceOpenOptions) (result *Baseline, receipt ReopenReceipt, resultErr error) {
	if o.WithAuthority == nil || o.Publisher == nil || o.Guard == nil || o.Budget == nil || len(ledgerKey) != 32 || !o.Guard.CoversNativePath(journalPath) {
		return nil, receipt, ErrSource
	}
	if err := o.Publisher.ValidateOwner(o.Guard, o.Budget); err != nil {
		return nil, receipt, err
	}
	var mu sync.Mutex
	active, entered := true, false
	var callbackErr error
	err := o.WithAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedMetadataPosition, permission metadatajournal.BoundReplicationAuthority) error {
		mu.Lock()
		defer mu.Unlock()
		if !active || entered || ctx == nil || (a.Role != authenticatedstate.SealPrimary && a.Role != authenticatedstate.SealReplica) || a.Instance != o.Identity || permission.Baseline.Identity != o.Identity || permission.Validate() != nil {
			callbackErr = errors.Join(callbackErr, ErrSource)
			return ErrSource
		}
		entered = true
		callbackErr = func() error {
			common, found, err := o.Publisher.Select(a)
			if err != nil || !found {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			var root authenticatedstate.SealRoot
			var position authenticatedstate.SealPosition
			for _, r := range common.Roots[:common.RootCount] {
				if r.Component == authenticatedstate.SealComponentMetadata {
					root = r
				}
			}
			for _, p := range common.Positions[:common.PositionCount] {
				if p.Component == authenticatedstate.SealComponentMetadata {
					position = p
				}
			}
			if root.Kind != authenticatedstate.SealRootState || root.IndexID != AuthenticatedMetadataSealIndexID || root.Codec != AuthenticatedMetadataSealCodec || position.Epoch != permission.Baseline.Epoch || position.Revision != permission.FileRevision || position.MetadataBaseline != permission.Baseline.Cut || allowed.Epoch != permission.Baseline.Epoch || position.Sequence > allowed.Sequence {
				return ErrSource
			}
			key, err := hkdf.Key(sha256.New, ledgerKey, permission.Baseline.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
			if err != nil {
				return err
			}
			defer clear(key)
			epoch, sequence, err := metadatajournal.BoundReplicationCursor(ctx, journalPath, key, permission)
			if err != nil {
				return err
			}
			native, err := metadatajournal.BoundReplicationPrefixHead(ctx, journalPath, key, permission, epoch, sequence)
			if err != nil {
				return err
			}
			permitted, err := metadatajournal.BoundReplicationPrefixHead(ctx, journalPath, key, permission, allowed.Epoch, allowed.Sequence)
			if err != nil || permitted.Hash != allowed.Hash {
				return errors.Join(ErrSource, err)
			}
			saved, err := metadatajournal.BoundReplicationPrefixHead(ctx, journalPath, key, permission, position.Epoch, position.Sequence)
			if err != nil || metadataSealPosition(saved) != position {
				return errors.Join(ErrSource, err)
			}
			options := o.OpenOptions
			options.SelectedSealRoot = &root
			options.SavedRoot = authenticatedstate.RootIdentity{Scope: root.State.Scope, Cut: root.State.Cut, Digest: root.State.Digest, ID: root.State.PackID, Revision: root.State.Revision, Rows: root.Rows}
			options.SavedHead, options.NativeHead, options.PermittedHead = saved, native, permitted
			options.MaxJournalBytes = permission.MaxFileBytes
			options.selectedNativeRoot = true
			if o.PhysicalTerminalReserve {
				options.haPhysicalSource = &haPhysicalSource{publisher: o.Publisher, guard: o.Guard, withAuthority: o.WithAuthority, role: a.Role, permission: permission, withConfirmedApply: o.WithConfirmedPhysicalApplyAuthority, recoveryPermitted: permitted, recoverySeal: common}
			}
			result, receipt, err = openFromRoot(ctx, ledgerKey, output, journalPath, options, nil)
			if err != nil {
				return err
			}
			final, found, err := o.Publisher.Select(a)
			if err != nil || !found || final != common {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			if result.haPhysicalSource == nil {
				result.haPhysicalSource = &haPhysicalSource{publisher: o.Publisher, guard: o.Guard, withAuthority: o.WithAuthority, role: a.Role, permission: permission}
			}
			return ctx.Err()
		}()
		return callbackErr
	})
	mu.Lock()
	active = false
	resultErr = errors.Join(err, callbackErr)
	if !entered {
		resultErr = errors.Join(ErrSource, resultErr)
	}
	mu.Unlock()
	if resultErr != nil && result != nil {
		resultErr = errors.Join(resultErr, result.Close())
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			result = nil
		}
		receipt = ReopenReceipt{}
	}
	return
}
