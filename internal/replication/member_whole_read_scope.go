package replication

import (
	"context"
	"errors"
	"sync"
)

type memberWholeReadScopeKey struct{}
type memberWholeReadScope struct {
	mu       sync.Mutex
	owner    *StatePublisher
	state    MemberState
	active   bool
	borrowed bool
	jobs     sync.WaitGroup
	failure  error
}

func (s *memberWholeReadScope) activate() { s.mu.Lock(); s.active = true; s.mu.Unlock() }
func (s *memberWholeReadScope) revoke() error {
	s.mu.Lock()
	s.active = false
	s.mu.Unlock()
	s.jobs.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}
func borrowMemberWholeReadScope(ctx context.Context, p *StatePublisher, visit func(MemberState) error) (resultErr error) {
	if ctx == nil || p == nil || visit == nil {
		return ErrMemberAuthority
	}
	scope, ok := ctx.Value(memberWholeReadScopeKey{}).(*memberWholeReadScope)
	if !ok || scope.owner != p {
		return ErrMemberAuthority
	}
	scope.mu.Lock()
	if !scope.active || scope.borrowed || ctx.Err() != nil {
		scope.mu.Unlock()
		return ErrMemberAuthority
	}
	scope.borrowed = true
	scope.jobs.Add(1)
	state := cloneMemberState(scope.state)
	scope.mu.Unlock()
	returned := false
	defer func() {
		scope.mu.Lock()
		scope.borrowed = false
		if !returned {
			scope.failure = errors.Join(scope.failure, ErrMemberAuthority)
		} else {
			scope.failure = errors.Join(scope.failure, resultErr)
		}
		scope.mu.Unlock()
		scope.jobs.Done()
	}()
	resultErr = visit(state)
	returned = true
	return resultErr
}
