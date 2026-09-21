package app

// The control plane's own per-minute ceilings, charged through keylimit against
// the calling Key and its Project. They are constants rather than configuration
// on purpose: this is the floor under an authenticated caller, and an operator
// who could lower it to zero would be assembling a deployment in which an
// authenticated principal has no bound at all.
//
// Reads are given the larger budget because polling a Run's state is the shape
// an integration actually has; a write is a journal append and is rarer.
const (
	governanceWriteClass      = "governance:write"
	governanceReadClass       = "governance:read"
	governanceWriteKeyRPM     = 120
	governanceWriteProjectRPM = 1_000
	governanceReadKeyRPM      = 600
	governanceReadProjectRPM  = 5_000
)
