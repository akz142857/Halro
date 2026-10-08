// Package authenticatedstate stores immutable ordered snapshot pages. A verified
// commitment proves values and the complete key set, but grants no role, native
// resume, metadata baseline or business readiness authority.
package authenticatedstate

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
)

const (
	MaxKeyBytes         = 32769 // A bucket ordinal plus the complete bbolt key.
	MaxValueBytes       = 3 << 20
	maxNodeBytes        = 512 << 10
	fanout              = 4
	maxHeight           = 32
	headerBytes         = 160
	refBytes            = 44
	BuildRAMBytes int64 = 32 << 20
	ReadRAMBytes  int64 = 32 << 20
)

var (
	ErrCorrupt = errors.New("authenticated snapshot corrupt or unavailable")
	ErrLimit   = errors.New("authenticated snapshot limit exceeded")
	ErrBusy    = errors.New("authenticated snapshot reader busy")
	ErrClosed  = errors.New("authenticated snapshot reader closed")
	ErrCursor  = errors.New("authenticated snapshot cursor does not match root and range")
)

type ref struct {
	offset uint64
	length uint32
	digest [32]byte
}
type descriptor struct {
	first, last []byte
	count       uint64
	height      uint16
	ref         ref
}
type node struct {
	key      []byte
	value    ref
	children []descriptor
	count    uint64
	height   uint16
}
type Row struct{ Key, Value []byte }

func validKey(k []byte) bool { return len(k) > 0 && len(k) <= MaxKeyBytes }
func (r ref) valid(extent uint64, max uint32) bool {
	return r.offset >= headerBytes && r.length > 0 && r.length <= max && r.offset <= math.MaxInt64-uint64(r.length) && r.offset+uint64(r.length) <= extent && r.digest != ([32]byte{})
}
func putRef(b []byte, r ref) []byte {
	b = binary.BigEndian.AppendUint64(b, r.offset)
	b = binary.BigEndian.AppendUint32(b, r.length)
	return append(b, r.digest[:]...)
}
func getRef(b []byte) ref {
	r := ref{offset: binary.BigEndian.Uint64(b), length: binary.BigEndian.Uint32(b[8:])}
	copy(r.digest[:], b[12:44])
	return r
}
func putKey(b, k []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(k)))
	return append(b, k...)
}
func (n node) bounds() ([]byte, []byte) {
	if n.height == 1 {
		return n.key, n.key
	}
	return n.children[0].first, n.children[len(n.children)-1].last
}
func (n node) validate(extent uint64) error {
	if n.height == 1 {
		if !validKey(n.key) || n.count != 1 || len(n.children) != 0 || !n.value.valid(extent, MaxValueBytes+1) {
			return ErrCorrupt
		}
		return nil
	}
	if n.height < 2 || n.height > maxHeight || n.key != nil || n.value != (ref{}) || len(n.children) == 0 || len(n.children) > fanout {
		return ErrCorrupt
	}
	var total uint64
	var highest uint16
	for i, c := range n.children {
		if !validKey(c.first) || !validKey(c.last) || bytes.Compare(c.first, c.last) > 0 || c.count == 0 || c.height == 0 || c.height >= n.height || !c.ref.valid(extent, maxNodeBytes) || (i > 0 && bytes.Compare(n.children[i-1].last, c.first) >= 0) || c.count > math.MaxUint64-total {
			return ErrCorrupt
		}
		total += c.count
		if c.height > highest {
			highest = c.height
		}
	}
	if total != n.count || highest+1 != n.height {
		return ErrCorrupt
	}
	return nil
}
func encodeNode(n node, extent uint64) ([]byte, error) {
	if err := n.validate(extent); err != nil {
		return nil, err
	}
	b := []byte{'H', 'S', 'N', 1}
	b = binary.BigEndian.AppendUint16(b, n.height)
	b = binary.BigEndian.AppendUint64(b, n.count)
	if n.height == 1 {
		b = putKey(b, n.key)
		b = putRef(b, n.value)
	} else {
		b = append(b, byte(len(n.children)))
		for _, c := range n.children {
			b = putKey(b, c.first)
			b = putKey(b, c.last)
			b = binary.BigEndian.AppendUint64(b, c.count)
			b = binary.BigEndian.AppendUint16(b, c.height)
			b = putRef(b, c.ref)
		}
	}
	if len(b) > maxNodeBytes {
		return nil, ErrLimit
	}
	return b, nil
}
func decodeNode(b []byte, extent uint64) (node, error) {
	var n node
	if len(b) < 14 || len(b) > maxNodeBytes || !bytes.Equal(b[:4], []byte{'H', 'S', 'N', 1}) {
		return n, ErrCorrupt
	}
	n.height = binary.BigEndian.Uint16(b[4:])
	n.count = binary.BigEndian.Uint64(b[6:])
	b = b[14:]
	take := func(size int) ([]byte, error) {
		if size < 0 || size > len(b) {
			return nil, ErrCorrupt
		}
		v := b[:size]
		b = b[size:]
		return v, nil
	}
	key := func() ([]byte, error) {
		h, e := take(2)
		if e != nil {
			return nil, e
		}
		size := int(binary.BigEndian.Uint16(h))
		if size == 0 || size > MaxKeyBytes {
			return nil, ErrCorrupt
		}
		v, e := take(size)
		return bytes.Clone(v), e
	}
	if n.height == 1 {
		k, e := key()
		if e != nil {
			return node{}, e
		}
		v, e := take(refBytes)
		if e != nil {
			return node{}, e
		}
		n.key = k
		n.value = getRef(v)
	} else {
		h, e := take(1)
		if e != nil || h[0] == 0 || h[0] > fanout {
			return node{}, ErrCorrupt
		}
		n.children = make([]descriptor, 0, int(h[0]))
		for i := 0; i < int(h[0]); i++ {
			lo, e := key()
			if e != nil {
				return node{}, e
			}
			hi, e := key()
			if e != nil {
				return node{}, e
			}
			v, e := take(10 + refBytes)
			if e != nil {
				return node{}, e
			}
			n.children = append(n.children, descriptor{lo, hi, binary.BigEndian.Uint64(v), binary.BigEndian.Uint16(v[8:]), getRef(v[10:])})
		}
	}
	if len(b) != 0 {
		return node{}, ErrCorrupt
	}
	if e := n.validate(extent); e != nil {
		return node{}, e
	}
	return n, nil
}
func emptyDigest(context [32]byte) [32]byte {
	return sha256.Sum256(append([]byte("halro:authenticated-state:empty:v1\x00"), context[:]...))
}
