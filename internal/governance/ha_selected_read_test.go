package governance

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestHASelectedGovernanceReadScopeRefusesForeignAndIgnoredFailures(t *testing.T) {
	a := authenticatedstate.SealEligibility{Instance: [32]byte{1}, Cluster: [32]byte{2}, Member: [32]byte{3}, Incarnation: [16]byte{4}, Role: authenticatedstate.SealReplica, Term: 7, OrderingIndex: 9, Coverage: authenticatedstate.SealCoverageAuthenticated}
	type provider = func(context.Context, func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error
	for _, name := range []string{"missing", "omitted", "member", "cluster", "instance", "incarnation", "term", "role", "old-index", "coverage", "duplicate", "ignored-visitor", "canceled"} {
		t.Run(name, func(t *testing.T) {
			h := &haSelectedStartupProvenance{role: a.Role, initialAuthority: a}
			var escaped func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error
			if name != "missing" {
				h.withReadAuthority = provider(func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
					escaped = visit
					b := a
					switch name {
					case "omitted":
						return nil
					case "member":
						b.Member[0]++
					case "cluster":
						b.Cluster[0]++
					case "instance":
						b.Instance[0]++
					case "incarnation":
						b.Incarnation[0]++
					case "term":
						b.Term++
					case "role":
						b.Role = authenticatedstate.SealPrimary
					case "old-index":
						b.OrderingIndex--
					case "coverage":
						b.Coverage = 0
					case "canceled":
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
					}
					_ = visit(ctx, b, HASelectedNativePosition{Sequence: 4, Hash: [32]byte{5}})
					if name == "duplicate" {
						_ = visit(ctx, b, HASelectedNativePosition{})
					}
					return nil // an external provider cannot erase a failed visitor.
				})
			}
			calls := 0
			err := h.withReadScope(context.Background(), func(context.Context, HASelectedNativePosition) error {
				calls++
				if name == "ignored-visitor" {
					return ErrUnavailable
				}
				return nil
			})
			if err == nil {
				t.Fatal("ordinary query accepted missing/foreign permission or ignored failure", name)
			}
			if calls > 1 {
				t.Fatal("one query entered twice", calls)
			}
			if escaped != nil && !errors.Is(escaped(context.Background(), a, HASelectedNativePosition{}), ErrUnavailable) {
				t.Fatal("escaped read callback retained permission")
			}
		})
	}
}
