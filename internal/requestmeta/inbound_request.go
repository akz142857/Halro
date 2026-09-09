package requestmeta

import "context"

type inboundRequestKey struct{}

// WithInboundRequest keeps the decoded request accepted at Halro's public API
// boundary. The value is deliberately held by reference and is not serialized
// here: failure capture is opt-in, and successful calls must not pay the cost of
// rendering a second copy of their request.
func WithInboundRequest(ctx context.Context, request any) context.Context {
	if ctx == nil || request == nil {
		return ctx
	}
	return context.WithValue(ctx, inboundRequestKey{}, request)
}

// InboundRequest returns the request as the caller expressed it, before Halro
// translates it into the provider-neutral semantic form used for routing.
func InboundRequest(ctx context.Context) (any, bool) {
	if ctx == nil {
		return nil, false
	}
	request := ctx.Value(inboundRequestKey{})
	return request, request != nil
}
