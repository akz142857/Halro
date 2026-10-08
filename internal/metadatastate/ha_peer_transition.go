package metadatastate

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// This cold owner retains the peer's old independently selected role. Only a
// durable, authenticated promise and the actual stopped Member/Ordering source
// scope may install its Replica sink; no ordinary facade is exposed.
type HAPeerTransitionSourceOpenOptions struct {
	OpenOptions
	Publisher                   *authenticatedstate.SealCandidatePublisher
	Guard                       *lock.Lock
	PeerAuthority               *replication.HAPromotionPeerAuthority
	WithAuthority               func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedMetadataPosition, metadatajournal.BoundReplicationAuthority) error) error
	WithConfirmedApplyAuthority func(context.Context, *Baseline, metadatajournal.Head, uint64, func(context.Context) error) error
}

type haPeerTransitionSource struct {
	authority          *replication.HAPromotionPeerAuthority
	permission         metadatajournal.BoundReplicationAuthority
	withAuthority      func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedMetadataPosition, metadatajournal.BoundReplicationAuthority) error) error
	withConfirmedApply func(context.Context, *Baseline, metadatajournal.Head, uint64, func(context.Context) error) error
}

// OpenHAPeerTransitionSourceRoot authenticates the original seven-root selection
// without relabeling an old Primary seal. Complete native bytes are inspected,
// but only the exact captured old Metadata prefix is applied. The returned
// Baseline is a cold sink/publication owner, never an ordinary DAO or Ready grant.
func OpenHAPeerTransitionSourceRoot(ctx context.Context, key []byte, output, journalPath string, o HAPeerTransitionSourceOpenOptions) (result *Baseline, receipt ReopenReceipt, resultErr error) {
	if o.PeerAuthority == nil || o.WithAuthority == nil || o.Publisher == nil || o.Guard == nil {
		return nil, receipt, ErrSource
	}
	claims := o.PeerAuthority.Claims()
	var permission metadatajournal.BoundReplicationAuthority
	withAuthority := func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedMetadataPosition, metadatajournal.BoundReplicationAuthority) error) error {
		var mu sync.Mutex
		active, entered := true, false
		var callbackErr error
		err := o.WithAuthority(ctx, func(ctx context.Context, a authenticatedstate.SealEligibility, allowed HASelectedMetadataPosition, p metadatajournal.BoundReplicationAuthority) error {
			mu.Lock()
			defer mu.Unlock()
			if !active || entered || ctx == nil {
				callbackErr = errors.Join(callbackErr, ErrSource)
				return ErrSource
			}
			entered = true
			selected, found, err := o.Publisher.SelectContext(ctx, a)
			if err != nil || !found || selected != claims.Seal || a.Role != claims.Seal.Role || allowed.Epoch != claims.Cursors[3].Generation || allowed.Sequence != claims.Cursors[3].Sequence || allowed.Hash != claims.Heads[3] {
				callbackErr = errors.Join(ErrSource, err)
				return callbackErr
			}
			permission = p
			callbackErr = visit(ctx, a, allowed, p)
			return callbackErr
		})
		mu.Lock()
		defer mu.Unlock()
		active = false
		if !entered {
			err = errors.Join(ErrSource, err)
		}
		return errors.Join(err, callbackErr)
	}
	result, receipt, resultErr = OpenHASelectedSourceRoot(ctx, key, output, journalPath, HASelectedSourceOpenOptions{OpenOptions: o.OpenOptions, Publisher: o.Publisher, Guard: o.Guard, WithAuthority: withAuthority})
	if resultErr != nil {
		return result, receipt, resultErr
	}
	// The callback above may permit a native received suffix, but it cannot
	// move the cold projection beyond the independently captured old prefix.
	if receipt.To.Sequence != claims.Cursors[3].Sequence || receipt.To.Hash != claims.Heads[3] || receipt.To.Epoch != claims.Cursors[3].Generation {
		cleanup := result.Close()
		if cleanup != nil {
			return result, ReopenReceipt{}, &BaselineOpenFailure{Owner: result, Cause: errors.Join(ErrSource, ownedfile.ErrRetirement, cleanup)}
		}
		return nil, ReopenReceipt{}, ErrSource
	}
	result.haPeerTransition = &haPeerTransitionSource{authority: o.PeerAuthority, permission: permission, withAuthority: withAuthority, withConfirmedApply: o.WithConfirmedApplyAuthority}
	return result, receipt, nil
}

// ConfigurePeerReplicaJournal installs physical Replica mode on the original
// completely authenticated descriptor. It neither closes/reopens a path nor
// promotes the old selected seal. A preparation without an actual promise refuses.
func (b *Baseline) ConfigurePeerReplicaJournal(ctx context.Context, key []byte, a metadatajournal.BoundReplicationAuthority) (result *metadatajournal.Log, resultErr error) {
	if b == nil || !b.mu.TryLock() {
		return nil, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	s := b.haPeerTransition
	if s == nil || s.authority == nil || s.authority.Claims().Promise == nil || s.permission != a || !b.ready.Load() || !b.linked || b.readOnly || b.journal == nil || len(key) != 32 || sha256.Sum256(key) != b.ledgerKeyTag {
		return nil, ErrSource
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	credit, err := b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: metadatajournal.FrozenReadRAMBytes})
	if err != nil {
		return nil, err
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ownedfile.ErrReplaced) {
			b.sealDebt = credit
			b.retirementErr = errors.Join(b.retirementErr, ownedfile.ErrRetirement, resultErr)
			b.ready.Store(false)
		} else if err := credit.Release(); err != nil {
			resultErr = errors.Join(resultErr, err)
			b.sealDebt = credit
			b.retirementErr = errors.Join(b.retirementErr, err)
			b.ready.Store(false)
		}
		if resultErr != nil {
			result = nil
		}
	}()
	var mu sync.Mutex
	active, entered := true, false
	var callbackErr error
	err = s.withAuthority(ctx, func(ctx context.Context, _ authenticatedstate.SealEligibility, allowed HASelectedMetadataPosition, actual metadatajournal.BoundReplicationAuthority) error {
		mu.Lock()
		defer mu.Unlock()
		if !active || entered || actual != a || allowed.Epoch != b.position.Epoch || allowed.Sequence != b.position.Sequence || allowed.Hash != b.position.Hash {
			callbackErr = errors.Join(callbackErr, ErrSource)
			return ErrSource
		}
		entered = true
		callbackErr = b.journal.ConfigureBoundReplicaContext(ctx, a, b.journal.Head())
		return callbackErr
	})
	mu.Lock()
	active = false
	if !entered {
		err = errors.Join(err, ErrSource)
	}
	err = errors.Join(err, callbackErr)
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	b.haReplicaPrepared = &a
	return b.journal, nil
}

// ReplayPeerTransitionTo is the sole reducer entry for this cold owner. The
// actual receiver's opaque confirmed-prefix scope must validate this exact
// owner, original Binding/FileRevision, Member promise, Ordering and requested
// prefix before lending a finite callback. Native head bytes grant no permission.
func (b *Baseline) ReplayPeerTransitionTo(ctx context.Context, expected metadatajournal.Head, through uint64) (result metadatajournal.Head, resultErr error) {
	if b == nil || !b.mu.TryLock() {
		return result, authenticatedstate.ErrBusy
	}
	s := b.haPeerTransition
	valid := s != nil && s.authority != nil && s.authority.Claims().Promise != nil && s.withConfirmedApply != nil && b.haReplicaPrepared != nil && *b.haReplicaPrepared == s.permission && b.ready.Load()
	b.mu.Unlock()
	if !valid {
		return result, ErrSource
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	var mu sync.Mutex
	active, entered := true, false
	var callbackErr error
	err := s.withConfirmedApply(ctx, b, expected, through, func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if !active || entered {
			callbackErr = errors.Join(callbackErr, ErrSource)
			return ErrSource
		}
		entered = true
		result, callbackErr = b.replayTo(ctx, expected, through, nil)
		return callbackErr
	})
	mu.Lock()
	active = false
	if !entered {
		err = errors.Join(err, ErrSource)
	}
	resultErr = errors.Join(err, callbackErr)
	if resultErr != nil {
		result = metadatajournal.Head{}
	}
	mu.Unlock()
	return
}
