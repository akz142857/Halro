package ledger

import (
	"context"
	"errors"
	"slices"

	"github.com/akz142857/Halro/internal/domain"
)

// AccountingAdmissionReader separates successful absence from read failure.
// Its owner supplies one trusted accounting view for all reads and Apply; this
// interface does not grant trust, reserve money, or select a Runtime backend.
type AccountingAdmissionReader interface {
	AccountingMaturityReader
	AccountingCohortReader
	AccountingObservationReader
	AccountingWorkUnitReader
	GovernanceCapacityReader
	AccountingListReader
	ReadBalance(context.Context, BalanceKey) (Balance, error)
	ReadWorkUnit(context.Context, string) (domain.WorkUnit, bool, error)
	ReadRun(context.Context, string) (domain.Run, bool, error)
	ReadGovernanceIdempotency(context.Context, string, string, string) (GovernanceIdempotency, bool, error)
}

var _ AccountingAdmissionReader = (*State)(nil)
var _ AccountingAdmissionReader = (*LedgerProjection)(nil)

func stateAdmissionRead[T any](ctx context.Context, state *State, read func() (T, bool)) (T, bool, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, false, err
	}
	if state == nil {
		return zero, false, errors.New("accounting state is absent")
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return zero, false, err
	}
	value, found := read()
	if err := ctx.Err(); err != nil {
		return zero, false, err
	}
	return value, found, nil
}

func (s *State) ReadBalance(ctx context.Context, key BalanceKey) (Balance, error) {
	value, _, err := stateAdmissionRead(ctx, s, func() (Balance, bool) {
		v, found := s.balances[key]
		return v, found
	})
	return value, err
}

func (s *State) ReadWorkUnit(ctx context.Context, id string) (domain.WorkUnit, bool, error) {
	return stateAdmissionRead(ctx, s, func() (domain.WorkUnit, bool) {
		v, found := s.workUnits[id]
		v.OutcomeDefinitions = slices.Clone(v.OutcomeDefinitions)
		if v.ClosedAt != nil {
			closedAt := *v.ClosedAt
			v.ClosedAt = &closedAt
		}
		return v, found
	})
}

func (s *State) ReadRun(ctx context.Context, id string) (domain.Run, bool, error) {
	return stateAdmissionRead(ctx, s, func() (domain.Run, bool) {
		v, found := s.runs[id]
		if v.ClosedAt != nil {
			closedAt := *v.ClosedAt
			v.ClosedAt = &closedAt
		}
		return v, found
	})
}

func (s *State) ReadGovernanceIdempotency(ctx context.Context, projectID, operation, keyHash string) (GovernanceIdempotency, bool, error) {
	return stateAdmissionRead(ctx, s, func() (GovernanceIdempotency, bool) {
		v, found := s.governanceIdempotency[governanceIdempotencyMapKey(projectID, operation, keyHash)]
		return v, found
	})
}
