package pagedscratchstate

import "bytes"

const nodeAuthenticationSlots = 1024

type nodeAuthenticationReceipt struct {
	raw   [pageBytes]byte
	link  link
	valid bool
}
type nodeAuthenticationReceipts [nodeAuthenticationSlots]nodeAuthenticationReceipt

func (s *Store) resetNodeReceipts() {
	if s.nodeReceipts != nil {
		for i := range s.nodeReceipts {
			s.nodeReceipts[i].valid = false
		}
	}
}
func (s *Store) invalidateNodeAuthentication(kind int, id uint64) {
	if kind != 0 || s.nodeReceipts == nil {
		return
	}
	receipt := &s.nodeReceipts[id%nodeAuthenticationSlots]
	if receipt.link.id == id {
		receipt.valid = false
	}
}

// This is an exact comparison against a complete previously authenticated page,
// not a cache-hit receipt. Expected hash is checked on each logical reference.
func (s *Store) reuseNodeAuthentication(kind int, expected link, raw []byte) (bool, error) {
	if kind != 0 || s.nodeReceipts == nil || s.blockBank == nil || !s.blockBank.active {
		return false, nil
	}
	receipt := &s.nodeReceipts[expected.id%nodeAuthenticationSlots]
	if !receipt.valid || receipt.link.id != expected.id {
		return false, nil
	}
	if receipt.link.hash != expected.hash || !bytes.Equal(receipt.raw[:], raw) {
		return false, ErrCorrupt
	}
	return true, nil
}
func (s *Store) rememberNodeAuthentication(kind int, expected link, raw []byte) {
	if kind != 0 || s.nodeReceipts == nil || s.blockBank == nil || !s.blockBank.active {
		return
	}
	receipt := &s.nodeReceipts[expected.id%nodeAuthenticationSlots]
	receipt.valid = false
	copy(receipt.raw[:], raw)
	receipt.link = expected
	receipt.valid = true
}
