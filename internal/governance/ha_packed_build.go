package governance

import (
	"context"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

// BuildPackedTypedHAGovernanceProjection is an explicit offline full-cut
// reducer for an actual Replica native owner. It trusts no saved projection,
// confirms no term/index, and preserves the native journal. Independent HA
// preflight and the later complete PublishHA must authenticate its allowed cut.
func BuildPackedTypedHAGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions) (*GovernanceProjection, TypedGovernanceReference, error) {
	if o.Native == nil || !o.Native.replica {
		return nil, TypedGovernanceReference{}, authenticatedstate.ErrRecovery
	}
	return buildFixedPackedTypedGovernanceProjection(ctx, o, nil)
}
