package requestmeta

import "context"

type runIDKey struct{}

func WithRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runIDKey{}, runID)
}

func RunID(ctx context.Context) (string, bool) {
	runID, ok := ctx.Value(runIDKey{}).(string)
	return runID, ok && runID != ""
}
