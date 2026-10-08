package app

import (
	"errors"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/store/lock"
)

type seedLockRetirementFailure struct {
	Guard *lock.Lock
	Cause error
}

func (e *seedLockRetirementFailure) Error() string { return e.Cause.Error() }
func (e *seedLockRetirementFailure) Unwrap() error { return e.Cause }

// An uncertain native/derived descendant retains real process exclusion.
// The error owns the guard and the original cause (including original fees),
// so another generation cannot race a descriptor that has not actually exited.
func retireSeedGuard(guard *lock.Lock, cause error) error {
	if errors.Is(cause, ownedfile.ErrRetirement) {
		return &seedLockRetirementFailure{guard, cause}
	}
	if err := guard.Close(); err != nil {
		return &seedLockRetirementFailure{guard, errors.Join(cause, err, ownedfile.ErrRetirement)}
	}
	return cause
}
