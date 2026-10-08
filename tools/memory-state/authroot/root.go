package authroot

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// root is ONLY the page experiment's authentication envelope. It omits the
// production Seal's identity, log positions, role, baseline and key lifecycle.
// Consequently verifyRoot grants no runtime/startup authority.
type root struct {
	scope                       uint16
	height                      uint16
	generation, revision, count uint64
	sequence                    uint64
	factHash                    [32]byte
	ref                         pageRef
}

const rootBytes = 8 + 2 + 2 + 2 + 8 + 8 + 8 + refBytes + 8 + sha256.Size + sha256.Size

func signRoot(r root, key []byte) ([]byte, error) {
	if len(key) != 32 || !r.valid() {
		return nil, errCorrupt
	}
	b := append([]byte{}, []byte("HMPROOT3")...)
	b = binary.BigEndian.AppendUint16(b, pageFormat)
	b = binary.BigEndian.AppendUint16(b, r.scope)
	b = binary.BigEndian.AppendUint16(b, r.height)
	b = binary.BigEndian.AppendUint64(b, r.generation)
	b = binary.BigEndian.AppendUint64(b, r.revision)
	b = binary.BigEndian.AppendUint64(b, r.count)
	b = appendRef(b, r.ref)
	b = binary.BigEndian.AppendUint64(b, r.sequence)
	b = append(b, r.factHash[:]...)
	mac := hmac.New(sha256.New, key)
	mac.Write(b)
	return mac.Sum(b), nil
}

func verifyRoot(b, key []byte) (root, error) {
	var r root
	if len(key) != 32 || len(b) != rootBytes || !bytes.Equal(b[:8], []byte("HMPROOT3")) || binary.BigEndian.Uint16(b[8:10]) != pageFormat {
		return r, errCorrupt
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(b[:len(b)-32])
	if !hmac.Equal(mac.Sum(nil), b[len(b)-32:]) {
		return r, errCorrupt
	}
	r.scope = binary.BigEndian.Uint16(b[10:12])
	r.height = binary.BigEndian.Uint16(b[12:14])
	r.generation = binary.BigEndian.Uint64(b[14:22])
	r.revision = binary.BigEndian.Uint64(b[22:30])
	r.count = binary.BigEndian.Uint64(b[30:38])
	r.ref = parseRef(b[38:98])
	r.sequence = binary.BigEndian.Uint64(b[98:106])
	copy(r.factHash[:], b[106:138])
	if !r.valid() {
		return root{}, errCorrupt
	}
	return r, nil
}

// Empty commitments are component-specific and have exactly one representation;
// a missing physical page is never an empty tree. Empty roots require no I/O.
func emptyRef(scope uint16) pageRef {
	b := append([]byte{}, []byte("halro:authroot-experiment:empty:v1")...)
	b = binary.BigEndian.AppendUint16(b, scope)
	return pageRef{digest: sha256.Sum256(b)}
}

func (r root) valid() bool {
	if r.scope == 0 || r.generation == 0 || r.revision == 0 || (r.sequence == 0) != (r.factHash == [32]byte{}) {
		return false
	}
	if r.count == 0 {
		return r.height == 0 && r.ref == emptyRef(r.scope)
	}
	return r.height > 0 && r.height <= maxDepth && r.ref.valid()
}
