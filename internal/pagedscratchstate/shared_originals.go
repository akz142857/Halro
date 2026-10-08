package pagedscratchstate

import "bytes"

// Reauthenticate every intent against the same current root before any mutation.
// Sorted intervals descend each relevant child once, carrying ancestor fences.
// No decoded node, value or absence receipt survives this Flush.
func (c *RowBuffer) checkOriginals(tx *Transaction, order []int) error {
	if err := tx.valid(); err != nil {
		return err
	}
	if tx.store.root.empty() {
		if tx.store.rows != 0 {
			return ErrCorrupt
		}
		return c.checkAbsent(order)
	}
	n, err := tx.rootNode()
	if err != nil {
		return err
	}
	return c.checkOriginalNode(tx, n, order, nil, nil, 0)
}
func (c *RowBuffer) checkAbsent(order []int) error {
	for _, i := range order {
		if c.entries[i].originalLen >= 0 {
			return ErrCorrupt
		}
	}
	return nil
}
func (c *RowBuffer) checkOriginalNode(tx *Transaction, n node, order []int, lo, hi []byte, depth int) error {
	if err := tx.valid(); err != nil {
		return err
	}
	if depth >= maxHeight {
		return ErrLimit
	}
	pos := 0
	for slot := 0; slot <= n.count; slot++ {
		start := pos
		for pos < len(order) && (slot == n.count || bytes.Compare(c.entries[order[pos]].key[:c.entries[order[pos]].keyLen], n.entries[slot].key) < 0) {
			pos++
		}
		if start < pos {
			if n.level == 0 {
				if err := c.checkAbsent(order[start:pos]); err != nil {
					return err
				}
			} else {
				child, lower, upper, err := tx.store.descend(n, slot, lo, hi)
				if err != nil {
					return err
				}
				if err := c.checkOriginalNode(tx, child, order[start:pos], lower, upper, depth+1); err != nil {
					return err
				}
			}
		}
		if slot < n.count && pos < len(order) {
			i := order[pos]
			e := &c.entries[i]
			if bytes.Equal(e.key[:e.keyLen], n.entries[slot].key) {
				if e.originalLen < 0 {
					return ErrCorrupt
				}
				value, err := tx.store.value(n.entries[slot])
				if err != nil {
					return err
				}
				if !bytes.Equal(value, c.values[i][0][:e.originalLen]) {
					return ErrCorrupt
				}
				pos++
			}
		}
	}
	if pos != len(order) {
		return ErrCorrupt
	}
	return tx.valid()
}
