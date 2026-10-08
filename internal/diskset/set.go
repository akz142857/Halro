// Package diskset holds a disposable exact digest set with fixed working
// memory. Neither its file nor its operation-local key/root can be reopened.
package diskset

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/akz142857/Halro/internal/ownedfile"
)

const nodeBytes = 160

var (
	ErrLimit      = errors.New("disposable digest set file limit exceeded")
	ErrCorrupt    = errors.New("disposable digest set identity or contents changed")
	ErrRetirement = errors.New("disposable digest set retirement is uncertain")
)

type reference struct {
	offset uint64
	hash   [32]byte
}

type node struct {
	bit      uint64
	children [2]reference
	key      [32]byte
	count    uint64
}

// Set is serial and holds one authenticated root and at most 257 path nodes.
// Every child includes a hash, so replay of an older valid node cannot turn
// a current member into authenticated absence. A failed mutation poisons the
// whole operation; it never publishes a usable partial reference set.
type Set struct {
	file        *os.File
	owner       *ownedfile.Owner
	key         [32]byte
	root        reference
	size, limit int64
	failure     error
	closed      bool
	closeErr    error
}

// Create requires the caller to acquire its whole-job resource envelope first.
// No mmap, history-sized freelist or decoded-key cache is retained.
func Create(ctx context.Context, path string, limit int64) (result *Set, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 3*nodeBytes || limit > 1<<40 {
		return nil, ErrLimit
	}
	f, owner, err := ownedfile.Create(path, os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	s := &Set{file: f, owner: owner, size: nodeBytes, limit: limit}
	defer func() {
		if result == nil {
			if err := s.Discard(); err != nil {
				resultErr = errors.Join(resultErr, ErrRetirement, err)
			}
		}
	}()
	if _, err := rand.Read(s.key[:]); err != nil {
		return nil, err
	}
	var header [nodeBytes]byte
	if n, err := f.Write(header[:]); err != nil || n != len(header) {
		return nil, errors.Join(err, io.ErrShortWrite)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Set) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil {
		return ErrCorrupt
	}
	if s.closed || s.failure != nil {
		return errors.Join(ErrCorrupt, s.failure)
	}
	if err := s.owner.VerifyFile(s.file); err != nil {
		return errors.Join(ErrCorrupt, err)
	}
	info, err := s.file.Stat()
	if err != nil || info.Size() != s.size {
		return errors.Join(ErrCorrupt, err)
	}
	return nil
}

func keyBit(key [32]byte, bit uint64) int { return int(key[bit/8] >> (7 - bit%8) & 1) }

func (s *Set) read(ctx context.Context, ref reference) (result node, resultErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if ref.offset < nodeBytes || ref.offset%nodeBytes != 0 || ref.offset > uint64(s.size-nodeBytes) {
		return result, ErrCorrupt
	}
	var raw [nodeBytes]byte
	if _, err := s.file.ReadAt(raw[:], int64(ref.offset)); err != nil {
		return result, err
	}
	if sha256.Sum256(raw[:]) != ref.hash {
		return result, ErrCorrupt
	}
	var offset [8]byte
	binary.BigEndian.PutUint64(offset[:], ref.offset)
	mac := hmac.New(sha256.New, s.key[:])
	mac.Write(offset[:])
	mac.Write(raw[:128])
	if !hmac.Equal(mac.Sum(nil), raw[128:]) {
		return result, ErrCorrupt
	}
	result.bit = binary.BigEndian.Uint64(raw[:8])
	for i := 0; i < 2; i++ {
		at := 8 + i*40
		result.children[i].offset = binary.BigEndian.Uint64(raw[at : at+8])
		copy(result.children[i].hash[:], raw[at+8:at+40])
	}
	copy(result.key[:], raw[88:120])
	result.count = binary.BigEndian.Uint64(raw[120:128])
	if result.bit > 256 || result.bit == 256 && result.children != ([2]reference{}) || result.bit < 256 && result.count != 0 || result.bit < 256 && (result.children[0].offset == 0 || result.children[1].offset == 0) {
		return node{}, ErrCorrupt
	}
	return result, nil
}

func (s *Set) write(ctx context.Context, offset uint64, n node) (reference, error) {
	if err := ctx.Err(); err != nil {
		return reference{}, err
	}
	if offset < nodeBytes || offset%nodeBytes != 0 || offset > uint64(s.size) {
		return reference{}, ErrCorrupt
	}
	if offset > uint64(s.limit-nodeBytes) {
		return reference{}, ErrLimit
	}
	var raw [nodeBytes]byte
	binary.BigEndian.PutUint64(raw[:8], n.bit)
	for i := 0; i < 2; i++ {
		at := 8 + i*40
		binary.BigEndian.PutUint64(raw[at:at+8], n.children[i].offset)
		copy(raw[at+8:at+40], n.children[i].hash[:])
	}
	copy(raw[88:120], n.key[:])
	binary.BigEndian.PutUint64(raw[120:128], n.count)
	var position [8]byte
	binary.BigEndian.PutUint64(position[:], offset)
	mac := hmac.New(sha256.New, s.key[:])
	mac.Write(position[:])
	mac.Write(raw[:128])
	copy(raw[128:], mac.Sum(nil))
	written, err := s.file.WriteAt(raw[:], int64(offset))
	if err != nil || written != len(raw) {
		return reference{}, errors.Join(err, io.ErrShortWrite)
	}
	if offset == uint64(s.size) {
		s.size += nodeBytes
	}
	return reference{offset: offset, hash: sha256.Sum256(raw[:])}, nil
}

func (s *Set) Contains(ctx context.Context, key [32]byte) (bool, error) {
	if err := s.check(ctx); err != nil {
		return false, err
	}
	ref, previous := s.root, -1
	for ref.offset != 0 {
		n, err := s.read(ctx, ref)
		if err != nil || int(n.bit) <= previous {
			return false, errors.Join(ErrCorrupt, err)
		}
		if n.bit == 256 {
			return n.key == key, s.check(ctx)
		}
		previous, ref = int(n.bit), n.children[keyBit(key, n.bit)]
	}
	return false, s.check(ctx)
}

func (s *Set) Add(ctx context.Context, key [32]byte) error {
	_, err := s.mutate(ctx, key, false)
	return err
}

// Increment counts an exact digest without growing the file for duplicates.
// An overflow or failed write poisons the disposable operation.
func (s *Set) Increment(ctx context.Context, key [32]byte) (uint64, error) {
	return s.mutate(ctx, key, true)
}

func (s *Set) mutate(ctx context.Context, key [32]byte, increment bool) (count uint64, resultErr error) {
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	defer func() {
		if resultErr != nil {
			s.failure = resultErr
		}
	}()
	var path [257]struct {
		ref reference
		n   node
	}
	depth, previous, ref := 0, -1, s.root
	var leaf node
	for ref.offset != 0 {
		n, err := s.read(ctx, ref)
		if err != nil || int(n.bit) <= previous || depth >= len(path) {
			return 0, errors.Join(ErrCorrupt, err)
		}
		path[depth].ref, path[depth].n = ref, n
		depth++
		if n.bit == 256 {
			leaf = n
			break
		}
		previous, ref = int(n.bit), n.children[keyBit(key, n.bit)]
	}
	if ref.offset != 0 && leaf.key == key {
		count = leaf.count
		if !increment {
			return count, s.check(ctx)
		}
		if count == ^uint64(0) {
			return 0, ErrLimit
		}
		count++
		leaf.count = count
		newRef, err := s.write(ctx, ref.offset, leaf)
		if err != nil {
			return 0, err
		}
		for i := depth - 2; i >= 0; i-- {
			parent := path[i].n
			parent.children[keyBit(key, parent.bit)] = newRef
			newRef, err = s.write(ctx, path[i].ref.offset, parent)
			if err != nil {
				return 0, err
			}
		}
		s.root = newRef
		return count, s.check(ctx)
	}
	if increment {
		count = 1
	}
	needed := int64(nodeBytes)
	if ref.offset != 0 {
		needed *= 2
	}
	if s.size > s.limit-needed {
		return 0, ErrLimit
	}
	newRef, err := s.write(ctx, uint64(s.size), node{bit: 256, key: key, count: count})
	if err != nil {
		return 0, err
	}
	if ref.offset == 0 {
		s.root = newRef
		return count, s.check(ctx)
	}
	bit := uint64(0)
	for bit < 256 && keyBit(key, bit) == keyBit(leaf.key, bit) {
		bit++
	}
	if bit == 256 {
		return 0, ErrCorrupt
	}
	at := 0
	for at < depth && path[at].n.bit < bit {
		at++
	}
	branch := node{bit: bit}
	direction := keyBit(key, bit)
	branch.children[direction], branch.children[1-direction] = newRef, path[at].ref
	newRef, err = s.write(ctx, uint64(s.size), branch)
	if err != nil {
		return 0, err
	}
	for i := at - 1; i >= 0; i-- {
		parent := path[i].n
		parent.children[keyBit(key, parent.bit)] = newRef
		newRef, err = s.write(ctx, path[i].ref.offset, parent)
		if err != nil {
			return 0, err
		}
	}
	s.root = newRef
	return count, s.check(ctx)
}

// Discard closes the real FD first and then retires only its captured inode.
// The caller retains whole-job credits on any uncertain close or removal.
func (s *Set) Discard() error {
	if s == nil {
		return nil
	}
	if !s.closed {
		s.closed = true
		s.closeErr = s.file.Close()
		clear(s.key[:])
	}
	if s.closeErr != nil {
		return s.closeErr
	}
	return s.owner.Retire()
}
