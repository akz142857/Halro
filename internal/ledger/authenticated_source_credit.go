package ledger

import (
	"errors"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// A failed actual Close retains its descriptor through cause and the entire
// decoder credit through permit. Returning a pathname error cannot retire IO.
type authenticatedSourceDebt struct {
	cause  error
	permit resourcebudget.Permit
}

func (e *authenticatedSourceDebt) Error() string { return e.cause.Error() }
func (e *authenticatedSourceDebt) Unwrap() error { return e.cause }

func finishAuthenticatedSourceCredit(permit resourcebudget.Permit, resultErr error) error {
	if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) {
		return &authenticatedSourceDebt{cause: resultErr, permit: permit}
	}
	if err := permit.Release(); err != nil {
		return &authenticatedSourceDebt{cause: errors.Join(resultErr, ownedfile.ErrRetirement, err), permit: permit}
	}
	return resultErr
}
