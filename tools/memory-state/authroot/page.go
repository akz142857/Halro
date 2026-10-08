package authroot

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
)

const (
	maxPageBytes  = 4096
	maxEntries    = 8
	maxKeyBytes   = 64
	maxValueBytes = 256
	maxDepth      = 16
	maxPageReads  = 1024
	maxRangeRows  = 32
	maxRangeBytes = 8192
	pageFormat    = 2
	leafKind      = 1
	branchKind    = 2
	refBytes      = 60
)

var (
	errCorrupt = errors.New("authenticated page corrupt or unavailable")
	errLimit   = errors.New("authenticated query budget exceeded")
)

type pageRef struct {
	pack   [16]byte
	offset uint64
	length uint32
	digest [32]byte
}

type entry struct{ key, value []byte }
type child struct {
	first, last []byte
	count       uint64
	height      uint16
	ref         pageRef
}
type page struct {
	scope    uint16
	kind     byte
	count    uint64
	entries  []entry
	children []child
}

func (r pageRef) valid() bool {
	return r.length > 0 && r.length <= maxPageBytes && r.offset <= math.MaxInt64-uint64(r.length)
}

func appendRef(b []byte, r pageRef) []byte {
	b = append(b, r.pack[:]...)
	b = binary.BigEndian.AppendUint64(b, r.offset)
	b = binary.BigEndian.AppendUint32(b, r.length)
	return append(b, r.digest[:]...)
}

func parseRef(b []byte) pageRef {
	var r pageRef
	copy(r.pack[:], b[:16])
	r.offset = binary.BigEndian.Uint64(b[16:24])
	r.length = binary.BigEndian.Uint32(b[24:28])
	copy(r.digest[:], b[28:60])
	return r
}

func validKey(k []byte) bool { return len(k) > 0 && len(k) <= maxKeyBytes }
func (p page) bounds() ([]byte, []byte) {
	if p.kind == leafKind {
		return p.entries[0].key, p.entries[len(p.entries)-1].key
	}
	return p.children[0].first, p.children[len(p.children)-1].last
}

func (p page) height() uint16 {
	if p.kind == leafKind {
		return 1
	}
	var height uint16
	for _, c := range p.children {
		if c.height > height {
			height = c.height
		}
	}
	return height + 1
}

func (p page) validate() error {
	if p.scope == 0 {
		return errCorrupt
	}
	switch p.kind {
	case leafKind:
		if len(p.entries) == 0 || len(p.entries) > maxEntries || len(p.children) != 0 || p.count != uint64(len(p.entries)) {
			return errCorrupt
		}
		for i, e := range p.entries {
			if !validKey(e.key) || len(e.value) > maxValueBytes || (i > 0 && bytes.Compare(p.entries[i-1].key, e.key) >= 0) {
				return errCorrupt
			}
		}
	case branchKind:
		if len(p.children) == 0 || len(p.children) > maxEntries || len(p.entries) != 0 {
			return errCorrupt
		}
		var total uint64
		for i, c := range p.children {
			if !validKey(c.first) || !validKey(c.last) || bytes.Compare(c.first, c.last) > 0 || c.count == 0 || c.height == 0 || c.height >= maxDepth || !c.ref.valid() || (i > 0 && bytes.Compare(p.children[i-1].last, c.first) >= 0) || c.count > math.MaxUint64-total {
				return errCorrupt
			}
			total += c.count
		}
		if total != p.count {
			return errCorrupt
		}
	default:
		return errCorrupt
	}
	return nil
}

func encodePage(p page) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	b := make([]byte, 0, maxPageBytes)
	b = append(b, 'H', 'M', 'P', 1, p.kind, 0)
	b = binary.BigEndian.AppendUint16(b, p.scope)
	b = binary.BigEndian.AppendUint16(b, pageFormat)
	b = binary.BigEndian.AppendUint64(b, p.count)
	n := len(p.entries)
	if p.kind == branchKind {
		n = len(p.children)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(n))
	addKey := func(k []byte) { b = append(b, byte(len(k))); b = append(b, k...) }
	for _, e := range p.entries {
		addKey(e.key)
		b = binary.BigEndian.AppendUint16(b, uint16(len(e.value)))
		b = append(b, e.value...)
	}
	for _, c := range p.children {
		addKey(c.first)
		addKey(c.last)
		b = binary.BigEndian.AppendUint64(b, c.count)
		b = binary.BigEndian.AppendUint16(b, c.height)
		b = appendRef(b, c.ref)
	}
	if len(b) > maxPageBytes {
		return nil, errLimit
	}
	return b, nil
}

// decodePage is called only after the caller validates length and digest. It
// also accepts hostile direct input for decoder tests, with limits before every
// allocation and no caller-controlled capacity.
func decodePage(b []byte, scope uint16) (page, error) {
	var p page
	if len(b) < 20 || len(b) > maxPageBytes || !bytes.Equal(b[:4], []byte{'H', 'M', 'P', 1}) || b[5] != 0 || binary.BigEndian.Uint16(b[6:8]) != scope || binary.BigEndian.Uint16(b[8:10]) != pageFormat {
		return p, errCorrupt
	}
	p.kind = b[4]
	p.scope = scope
	p.count = binary.BigEndian.Uint64(b[10:18])
	n := int(binary.BigEndian.Uint16(b[18:20]))
	if n == 0 || n > maxEntries {
		return page{}, errCorrupt
	}
	b = b[20:]
	take := func(n int) ([]byte, error) {
		if n < 0 || n > len(b) {
			return nil, errCorrupt
		}
		v := b[:n]
		b = b[n:]
		return v, nil
	}
	key := func() ([]byte, error) {
		h, e := take(1)
		if e != nil {
			return nil, e
		}
		n := int(h[0])
		if n == 0 || n > maxKeyBytes {
			return nil, errCorrupt
		}
		v, e := take(n)
		return bytes.Clone(v), e
	}
	switch p.kind {
	case leafKind:
		p.entries = make([]entry, 0, n)
		for i := 0; i < n; i++ {
			k, e := key()
			if e != nil {
				return page{}, e
			}
			h, e := take(2)
			if e != nil {
				return page{}, e
			}
			size := int(binary.BigEndian.Uint16(h))
			if size > maxValueBytes {
				return page{}, errCorrupt
			}
			v, e := take(size)
			if e != nil {
				return page{}, e
			}
			p.entries = append(p.entries, entry{k, bytes.Clone(v)})
		}
	case branchKind:
		p.children = make([]child, 0, n)
		for i := 0; i < n; i++ {
			lo, e := key()
			if e != nil {
				return page{}, e
			}
			hi, e := key()
			if e != nil {
				return page{}, e
			}
			h, e := take(10 + refBytes)
			if e != nil {
				return page{}, e
			}
			p.children = append(p.children, child{lo, hi, binary.BigEndian.Uint64(h[:8]), binary.BigEndian.Uint16(h[8:10]), parseRef(h[10:])})
		}
	default:
		return page{}, errCorrupt
	}
	if len(b) != 0 {
		return page{}, errCorrupt
	}
	if err := p.validate(); err != nil {
		return page{}, err
	}
	return p, nil
}

func refFor(pack [16]byte, offset uint64, b []byte) pageRef {
	return pageRef{pack: pack, offset: offset, length: uint32(len(b)), digest: sha256.Sum256(b)}
}
