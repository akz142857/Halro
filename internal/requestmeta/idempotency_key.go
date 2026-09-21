package requestmeta

import "context"

type idempotencyKeyKey struct{}

// WithIdempotencyKey carries the caller's Idempotency-Key from the protocol
// facade to the service.
//
// It travels in the context rather than in every signature for the same reason
// the Run ID does: it is optional metadata about the request rather than part
// of the operation, and threading it through Chat, ChatStream, Embeddings and
// both facades would change five signatures to say what one context value
// says — and would let a path be added that silently drops it.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	if ctx == nil || key == "" {
		return ctx
	}
	return context.WithValue(ctx, idempotencyKeyKey{}, key)
}

// IdempotencyKey reports the caller's key, and whether they sent one at all.
// The two are different: no key means the request is not idempotent and nothing
// durable is written for it.
func IdempotencyKey(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	key, ok := ctx.Value(idempotencyKeyKey{}).(string)
	return key, ok && key != ""
}
