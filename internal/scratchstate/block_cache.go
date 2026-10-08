package scratchstate

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
)

const (
	blockBankSlots = 8192
	blockBankWays  = 4
	blockBankBytes = 4096
)

// A bank is confined to one serialized transaction on the original Store FDs.
// It contains raw bytes, never a trusted node. Default reads verify MAC, hash,
// kind and ID on every hit; optional reuse compares a separately authenticated
// full-page copy and the exact expected hash within the same transaction.
// Existing node overwrites may be combined only within this transaction.
// First mutation, eviction and successful exit compare the actual old FD
// prefix; the complete dirty set is written and synced before successful exit. No bank survives a transaction or becomes saved-state authority.
type transactionBlockBank struct {
	blocks [blockBankSlots]transactionBlock
	check  [blockBankBytes]byte
	active bool
	wrote  bool
}

type transactionBlock struct {
	raw      [blockBankBytes]byte
	index    uint64
	length   uint16
	hits     uint16
	kind     uint8
	valid    bool
	dirty    uint8
	original [32]byte
}

func (b *transactionBlockBank) begin() {
	for i := range b.blocks {
		b.blocks[i].valid = false
	}
	b.active = true
	b.wrote = false
}
func (b *transactionBlockBank) end() { b.active = false }

func (s *Store) cachedBlock(kind int, id uint64) (*transactionBlock, int) {
	index := id / (blockBankBytes / pageBytes)
	// Four-way sets avoid letting one cold block repeatedly expel a hot root.
	// Frequencies saturate, remain transaction-local, and occupy existing
	// fixed metadata padding. Each file still has exactly half the bank.
	base := int((index%(blockBankSlots/blockBankWays/2)*2 + uint64(kind)) * blockBankWays)
	blocks := s.blockBank.blocks[base : base+blockBankWays]
	for i := range blocks {
		if blocks[i].valid && blocks[i].kind == uint8(kind) && blocks[i].index == index {
			return &blocks[i], int(id%(blockBankBytes/pageBytes)) * pageBytes
		}
	}
	victim := &blocks[0]
	for i := range blocks {
		if !blocks[i].valid {
			victim = &blocks[i]
			break
		}
		if blocks[i].hits < victim.hits {
			victim = &blocks[i]
		}
	}
	return victim, int(id%(blockBankBytes/pageBytes)) * pageBytes
}

func (s *Store) verifyBlock(block *transactionBlock) error {
	if !block.valid {
		return nil
	}
	if block.kind > 1 || block.length == 0 || int(block.length) > blockBankBytes {
		return ErrCorrupt
	}
	actual := s.blockBank.check[:block.length]
	if _, err := s.files[block.kind].ReadAt(actual, int64(block.index)*blockBankBytes); err != nil {
		return errors.Join(ErrCorrupt, err)
	}
	if block.dirty != 0 {
		if sha256.Sum256(actual) != block.original {
			return ErrCorrupt
		}
		return nil
	}
	if !bytes.Equal(actual, block.raw[:block.length]) {
		return ErrCorrupt
	}
	return nil
}

func (s *Store) verifyBlockBank() error {
	if s.blockBank == nil || !s.blockBank.active {
		return nil
	}
	for i := range s.blockBank.blocks {
		if err := s.verifyBlock(&s.blockBank.blocks[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readPageBytes(kind int, id uint64, out []byte) (int, error) {
	if s.blockBank == nil || !s.blockBank.active {
		return s.files[kind].ReadAt(out, int64(id)*pageBytes)
	}
	block, offset := s.cachedBlock(kind, id)
	index := id / (blockBankBytes / pageBytes)
	if !block.valid || block.kind != uint8(kind) || block.index != index || offset+pageBytes > int(block.length) {
		if err := s.flushBlock(block); err != nil {
			return 0, err
		}
		if err := s.verifyBlock(block); err != nil {
			return 0, err
		}
		s.invalidateBlockAuthentication(block)
		s.invalidateNodeBlockAuthentication(kind, index)
		block.valid = false
		length := int(min(uint64(blockBankBytes), (s.next[kind]-index*(blockBankBytes/pageBytes))*uint64(pageBytes)))
		n, err := s.files[kind].ReadAt(block.raw[:length], int64(index)*blockBankBytes)
		// An allocation can have advanced next before its first write. Only
		// the actual returned bytes are cached, and the requested page must
		// exist in full. The exact EOF is independently checked at exit.
		if err != nil && !errors.Is(err, io.EOF) || n < offset+pageBytes {
			return 0, errors.Join(ErrCorrupt, err)
		}
		block.index, block.length, block.kind, block.valid, block.hits, block.dirty = index, uint16(n), uint8(kind), true, 0, 0
		clear(block.original[:])
	}
	if block.hits != ^uint16(0) {
		block.hits++
	}
	return copy(out, block.raw[offset:offset+pageBytes]), nil
}

func (s *Store) verifyCachedPageBeforeWrite(kind int, id uint64) error {
	if s.blockBank == nil || !s.blockBank.active {
		return nil
	}
	block, offset := s.cachedBlock(kind, id)
	if !block.valid || block.kind != uint8(kind) || block.index != id/(blockBankBytes/pageBytes) || offset >= int(block.length) {
		return nil
	}
	if offset+pageBytes > int(block.length) {
		return ErrCorrupt
	}
	actual := s.blockBank.check[:pageBytes]
	if _, err := s.files[kind].ReadAt(actual, int64(id)*pageBytes); err != nil {
		return errors.Join(ErrCorrupt, err)
	}
	if !bytes.Equal(actual, block.raw[offset:offset+pageBytes]) {
		return ErrCorrupt
	}
	return nil
}

func (s *Store) recordCachedPageWrite(kind int, id uint64, page []byte) error {
	if s.blockBank == nil || !s.blockBank.active {
		return nil
	}
	block, offset := s.cachedBlock(kind, id)
	if !block.valid || block.kind != uint8(kind) || block.index != id/(blockBankBytes/pageBytes) {
		return nil
	}
	if offset > int(block.length) {
		// Never invent the intervening bytes. Verify the old prefix before
		// removing its observation; the next read will obtain the new bytes.
		if err := s.verifyBlock(block); err != nil {
			return err
		}
		s.invalidateBlockAuthentication(block)
		block.valid = false
		return nil
	}
	copy(block.raw[offset:offset+pageBytes], page)
	block.length = uint16(max(int(block.length), offset+pageBytes))
	return nil
}
