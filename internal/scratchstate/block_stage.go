package scratchstate

import (
	"crypto/sha256"
	"errors"
	"io"
)

// Only complete, already-observed node pages can defer an overwrite. New EOF,
// blob allocation and uncached pages retain the ordinary immediate FD path.
// There is no saved-root recovery contract for this fresh private reducer.
func (s *Store) stageCachedNodePage(kind int, id uint64, page []byte) (bool, error) {
	if kind != 0 || s.blockBank == nil || !s.blockBank.active {
		return false, nil
	}
	block, offset := s.cachedBlock(kind, id)
	if !block.valid || block.kind != uint8(kind) || block.index != id/(blockBankBytes/pageBytes) {
		return false, nil
	}
	if offset+pageBytes > int(block.length) {
		// An immediate append in this same block must not change the original
		// prefix while earlier node overwrites are still pending.
		return false, s.flushBlock(block)
	}
	if block.dirty == 0 {
		if err := s.verifyBlock(block); err != nil {
			return false, err
		}
		block.original = sha256.Sum256(block.raw[:block.length])
	}
	copy(block.raw[offset:offset+pageBytes], page)
	block.dirty |= 1 << (offset / pageBytes)
	return true, nil
}

func (s *Store) flushBlock(block *transactionBlock) error {
	if !block.valid || block.dirty == 0 {
		return nil
	}
	if block.kind != 0 || block.length == 0 || int(block.length) > blockBankBytes {
		return ErrCorrupt
	}
	if err := s.verifyNames(); err != nil {
		return err
	}
	if err := s.owners[block.kind].VerifyFile(s.files[block.kind]); err != nil {
		return err
	}
	if err := s.verifyBlock(block); err != nil {
		return err
	}
	// Adjacent dirty pages share one real WriteAt. No untouched original byte
	// or newly allocated range is invented or written by this flush.
	for page := 0; page < blockBankBytes/pageBytes; {
		if block.dirty&(1<<page) == 0 {
			page++
			continue
		}
		start := page
		for page < blockBankBytes/pageBytes && block.dirty&(1<<page) != 0 {
			page++
		}
		end := page * pageBytes
		if end > int(block.length) {
			return ErrCorrupt
		}
		data := block.raw[start*pageBytes : end]
		n, err := s.files[block.kind].WriteAt(data, int64(block.index)*blockBankBytes+int64(start*pageBytes))
		if err != nil || n != len(data) {
			// Preserve the original hash, dirty bytes and captured owner. This
			// candidate cannot retry or export a root after an uncertain flush.
			return errors.Join(err, io.ErrShortWrite)
		}
	}
	block.dirty = 0
	clear(block.original[:])
	return s.verifyBlock(block)
}

func (s *Store) flushDirtyBank() error {
	if s.blockBank == nil || !s.blockBank.active {
		return nil
	}
	for i := range s.blockBank.blocks {
		if err := s.flushBlock(&s.blockBank.blocks[i]); err != nil {
			return err
		}
	}
	return nil
}

// A successful mutation cannot become the next View's source before all real
// node/blob writes and both FD Syncs finish. This adds a transaction boundary
// without moving the original periodic whole-file DONTNEED boundary earlier.
func (s *Store) syncBankTransaction() error {
	if s.blockBank == nil || !s.blockBank.active || !s.blockBank.wrote {
		return nil
	}
	if err := s.verifyNames(); err != nil {
		return err
	}
	for i, file := range s.files {
		if err := s.owners[i].VerifyFile(file); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
	}
	return nil
}
