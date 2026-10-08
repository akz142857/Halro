package statecut

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/replication"
)

// HASelectedCut describes facts obtained under actual member/order freezes.
// Copies are observations, not reusable role/write/promotion permission.
type HASelectedCut struct {
	Seal         authenticatedstate.CandidateSeal
	Authority    authenticatedstate.SealEligibility
	Member       replication.MemberState
	ActualMember replication.MemberState
	AllowedIndex uint64
	Allowed      [4]replication.StoreCursor
	Heads        [4][32]byte
}

func validateHAMemberAuthority(member replication.MemberState, baseline *replication.MetadataBaselineAuthority) error {
	return baseline.ValidateMember(member)
}

// WithHASelectedCut authenticates the entire actual ordering descriptor, the
// independent signed boundary and both saved/allowed four-source heads before
// selecting any content roots. Candidate role/term/index never supply authority.
// The callback must not reenter Member or Ordering or publish member progress.
func WithHASelectedCut(ctx context.Context, o HAOwners, visit func(context.Context, HASelectedCut) error) (resultErr error) {
	return withHASelectedCut(ctx, o, nil, visit)
}

// WithHAConfirmedSelectedCut is only for a real ReplicaApplier startup job.
// The old common seal remains independently selected at its saved cut, while
// candidate recovery is limited by the original receiver's Confirmed prefix.
// It does not mint ordinary read permission or advance Member Applied.
func WithHAConfirmedSelectedCut(ctx context.Context, o HAOwners, receiver *replication.ReplicaReceiver, prefix *replication.ReplicaConfirmedPrefix, visit func(context.Context, HASelectedCut) error) error {
	if prefix == nil || receiver == nil || o.PromotionRecovery != nil {
		return authenticatedstate.ErrSealIneligible
	}
	return prefix.WithPosition(ctx, receiver, o.Ordering, func(ctx context.Context, position replication.ReplicaConfirmedPosition) error {
		return withHASelectedCut(ctx, o, &position, visit)
	})
}

func withHASelectedCut(ctx context.Context, o HAOwners, applying *replication.ReplicaConfirmedPosition, visit func(context.Context, HASelectedCut) error) error {
	if o.PeerPromotionRecovery != nil || o.PeerPromotionCompletion != nil {
		return authenticatedstate.ErrSealIneligible
	}
	return withHASelectedCutMode(ctx, o, applying, nil, visit)
}
func withHASelectedCutMode(ctx context.Context, o HAOwners, applying *replication.ReplicaConfirmedPosition, peer *replication.HAPromotionPeerAuthority, visit func(context.Context, HASelectedCut) error) (resultErr error) {
	return withHASelectedCutRecoveryMode(ctx, o, applying, peer, nil, visit)
}

// WithHAReceivedSelectedCut verifies the old seven roots while one genuinely
// accepted native/Ordering frame awaits Member publication. It supplies only
// a cold observation: no normal selected scope, confirmed prefix or write grant.
func WithHAReceivedSelectedCut(ctx context.Context, o HAOwners, received *replication.ReplicaReceivedOrderingAuthority, visit func(context.Context, HASelectedCut) error) error {
	if received == nil || o.PromotionRecovery != nil || o.PeerPromotionRecovery != nil || o.PeerPromotionCompletion != nil {
		return authenticatedstate.ErrSealIneligible
	}
	return withHASelectedCutRecoveryMode(ctx, o, nil, nil, received, visit)
}
func withHASelectedCutRecoveryMode(ctx context.Context, o HAOwners, applying *replication.ReplicaConfirmedPosition, peer *replication.HAPromotionPeerAuthority, received *replication.ReplicaReceivedOrderingAuthority, visit func(context.Context, HASelectedCut) error) (resultErr error) {
	stage := "actual owner inventory"
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("HA saved-cut qualification %s: %w", stage, resultErr)
		}
	}()
	c := o.Components
	if c.Budget == nil || c.Guard == nil || c.Publisher == nil || o.Member == nil || o.Ordering == nil || o.Source == nil || o.Baseline == nil || visit == nil {
		return authenticatedstate.ErrSealIneligible
	}
	if err := c.Publisher.ValidateOwner(c.Guard, c.Budget); err != nil {
		return err
	}
	stage = "independent carrier and actual Member"
	claims := o.Baseline.Claims()
	return o.Member.WithReadAuthority(ctx, c.Budget, func(ctx context.Context, member replication.MemberState, authorize func(func() error) error) error {
		actualMember := member
		if peer != nil {
			if o.PromotionRecovery != nil || o.PeerPromotionRecovery != peer || applying != nil {
				return authenticatedstate.ErrSealIneligible
			}
			if o.PeerPromotionCompletion != nil {
				if o.PeerPromotionCompletion.Peer() != peer {
					return replication.ErrMemberAuthority
				}
				if err := o.PeerPromotionCompletion.ValidateRecoveryMember(actualMember, o.Ordering); err != nil {
					return err
				}
			} else if err := peer.ValidateRecoveryMember(actualMember); err != nil {
				return err
			}
			historical := peer.Claims()
			if historical.BaselineDigest != o.Baseline.Descriptor().AuthorityDigest {
				return replication.ErrMemberAuthority
			}
			member = historical.Before
			if err := validateHAMemberAuthority(member, o.Baseline); err != nil {
				return err
			}
		} else if o.PromotionRecovery != nil {
			if err := o.PromotionRecovery.ValidateRecoveryMember(actualMember); err != nil {
				return err
			}
			historical := o.PromotionRecovery.Claims()
			if historical.BaselineDigest != o.Baseline.Descriptor().AuthorityDigest {
				return replication.ErrMemberAuthority
			}
			member = historical.Before
		} else if err := validateHAMemberAuthority(member, o.Baseline); err != nil {
			return err
		}
		if member.Projection.Index != member.AppliedIndex || member.ConfirmedIndex < claims.Index+1 {
			return replication.ErrMemberAuthority
		}
		stage = "actual complete Ordering head"
		return o.Ordering.WithFrozenHead(ctx, c.Budget, func(ctx context.Context, head replication.FrozenOrderingHead) error {
			if received != nil {
				if err := received.ValidateObservedOwners(o.Member, o.Ordering, o.Source, o.Baseline, c.Budget, actualMember, head); err != nil {
					return err
				}
			} else if head.Header.ClusterID != member.ClusterID || head.Header.Incarnation != member.Incarnation || head.Index != actualMember.DurableIndex || head.MAC != actualMember.OrderingHeadMAC || (head.Term != actualMember.Term && !((o.PromotionRecovery != nil || peer != nil && o.PeerPromotionCompletion != nil) && head.Index == member.DurableIndex && head.Term == member.Term)) {
				return authenticatedstate.ErrSealIneligible
			}
			if o.PromotionRecovery != nil && head.Index > member.DurableIndex {
				if err := o.PromotionRecovery.ValidateOrdering(o.Ordering); err != nil {
					return err
				}
			}
			stage = "signed boundary record"
			boundary, err := o.Ordering.Record(claims.Index + 1)
			if err != nil {
				return err
			}
			descriptor, err := replication.DecodeMetadataBaselineMetadata(boundary.Metadata)
			if err != nil || boundary.Kind != replication.KindMetadataBaseline || boundary.Term != claims.Term || boundary.PreviousMAC != claims.OrderingHead || descriptor != o.Baseline.Descriptor() {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			stage = "actual old/successor source boundary"
			if err := o.Source.ValidateMetadataBaseline(boundary); err != nil {
				return err
			}
			stage = "actual role/applied permitted cut"
			index, role := member.AppliedIndex, authenticatedstate.SealReplica
			if applying != nil {
				if member.Role != replication.RoleReplica || applying.ClusterID != member.ClusterID || applying.Incarnation != member.Incarnation || applying.NodeID != member.NodeID || applying.PrimaryNodeID != o.Baseline.CurrentPrimary() || applying.Term != member.Term || applying.PromisedTerm != member.PromisedTerm || applying.Durable != member.DurableIndex || applying.Confirmed != member.ConfirmedIndex || applying.Applied != member.AppliedIndex || applying.Projection != member.Projection || member.AppliedIndex >= member.ConfirmedIndex {
					return replication.ErrMemberAuthority
				}
				index = applying.Confirmed
			}
			if member.Role == replication.RolePrimary {
				index, role = member.DurableIndex, authenticatedstate.SealPrimary
			} else if member.Role != replication.RoleReplica {
				return replication.ErrMemberAuthority
			}
			if index < claims.Index+1 {
				return authenticatedstate.ErrSealIneligible
			}
			applied, err := o.Ordering.StoreCursorsThroughContext(ctx, member.AppliedIndex)
			if err != nil {
				return err
			}
			if applied[3].Generation != member.Projection.MetadataEpoch || applied[3].Sequence != member.Projection.MetadataSequence {
				return replication.ErrMemberAuthority
			}
			allowed, err := o.Ordering.StoreCursorsThroughContext(ctx, index)
			if err != nil {
				return err
			}
			stage = "actual MACed common Seal"
			selected, found, err := c.Publisher.CurrentContext(ctx)
			if err != nil || !found {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			if o.PromotionRecovery != nil && selected != o.PromotionRecovery.Claims().Seal {
				return authenticatedstate.ErrSealIneligible
			}
			if peer != nil && selected != peer.Claims().Seal {
				return authenticatedstate.ErrSealIneligible
			}
			if selected.OrderingIndex < claims.Index+1 || selected.OrderingIndex > index || selected.Role != role {
				return authenticatedstate.ErrSealIneligible
			}
			stage = "saved Ordering evidence"
			saved, err := o.Ordering.StoreCursorsThroughContext(ctx, selected.OrderingIndex)
			if err != nil {
				return err
			}
			record, err := o.Ordering.Record(selected.OrderingIndex)
			if err != nil || record.Term != member.Term {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			offset, err := o.Ordering.PrefixOffset(selected.OrderingIndex)
			if err != nil {
				return err
			}
			a := authenticatedstate.SealEligibility{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: claims.New.Baseline.Identity, Cluster: sha256.Sum256([]byte(member.ClusterID)), Member: sha256.Sum256([]byte(member.NodeID)), Role: role, Term: member.Term, OrderingIndex: selected.OrderingIndex, OrderingOffset: uint64(offset), OrderingMAC: record.MAC, Coverage: authenticatedstate.SealCoverageAuthenticated}
			copy(a.Positions[:], selected.Positions[:])
			incarnation := sha256.Sum256([]byte(member.Incarnation))
			copy(a.Incarnation[:], incarnation[:16])
			cut := HASelectedCut{Seal: selected, Authority: a, Member: member, ActualMember: actualMember, AllowedIndex: index, Allowed: allowed}
			for store := replication.StoreLedger; store <= replication.StoreMetadata; store++ {
				slot := store - replication.StoreLedger
				stage = fmt.Sprintf("actual saved/allowed source %d", store)
				position := selected.Positions[[]int{0, 3, 2, 1}[slot]]
				generation := position.Generation
				if store == replication.StoreMetadata {
					generation = position.Epoch
					if position.Revision != claims.New.FileRevision || position.MetadataBaseline != claims.New.Baseline.Cut || generation != claims.New.Baseline.Epoch {
						return authenticatedstate.ErrSealIneligible
					}
				}
				if store == replication.StoreAudit || store == replication.StoreGovernance {
					if position.Generation != 0 {
						return authenticatedstate.ErrSealIneligible
					}
					generation = 1
				}
				hash, err := o.Source.HeadAt(store, saved[slot])
				if err != nil || generation != saved[slot].Generation || position.Sequence != saved[slot].Sequence || position.Hash != hash {
					return errors.Join(authenticatedstate.ErrSealIneligible, err)
				}
				cut.Heads[slot], err = o.Source.HeadAt(store, allowed[slot])
				if err != nil {
					return err
				}
			}
			stage = "Usage saved prefix equals Ledger"
			usage := selected.Positions[0]
			usage.Component = authenticatedstate.SealComponentUsage
			if selected.Positions[4] != usage {
				return authenticatedstate.ErrSealIneligible
			}
			stage = "independent role/cut versus common Seal"
			if err := authenticatedstate.ValidateSealEligibility(selected, a); err != nil {
				return err
			}
			qualified, found, err := c.Publisher.SelectContext(ctx, a)
			if err != nil || !found || qualified != selected {
				return errors.Join(authenticatedstate.ErrSealIneligible, err)
			}
			stage = "frozen Member publication authorization"
			if received != nil {
				return authorize(func() error { return visit(ctx, cut) })
			}
			if applying != nil {
				if err := replication.ReconstructRangeEach(member.ClusterID, member.Incarnation, 1, member.DurableIndex, o.Ordering, o.Source, func(replication.LocalCommit) error { return ctx.Err() }); err != nil {
					return err
				}
			}
			return authorize(func() (resultErr error) {
				scope := &haSelectedCutScope{owners: o, cut: cut}
				scope.active.Store(true)
				defer func() {
					scope.mu.Lock()
					scope.active.Store(false)
					scope.mu.Unlock()
					scope.borrowers.Wait()
					scope.mu.Lock()
					resultErr = errors.Join(resultErr, scope.failure)
					scope.mu.Unlock()
				}()
				ctx = context.WithValue(ctx, haSelectedCutScopeKey{}, scope)
				if applying != nil {
					return visitHAConfirmedMetadataScope(ctx, o, HAConfirmedSources{Position: *applying, Allowed: cut.Allowed, Heads: cut.Heads}, func(ctx context.Context, _ HAConfirmedSources) error { return visit(ctx, cut) })
				}
				return visit(ctx, cut)
			})
		})
	})
}

// A scoped loan exists only during the actual Member/Ordering/source callback.
// The saved observation is never a reusable authorization capability.
type haSelectedCutScopeKey struct{}
type haSelectedCutScope struct {
	owners    HAOwners
	cut       HASelectedCut
	active    atomic.Bool
	mu        sync.Mutex
	borrowers sync.WaitGroup
	count     int
	failure   error
}

func withBorrowedHASelectedCut(ctx context.Context, o HAOwners, visit func(context.Context, HASelectedCut) error) error {
	scope, ok := ctx.Value(haSelectedCutScopeKey{}).(*haSelectedCutScope)
	if !ok || !scope.active.Load() || ctx.Err() != nil || visit == nil {
		return errors.Join(authenticatedstate.ErrSealIneligible, ctx.Err())
	}
	source, target := scope.owners, o
	if source.PeerPromotionCompletion != target.PeerPromotionCompletion || source.PeerPromotionRecovery != target.PeerPromotionRecovery || source.PromotionRecovery != target.PromotionRecovery || source.Member != target.Member || source.Ordering != target.Ordering || source.Source != target.Source || source.Baseline != target.Baseline || source.Components.Publisher != target.Components.Publisher || source.Components.Guard != target.Components.Guard || source.Components.Budget != target.Components.Budget {
		return authenticatedstate.ErrSealIneligible
	}
	scope.mu.Lock()
	if !scope.active.Load() || scope.count >= 8 {
		scope.mu.Unlock()
		return authenticatedstate.ErrSealIneligible
	}
	scope.count++
	scope.borrowers.Add(1)
	scope.mu.Unlock()
	err := visit(ctx, scope.cut)
	scope.mu.Lock()
	scope.count--
	scope.failure = errors.Join(scope.failure, err)
	scope.mu.Unlock()
	scope.borrowers.Done()
	return err
}
