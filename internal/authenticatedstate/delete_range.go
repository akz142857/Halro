package authenticatedstate

import "bytes"

func outsideDeleteRange(first, last, lo, hi []byte) bool {
	return len(lo) != 0 && bytes.Compare(last, lo) < 0 || len(hi) != 0 && bytes.Compare(first, hi) >= 0
}

func coveredDeleteRange(first, last, lo, hi []byte) bool {
	return (len(lo) == 0 || bytes.Compare(first, lo) >= 0) && (len(hi) == 0 || bytes.Compare(last, hi) < 0)
}

// pruneRange authenticates the boundary path. Fully covered child descriptors
// are counts and bounds committed by the loaded parent, not ordinary DB hints.
// Their descendants and values are deliberately not read: deletion asserts no
// surviving value from them and never signs replacement content for them.
func (t *Transaction) pruneRange(q *query, d descriptor, lo, hi []byte) (descriptor, uint64, error) {
	n, err := q.load(d)
	if err != nil {
		return descriptor{}, 0, err
	}
	d.first, d.last = n.bounds()
	if outsideDeleteRange(d.first, d.last, lo, hi) {
		return d, 0, nil
	}
	if coveredDeleteRange(d.first, d.last, lo, hi) {
		return descriptor{}, n.count, nil
	}
	if n.height == 1 {
		return descriptor{}, 0, ErrCorrupt // a leaf is either outside or covered
	}
	children := make([]descriptor, 0, fanout)
	var removed uint64
	for _, child := range n.children {
		if err := t.ctx.Err(); err != nil {
			return descriptor{}, 0, err
		}
		var dropped uint64
		switch {
		case outsideDeleteRange(child.first, child.last, lo, hi):
			children = append(children, child)
		case coveredDeleteRange(child.first, child.last, lo, hi):
			dropped = child.count
		default:
			next, count, err := t.pruneRange(q, child, lo, hi)
			if err != nil {
				return descriptor{}, 0, err
			}
			dropped = count
			if next.count != 0 {
				children = append(children, next)
			}
		}
		if dropped > n.count-removed {
			return descriptor{}, 0, ErrCorrupt
		}
		removed += dropped
	}
	if removed == 0 {
		return d, 0, nil
	}
	if len(children) == 0 {
		return descriptor{}, removed, nil
	}
	// Unary internal branches retain their level; only the final root collapses.
	for i := range children {
		for children[i].height < n.height-1 {
			c := children[i]
			padded, err := t.writeNode(node{children: []descriptor{c}, count: c.count, height: c.height + 1})
			if err != nil {
				return descriptor{}, 0, err
			}
			children[i] = padded
		}
	}
	next, err := t.branch(children)
	if err != nil {
		return descriptor{}, 0, err
	}
	if next.count != n.count-removed || next.height != n.height {
		return descriptor{}, 0, ErrCorrupt
	}
	return next, removed, nil
}

// DeleteRange removes [lo, hi) in one callback-scoped mutation. Empty endpoints
// are unbounded. Work depends on authenticated boundary height, not the number
// of removed rows; values in discarded subtrees are not audited. Callers must
// independently authorize the range (for example, a complete native bucket
// deletion). This supplies no source, role, or journal resume authority.
func (t *Transaction) DeleteRange(lo, hi []byte) (removed uint64, resultErr error) {
	if err := t.enter(); err != nil {
		return 0, err
	}
	defer t.mu.Unlock()
	defer func() {
		if resultErr != nil {
			t.fail(resultErr)
			removed = 0
		}
	}()
	if len(lo) > MaxKeyBytes || len(hi) > MaxKeyBytes || len(lo) != 0 && len(hi) != 0 && bytes.Compare(lo, hi) >= 0 || t.writes >= t.limits.mutations || len(lo)+len(hi) > t.limits.bytes-t.inputBytes {
		return 0, ErrLimit
	}
	t.writes++
	t.inputBytes += len(lo) + len(hi)
	lo, hi = bytes.Clone(lo), bytes.Clone(hi)
	if t.candidate.count == 0 {
		return 0, nil
	}
	q := t.query()
	defer func() { t.reads = q.reads }()
	root, removed, err := t.pruneRange(q, descriptor{count: t.candidate.count, height: t.candidate.height, ref: t.candidate.root}, lo, hi)
	if err != nil || removed == 0 {
		return 0, err
	}
	if root.count == 0 {
		t.candidate.count, t.candidate.height = 0, 0
		t.candidate.root = ref{digest: emptyDigest(t.candidate.context)}
		return removed, nil
	}
	for root.height > 1 {
		q.snapshot.current = t.candidate
		n, err := q.load(root)
		if err != nil {
			return 0, err
		}
		if len(n.children) != 1 {
			break
		}
		root = n.children[0]
	}
	t.candidate.count, t.candidate.height, t.candidate.root = root.count, root.height, root.ref
	return removed, t.boundPending()
}
