package ledger

import (
	"context"
	"errors"
	"testing"
)

func TestVerifiedAnchorCacheResumesOnlyFromItsOwnRootAndGeneration(t *testing.T) {
	root := ReplayAnchor{Head: Watermark{Generation: 2, Offset: 100, Sequence: 10}, Hash: [32]byte{1}, Epoch: 3}
	tip := ReplayAnchor{Head: Watermark{Generation: 2, Offset: 400, Sequence: 40}, Hash: [32]byte{2}, Epoch: 3}
	later := ReplayAnchor{Head: Watermark{Generation: 2, Offset: 500, Sequence: 50}}
	var c verifiedAnchorCache
	if got, _ := c.resume(root, false, 2, [2]ReplayAnchor{tip, later}); got != root {
		t.Fatal("empty cache resumed", got)
	}
	c.remember(root, false, tip, [2]ReplayAnchor{root, tip})
	if got, _ := c.resume(root, false, 2, [2]ReplayAnchor{tip, later}); got != tip {
		t.Fatal("proven tip not resumed", got)
	}
	earlier := ReplayAnchor{Head: Watermark{Generation: 2, Offset: 300, Sequence: 30}}
	other := ReplayAnchor{Head: Watermark{Generation: 2, Offset: 0, Sequence: 0}}
	resumed := func(r ReplayAnchor, complete bool, active uint64, anchors [2]ReplayAnchor) ReplayAnchor {
		got, _ := c.resume(r, complete, active, anchors)
		return got
	}
	for name, got := range map[string]ReplayAnchor{
		"unproven anchor before tip": resumed(root, false, 2, [2]ReplayAnchor{earlier, later}),
		"other root":                 resumed(other, false, 2, [2]ReplayAnchor{tip, later}),
		"after roll":                 resumed(root, false, 3, [2]ReplayAnchor{tip, later}),
		"complete history":           resumed(root, true, 2, [2]ReplayAnchor{tip, later}),
	} {
		if got == tip {
			t.Fatal(name, "resumed from the cached tip")
		}
	}
	// An anchor the remembering call proved counts as seen even before the tip.
	if got, seen := c.resume(root, false, 2, [2]ReplayAnchor{root, later}); got != tip || !seen[0] || seen[1] {
		t.Fatal("previously proven anchor not carried", got, seen)
	}
	c.remember(root, true, later, [2]ReplayAnchor{later, later})
	if got, _ := c.resume(root, false, 2, [2]ReplayAnchor{later, later}); got != tip {
		t.Fatal("complete-history source fed the cache", got)
	}
	c.forget()
	if got, _ := c.resume(root, false, 2, [2]ReplayAnchor{tip, later}); got != root {
		t.Fatal("forgotten cache resumed", got)
	}
}

func TestVerifyAnchorsStillRejectsForgedAnchorsAfterACachedVerification(t *testing.T) {
	f := newSelectedStartupFixture(t, false)
	f.reopen(t)
	lease, err := f.native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := f.ledgerOptions
	prefix := ProjectionPrefix{Head: o.SavedAnchor.Head, Hash: o.SavedAnchor.Hash}
	o.Prefix = &prefix
	p, _, err := OpenSelectedAuthenticatedLedgerProjection(context.Background(), f.native, f.ledgerDirectory, o, SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.native.AuditSelectedStartup(context.Background(), o.Budget); err != nil {
		t.Fatal(err)
	}
	ref, err := p.ReadUsageSourceReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	verify := func(anchors func(AuthenticatedUsageSource) [2]ReplayAnchor) error {
		return p.WithFrozenUsageSource(context.Background(), f.native, o.Budget, ref.Scope, func(ctx context.Context, s AuthenticatedUsageSource) error {
			return s.VerifyAnchors(ctx, anchors(s))
		})
	}
	genuine := func(s AuthenticatedUsageSource) [2]ReplayAnchor { return [2]ReplayAnchor{ref.Anchor, s.NativeHead()} }
	if err := verify(genuine); err != nil {
		t.Fatal("genuine anchors", err)
	}
	f.native.anchors.mu.Lock()
	cached, tip := f.native.anchors.valid, f.native.anchors.tip
	f.native.anchors.mu.Unlock()
	if !cached {
		t.Fatal("successful routine verification did not remember its tip")
	}
	forgedHead := tip
	forgedHead.Hash[0] ^= 1
	if err := verify(func(AuthenticatedUsageSource) [2]ReplayAnchor { return [2]ReplayAnchor{tip, forgedHead} }); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("forged anchor at the cached tip accepted", err)
	}
	f.native.anchors.mu.Lock()
	cached = f.native.anchors.valid
	f.native.anchors.mu.Unlock()
	if cached {
		t.Fatal("failed verification kept the cache")
	}
	forgedEarlier := ref.Anchor
	forgedEarlier.Hash[0] ^= 1
	if err := verify(func(s AuthenticatedUsageSource) [2]ReplayAnchor {
		return [2]ReplayAnchor{forgedEarlier, s.NativeHead()}
	}); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("forged anchor before the tip accepted", err)
	}
	before := f.native.VerifiedAnchorReplays()
	if err := verify(genuine); err != nil {
		t.Fatal("genuine anchors after refusals", err)
	}
	if f.native.VerifiedAnchorReplays() == before && ref.Anchor.Head != tip.Head {
		t.Fatal("verification after a refusal did not replay from its root")
	}
}
