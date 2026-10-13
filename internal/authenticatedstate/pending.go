package authenticatedstate

import "bytes"

// pendingBase marks references to nodes and values a transaction has built but
// not yet written. A real reference always lies inside the pack and carries a
// digest (ref.valid), so a pending one can never be mistaken for it (board §57).
const pendingBase = uint64(1) << 62

// pendingFlushBytes bounds a transaction's unwritten nodes and values. Beyond
// it the reachable part is written out early, so large native frames keep a
// fixed RAM footprint within UpdateRAMBytes. A variable only so tests can drive
// partial write-outs.
var pendingFlushBytes = 16 << 20

type pendingEntry struct {
	node  *node  // nil for a value blob
	value []byte // a value blob, including its leading format byte
}

// txPending holds a transaction's unwritten nodes. Nodes replaced by a later
// change in the same transaction are never written: only those still reachable
// from the candidate root are, once, at commit or at the RAM bound.
type txPending struct {
	entries map[uint64]pendingEntry
	next    uint64
	bytes   int
}

func isPending(r ref) bool { return r.offset >= pendingBase }

func (p *txPending) add(e pendingEntry, length uint32, size int) ref {
	if p.entries == nil {
		p.entries = map[uint64]pendingEntry{}
	}
	r := ref{offset: pendingBase + p.next, length: length}
	p.next++
	p.entries[r.offset] = e
	p.bytes += size
	return r
}

func (p *txPending) node(r ref) (node, bool) {
	e, ok := p.entries[r.offset]
	if !ok || e.node == nil {
		return node{}, false
	}
	n := *e.node
	n.children = append([]descriptor(nil), n.children...)
	return n, true
}

func (p *txPending) value(r ref) ([]byte, bool) {
	e, ok := p.entries[r.offset]
	if !ok || e.node != nil || uint32(len(e.value)) != r.length {
		return nil, false
	}
	return bytes.Clone(e.value), true
}

func pendingNodeSize(n node) int {
	size := 64 + len(n.key)
	for _, c := range n.children {
		size += 64 + len(c.first) + len(c.last)
	}
	return size
}

// flushPending writes every pending node still reachable from the candidate
// root, children before parents, each encoded at its real offset, and drops
// the rest. Callers invoke it only between complete changes, when the
// candidate root is the sole holder of new nodes.
func (t *Transaction) flushPending() error {
	if len(t.pending.entries) == 0 {
		return nil
	}
	if t.candidate.count != 0 && isPending(t.candidate.root) {
		d, err := t.flushDescriptor(descriptor{count: t.candidate.count, height: t.candidate.height, ref: t.candidate.root})
		if err != nil {
			return err
		}
		t.candidate.root = d.ref
	}
	t.pending = txPending{next: t.pending.next}
	return nil
}

func (t *Transaction) flushDescriptor(d descriptor) (descriptor, error) {
	if !isPending(d.ref) {
		return d, nil
	}
	n, ok := t.pending.node(d.ref)
	if !ok {
		return descriptor{}, ErrCorrupt
	}
	if n.height == 1 {
		if isPending(n.value) {
			blob, ok := t.pending.value(n.value)
			if !ok {
				return descriptor{}, ErrCorrupt
			}
			v, err := t.append(blob)
			if err != nil {
				return descriptor{}, err
			}
			n.value = v
		}
	} else {
		for i := range n.children {
			c, err := t.flushDescriptor(n.children[i])
			if err != nil {
				return descriptor{}, err
			}
			n.children[i] = c
		}
	}
	raw, err := encodeNode(n, t.candidate.extent)
	if err != nil {
		return descriptor{}, err
	}
	r, err := t.append(raw)
	if err != nil {
		return descriptor{}, err
	}
	t.nodes++
	d.ref = r
	return d, nil
}

// boundPending writes out early once unwritten work exceeds its RAM bound.
func (t *Transaction) boundPending() error {
	if t.pending.bytes <= pendingFlushBytes {
		return nil
	}
	return t.flushPending()
}
