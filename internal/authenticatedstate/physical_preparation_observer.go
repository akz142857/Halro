package authenticatedstate

import "context"

type physicalPreparationObserverKey struct{}

// WithPhysicalPreparationObserver is a bounded fault-observation seam. It does
// not grant source, selection, target ownership or recovery authority.
func WithPhysicalPreparationObserver(ctx context.Context, observe func(string) error) context.Context {
	return context.WithValue(ctx, physicalPreparationObserverKey{}, observe)
}

func physicalPreparationObserver(ctx context.Context) func(string) error {
	observe, _ := ctx.Value(physicalPreparationObserverKey{}).(func(string) error)
	return observe
}
