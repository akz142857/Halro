package pagedscratchstate

import (
	"bytes"
	"encoding/binary"
)

const (
	degree           = 6
	maxEntries       = 2*degree - 1
	inlineKeyBytes   = 148
	inlineValueBytes = 64
	entryBytes       = 352
)

// Only the fresh owner holds the expected root hash/count. A page holds sorted
// full keys and authenticated child hash/count pairs; no saved root can open it.
type child struct {
	link link
	rows uint64
}
type entry struct {
	key, value       []byte
	keyRef, valueRef blobRef
}
type node struct {
	id           uint64
	level, count int
	rows         uint64
	entries      [maxEntries + 1]entry
	children     [maxEntries + 2]child
}

func decodeBlob(b []byte) blobRef { return blobRef{decodeLink(b), binary.BigEndian.Uint32(b[40:44])} }
func putBlobRef(b []byte, r blobRef) {
	putLink(b[:40], r.link)
	binary.BigEndian.PutUint32(b[40:44], r.size)
}
func decodeChild(b []byte) child { return child{decodeLink(b), binary.BigEndian.Uint64(b[40:48])} }
func putChild(b []byte, c child) {
	putLink(b[:40], c.link)
	binary.BigEndian.PutUint64(b[40:48], c.rows)
}
func (s *Store) readNode(r link) (node, error) {
	raw, err := s.readPage(0, r)
	if err != nil {
		return node{}, err
	}
	if !bytes.Equal(raw[:4], []byte("SPT1")) {
		return node{}, ErrCorrupt
	}
	n := node{id: r.id, level: int(raw[16]), count: int(raw[17]), rows: binary.BigEndian.Uint64(raw[24:32])}
	if n.count > maxEntries || n.level >= maxHeight {
		return node{}, ErrCorrupt
	}
	n.children[0] = decodeChild(raw[32:80])
	for i := 0; i < n.count; i++ {
		b := raw[80+i*entryBytes : 80+(i+1)*entryBytes]
		n.children[i+1] = decodeChild(b[:48])
		e := &n.entries[i]
		e.keyRef, e.valueRef = decodeBlob(b[48:92]), decodeBlob(b[92:136])
		kl, vl := int(binary.BigEndian.Uint16(b[136:138])), int(binary.BigEndian.Uint16(b[138:140]))
		if kl > inlineKeyBytes || vl > inlineValueBytes {
			return node{}, ErrCorrupt
		}
		if kl > 0 {
			if e.keyRef != (blobRef{}) {
				return node{}, ErrCorrupt
			}
			e.key = bytes.Clone(b[140 : 140+kl])
		} else {
			e.key, err = s.readBlob(e.keyRef, MaxKeyBytes)
			if err != nil {
				return node{}, err
			}
		}
		if len(e.key) == 0 || len(e.key) > MaxKeyBytes || i > 0 && bytes.Compare(n.entries[i-1].key, e.key) >= 0 {
			return node{}, ErrCorrupt
		}
		if e.valueRef.size > 0 {
			if vl != 0 || e.valueRef.size > MaxValueBytes || e.valueRef.link.empty() {
				return node{}, ErrCorrupt
			}
		} else {
			if !e.valueRef.link.empty() {
				return node{}, ErrCorrupt
			}
			e.value = bytes.Clone(b[288 : 288+vl])
		}
	}
	if err := s.checkNode(n); err != nil {
		return node{}, err
	}
	return n, nil
}
func (s *Store) checkNode(n node) error {
	if n.count < 0 || n.count > maxEntries || n.level < 0 || n.level >= maxHeight {
		return ErrCorrupt
	}
	rows := uint64(n.count)
	for i := 0; i <= n.count; i++ {
		c := n.children[i]
		if n.level == 0 {
			if c != (child{}) {
				return ErrCorrupt
			}
			continue
		}
		if !c.link.valid() || c.link.empty() || c.link.id == n.id || c.rows == 0 {
			return ErrCorrupt
		}
		for j := 0; j < i; j++ {
			if n.children[j].link.id == c.link.id {
				return ErrCorrupt
			}
		}
		if rows+c.rows < rows {
			return ErrLimit
		}
		rows += c.rows
	}
	if rows != n.rows || rows > uint64(s.ceiling/pageBytes)*maxEntries {
		return ErrCorrupt
	}
	return nil
}
func (s *Store) readChild(n node, i int) (node, error) {
	if n.level == 0 || i < 0 || i > n.count {
		return node{}, ErrCorrupt
	}
	c := n.children[i]
	out, err := s.readNode(c.link)
	if err != nil {
		return node{}, err
	}
	if out.level+1 != n.level || out.rows != c.rows || out.count < degree-1 {
		return node{}, ErrCorrupt
	}
	if i > 0 && bytes.Compare(out.entries[0].key, n.entries[i-1].key) <= 0 || i < n.count && bytes.Compare(out.entries[out.count-1].key, n.entries[i].key) >= 0 {
		return node{}, ErrCorrupt
	}
	return out, nil
}

// Carry the outer path fences too: the first/last child of an internal page
// must not escape a grandparent boundary that has no local separator.
func (s *Store) descend(n node, i int, lo, hi []byte) (node, []byte, []byte, error) {
	if i > 0 {
		lo = n.entries[i-1].key
	}
	if i < n.count {
		hi = n.entries[i].key
	}
	out, err := s.readChild(n, i)
	if err != nil {
		return node{}, nil, nil, err
	}
	if lo != nil && bytes.Compare(out.entries[0].key, lo) <= 0 || hi != nil && bytes.Compare(out.entries[out.count-1].key, hi) >= 0 {
		return node{}, nil, nil, ErrCorrupt
	}
	return out, lo, hi, nil
}
func (s *Store) writeNode(n node) (child, error) {
	n.rows = uint64(n.count)
	for i := 0; i <= n.count; i++ {
		if n.rows+n.children[i].rows < n.rows {
			return child{}, ErrLimit
		}
		n.rows += n.children[i].rows
	}
	if err := s.checkNode(n); err != nil {
		return child{}, err
	}
	var raw [pageBytes - 32]byte
	copy(raw[:], "SPT1")
	raw[16], raw[17] = byte(n.level), byte(n.count)
	binary.BigEndian.PutUint64(raw[24:32], n.rows)
	putChild(raw[32:80], n.children[0])
	for i := 0; i < n.count; i++ {
		e := n.entries[i]
		if len(e.key) == 0 || i > 0 && bytes.Compare(n.entries[i-1].key, e.key) >= 0 {
			return child{}, ErrCorrupt
		}
		b := raw[80+i*entryBytes : 80+(i+1)*entryBytes]
		putChild(b[:48], n.children[i+1])
		putBlobRef(b[48:92], e.keyRef)
		putBlobRef(b[92:136], e.valueRef)
		if e.keyRef.size == 0 {
			if len(e.key) > inlineKeyBytes || !e.keyRef.link.empty() {
				return child{}, ErrCorrupt
			}
			binary.BigEndian.PutUint16(b[136:138], uint16(len(e.key)))
			copy(b[140:288], e.key)
		}
		if e.valueRef.size == 0 {
			if len(e.value) > inlineValueBytes || !e.valueRef.link.empty() {
				return child{}, ErrCorrupt
			}
			binary.BigEndian.PutUint16(b[138:140], uint16(len(e.value)))
			copy(b[288:], e.value)
		}
	}
	r, err := s.writePage(0, n.id, raw[:])
	return child{r, n.rows}, err
}
func search(n node, key []byte) int {
	lo, hi := 0, n.count
	for lo < hi {
		mid := (lo + hi) / 2
		if bytes.Compare(n.entries[mid].key, key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
func (s *Store) value(e entry) ([]byte, error) {
	if e.valueRef.size > 0 {
		return s.readBlob(e.valueRef, MaxValueBytes)
	}
	if len(e.value) == 0 {
		return []byte{}, nil
	}
	return bytes.Clone(e.value), nil
}
func (s *Store) newEntry(key, value []byte) (entry, error) {
	e := entry{key: bytes.Clone(key)}
	var err error
	if len(key) > inlineKeyBytes {
		e.keyRef, err = s.putBlob(key)
		if err != nil {
			return entry{}, err
		}
	}
	if len(value) > inlineValueBytes {
		e.valueRef, err = s.putBlob(value)
	} else {
		e.value = bytes.Clone(value)
	}
	return e, err
}
func (s *Store) freeEntry(e entry) error {
	if err := s.freeBlob(e.keyRef); err != nil {
		return err
	}
	return s.freeBlob(e.valueRef)
}
func (t *Transaction) rootNode() (node, error) {
	n, err := t.store.readNode(t.store.root)
	if err == nil && n.rows != t.store.rows {
		err = ErrCorrupt
	}
	return n, err
}
func (t *Transaction) Get(key []byte) ([]byte, bool, error) {
	if err := t.valid(); err != nil {
		return nil, false, t.fail(err)
	}
	if len(key) == 0 || len(key) > MaxKeyBytes {
		return nil, false, t.fail(ErrLimit)
	}
	if t.store.root.empty() {
		if t.store.rows != 0 {
			return nil, false, t.fail(ErrCorrupt)
		}
		return nil, false, nil
	}
	n, err := t.rootNode()
	if err != nil {
		return nil, false, t.fail(err)
	}
	var lo, hi []byte
	for depth := 0; depth < maxHeight; depth++ {
		i := search(n, key)
		if i < n.count && bytes.Equal(n.entries[i].key, key) {
			value, err := t.store.value(n.entries[i])
			return value, err == nil, t.fail(err)
		}
		if n.level == 0 {
			return nil, false, nil
		}
		n, lo, hi, err = t.store.descend(n, i, lo, hi)
		if err != nil {
			return nil, false, t.fail(err)
		}
	}
	return nil, false, t.fail(ErrLimit)
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
	s := t.store
	if s.operation == ^uint64(0) {
		return t.fail(ErrLimit)
	}
	s.operation++
	var n node
	var err error
	if s.root.empty() {
		if s.rows != 0 {
			return t.fail(ErrCorrupt)
		}
		n.id, err = s.allocate(0)
	} else {
		n, err = t.rootNode()
	}
	if err != nil {
		return t.fail(err)
	}
	c, promoted, right, added, err := s.insert(n, key, value, 0)
	if err != nil {
		return t.fail(err)
	}
	if promoted != nil {
		id, err := s.allocate(0)
		if err != nil {
			return t.fail(err)
		}
		parent := node{id: id, level: n.level + 1, count: 1}
		parent.entries[0] = *promoted
		parent.children[0], parent.children[1] = c, right
		c, err = s.writeNode(parent)
		if err != nil {
			return t.fail(err)
		}
	}
	wanted := s.rows
	if added {
		wanted++
	}
	if c.rows != wanted {
		return t.fail(ErrCorrupt)
	}
	s.root, s.rows = c.link, c.rows
	return nil
}
func (s *Store) insert(n node, key, value []byte, depth int) (child, *entry, child, bool, error) {
	if depth >= maxHeight {
		return child{}, nil, child{}, false, ErrLimit
	}
	i := search(n, key)
	added := false
	if i < n.count && bytes.Equal(n.entries[i].key, key) {
		old := n.entries[i].valueRef
		n.entries[i].valueRef = blobRef{}
		n.entries[i].value = nil
		var err error
		if len(value) > inlineValueBytes {
			n.entries[i].valueRef, err = s.putBlob(value)
		} else {
			n.entries[i].value = bytes.Clone(value)
		}
		if err == nil {
			err = s.freeBlob(old)
		}
		if err != nil {
			return child{}, nil, child{}, false, err
		}
	} else if n.level == 0 {
		e, err := s.newEntry(key, value)
		if err != nil {
			return child{}, nil, child{}, false, err
		}
		copy(n.entries[i+1:], n.entries[i:n.count])
		n.entries[i] = e
		n.count++
		added = true
	} else {
		below, err := s.readChild(n, i)
		if err != nil {
			return child{}, nil, child{}, false, err
		}
		c, promoted, right, wasAdded, err := s.insert(below, key, value, depth+1)
		if err != nil {
			return child{}, nil, child{}, false, err
		}
		added = wasAdded
		n.children[i] = c
		if promoted != nil {
			copy(n.entries[i+1:], n.entries[i:n.count])
			copy(n.children[i+2:], n.children[i+1:n.count+1])
			n.entries[i] = *promoted
			n.children[i+1] = right
			n.count++
		}
	}
	if n.count <= maxEntries {
		c, err := s.writeNode(n)
		return c, nil, child{}, added, err
	}
	median := n.count / 2
	promoted := n.entries[median]
	id, err := s.allocate(0)
	if err != nil {
		return child{}, nil, child{}, false, err
	}
	right := node{id: id, level: n.level, count: n.count - median - 1}
	copy(right.entries[:], n.entries[median+1:n.count])
	copy(right.children[:], n.children[median+1:n.count+1])
	n.count = median
	lc, err := s.writeNode(n)
	if err != nil {
		return child{}, nil, child{}, false, err
	}
	rc, err := s.writeNode(right)
	return lc, &promoted, rc, added, err
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
	s := t.store
	if s.operation == ^uint64(0) {
		return t.fail(ErrLimit)
	}
	s.operation++
	if s.root.empty() {
		if s.rows != 0 {
			return t.fail(ErrCorrupt)
		}
		return nil
	}
	n, err := t.rootNode()
	if err != nil {
		return t.fail(err)
	}
	c, removed, err := s.remove(n, key, 0)
	if err != nil {
		return t.fail(err)
	}
	wanted := s.rows
	if removed {
		if wanted == 0 {
			return t.fail(ErrCorrupt)
		}
		wanted--
	}
	n, err = s.readNode(c.link)
	if err != nil {
		return t.fail(err)
	}
	if n.count == 0 {
		if n.level == 0 {
			c = child{}
		} else {
			c = n.children[0]
		}
		if err = s.release(0, n.id); err != nil {
			return t.fail(err)
		}
	}
	if c.rows != wanted {
		return t.fail(ErrCorrupt)
	}
	s.root, s.rows = c.link, c.rows
	return nil
}
func (s *Store) remove(n node, key []byte, depth int) (child, bool, error) {
	if depth >= maxHeight {
		return child{}, false, ErrLimit
	}
	i := search(n, key)
	if i < n.count && bytes.Equal(n.entries[i].key, key) {
		old := n.entries[i]
		if n.level == 0 {
			copy(n.entries[i:], n.entries[i+1:n.count])
			n.count--
			n.entries[n.count] = entry{}
		} else {
			below, err := s.readChild(n, i)
			if err != nil {
				return child{}, false, err
			}
			c, e, err := s.extractMax(below, depth+1)
			if err != nil {
				return child{}, false, err
			}
			n.entries[i] = e
			n.children[i] = c
			if err = s.rebalance(&n, i); err != nil {
				return child{}, false, err
			}
		}
		if err := s.freeEntry(old); err != nil {
			return child{}, false, err
		}
		c, err := s.writeNode(n)
		return c, true, err
	}
	if n.level == 0 {
		c, err := s.writeNode(n)
		return c, false, err
	}
	below, err := s.readChild(n, i)
	if err != nil {
		return child{}, false, err
	}
	c, removed, err := s.remove(below, key, depth+1)
	if err != nil {
		return child{}, false, err
	}
	n.children[i] = c
	if removed {
		if err = s.rebalance(&n, i); err != nil {
			return child{}, false, err
		}
	}
	c, err = s.writeNode(n)
	return c, removed, err
}
func (s *Store) extractMax(n node, depth int) (child, entry, error) {
	if depth >= maxHeight || n.count == 0 {
		return child{}, entry{}, ErrCorrupt
	}
	var e entry
	if n.level == 0 {
		n.count--
		e = n.entries[n.count]
		n.entries[n.count] = entry{}
	} else {
		below, err := s.readChild(n, n.count)
		if err != nil {
			return child{}, entry{}, err
		}
		c, value, err := s.extractMax(below, depth+1)
		if err != nil {
			return child{}, entry{}, err
		}
		n.children[n.count] = c
		e = value
		if err = s.rebalance(&n, n.count); err != nil {
			return child{}, entry{}, err
		}
	}
	c, err := s.writeNode(n)
	return c, e, err
}

// An underfull child is read only on its original expected hash/count during
// the current mutation. Siblings retain normal minimum occupancy validation.
func (s *Store) underfull(n node, i int) (node, error) {
	c := n.children[i]
	out, err := s.readNode(c.link)
	if err != nil {
		return node{}, err
	}
	if out.level+1 != n.level || out.rows != c.rows || out.count < degree-2 {
		return node{}, ErrCorrupt
	}
	if i > 0 && bytes.Compare(out.entries[0].key, n.entries[i-1].key) <= 0 || i < n.count && bytes.Compare(out.entries[out.count-1].key, n.entries[i].key) >= 0 {
		return node{}, ErrCorrupt
	}
	return out, nil
}
func (s *Store) rebalance(n *node, i int) error {
	cur, err := s.underfull(*n, i)
	if err != nil {
		return err
	}
	if cur.count >= degree-1 {
		return nil
	}
	if i > 0 {
		left, err := s.readChild(*n, i-1)
		if err != nil {
			return err
		}
		if left.count >= degree {
			copy(cur.entries[1:], cur.entries[:cur.count])
			copy(cur.children[1:], cur.children[:cur.count+1])
			cur.entries[0] = n.entries[i-1]
			cur.children[0] = left.children[left.count]
			n.entries[i-1] = left.entries[left.count-1]
			left.entries[left.count-1] = entry{}
			left.children[left.count] = child{}
			left.count--
			cur.count++
			n.children[i-1], err = s.writeNode(left)
			if err != nil {
				return err
			}
			n.children[i], err = s.writeNode(cur)
			return err
		}
	}
	if i < n.count {
		right, err := s.readChild(*n, i+1)
		if err != nil {
			return err
		}
		if right.count >= degree {
			cur.entries[cur.count] = n.entries[i]
			cur.children[cur.count+1] = right.children[0]
			n.entries[i] = right.entries[0]
			cur.count++
			copy(right.entries[:], right.entries[1:right.count])
			copy(right.children[:], right.children[1:right.count+1])
			right.entries[right.count-1] = entry{}
			right.children[right.count] = child{}
			right.count--
			n.children[i], err = s.writeNode(cur)
			if err != nil {
				return err
			}
			n.children[i+1], err = s.writeNode(right)
			return err
		}
	}
	leftIndex := i
	var left, right node
	if i > 0 {
		leftIndex = i - 1
		left, err = s.readChild(*n, i-1)
		right = cur
	} else {
		left = cur
		right, err = s.readChild(*n, i+1)
	}
	if err != nil {
		return err
	}
	pos := left.count
	left.entries[pos] = n.entries[leftIndex]
	copy(left.entries[pos+1:], right.entries[:right.count])
	copy(left.children[pos+1:], right.children[:right.count+1])
	left.count += 1 + right.count
	c, err := s.writeNode(left)
	if err != nil {
		return err
	}
	if err = s.release(0, right.id); err != nil {
		return err
	}
	n.children[leftIndex] = c
	copy(n.entries[leftIndex:], n.entries[leftIndex+1:n.count])
	copy(n.children[leftIndex+1:], n.children[leftIndex+2:n.count+1])
	n.entries[n.count-1] = entry{}
	n.children[n.count] = child{}
	n.count--
	return nil
}
