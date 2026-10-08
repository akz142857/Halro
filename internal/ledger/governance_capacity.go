package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// GovernanceCapacityReader supplies admission counts without materializing
// historical lists. A read error cannot be interpreted as available capacity.
// The caller still owns the project's governance mutation lock; these reads do
// not reserve capacity or authenticate an existing projection file.
type GovernanceCapacityReader interface {
	ReadOpenWorkUnitCount(context.Context, string) (int64, error)
	ReadRunCapacity(context.Context, string, string, time.Time) (RunCapacity, error)
}

type RunCapacity struct {
	ActiveRuns   int64
	WorkUnitRuns int64
}

var _ GovernanceCapacityReader = (*State)(nil)
var _ GovernanceCapacityReader = (*LedgerProjection)(nil)

func (s *State) ReadOpenWorkUnitCount(ctx context.Context, projectID string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s == nil || projectID == "" {
		return 0, errors.New("accounting state and project are required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var count int64
	for _, item := range s.workUnits {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if item.ProjectID == projectID && item.Status == domain.WorkUnitOpen {
			count++
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

// Both counts describe the same State view. Expired runs do not consume active
// capacity; all runs, including closed and expired ones, count toward the
// work unit's lifetime limit. This adapter retains State's historical scan cost
// but does not allocate/copy a list proportional to that history.
func (s *State) ReadRunCapacity(ctx context.Context, projectID, workUnitID string, at time.Time) (RunCapacity, error) {
	if err := ctx.Err(); err != nil {
		return RunCapacity{}, err
	}
	if s == nil || projectID == "" {
		return RunCapacity{}, errors.New("accounting state and project are required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result RunCapacity
	for _, item := range s.runs {
		if err := ctx.Err(); err != nil {
			return RunCapacity{}, err
		}
		if item.ProjectID != projectID {
			continue
		}
		if item.Status == domain.RunActive && at.Before(item.ExpiresAt) {
			result.ActiveRuns++
		}
		if workUnitID != "" && item.WorkUnitID == workUnitID {
			result.WorkUnitRuns++
		}
	}
	if err := ctx.Err(); err != nil {
		return RunCapacity{}, err
	}
	return result, nil
}
