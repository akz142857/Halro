package metadatastate

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const (
	AuthenticatedMetadataSealIndexID uint16 = 1
	AuthenticatedMetadataSealCodec   uint16 = 2
)

// AuthenticatedMetadataSealReference binds the authenticated content root to
// the exact Metadata journal position whose reducer produced it. It does not
// establish cross-component cut, identity/key eligibility, or Runtime use.
type AuthenticatedMetadataSealReference struct {
	Schema   authenticatedstate.SealRootSchema
	Root     authenticatedstate.SealRoot
	Position authenticatedstate.SealPosition
}

func metadataSealPosition(position metadatajournal.Head) authenticatedstate.SealPosition {
	return authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentMetadata,
		Epoch: position.Epoch, Revision: position.FileRevision, Sequence: position.Sequence,
		Offset: uint64(position.Offset), Hash: position.Hash, MetadataBaseline: position.Baseline.Cut}
}

// SealReference exports this owner's current root and applied position as one
// fixed receipt. The owner lock excludes append/replay/trim while both are
// captured; the underlying Store independently verifies its current pack/root.
func (b *Baseline) SealReference(ctx context.Context) (result AuthenticatedMetadataSealReference, resultErr error) {
	if b == nil || !b.mu.TryLock() {
		return result, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	return b.sealReferenceLocked(ctx)
}

func (b *Baseline) sealReferenceLocked(ctx context.Context) (AuthenticatedMetadataSealReference, error) {
	return b.sealReferenceLockedMode(ctx, true)
}

func (b *Baseline) sealReferenceLockedMode(ctx context.Context, requireReady bool) (result AuthenticatedMetadataSealReference, resultErr error) {
	if requireReady && !b.ready.Load() || !b.linked || b.journal == nil || b.store == nil {
		return result, authenticatedstate.ErrClosed
	}
	if !b.journal.SelectedStartupStructuralReady() {
		return result, metadatajournal.ErrStartupAuditPending
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	position := b.position
	head := b.journal.Head()
	if position.Baseline != head.Baseline || position.Epoch != head.Epoch || position.FileRevision != head.FileRevision || position.FormatVersion != 2 || position.Offset < 0 || position.Sequence < head.TrimmedThrough || position.Sequence > head.Sequence || position.Hash == ([32]byte{}) || position.Baseline.Version != 1 || position.Baseline.Epoch != position.Epoch || position.Baseline.Scope != baselineScope(BaselineOptions{Identity: b.evidence.Identity, InstanceID: b.evidence.InstanceID, KeyGeneration: b.evidence.KeyGeneration}) || position.Baseline.RootID == ([16]byte{}) || position.Baseline.Cut == ([32]byte{}) {
		return result, errors.Join(ErrSource, errors.New("metadata applied position is not bound to its native journal"))
	}
	permit, err := b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, permit.Release())
		if resultErr != nil {
			result = AuthenticatedMetadataSealReference{}
		}
	}()
	identity, err := b.store.RootIdentity(ctx)
	if err != nil {
		return result, err
	}
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentMetadata, IndexID: AuthenticatedMetadataSealIndexID, Codec: AuthenticatedMetadataSealCodec}
	root, err := b.store.SealRootReference(ctx, schema)
	if err != nil {
		return result, err
	}
	if identity.Scope != position.Baseline.Scope || identity.Revision == 0 || root.Rows == 0 || root.Empty || identity.Cut == ([32]byte{}) || root.FileID != identity.ID || root.Rows != identity.Rows {
		return result, errors.Join(ErrSource, errors.New("metadata seal root differs from its authenticated identity"))
	}
	// Check the root's authenticated bookkeeping against the precise applied
	// position. The native head may legitimately contain an unapplied tail.
	pin, err := b.store.Snapshot(ctx)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	for _, entry := range []struct {
		name  string
		value uint64
	}{{"metadata_journal_epoch", position.Epoch}, {"applied_journal_sequence", position.Sequence}} {
		key, err := RowKey([][]byte{[]byte("meta")}, []byte(entry.name), false)
		if err != nil {
			return result, err
		}
		value, found, err := pin.Get(ctx, key)
		if err != nil || !found || len(value) != 8 || decodeMetadataSealUint(value) != entry.value {
			return result, errors.Join(ErrSource, errors.New("metadata seal root bookkeeping does not match applied position"), err)
		}
	}
	result = AuthenticatedMetadataSealReference{
		Schema:   schema,
		Root:     root,
		Position: metadataSealPosition(position),
	}
	return result, nil
}

func decodeMetadataSealUint(raw []byte) uint64 {
	var value uint64
	for _, b := range raw {
		value = value<<8 | uint64(b)
	}
	return value
}
