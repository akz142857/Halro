package app

import "context"

type selectedSeedFaultKey struct{}

// Fault callbacks can only interrupt a qualified installation. They never
// replace a source, selected permission, filesystem identity, or owner.
func selectedSeedFault(ctx context.Context, phase string) error {
	if fault, ok := ctx.Value(selectedSeedFaultKey{}).(func(string) error); ok {
		return fault(phase)
	}
	return nil
}
