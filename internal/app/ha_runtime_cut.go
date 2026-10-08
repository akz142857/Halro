package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// haRuntimeCut describes an independently authenticated role-permitted native
// prefix. It authenticates neither a saved derived root nor incremental startup.
// In particular Primary's local prefix may lead its transferable applied prefix.
type haRuntimeCut struct {
	member                      replication.MemberState
	orderingIndex, orderingTerm uint64
	orderingMAC                 [32]byte
	permitted                   [4]replication.StoreCursor
	heads                       [4][32]byte
}

// authenticateHARuntimeCut runs while construction/recovery owns all domain
// writers, before public admission. The budgeted path authenticates the actual
// state file and entire ordering descriptor, then rechecks role authority before
// returning. Four native heads belong to that same allowed cut, never whichever
// newer bytes happen to be present. No native or member file is modified here.
func (r *replicationRuntime) authenticateHARuntimeCut(ctx context.Context, budget *resourcebudget.Manager) (result haRuntimeCut, resultErr error) {
	if r == nil || r.publisher == nil || r.journal == nil || r.source == nil {
		return result, errors.New("HA cut requires actual member, ordering and native source owners")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var workspace resourcebudget.Permit
	if budget != nil {
		// Native HeadAt performs full source decoding. Borrow the existing
		// replication reconstruction workspace, independently of its fixed
		// idle readers and the much smaller member/ordering control records.
		var err error
		workspace, err = budget.TryAcquireRecovery(resourcebudget.Cost{RAM: replication.MinPrimaryCandidateRAMBytes})
		if err != nil {
			return result, err
		}
		defer func() {
			resultErr = errors.Join(resultErr, workspace.Release())
			if resultErr != nil {
				result = haRuntimeCut{}
			}
		}()
	}
	build := func(ctx context.Context, state replication.MemberState) error {
		if err := state.Validate(); err != nil {
			return err
		}
		if state.Role != r.role || state.Projection.Index != state.AppliedIndex {
			return replication.ErrMemberAuthority
		}
		check := func(ctx context.Context, head replication.FrozenOrderingHead) error {
			if head.Header.ClusterID != state.ClusterID || head.Header.Incarnation != state.Incarnation || head.Index != state.DurableIndex || head.MAC != state.OrderingHeadMAC || head.Term > state.Term {
				return errors.New("HA member and ordering authorities name different native history")
			}
			applied, err := r.journal.StoreCursorsThroughContext(ctx, state.AppliedIndex)
			if err != nil {
				return err
			}
			metadata := applied[replication.StoreMetadata-replication.StoreLedger]
			if metadata.Generation != state.Projection.MetadataEpoch || metadata.Sequence != state.Projection.MetadataSequence {
				return errors.New("HA Metadata projection cursor does not belong to the applied ordering prefix")
			}
			candidate := haRuntimeCut{member: state, orderingIndex: state.AppliedIndex, permitted: applied}
			if state.Role == replication.RolePrimary {
				candidate.orderingIndex, candidate.permitted = head.Index, head.StoreCursors
			}
			if candidate.orderingIndex != 0 {
				record, err := r.journal.Record(candidate.orderingIndex)
				if err != nil {
					return err
				}
				candidate.orderingTerm, candidate.orderingMAC = record.Term, record.MAC
			}
			for store := replication.StoreLedger; store <= replication.StoreMetadata; store++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				head, err := r.source.HeadAt(store, candidate.permitted[store-replication.StoreLedger])
				if err != nil {
					return fmt.Errorf("authenticate HA store %d permitted head: %w", store, err)
				}
				candidate.heads[store-replication.StoreLedger] = head
			}
			result = candidate
			return ctx.Err()
		}
		if budget != nil {
			return r.journal.WithFrozenHead(ctx, budget, check)
		}
		// Legacy unbudgeted runtimes retain their existing authenticated-open
		// trust. This path still verifies the bounded cut and final member view.
		index, term, mac := r.journal.Head()
		cursors, err := r.journal.StoreCursorsThroughContext(ctx, index)
		if err != nil {
			return err
		}
		return check(ctx, replication.FrozenOrderingHead{Header: replication.OrderingHeader{ClusterID: state.ClusterID, Incarnation: state.Incarnation}, Index: index, Term: term, MAC: mac, StoreCursors: cursors})
	}
	if budget != nil {
		resultErr = workspace.WithRecoveryScope(ctx, func(ctx context.Context) error {
			return r.publisher.WithReadAuthority(ctx, budget, func(ctx context.Context, state replication.MemberState, authorize func(func() error) error) error {
				if err := build(ctx, state); err != nil {
					return err
				}
				return authorize(func() error { return ctx.Err() })
			})
		})
	} else {
		before := r.publisher.Snapshot()
		resultErr = build(ctx, before)
		if resultErr == nil && !sameReplicaOwnerBuildCut(before, r.publisher.Snapshot()) {
			resultErr = replication.ErrMemberAuthority
		}
	}
	if resultErr != nil {
		result = haRuntimeCut{}
	}
	return result, resultErr
}
