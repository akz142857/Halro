package scratchstate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

const payloadBytes = pageBytes - 92

func appendLink(b []byte, r link) []byte {
	b = binary.BigEndian.AppendUint64(b, r.id)
	return append(b, r.hash[:]...)
}
func putLink(b []byte, r link) {
	binary.BigEndian.PutUint64(b[:8], r.id)
	copy(b[8:40], r.hash[:])
}
func decodeLink(b []byte) link {
	var r link
	r.id = binary.BigEndian.Uint64(b)
	copy(r.hash[:], b[8:40])
	return r
}
func (r link) empty() bool { return r.id == 0 && r.hash == ([32]byte{}) }
func (r link) valid() bool { return r.id != 0 || r.hash == ([32]byte{}) }
func (s *Store) pageMAC(raw []byte) [32]byte {
	return s.keyedMAC(raw, nil)
}
func (s *Store) readPage(kind int, r link) (raw [pageBytes]byte, resultErr error) {
	if kind < 0 || kind > 1 || r.id == 0 || r.id >= s.next[kind] {
		return raw, ErrCorrupt
	}
	if _, err := s.readPageBytes(kind, r.id, raw[:]); err != nil {
		return raw, errors.Join(ErrCorrupt, err)
	}
	reused, err := s.reuseNodeAuthentication(kind, r, raw[:])
	if err != nil {
		return raw, err
	}
	if !reused {
		wanted := s.pageMAC(raw[:pageBytes-32])
		if !hmac.Equal(wanted[:], raw[pageBytes-32:]) || sha256.Sum256(raw[:]) != r.hash || raw[5] != byte(kind) || binary.BigEndian.Uint64(raw[8:16]) != r.id {
			return raw, ErrCorrupt
		}
		s.rememberNodeAuthentication(kind, r, raw[:])
	}
	s.readBytes += pageBytes
	if s.readBytes >= cacheWindowBytes {
		if err := s.flushCache(true); err != nil {
			return raw, err
		}
	}
	return raw, nil
}
func (s *Store) writePage(kind int, id uint64, raw []byte) (link, error) {
	if len(raw) > pageBytes-32 || id == 0 || id >= s.next[kind] {
		return link{}, ErrCorrupt
	}
	var page [pageBytes]byte
	copy(page[:], raw)
	page[5] = byte(kind)
	binary.BigEndian.PutUint64(page[8:16], id)
	d := s.pageMAC(page[:pageBytes-32])
	copy(page[pageBytes-32:], d[:])
	s.invalidateNodeAuthentication(kind, id)
	staged, err := s.stageCachedNodePage(kind, id, page[:])
	if err != nil {
		return link{}, err
	}
	if !staged {
		if err := s.verifyCachedPageBeforeWrite(kind, id); err != nil {
			return link{}, err
		}
		n, err := s.files[kind].WriteAt(page[:], int64(id)*pageBytes)
		if err != nil || n != len(page) {
			return link{}, errors.Join(err, io.ErrShortWrite)
		}
		if err := s.recordCachedPageWrite(kind, id, page[:]); err != nil {
			return link{}, err
		}
	}
	if s.blockBank != nil && s.blockBank.active {
		s.blockBank.wrote = true
	}
	s.unsynced += pageBytes
	if err := s.flushCache(false); err != nil {
		return link{}, err
	}
	return link{id, sha256.Sum256(page[:])}, nil
}
func (s *Store) allocate(kind int) (uint64, error) {
	if !s.free[kind].empty() {
		if s.freeCount[kind] == 0 {
			return 0, ErrCorrupt
		}
		r := s.free[kind]
		raw, err := s.readPage(kind, r)
		if err != nil {
			return 0, err
		}
		if !bytes.Equal(raw[:4], []byte("SCF1")) {
			return 0, ErrCorrupt
		}
		next := decodeLink(raw[16:56])
		if !next.valid() || next.id == r.id {
			return 0, ErrCorrupt
		}
		s.free[kind] = next
		s.freeCount[kind]--
		return r.id, nil
	}
	if s.next[0]+s.next[1] >= uint64(s.ceiling/pageBytes) {
		return 0, ErrLimit
	}
	id := s.next[kind]
	s.next[kind]++
	return id, nil
}
func (s *Store) release(kind int, id uint64) error {
	if id == 0 || id >= s.next[kind] || s.freeCount[kind] >= s.next[kind]-1 {
		return ErrCorrupt
	}
	var raw [56]byte
	copy(raw[:], "SCF1")
	putLink(raw[16:], s.free[kind])
	r, err := s.writePage(kind, id, raw[:])
	if err == nil {
		s.free[kind] = r
		s.freeCount[kind]++
	}
	return err
}
func (s *Store) putBlob(value []byte) (blobRef, error) {
	if len(value) == 0 {
		return blobRef{}, nil
	}
	if len(value) > MaxValueBytes {
		return blobRef{}, ErrLimit
	}
	r := blobRef{size: uint32(len(value))}
	for end := len(value); end > 0; {
		start := max(0, end-payloadBytes)
		id, err := s.allocate(1)
		if err != nil {
			return blobRef{}, err
		}
		var raw [pageBytes - 32]byte
		copy(raw[:], "SCB1")
		putLink(raw[16:], r.link)
		binary.BigEndian.PutUint32(raw[56:60], uint32(end-start))
		copy(raw[60:], value[start:end])
		r.link, err = s.writePage(1, id, raw[:60+end-start])
		if err != nil {
			return blobRef{}, err
		}
		end = start
	}
	return r, nil
}
func (s *Store) readBlob(r blobRef, limit int) ([]byte, error) {
	if r.size == 0 {
		if !r.link.empty() {
			return nil, ErrCorrupt
		}
		return nil, nil
	}
	if r.size > uint32(limit) || r.link.empty() {
		return nil, ErrCorrupt
	}
	out := make([]byte, 0, int(r.size))
	next := r.link
	for !next.empty() {
		if len(out) >= int(r.size) {
			return nil, ErrCorrupt
		}
		raw, err := s.readPage(1, next)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(raw[:4], []byte("SCB1")) {
			return nil, ErrCorrupt
		}
		n := int(binary.BigEndian.Uint32(raw[56:60]))
		following := decodeLink(raw[16:56])
		if n <= 0 || n > payloadBytes || n > int(r.size)-len(out) || !following.valid() || following.id == next.id {
			return nil, ErrCorrupt
		}
		out = append(out, raw[60:60+n]...)
		next = following
	}
	if len(out) != int(r.size) {
		return nil, ErrCorrupt
	}
	return out, nil
}
func (s *Store) freeBlob(r blobRef) error {
	if r.size == 0 {
		if !r.link.empty() {
			return ErrCorrupt
		}
		return nil
	}
	remaining := int(r.size)
	next := r.link
	for !next.empty() {
		raw, err := s.readPage(1, next)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw[:4], []byte("SCB1")) {
			return ErrCorrupt
		}
		n := int(binary.BigEndian.Uint32(raw[56:60]))
		following := decodeLink(raw[16:56])
		if n <= 0 || n > payloadBytes || n > remaining || !following.valid() || following.id == next.id {
			return ErrCorrupt
		}
		remaining -= n
		if err := s.release(1, next.id); err != nil {
			return err
		}
		next = following
	}
	if remaining != 0 {
		return ErrCorrupt
	}
	return nil
}
