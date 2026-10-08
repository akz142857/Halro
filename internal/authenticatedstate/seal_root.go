package authenticatedstate

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// SealRootReference returns the actual authenticated root-page reference for
// this Store's complete logical namespace. The caller's schema entry must name
// that entire namespace; this method cannot prove that a caller-defined
// IndexID covers every domain index it is expected to cover.
func (s *Store) SealRootReference(ctx context.Context, schema SealRootSchema) (result SealRoot, resultErr error) {
	if s == nil || !s.mu.TryLock() {
		return result, ErrBusy
	}
	defer s.mu.Unlock()
	if s.closing || s.closed {
		return result, ErrClosed
	}
	if schema.Kind != SealRootState || schema.Component < SealComponentLedger || schema.Component > SealComponentLogDirectory || schema.IndexID == 0 || schema.Codec == 0 {
		return result, ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := s.fault(); err != nil {
		return result, err
	}
	permit, err := s.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		return result, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, permit.Release())
		if resultErr != nil {
			result = SealRoot{}
		}
	}()
	if err := errors.Join(s.checkPack(), s.checkRoot()); err != nil {
		s.poison(err)
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return sealRootFor(s.current, s.rootDigest, schema), nil
}

func sealRootFor(current cowRoot, digest [32]byte, schema SealRootSchema) SealRoot {
	content := current.content
	root := SealRoot{
		Kind:      SealRootState,
		State:     StateRootCommitment{Scope: content.context, Cut: current.cut, Digest: digest, PackID: content.id, Revision: current.revision, Extent: content.extent, Height: content.height},
		Component: schema.Component,
		IndexID:   schema.IndexID,
		Codec:     schema.Codec,
		Rows:      content.count,
		Digest:    content.root.digest,
	}
	if content.count == 0 {
		root.Empty = true
	} else {
		root.FileID = content.id
		root.Offset = content.root.offset
		root.Length = content.root.length
	}
	return root
}

func validSealRootReference(root SealRoot) bool {
	if root.Kind == SealRootNative {
		if root.State != (StateRootCommitment{}) {
			return false
		}
	} else if root.Kind == SealRootState {
		state := root.State
		if state.Scope == ([32]byte{}) || state.Cut == ([32]byte{}) || state.Digest == ([32]byte{}) || state.PackID == ([16]byte{}) || state.Revision == 0 || state.Extent < headerBytes || state.Extent > math.MaxInt64 {
			return false
		}
		if root.Empty {
			if state.Height != 0 || root.Digest != emptyDigest(state.Scope) {
				return false
			}
		} else if state.Height == 0 || state.Height > maxHeight || root.FileID != state.PackID || root.Offset < headerBytes || root.Length > maxNodeBytes || root.Offset > state.Extent || uint64(root.Length) > state.Extent-root.Offset {
			return false
		}
	} else {
		return false
	}

	if root.Component < SealComponentLedger || root.Component > SealComponentLogDirectory || root.IndexID == 0 || root.Codec == 0 || root.Digest == ([32]byte{}) {
		return false
	}
	if root.Empty {
		return root.Rows == 0 && root.FileID == ([16]byte{}) && root.Offset == 0 && root.Length == 0
	}
	return root.Rows > 0 && root.FileID != ([16]byte{}) && root.Length > 0 && root.Offset <= uint64(^uint64(0)>>1)-uint64(root.Length)
}

// Reconstruct only from a complete independently selected seal commitment.
// A page digest alone cannot select a historical COW revision or its pack extent.
func selectedCOWRoot(selected SealRoot, schema SealRootSchema, key []byte, scope, cut [32]byte) (cowRoot, [32]byte, error) {
	var zero [32]byte
	if schema.Kind != SealRootState || selected.Kind != schema.Kind || selected.Component != schema.Component || selected.IndexID != schema.IndexID || selected.Codec != schema.Codec || !validSealRootReference(selected) || selected.State.Scope != scope || selected.State.Cut != cut {
		return cowRoot{}, zero, ErrRecovery
	}
	c := commitment{context: scope, id: selected.State.PackID, extent: selected.State.Extent, count: selected.Rows, height: selected.State.Height, root: ref{offset: selected.Offset, length: selected.Length, digest: selected.Digest}}
	r := cowRoot{cut: cut, revision: selected.State.Revision, content: c}
	raw, err := r.encode(key)
	if err != nil || sha256.Sum256(raw) != selected.State.Digest {
		return cowRoot{}, zero, errors.Join(ErrRecovery, err)
	}
	return r, selected.State.Digest, nil
}
