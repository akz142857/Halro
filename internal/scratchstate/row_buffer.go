package scratchstate

import (
	"bytes"
	"slices"
)

const (
	RowBufferRAMBytes    = 8 << 20
	hotScratchSlots      = 512
	hotScratchKeyBytes   = 256
	hotScratchValueBytes = 4 << 10
)

type hotScratchEntry struct {
	key                     [hotScratchKeyBytes]byte
	keyLen                  int
	originalLen, currentLen int // -1 is authenticated absence, 0 is a present empty value.
	used, dirty             bool
	stamp                   uint64
}

// Only the fresh, unpublished reducer uses this owner-paid workspace. It holds
// At most 512 row intents, never historical versions or an open-time index.
// Reads start from the authenticated scratch transaction; staged values are
// private intents. Sorted batch merge rechecks the complete original value before
// overwriting, and every cursor flushes first. No intent crosses a transaction.
// RowBufferRAMBytes must be acquired before allocating a RowBuffer. Callers
// select mutable namespaces, flush before ordered reads and successful exit,
// and Reset only after their serialized transaction callback has stopped.
type RowBuffer struct {
	entries [hotScratchSlots]hotScratchEntry
	values  [hotScratchSlots][2][hotScratchValueBytes]byte
	clock   uint64
	tx      *Transaction
	order   [hotScratchSlots]int
}

func (c *RowBuffer) Reset() {
	clear(c.entries[:])
	clear(c.values[:])
	c.clock = 0
	c.tx = nil
}
func (c *RowBuffer) bind(tx *Transaction) error {
	if tx == nil {
		return ErrPoisoned
	}
	if err := tx.valid(); err != nil {
		return tx.fail(err)
	}
	if c.tx != nil && c.tx != tx {
		return tx.fail(ErrPoisoned)
	}
	c.tx = tx
	return nil
}
func (c *RowBuffer) Put(tx *Transaction, key, value []byte) error {
	return c.put(tx, key, value, false)
}
func (c *RowBuffer) Delete(tx *Transaction, key []byte) error { return c.put(tx, key, nil, true) }
func (c *RowBuffer) entry(tx *Transaction, key []byte) (int, bool, error) {
	victim := 0
	for i := range c.entries {
		e := &c.entries[i]
		if e.used && bytes.Equal(e.key[:e.keyLen], key) {
			c.clock++
			e.stamp = c.clock
			return i, true, nil
		}
		if c.entries[victim].used && (!e.used || e.stamp < c.entries[victim].stamp) {
			victim = i
		}
	}
	if c.entries[victim].used {
		if err := c.Flush(tx); err != nil {
			return 0, false, err
		}
		victim = 0
	}
	raw, found, err := tx.Get(key)
	if err != nil {
		return 0, false, err
	}
	if len(raw) > hotScratchValueBytes {
		return 0, false, nil
	}
	e := &c.entries[victim]
	*e = hotScratchEntry{keyLen: len(key), originalLen: -1, currentLen: -1, used: true}
	copy(e.key[:], key)
	if found {
		e.originalLen, e.currentLen = len(raw), len(raw)
		copy(c.values[victim][0][:], raw)
		copy(c.values[victim][1][:], raw)
	}
	c.clock++
	e.stamp = c.clock
	return victim, true, nil
}
func (c *RowBuffer) Get(tx *Transaction, key []byte) ([]byte, bool, error) {
	if err := c.bind(tx); err != nil {
		return nil, false, err
	}
	if len(key) > hotScratchKeyBytes {
		return tx.Get(key)
	}
	i, cached, err := c.entry(tx, key)
	if err != nil {
		return nil, false, err
	}
	if !cached {
		return tx.Get(key)
	}
	e := &c.entries[i]
	if e.currentLen < 0 {
		return nil, false, nil
	}
	return bytes.Clone(c.values[i][1][:e.currentLen]), true, nil
}
func (c *RowBuffer) put(tx *Transaction, key, value []byte, deleted bool) error {
	if err := c.bind(tx); err != nil {
		return err
	}
	if tx.readOnly {
		return tx.fail(ErrPoisoned)
	}
	if len(key) > hotScratchKeyBytes {
		if deleted {
			return tx.Delete(key)
		}
		return tx.Put(key, value)
	}
	if len(value) > hotScratchValueBytes {
		if err := c.Flush(tx); err != nil {
			return err
		}
		return tx.Put(key, value)
	}
	i, cached, err := c.entry(tx, key)
	if err != nil {
		return err
	}
	if !cached {
		if deleted {
			return tx.Delete(key)
		}
		return tx.Put(key, value)
	}
	e := &c.entries[i]
	e.currentLen = len(value)
	e.dirty = true
	if deleted {
		e.currentLen = -1
	} else {
		copy(c.values[i][1][:], value)
	}
	return nil
}
func (c *RowBuffer) Flush(tx *Transaction) error {
	if err := c.bind(tx); err != nil {
		return err
	}
	n := 0
	for i := range c.entries {
		if c.entries[i].used {
			c.order[n] = i
			n++
		}
	}
	if n == 0 {
		return nil
	}
	order := c.order[:n]
	slices.SortFunc(order, func(a, b int) int {
		return bytes.Compare(c.entries[a].key[:c.entries[a].keyLen], c.entries[b].key[:c.entries[b].keyLen])
	})
	expected := tx.store.rows
	changed := false
	for _, i := range order {
		e := &c.entries[i]
		changed = changed || c.changed(i)
		if e.originalLen < 0 && e.currentLen >= 0 {
			if expected == ^uint64(0) {
				return tx.fail(ErrLimit)
			}
			expected++
		} else if e.originalLen >= 0 && e.currentLen < 0 {
			if expected == 0 {
				return tx.fail(ErrCorrupt)
			}
			expected--
		}
	}
	if changed {
		if tx.readOnly {
			return tx.fail(ErrPoisoned)
		}
		if err := tx.store.startMutation(); err != nil {
			return tx.fail(err)
		}
	}
	root, err := c.mergeRows(tx, tx.store.root, order, 0)
	if err != nil {
		return tx.fail(err)
	}
	rows := uint64(0)
	if !root.empty() {
		node, err := tx.store.readNode(root)
		if err != nil {
			return tx.fail(err)
		}
		rows = node.rows
	}
	if rows != expected {
		return tx.fail(ErrCorrupt)
	}
	tx.store.root, tx.store.rows = root, rows
	clear(c.entries[:])
	return nil
}
