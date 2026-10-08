package pagedscratchstate

import "bytes"

type Row struct{ Key, Value []byte }
type frame struct {
	node  node
	index int
}
type Iterator struct {
	tx                 *Transaction
	stack              [maxHeight]frame
	depth              int
	previous           []byte
	visited, operation uint64
}

func (t *Transaction) Iterator() (*Iterator, error) {
	if err := t.valid(); err != nil {
		return nil, t.fail(err)
	}
	i := &Iterator{tx: t, operation: t.store.operation}
	if t.store.root.empty() {
		if t.store.rows != 0 {
			return nil, t.fail(ErrCorrupt)
		}
		return i, nil
	}
	n, err := t.rootNode()
	if err == nil {
		err = i.pushLeft(n)
	}
	if err != nil {
		return nil, t.fail(err)
	}
	return i, nil
}
func (i *Iterator) pushLeft(n node) error {
	for {
		if i.depth >= maxHeight {
			return ErrLimit
		}
		i.stack[i.depth] = frame{node: n}
		i.depth++
		if n.level == 0 {
			return nil
		}
		below, err := i.tx.store.readChild(n, 0)
		if err != nil {
			return err
		}
		n = below
	}
}
func (i *Iterator) Next() (Row, bool, error) {
	if err := i.tx.valid(); err != nil {
		return Row{}, false, i.tx.fail(err)
	}
	if i.operation != i.tx.store.operation {
		return Row{}, false, i.tx.fail(ErrPoisoned)
	}
	for i.depth > 0 {
		f := &i.stack[i.depth-1]
		if f.index == f.node.count {
			*f = frame{}
			i.depth--
			continue
		}
		e := f.node.entries[f.index]
		f.index++
		if i.previous != nil && bytes.Compare(i.previous, e.key) >= 0 || i.visited >= i.tx.store.rows {
			return Row{}, false, i.tx.fail(ErrCorrupt)
		}
		value, err := i.tx.store.value(e)
		if err != nil {
			return Row{}, false, i.tx.fail(err)
		}
		i.previous = bytes.Clone(e.key)
		i.visited++
		if f.node.level > 0 {
			below, err := i.tx.store.readChild(f.node, f.index)
			if err != nil {
				return Row{}, false, i.tx.fail(err)
			}
			if err = i.pushLeft(below); err != nil {
				return Row{}, false, i.tx.fail(err)
			}
		}
		return Row{bytes.Clone(e.key), value}, true, nil
	}
	if i.visited != i.tx.store.rows {
		return Row{}, false, i.tx.fail(ErrCorrupt)
	}
	return Row{}, false, nil
}
func (t *Transaction) Seek(key []byte) (Row, bool, error)   { return t.boundary(key, false, false) }
func (t *Transaction) Floor(key []byte) (Row, bool, error)  { return t.boundary(key, true, false) }
func (t *Transaction) Before(key []byte) (Row, bool, error) { return t.boundary(key, true, true) }
func (t *Transaction) After(key []byte) (Row, bool, error)  { return t.boundary(key, false, true) }
func (t *Transaction) boundary(key []byte, reverse, strict bool) (Row, bool, error) {
	if err := t.valid(); err != nil {
		return Row{}, false, t.fail(err)
	}
	if len(key) > MaxKeyBytes {
		return Row{}, false, t.fail(ErrLimit)
	}
	if t.store.root.empty() {
		if t.store.rows != 0 {
			return Row{}, false, t.fail(ErrCorrupt)
		}
		return Row{}, false, nil
	}
	n, err := t.rootNode()
	if err != nil {
		return Row{}, false, t.fail(err)
	}
	var candidate *entry
	var lo, hi []byte
	for depth := 0; depth < maxHeight; depth++ {
		i := search(n, key)
		equal := i < n.count && bytes.Equal(n.entries[i].key, key)
		if equal && !strict {
			e := n.entries[i]
			candidate = &e
			break
		}
		childIndex := i
		if reverse {
			if equal && !strict {
				childIndex = i + 1
			}
			if childIndex > 0 {
				e := n.entries[childIndex-1]
				candidate = &e
			}
		} else {
			if equal {
				childIndex = i + 1
			}
			if childIndex < n.count {
				e := n.entries[childIndex]
				candidate = &e
			}
		}
		if n.level == 0 {
			break
		}
		n, lo, hi, err = t.store.descend(n, childIndex, lo, hi)
		if err != nil {
			return Row{}, false, t.fail(err)
		}
	}
	if candidate == nil {
		return Row{}, false, nil
	}
	v, err := t.store.value(*candidate)
	return Row{bytes.Clone(candidate.key), v}, err == nil, t.fail(err)
}
