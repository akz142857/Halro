package scratchstate

import (
	"bytes"
	"encoding/binary"
)

const (
	inlineKeyPrefixBytes = 64
	inlineValueBytes     = 128
	inlineKeyTailOffset  = 268 + inlineValueBytes
	inlineKeyBytes       = inlineKeyPrefixBytes + pageBytes - 32 - inlineKeyTailOffset
)

// A disk treap has fixed-size nodes and an independently randomized priority.
// The explicit height limit is checked on every operation and traversal. Large
// keys/values use authenticated disk blobs rather than widening a node or cache.
type node struct {
	id, priority, rows uint64
	left, right        link
	keyRef, valueRef   blobRef
	key, value         []byte
}

func appendBlob(b []byte, r blobRef) []byte {
	b = appendLink(b, r.link)
	return binary.BigEndian.AppendUint32(b, r.size)
}
func decodeBlob(b []byte) blobRef { return blobRef{decodeLink(b), binary.BigEndian.Uint32(b[40:44])} }
func putBlobRef(b []byte, r blobRef) {
	putLink(b[:40], r.link)
	binary.BigEndian.PutUint32(b[40:44], r.size)
}
func (s *Store) priority(key []byte) uint64 {
	digest := s.keyedMAC([]byte("scratch-priority-v1"), key)
	return binary.BigEndian.Uint64(digest[:])
}
func (s *Store) readNode(r link) (node, error) {
	raw, err := s.readPage(0, r)
	if err != nil {
		return node{}, err
	}
	if !bytes.Equal(raw[:4], []byte("SCT1")) {
		return node{}, ErrCorrupt
	}
	n := node{id: r.id, priority: binary.BigEndian.Uint64(raw[16:24]), rows: binary.BigEndian.Uint64(raw[24:32]), left: decodeLink(raw[32:72]), right: decodeLink(raw[72:112]), keyRef: decodeBlob(raw[112:156]), valueRef: decodeBlob(raw[156:200])}
	kl, vl := int(binary.BigEndian.Uint16(raw[200:202])), int(binary.BigEndian.Uint16(raw[202:204]))
	if kl > inlineKeyBytes || vl > inlineValueBytes || !n.left.valid() || !n.right.valid() || n.left.id == n.id || n.right.id == n.id || n.rows == 0 || n.rows > s.next[0]-1 {
		return node{}, ErrCorrupt
	}
	if kl > 0 {
		if n.keyRef.size != 0 || !n.keyRef.link.empty() {
			return node{}, ErrCorrupt
		}
		n.key = make([]byte, kl)
		copy(n.key, raw[204:204+min(kl, inlineKeyPrefixBytes)])
		if kl > inlineKeyPrefixBytes {
			copy(n.key[inlineKeyPrefixBytes:], raw[inlineKeyTailOffset:inlineKeyTailOffset+kl-inlineKeyPrefixBytes])
		}
	} else {
		n.key, err = s.readBlob(n.keyRef, MaxKeyBytes)
		if err != nil {
			return node{}, err
		}
	}
	if n.valueRef.size != 0 {
		if vl != 0 || n.valueRef.size > MaxValueBytes {
			return node{}, ErrCorrupt
		}
	} else {
		if !n.valueRef.link.empty() {
			return node{}, ErrCorrupt
		}
		n.value = bytes.Clone(raw[268 : 268+vl])
	}
	if len(n.key) == 0 || len(n.key) > MaxKeyBytes || n.priority != s.priority(n.key) {
		return node{}, ErrCorrupt
	}
	return n, nil
}
func (s *Store) childRows(r link, parent node, left bool) (uint64, error) {
	if r.empty() {
		return 0, nil
	}
	n, err := s.readNode(r)
	if err != nil {
		return 0, err
	}
	if n.priority < parent.priority || left && bytes.Compare(n.key, parent.key) >= 0 || !left && bytes.Compare(n.key, parent.key) <= 0 {
		return 0, ErrCorrupt
	}
	return n.rows, nil
}
func (s *Store) writeNode(n node) (link, error) {
	return s.writeNodeWithChild(n, nil)
}
func (s *Store) writeNodeWithChild(n node, checked *childReceipt) (link, error) {
	if checked != nil {
		if checked.source != s || checked.operation == 0 || checked.operation != s.operation || checked.consumed || checked.child.id != checked.root.id || checked.root.empty() || checked.root != n.left && checked.root != n.right {
			return link{}, ErrCorrupt
		}
		checked.consumed = true
	}
	l, err := s.childRowsWithReceipt(n.left, n, true, checked)
	if err != nil {
		return link{}, err
	}
	r, err := s.childRowsWithReceipt(n.right, n, false, checked)
	if err != nil {
		return link{}, err
	}
	if l+r+1 < l || l+r+1 > s.next[0]-1 {
		return link{}, ErrCorrupt
	}
	n.rows = l + r + 1
	// The existing page has 84 authenticated padding bytes after the inline
	// value. Use those for compound keys before allocating a separate blob.
	// Nodes, value capacity, MACs, block bank and workspace remain the same size.
	// Scratch has no reopen API; this layout never becomes a saved-state format.
	var raw [pageBytes - 32]byte
	copy(raw[:], "SCT1")
	binary.BigEndian.PutUint64(raw[16:24], n.priority)
	binary.BigEndian.PutUint64(raw[24:32], n.rows)
	putLink(raw[32:72], n.left)
	putLink(raw[72:112], n.right)
	putBlobRef(raw[112:156], n.keyRef)
	putBlobRef(raw[156:200], n.valueRef)
	if n.keyRef.size == 0 {
		if len(n.key) == 0 || len(n.key) > inlineKeyBytes {
			return link{}, ErrCorrupt
		}
		binary.BigEndian.PutUint16(raw[200:202], uint16(len(n.key)))
	}
	if n.valueRef.size == 0 {
		if len(n.value) > inlineValueBytes {
			return link{}, ErrCorrupt
		}
		binary.BigEndian.PutUint16(raw[202:204], uint16(len(n.value)))
	}
	if n.keyRef.size == 0 {
		copy(raw[204:268], n.key)
		if len(n.key) > inlineKeyPrefixBytes {
			copy(raw[inlineKeyTailOffset:], n.key[inlineKeyPrefixBytes:])
		}
	}
	if n.valueRef.size == 0 {
		copy(raw[268:inlineKeyTailOffset], n.value)
	}
	return s.writePage(0, n.id, raw[:])
}
func (s *Store) createNode(key, value []byte) (node, error) {
	id, err := s.allocate(0)
	if err != nil {
		return node{}, err
	}
	n := node{id: id, priority: s.priority(key), rows: 1, key: bytes.Clone(key), value: bytes.Clone(value)}
	if len(key) > inlineKeyBytes {
		n.keyRef, err = s.putBlob(key)
		if err != nil {
			return node{}, err
		}
	}
	if len(value) > inlineValueBytes {
		n.valueRef, err = s.putBlob(value)
		if err != nil {
			return node{}, err
		}
	}
	return n, nil
}
func (t *Transaction) Get(key []byte) ([]byte, bool, error) {
	if err := t.valid(); err != nil {
		return nil, false, t.fail(err)
	}
	if len(key) == 0 || len(key) > MaxKeyBytes {
		return nil, false, t.fail(ErrLimit)
	}
	r := t.store.root
	for depth := 0; !r.empty(); depth++ {
		if depth >= maxHeight {
			return nil, false, t.fail(ErrLimit)
		}
		n, err := t.store.readNode(r)
		if err != nil {
			return nil, false, t.fail(err)
		}
		c := bytes.Compare(key, n.key)
		if c == 0 {
			value, err := t.store.nodeValue(n)
			return value, err == nil, t.fail(err)
		}
		if c < 0 {
			r = n.left
		} else {
			r = n.right
		}
	}
	return nil, false, nil
}
func (s *Store) nodeValue(n node) ([]byte, error) {
	if n.valueRef.size != 0 {
		return s.readBlob(n.valueRef, MaxValueBytes)
	}
	if len(n.value) == 0 {
		return []byte{}, nil
	}
	return n.value, nil
}
func (t *Transaction) Put(key, value []byte) error {
	if err := t.valid(); err != nil {
		return t.fail(err)
	}
	if t.readOnly {
		return t.fail(ErrPoisoned)
	}
	if len(key) == 0 || len(key) > MaxKeyBytes || len(value) > MaxValueBytes {
		return t.fail(ErrLimit)
	}
	if err := t.store.startMutation(); err != nil {
		return t.fail(err)
	}
	r, added, err := t.store.put(t.store.root, key, value, 0)
	if err != nil {
		return t.fail(err)
	}
	t.store.root = r
	if added {
		t.store.rows++
	}
	return nil
}
func (s *Store) put(r link, key, value []byte, depth int) (link, bool, error) {
	if depth >= maxHeight {
		return link{}, false, ErrLimit
	}
	if r.empty() {
		n, err := s.createNode(key, value)
		if err != nil {
			return link{}, false, err
		}
		r, err = s.writeNode(n)
		return r, true, err
	}
	n, err := s.readNode(r)
	if err != nil {
		return link{}, false, err
	}
	c := bytes.Compare(key, n.key)
	if c == 0 {
		old := n.valueRef
		n.value = bytes.Clone(value)
		n.valueRef = blobRef{}
		if len(value) > inlineValueBytes {
			n.valueRef, err = s.putBlob(value)
			if err != nil {
				return link{}, false, err
			}
		}
		r, err = s.writeNode(n)
		if err == nil {
			err = s.freeBlob(old)
		}
		return r, false, err
	}
	var added bool
	if c < 0 {
		n.left, added, err = s.put(n.left, key, value, depth+1)
	} else {
		n.right, added, err = s.put(n.right, key, value, depth+1)
	}
	if err != nil {
		return link{}, false, err
	}
	childLink := n.left
	if c > 0 {
		childLink = n.right
	}
	checked, err := s.readChild(childLink)
	if err != nil {
		return link{}, false, err
	}
	child := checked.child
	if child.priority < n.priority {
		if c < 0 {
			n.left = child.right
		} else {
			n.right = child.left
		}
		old, err := s.writeNode(n)
		if err != nil {
			return link{}, false, err
		}
		if c < 0 {
			child.right = old
		} else {
			child.left = old
		}
		r, err = s.writeNode(child)
		return r, added, err
	}
	r, err = s.writeNodeWithChild(n, &checked)
	return r, added, err
}
func (t *Transaction) Delete(key []byte) error {
	if err := t.valid(); err != nil {
		return t.fail(err)
	}
	if t.readOnly {
		return t.fail(ErrPoisoned)
	}
	if len(key) == 0 || len(key) > MaxKeyBytes {
		return t.fail(ErrLimit)
	}
	if err := t.store.startMutation(); err != nil {
		return t.fail(err)
	}
	r, removed, err := t.store.remove(t.store.root, key, 0)
	if err != nil {
		return t.fail(err)
	}
	t.store.root = r
	if removed {
		t.store.rows--
	}
	return nil
}
func (s *Store) remove(r link, key []byte, depth int) (link, bool, error) {
	if depth >= maxHeight {
		return link{}, false, ErrLimit
	}
	if r.empty() {
		return r, false, nil
	}
	n, err := s.readNode(r)
	if err != nil {
		return link{}, false, err
	}
	c := bytes.Compare(key, n.key)
	if c == 0 {
		r, err = s.merge(n.left, n.right, depth+1)
		if err == nil {
			err = s.freeBlob(n.keyRef)
		}
		if err == nil {
			err = s.freeBlob(n.valueRef)
		}
		if err == nil {
			err = s.release(0, n.id)
		}
		return r, true, err
	}
	var removed bool
	if c < 0 {
		n.left, removed, err = s.remove(n.left, key, depth+1)
	} else {
		n.right, removed, err = s.remove(n.right, key, depth+1)
	}
	if err != nil || !removed {
		return r, removed, err
	}
	r, err = s.writeNode(n)
	return r, true, err
}
func (s *Store) merge(left, right link, depth int) (link, error) {
	if depth >= maxHeight {
		return link{}, ErrLimit
	}
	if left.empty() {
		return right, nil
	}
	if right.empty() {
		return left, nil
	}
	l, err := s.readNode(left)
	if err != nil {
		return link{}, err
	}
	r, err := s.readNode(right)
	if err != nil {
		return link{}, err
	}
	if bytes.Compare(l.key, r.key) >= 0 {
		return link{}, ErrCorrupt
	}
	if l.priority <= r.priority {
		l.right, err = s.merge(l.right, right, depth+1)
		if err != nil {
			return link{}, err
		}
		return s.writeNode(l)
	}
	r.left, err = s.merge(left, r.left, depth+1)
	if err != nil {
		return link{}, err
	}
	return s.writeNode(r)
}
