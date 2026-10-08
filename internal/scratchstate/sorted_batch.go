package scratchstate

import (
	"bytes"
	"sort"
)

// The finite sorted intents are merged into the original authenticated treap.
// Each touched path carries its caller's expected hash. Original presence and
// complete value are checked where the merge reaches that key (or proves its
// absence), including unchanged read intents. No unverified disk index, saved
// scratch root, or unbounded in-memory tree is introduced.
func (c *RowBuffer) key(i int) []byte { return c.entries[i].key[:c.entries[i].keyLen] }
func (c *RowBuffer) checkOriginal(tx *Transaction, i int, n *node) error {
	e := &c.entries[i]
	if (n != nil) != (e.originalLen >= 0) {
		return ErrCorrupt
	}
	if n != nil {
		raw, err := tx.store.nodeValue(*n)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, c.values[i][0][:e.originalLen]) {
			return ErrCorrupt
		}
	}
	return nil
}
func (c *RowBuffer) changed(i int) bool {
	e := &c.entries[i]
	return e.dirty && (e.currentLen != e.originalLen || e.currentLen >= 0 && !bytes.Equal(c.values[i][1][:e.currentLen], c.values[i][0][:e.originalLen]))
}
func (c *RowBuffer) replace(tx *Transaction, i int, n node) (node, error) {
	if !c.changed(i) {
		return n, nil
	}
	if tx.readOnly {
		return node{}, ErrPoisoned
	}
	old := n.valueRef
	n.value = bytes.Clone(c.values[i][1][:c.entries[i].currentLen])
	n.valueRef = blobRef{}
	var err error
	if len(n.value) > inlineValueBytes {
		n.valueRef, err = tx.store.putBlob(n.value)
		if err != nil {
			return node{}, err
		}
	}
	if err := tx.store.freeBlob(old); err != nil {
		return node{}, err
	}
	return n, nil
}
func (s *Store) checkNodeChildren(n node) error {
	l, err := s.childRows(n.left, n, true)
	if err != nil {
		return err
	}
	r, err := s.childRows(n.right, n, false)
	if err != nil {
		return err
	}
	if l+r+1 != n.rows {
		return ErrCorrupt
	}
	return nil
}

// Split rewrites only authenticated ancestors of the pivot. Detached children
// retain their original hash links; equal retains its actual node/blob owner.
func (s *Store) splitBatch(r link, key []byte, depth int) (link, *node, link, error) {
	if depth >= maxHeight {
		return link{}, nil, link{}, ErrLimit
	}
	if r.empty() {
		return link{}, nil, link{}, nil
	}
	n, err := s.readNode(r)
	if err != nil {
		return link{}, nil, link{}, err
	}
	if err := s.checkNodeChildren(n); err != nil {
		return link{}, nil, link{}, err
	}
	switch bytes.Compare(key, n.key) {
	case 0:
		return n.left, &n, n.right, nil
	case -1:
		l, equal, right, err := s.splitBatch(n.left, key, depth+1)
		if err != nil {
			return link{}, nil, link{}, err
		}
		if right != n.left {
			n.left = right
			r, err = s.writeNode(n)
		}
		return l, equal, r, err
	default:
		left, equal, right, err := s.splitBatch(n.right, key, depth+1)
		if err != nil {
			return link{}, nil, link{}, err
		}
		if left != n.right {
			n.right = left
			r, err = s.writeNode(n)
		}
		return r, equal, right, err
	}
}

func (c *RowBuffer) mergeRows(tx *Transaction, r link, order []int, depth int) (link, error) {
	if err := tx.valid(); err != nil {
		return link{}, err
	}
	if len(order) == 0 {
		return r, nil
	}
	if depth >= maxHeight {
		return link{}, ErrLimit
	}
	s := tx.store
	pivot := -1
	priority := ^uint64(0)
	for p, i := range order {
		if c.entries[i].currentLen >= 0 {
			v := s.priority(c.key(i))
			if pivot < 0 || v < priority {
				pivot, priority = p, v
			}
		}
	}
	var n node
	var err error
	if !r.empty() {
		n, err = s.readNode(r)
		if err != nil {
			return link{}, err
		}
		if err := s.checkNodeChildren(n); err != nil {
			return link{}, err
		}
	}
	if !r.empty() && (pivot < 0 || n.priority <= priority) {
		p := sort.Search(len(order), func(p int) bool { return bytes.Compare(c.key(order[p]), n.key) >= 0 })
		equal := p < len(order) && bytes.Equal(c.key(order[p]), n.key)
		tail := p
		if equal {
			tail++
		}
		oldLeft, oldRight := n.left, n.right
		if equal {
			if err := c.checkOriginal(tx, order[p], &n); err != nil {
				return link{}, err
			}
		}
		n.left, err = c.mergeRows(tx, n.left, order[:p], depth+1)
		if err != nil {
			return link{}, err
		}
		n.right, err = c.mergeRows(tx, n.right, order[tail:], depth+1)
		if err != nil {
			return link{}, err
		}
		if equal && c.entries[order[p]].currentLen < 0 {
			if tx.readOnly {
				return link{}, ErrPoisoned
			}
			root, err := s.merge(n.left, n.right, depth+1)
			if err != nil {
				return link{}, err
			}
			if err := s.freeBlob(n.keyRef); err != nil {
				return link{}, err
			}
			if err := s.freeBlob(n.valueRef); err != nil {
				return link{}, err
			}
			if err := s.release(0, n.id); err != nil {
				return link{}, err
			}
			return root, nil
		}
		changed := equal && c.changed(order[p])
		if equal {
			n, err = c.replace(tx, order[p], n)
			if err != nil {
				return link{}, err
			}
		}
		if changed || n.left != oldLeft || n.right != oldRight {
			return s.writeNode(n)
		}
		return r, nil
	}
	if pivot < 0 {
		for _, i := range order {
			if err := c.checkOriginal(tx, i, nil); err != nil {
				return link{}, err
			}
		}
		return r, nil
	}
	i := order[pivot]
	left, equal, right, err := s.splitBatch(r, c.key(i), depth)
	if err != nil {
		return link{}, err
	}
	if err := c.checkOriginal(tx, i, equal); err != nil {
		return link{}, err
	}
	if equal == nil {
		if tx.readOnly {
			return link{}, ErrPoisoned
		}
		n, err = s.createNode(c.key(i), c.values[i][1][:c.entries[i].currentLen])
	} else {
		n, err = c.replace(tx, i, *equal)
	}
	if err != nil {
		return link{}, err
	}
	n.left, err = c.mergeRows(tx, left, order[:pivot], depth+1)
	if err != nil {
		return link{}, err
	}
	n.right, err = c.mergeRows(tx, right, order[pivot+1:], depth+1)
	if err != nil {
		return link{}, err
	}
	return s.writeNode(n)
}
