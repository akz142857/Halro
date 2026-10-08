package statecut

import (
	"context"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
)

type haConfirmedMetadataScopeKey struct{}
type haConfirmedMetadataScope struct {
	owners    HAOwners
	cut       HAConfirmedSources
	mu        sync.Mutex
	active    bool
	borrowers sync.WaitGroup
	count     int
	failure   error
}

func visitHAConfirmedMetadataScope(ctx context.Context, o HAOwners, cut HAConfirmedSources, visit func(context.Context, HAConfirmedSources) error) (resultErr error) {
	scope := &haConfirmedMetadataScope{owners: o, cut: cut, active: true}
	defer func() {
		scope.mu.Lock()
		scope.active = false
		scope.mu.Unlock()
		scope.borrowers.Wait()
		scope.mu.Lock()
		resultErr = errors.Join(resultErr, scope.failure)
		scope.mu.Unlock()
	}()
	return visit(context.WithValue(ctx, haConfirmedMetadataScopeKey{}, scope), cut)
}

// WithHAConfirmedMetadataApplyAuthority consumes only the still-active loan
// minted by the genuine ReplicaConfirmedPrefix and actual four-source freeze.
// It neither reenters Member nor gives a startup/common or ordinary read grant.
func WithHAConfirmedMetadataApplyAuthority(ctx context.Context, o HAOwners, owner *metadatastate.Baseline, expected metadatajournal.Head, through uint64, visit func(context.Context) error) (resultErr error) {
	scope, ok := ctx.Value(haConfirmedMetadataScopeKey{}).(*haConfirmedMetadataScope)
	if !ok || scope == nil || owner == nil || visit == nil {
		return authenticatedstate.ErrSealIneligible
	}
	scope.mu.Lock()
	same := scope.owners.Member == o.Member && scope.owners.Ordering == o.Ordering && scope.owners.Source == o.Source && scope.owners.Baseline == o.Baseline && scope.owners.Components.Metadata == owner && o.Components.Metadata == owner && scope.owners.Components.Budget == o.Components.Budget
	if !scope.active || !same || scope.count >= 8 || ctx.Err() != nil {
		scope.mu.Unlock()
		return authenticatedstate.ErrSealIneligible
	}
	scope.count++
	scope.borrowers.Add(1)
	scope.mu.Unlock()
	completed := false
	defer func() {
		scope.mu.Lock()
		scope.count--
		scope.failure = errors.Join(scope.failure, resultErr)
		if !completed {
			scope.failure = errors.Join(scope.failure, authenticatedstate.ErrSealIneligible)
		}
		scope.mu.Unlock()
		scope.borrowers.Done()
	}()
	claims := o.Baseline.Claims()
	allowed := scope.cut.Allowed[3]
	if expected.Baseline != claims.New.Baseline || expected.FileRevision != claims.New.FileRevision || expected.Epoch != allowed.Generation || expected.Sequence < allowed.Sequence || through != allowed.Sequence {
		return authenticatedstate.ErrSealIneligible
	}
	actual, err := owner.EpochHead()
	if err != nil || actual != expected {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	resultErr = errors.Join(visit(ctx), ctx.Err())
	completed = true
	return resultErr
}
