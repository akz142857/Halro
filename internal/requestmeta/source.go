package requestmeta

import (
	"context"
	"net/netip"
)

type sourceIPKey struct{}

func WithSourceIP(ctx context.Context, address netip.Addr) context.Context {
	return context.WithValue(ctx, sourceIPKey{}, address.Unmap())
}

func SourceIP(ctx context.Context) (netip.Addr, bool) {
	address, ok := ctx.Value(sourceIPKey{}).(netip.Addr)
	return address, ok && address.IsValid()
}
