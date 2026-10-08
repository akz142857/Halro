package scratchstate

import "bytes"

type Row struct{ Key, Value []byte }
type Iterator struct {
	tx       *Transaction
	stack    [maxHeight]link
	depth    int
	previous []byte
	visited  uint64
}

func (t *Transaction) Iterator() (*Iterator, error) {
	if err := t.valid(); err != nil {
		return nil, t.fail(err)
	}
	i := &Iterator{tx: t}
	if err := i.pushLeft(t.store.root); err != nil {
		return nil, t.fail(err)
	}
	return i, nil
}
func (i *Iterator) pushLeft(r link) error {
	for !r.empty() {
		if i.depth == maxHeight {
			return ErrLimit
		}
		n, err := i.tx.store.readNode(r)
		if err != nil {
			return err
		}
		l, err := i.tx.store.childRows(n.left, n, true)
		if err != nil {
			return err
		}
		right, err := i.tx.store.childRows(n.right, n, false)
		if err != nil || l+right+1 != n.rows {
			return ErrCorrupt
		}
		i.stack[i.depth] = r
		i.depth++
		r = n.left
	}
	return nil
}
func (i *Iterator) Next() (Row, bool, error) {
	if err := i.tx.valid(); err != nil {
		return Row{}, false, i.tx.fail(err)
	}
	if i.depth == 0 {
		if i.visited != i.tx.store.rows {
			return Row{}, false, i.tx.fail(ErrCorrupt)
		}
		return Row{}, false, nil
	}
	i.depth--
	r := i.stack[i.depth]
	i.stack[i.depth] = link{}
	n, err := i.tx.store.readNode(r)
	if err != nil {
		return Row{}, false, i.tx.fail(err)
	}
	if i.previous != nil && bytes.Compare(i.previous, n.key) >= 0 || i.visited >= i.tx.store.rows {
		return Row{}, false, i.tx.fail(ErrCorrupt)
	}
	i.previous = bytes.Clone(n.key)
	i.visited++
	if err := i.pushLeft(n.right); err != nil {
		return Row{}, false, i.tx.fail(err)
	}
	value, err := i.tx.store.nodeValue(n)
	return Row{n.key, value}, err == nil, i.tx.fail(err)
}
func (t *Transaction) Seek(key []byte) (Row, bool, error)   { return t.boundary(key, false, false) }
func (t *Transaction) Floor(key []byte) (Row, bool, error)  { return t.boundary(key, true, false) }
func (t *Transaction) Before(key []byte) (Row, bool, error) { return t.boundary(key, true, true) }
func (t *Transaction) After(key []byte) (Row, bool, error)  { return t.boundary(key, false, true) }
func (t *Transaction) boundary(key []byte, floor, strict bool) (Row, bool, error) {
	if err := t.valid(); err != nil {
		return Row{}, false, t.fail(err)
	}
	if len(key) > MaxKeyBytes {
		return Row{}, false, t.fail(ErrLimit)
	}
	r := t.store.root
	var found *node
	for depth := 0; !r.empty(); depth++ {
		if depth >= maxHeight {
			return Row{}, false, t.fail(ErrLimit)
		}
		n, err := t.store.readNode(r)
		if err != nil {
			return Row{}, false, t.fail(err)
		}
		c := bytes.Compare(n.key, key)
		eligible := c > 0 || c == 0 && !strict
		if floor {
			eligible = c < 0 || c == 0 && !strict
		}
		if eligible {
			candidate := n
			found = &candidate
			if floor {
				r = n.right
			} else {
				r = n.left
			}
		} else {
			if floor {
				r = n.left
			} else {
				r = n.right
			}
		}
	}
	if found == nil {
		return Row{}, false, nil
	}
	value, err := t.store.nodeValue(*found)
	return Row{found.key, value}, err == nil, t.fail(err)
}
