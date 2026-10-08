package usage

// A private, disposable comparison index. Unlike a general-purpose mapped DB,
// it has no history-sized freelist, node cache or heap map. Compressed binary
// radix paths have at most 256 branches. Roots and the MAC key exist only in
// this process; a saved file is never reopened as trusted verification state.
import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

const archiveVerifyNodeBytes = 160
const archiveVerifyIndexRowWork = (4*257 + 8) * archiveVerifyNodeBytes

type archiveVerifyIndex struct {
	file      *os.File
	identity  os.FileInfo
	tree      *archiveTreeBudget
	key       [32]byte
	roots     [3]uint64 // event, sequence, path
	size, max int64
}

type archiveVerifyNode struct {
	bit      uint64 // 256 is a leaf; branches increase strictly along each path
	children [2]uint64
	key      [32]byte
	value    [40]byte
}

func newArchiveVerifyIndex(path string, max int64) (_ *archiveVerifyIndex, err error) {
	return newArchiveVerifyIndexWithTree(path, max, nil)
}

func newArchiveVerifyIndexWithTree(path string, max int64, tree *archiveTreeBudget) (_ *archiveVerifyIndex, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	x := &archiveVerifyIndex{file: f, size: archiveVerifyNodeBytes, max: max, tree: tree}
	x.identity, err = f.Stat()
	defer func() {
		if err != nil {
			closeErr := f.Close()
			err = errors.Join(err, closeErr)
			if closeErr == nil && x.identity != nil {
				err = errors.Join(err, unlinkOwnedArchiveScratch(path, x.identity))
			}
			clear(x.key[:])
		}
	}()
	if err != nil {
		return nil, err
	}
	if _, err = rand.Read(x.key[:]); err != nil {
		return nil, err
	}
	if max < archiveVerifyNodeBytes || max > 1<<40 {
		return nil, ErrDiskUsageLimit
	}
	var header [archiveVerifyNodeBytes]byte
	if err := tree.take(len(header)); err != nil {
		return nil, err
	}
	if _, err = f.Write(header[:]); err != nil {
		return nil, err
	}
	x.identity, err = f.Stat()
	return x, err
}

func archiveVerifyKeyBit(key [32]byte, bit uint64) uint64 {
	return uint64(key[bit/8] >> (7 - bit%8) & 1)
}

func (x *archiveVerifyIndex) read(ctx context.Context, offset uint64, budget *archiveVerifyReadBudget) (archiveVerifyNode, error) {
	var node archiveVerifyNode
	if err := ctx.Err(); err != nil {
		return node, err
	}
	if offset < archiveVerifyNodeBytes || offset%archiveVerifyNodeBytes != 0 || offset > uint64(x.size-archiveVerifyNodeBytes) {
		return node, ErrDiskUsageCorrupt
	}
	if err := budget.reserve(archiveVerifyNodeBytes); err != nil {
		return node, err
	}
	var raw [archiveVerifyNodeBytes]byte
	if _, err := x.file.ReadAt(raw[:], int64(offset)); err != nil {
		return node, err
	}
	var at [8]byte
	binary.BigEndian.PutUint64(at[:], offset)
	mac := hmac.New(sha256.New, x.key[:])
	mac.Write(at[:])
	mac.Write(raw[:128])
	if !hmac.Equal(raw[128:], mac.Sum(nil)) {
		return node, ErrDiskUsageCorrupt
	}
	node.bit = binary.BigEndian.Uint64(raw[:8])
	node.children[0], node.children[1] = binary.BigEndian.Uint64(raw[8:16]), binary.BigEndian.Uint64(raw[16:24])
	copy(node.key[:], raw[24:56])
	copy(node.value[:], raw[56:96])
	if node.bit > 256 || node.bit < 256 && (node.children[0] == 0 || node.children[1] == 0) || node.bit == 256 && node.children != ([2]uint64{}) {
		return node, ErrDiskUsageCorrupt
	}
	return node, nil
}

func (x *archiveVerifyIndex) write(ctx context.Context, at uint64, node archiveVerifyNode, budget *archiveVerifyReadBudget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if at < archiveVerifyNodeBytes || at%archiveVerifyNodeBytes != 0 || at > uint64(x.size) {
		return ErrDiskUsageCorrupt
	}
	if int64(at)+archiveVerifyNodeBytes > x.max {
		return ErrDiskUsageLimit
	}
	if err := budget.reserve(archiveVerifyNodeBytes); err != nil {
		return err
	}
	if at == uint64(x.size) {
		if err := x.tree.take(archiveVerifyNodeBytes); err != nil {
			return err
		}
	}
	var raw [archiveVerifyNodeBytes]byte
	binary.BigEndian.PutUint64(raw[:8], node.bit)
	binary.BigEndian.PutUint64(raw[8:16], node.children[0])
	binary.BigEndian.PutUint64(raw[16:24], node.children[1])
	copy(raw[24:56], node.key[:])
	copy(raw[56:96], node.value[:])
	var offset [8]byte
	binary.BigEndian.PutUint64(offset[:], at)
	mac := hmac.New(sha256.New, x.key[:])
	mac.Write(offset[:])
	mac.Write(raw[:128])
	copy(raw[128:], mac.Sum(nil))
	n, err := x.file.WriteAt(raw[:], int64(at))
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	if at == uint64(x.size) {
		x.size += archiveVerifyNodeBytes
	}
	return nil
}

func (x *archiveVerifyIndex) lookup(ctx context.Context, tree int, key [32]byte, budget *archiveVerifyReadBudget) ([40]byte, bool, error) {
	var zero [40]byte
	if tree < 0 || tree >= len(x.roots) {
		return zero, false, ErrDiskUsageCorrupt
	}
	ref, previous := x.roots[tree], -1
	for ref != 0 {
		n, err := x.read(ctx, ref, budget)
		if err != nil {
			return zero, false, err
		}
		if int(n.bit) <= previous {
			return zero, false, ErrDiskUsageCorrupt
		}
		if n.bit == 256 {
			return n.value, n.key == key, nil
		}
		previous, ref = int(n.bit), n.children[archiveVerifyKeyBit(key, n.bit)]
	}
	return zero, false, ctx.Err()
}

func (x *archiveVerifyIndex) insert(ctx context.Context, tree int, key [32]byte, value [40]byte, budget *archiveVerifyReadBudget) error {
	if tree < 0 || tree >= len(x.roots) {
		return ErrDiskUsageCorrupt
	}
	ref, previous := x.roots[tree], -1
	var leaf archiveVerifyNode
	for ref != 0 {
		n, err := x.read(ctx, ref, budget)
		if err != nil {
			return err
		}
		if int(n.bit) <= previous {
			return ErrDiskUsageCorrupt
		}
		if n.bit == 256 {
			leaf = n
			break
		}
		previous, ref = int(n.bit), n.children[archiveVerifyKeyBit(key, n.bit)]
	}
	if ref != 0 && leaf.key == key {
		return ErrDiskUsageCorrupt
	}
	newLeaf := uint64(x.size)
	if err := x.write(ctx, newLeaf, archiveVerifyNode{bit: 256, key: key, value: value}, budget); err != nil {
		return err
	}
	if ref == 0 {
		x.roots[tree] = newLeaf
		return nil
	}
	bit := uint64(0)
	for bit < 256 && archiveVerifyKeyBit(key, bit) == archiveVerifyKeyBit(leaf.key, bit) {
		bit++
	}
	if bit == 256 {
		return ErrDiskUsageCorrupt
	}
	ref, parentRef, previous := x.roots[tree], uint64(0), -1
	var parent archiveVerifyNode
	for {
		n, err := x.read(ctx, ref, budget)
		if err != nil {
			return err
		}
		if int(n.bit) <= previous {
			return ErrDiskUsageCorrupt
		}
		if n.bit >= bit {
			break
		}
		parentRef, parent, previous = ref, n, int(n.bit)
		ref = n.children[archiveVerifyKeyBit(key, n.bit)]
	}
	branch := archiveVerifyNode{bit: bit}
	direction := archiveVerifyKeyBit(key, bit)
	branch.children[direction], branch.children[1-direction] = newLeaf, ref
	branchRef := uint64(x.size)
	if err := x.write(ctx, branchRef, branch, budget); err != nil {
		return err
	}
	if parentRef == 0 {
		x.roots[tree] = branchRef
		return nil
	}
	parent.children[archiveVerifyKeyBit(key, parent.bit)] = branchRef
	return x.write(ctx, parentRef, parent, budget)
}

func (x *archiveVerifyIndex) check() error {
	current, err := x.file.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(x.file.Name())
	if err != nil {
		return err
	}
	if !named.Mode().IsRegular() || !os.SameFile(x.identity, current) || !os.SameFile(x.identity, named) || current.Size() != x.size || named.Size() != x.size {
		return ErrDiskUsageCorrupt
	}
	return nil
}

func (x *archiveVerifyIndex) close() error { clear(x.key[:]); return x.file.Close() }

func (x *archiveVerifyIndex) discard() error {
	if err := x.close(); err != nil {
		return err
	}
	return unlinkOwnedArchiveScratch(x.file.Name(), x.identity)
}
