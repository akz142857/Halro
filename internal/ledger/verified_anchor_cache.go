package ledger

import (
	"sync"
	"sync/atomic"
)

// verifiedAnchorCache remembers the furthest native position this process has
// proven on the authenticated chain with a VerifyAnchors replay from a given
// root (board §51). A later verification from the same root may resume there
// instead of replaying everything since the root. It holds no consumer-supplied
// value: only a head a successful replay actually reached is remembered.
//
// Only routine sources (verifying from the startup cut) use it. A
// complete-history source deliberately re-authenticates the whole saved prefix
// on every call, so later corruption of an already audited frame is still
// found; it neither reads nor feeds the cache.
//
// The cache lives with the Log object, so a reopen starts empty. It is used only
// within the active generation, which compaction never rewrites; the first
// verification after a Roll replays from the root again.
type verifiedAnchorCache struct {
	mu       sync.Mutex
	valid    bool
	root     ReplayAnchor
	tip      ReplayAnchor
	proven   [2]ReplayAnchor // the anchors the remembering verification proved
	replayed atomic.Int64    // records visited by VerifyAnchors replays; observed by tests
}

// resume returns where a verification from root may start and which anchors
// are already proven. It resumes at the remembered tip only when that tip was
// proven from the same root by a routine source and lies in the active
// generation, and each anchor either equals one the remembering verification
// proved or does not precede the tip (so the replay itself must reach it).
// Otherwise it returns root and nothing proven.
func (c *verifiedAnchorCache) resume(root ReplayAnchor, complete bool, active uint64, anchors [2]ReplayAnchor) (ReplayAnchor, [2]bool) {
	if complete {
		return root, [2]bool{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || c.root != root || c.tip.Head.Generation != active {
		return root, [2]bool{}
	}
	var seen [2]bool
	for i, a := range anchors {
		seen[i] = a == c.proven[0] || a == c.proven[1] || a == c.tip
		if !seen[i] && projectionPrefixBefore(a.Head, c.tip.Head) {
			return root, [2]bool{}
		}
	}
	return c.tip, seen
}

func (c *verifiedAnchorCache) remember(root ReplayAnchor, complete bool, tip ReplayAnchor, proven [2]ReplayAnchor) {
	if complete {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.valid, c.root, c.tip, c.proven = true, root, tip, proven
}

func (c *verifiedAnchorCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.valid = false
}

// VerifiedAnchorReplays counts the native records VerifyAnchors replays have
// visited on this Log. It is an observation for tests and diagnostics.
func (l *Log) VerifiedAnchorReplays() int64 { return l.anchors.replayed.Load() }
