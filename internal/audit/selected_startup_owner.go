package audit

import (
	"context"
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type selectedAuditProvenance struct {
	ha        *haSelectedStartupProvenance
	publisher *authenticatedstate.SealCandidatePublisher
	guard     *lock.Lock
	seal      authenticatedstate.CandidateSeal
	identity  [32]byte
}

// ValidateSelectedStartupOwner proves actual selected construction and exact
// independently selected publisher/identity/lifecycle. It grants no readiness
// and does not authenticate old bytes; the finite native audit owns that job.
func (l *Log) ValidateSelectedStartupOwner(ctx context.Context, budget *resourcebudget.Manager, publisher *authenticatedstate.SealCandidatePublisher, guard *lock.Lock, authority authenticatedstate.SealEligibility, identity [32]byte) error {
	if l == nil || budget == nil || publisher == nil || guard == nil || ctx.Err() != nil {
		return errors.Join(ErrStartupCache, ctx.Err())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.startupProvenance
	if p == nil || p.ha != nil || p.publisher != publisher || p.guard != guard || p.identity != identity || l.cacheBudget != budget || l.closing || l.closed || l.file == nil || l.terminalErr != nil || l.replica || l.beforeDurable != nil || l.afterDurable != nil || authority.Role != authenticatedstate.SealStandalone || authority.Coverage != authenticatedstate.SealCoverageAuthenticated || !guard.CoversNativePath(l.path) {
		return ErrStartupCache
	}
	if err := publisher.ValidateOwner(guard, budget); err != nil {
		return err
	}
	seal, found, err := publisher.Select(authority)
	if err != nil || !found || seal != p.seal || ctx.Err() != nil {
		return errors.Join(ErrStartupCache, err, ctx.Err())
	}
	actual, err := l.file.Stat()
	named, nameErr := os.Lstat(l.path)
	if err != nil || nameErr != nil || !named.Mode().IsRegular() || !os.SameFile(actual, named) || actual.Size() != l.offset {
		return errors.Join(ErrStartupCache, err, nameErr)
	}
	return nil
}
