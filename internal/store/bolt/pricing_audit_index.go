package bolt

// Disposable exact uint64 membership for timeline version auditing. Compressed
// radix paths have at most 64 branches; no node cache or history-sized map is
// retained. A per-process MAC binds every node and its physical offset.
import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"math/bits"
	"os"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ownedfile"
)

var ErrPricingAuditLimit = errors.New("pricing audit exceeds bounded working allowance")
var ErrPricingAuditCorrupt = errors.New("pricing audit index is corrupt")

const priceAuditNodeBytes = 64
const priceAuditDiskBytes = 64 << 20

type priceAuditNode struct{ bit, left, right, version uint64 }
type priceAuditIndex struct {
	file    *os.File
	owner   *ownedfile.Owner
	key     [32]byte
	mac     hash.Hash
	raw     [priceAuditNodeBytes]byte
	offset  [8]byte
	digest  [32]byte
	root    uint64
	size    int64
	max     int64
	retired *bool
}

func newPriceAuditIndex(path string) (*priceAuditIndex, error) {
	return newPriceAuditIndexWithRetirement(path, nil)
}
func newPriceAuditIndexWithRetirement(path string, retired *bool) (_ *priceAuditIndex, resultErr error) {
	f, owner, err := ownedfile.Create(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	x := &priceAuditIndex{file: f, owner: owner, size: priceAuditNodeBytes, max: priceAuditDiskBytes, retired: retired}
	if retired != nil {
		*retired = false
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, x.close())
		}
	}()
	if _, err := rand.Read(x.key[:]); err != nil {
		return nil, err
	}
	x.mac = hmac.New(sha256.New, x.key[:])
	var header [priceAuditNodeBytes]byte
	if _, err := f.Write(header[:]); err != nil {
		return nil, err
	}
	return x, nil
}

func (x *priceAuditIndex) read(ctx context.Context, at uint64) (priceAuditNode, error) {
	var n priceAuditNode
	if err := ctx.Err(); err != nil {
		return n, err
	}
	if at < priceAuditNodeBytes || at%priceAuditNodeBytes != 0 || at > uint64(x.size-priceAuditNodeBytes) {
		return n, ErrPricingAuditCorrupt
	}
	raw := &x.raw
	if _, err := x.file.ReadAt(raw[:], int64(at)); err != nil {
		return n, err
	}
	binary.BigEndian.PutUint64(x.offset[:], at)
	x.mac.Reset()
	x.mac.Write(x.offset[:])
	x.mac.Write(raw[:32])
	if !hmac.Equal(raw[32:], x.mac.Sum(x.digest[:0])) {
		return n, ErrPricingAuditCorrupt
	}
	n = priceAuditNode{binary.BigEndian.Uint64(raw[:8]), binary.BigEndian.Uint64(raw[8:16]), binary.BigEndian.Uint64(raw[16:24]), binary.BigEndian.Uint64(raw[24:32])}
	if n.bit > 64 || n.bit < 64 && (n.left == 0 || n.right == 0) || n.bit == 64 && (n.left != 0 || n.right != 0) {
		return priceAuditNode{}, ErrPricingAuditCorrupt
	}
	return n, nil
}

func (x *priceAuditIndex) write(ctx context.Context, at uint64, n priceAuditNode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if at < priceAuditNodeBytes || at%priceAuditNodeBytes != 0 || at > uint64(x.size) {
		return ErrPricingAuditCorrupt
	}
	if int64(at)+priceAuditNodeBytes > x.max {
		return ErrPricingAuditLimit
	}
	raw := &x.raw
	binary.BigEndian.PutUint64(raw[:8], n.bit)
	binary.BigEndian.PutUint64(raw[8:16], n.left)
	binary.BigEndian.PutUint64(raw[16:24], n.right)
	binary.BigEndian.PutUint64(raw[24:32], n.version)
	binary.BigEndian.PutUint64(x.offset[:], at)
	x.mac.Reset()
	x.mac.Write(x.offset[:])
	x.mac.Write(raw[:32])
	copy(raw[32:], x.mac.Sum(x.digest[:0]))
	if _, err := x.file.WriteAt(raw[:], int64(at)); err != nil {
		return err
	}
	if int64(at) == x.size {
		x.size += priceAuditNodeBytes
	}
	return nil
}

func priceAuditBranch(version, bit uint64) bool { return version&(uint64(1)<<(63-bit)) != 0 }

func (x *priceAuditIndex) add(ctx context.Context, version uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if x.root == 0 {
		at := uint64(x.size)
		if err := x.write(ctx, at, priceAuditNode{bit: 64, version: version}); err != nil {
			return err
		}
		x.root = at
		return nil
	}
	at, previousBit := x.root, -1
	var leaf priceAuditNode
	for step := 0; step <= 64; step++ {
		n, err := x.read(ctx, at)
		if err != nil {
			return err
		}
		if int(n.bit) <= previousBit {
			return ErrPricingAuditCorrupt
		}
		if n.bit == 64 {
			leaf = n
			break
		}
		previousBit = int(n.bit)
		at = n.left
		if priceAuditBranch(version, n.bit) {
			at = n.right
		}
	}
	if leaf.bit != 64 {
		return ErrPricingAuditCorrupt
	}
	if leaf.version == version {
		return domain.ErrPriceTimelineConflict
	}
	split := uint64(bits.LeadingZeros64(version ^ leaf.version))
	at, previousBit = x.root, -1
	var parent priceAuditNode
	var parentAt uint64
	for step := 0; step <= 64; step++ {
		n, err := x.read(ctx, at)
		if err != nil {
			return err
		}
		if int(n.bit) <= previousBit {
			return ErrPricingAuditCorrupt
		}
		if n.bit >= split {
			break
		}
		parent, parentAt, previousBit = n, at, int(n.bit)
		at = n.left
		if priceAuditBranch(version, n.bit) {
			at = n.right
		}
	}
	if x.size > x.max-2*priceAuditNodeBytes {
		return ErrPricingAuditLimit
	}
	newLeaf := uint64(x.size)
	if err := x.write(ctx, newLeaf, priceAuditNode{bit: 64, version: version}); err != nil {
		return err
	}
	branchAt := uint64(x.size)
	branch := priceAuditNode{bit: split, left: newLeaf, right: at}
	if priceAuditBranch(version, split) {
		branch.left, branch.right = at, newLeaf
	}
	if err := x.write(ctx, branchAt, branch); err != nil {
		return err
	}
	if parentAt == 0 {
		x.root = branchAt
		return nil
	}
	if priceAuditBranch(version, parent.bit) {
		parent.right = branchAt
	} else {
		parent.left = branchAt
	}
	return x.write(ctx, parentAt, parent)
}

func (x *priceAuditIndex) close() error {
	if x == nil {
		return nil
	}
	clear(x.key[:])
	x.mac = nil
	clear(x.raw[:])
	clear(x.offset[:])
	clear(x.digest[:])
	if err := x.file.Close(); err != nil {
		return err
	}
	if x.retired != nil {
		*x.retired = true
	}
	return x.owner.Remove()
}
