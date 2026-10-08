package bolt

// Process-local pricing observations. A compressed 256-bit Merkle radix tree
// keeps the live root in RAM; leaf/branch rollback, deletion and false absence
// cannot be accepted by restoring an older but otherwise valid disk record.
import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"math/bits"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
)

var ErrPricingClockLimit = errors.New("pricing clock index exceeds bounded working allowance")
var ErrPricingClockCorrupt = errors.New("pricing clock index is corrupt")

const pricingClockNodeBytes = 192
const pricingClockDiskBytes = 64 << 20
const pricingStateSlots = 4096

type pricingClockNode struct {
	bit                      uint16
	left, right              uint64
	key, leftHash, rightHash [32]byte
	selectedSec              int64
	selectedNano             uint32
	observed                 int64
}
type pricingClockStep struct {
	at   uint64
	node pricingClockNode
}
type pricingClockIndex struct {
	mu            sync.Mutex
	file          *os.File
	owner         *ownedfile.Owner
	dir           string
	dirInfo       os.FileInfo
	fileRetired   bool
	origin        time.Time
	key, rootHash [32]byte
	mac           hash.Hash
	raw           [pricingClockNodeBytes]byte
	offset        [8]byte
	root          uint64
	size, max     int64
	err           error
}

func newPricingClockIndex(parent string) (_ *pricingClockIndex, resultErr error) {
	x := &pricingClockIndex{origin: time.Now(), size: pricingClockNodeBytes, max: pricingClockDiskBytes}
	dir, err := os.MkdirTemp(parent, ".halro-price-clock-")
	if err != nil {
		return nil, err
	}
	x.dir = dir
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, x.close())
		}
	}()
	directory, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	x.dirInfo, err = directory.Stat()
	err = errors.Join(err, directory.Close())
	if err != nil {
		return nil, err
	}
	x.file, x.owner, err = ownedfile.Create(filepath.Join(dir, "observations"), os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if _, err := rand.Read(x.key[:]); err != nil {
		return nil, err
	}
	x.mac = hmac.New(sha256.New, x.key[:])
	if _, err := x.file.Write(x.raw[:]); err != nil {
		return nil, err
	}
	return x, nil
}

func pricingClockBranch(key [32]byte, bit uint16) bool { return key[bit/8]&(1<<(7-bit%8)) != 0 }
func pricingClockSplit(a, b [32]byte) uint16 {
	for i := range a {
		if a[i] != b[i] {
			return uint16(i*8 + bits.LeadingZeros8(a[i]^b[i]))
		}
	}
	return 256
}

func (x *pricingClockIndex) read(at uint64, expected [32]byte) (n pricingClockNode, err error) {
	if at < pricingClockNodeBytes || at%pricingClockNodeBytes != 0 || at > uint64(x.size-pricingClockNodeBytes) {
		return n, ErrPricingClockCorrupt
	}
	r := &x.raw
	if _, err := x.file.ReadAt(r[:], int64(at)); err != nil {
		return n, err
	}
	binary.BigEndian.PutUint64(x.offset[:], at)
	x.mac.Reset()
	x.mac.Write(x.offset[:])
	x.mac.Write(r[:160])
	var digest [32]byte
	if !hmac.Equal(r[160:], expected[:]) || !hmac.Equal(r[160:], x.mac.Sum(digest[:0])) {
		return n, ErrPricingClockCorrupt
	}
	n.bit = binary.BigEndian.Uint16(r[:2])
	n.left, n.right = binary.BigEndian.Uint64(r[8:16]), binary.BigEndian.Uint64(r[16:24])
	copy(n.key[:], r[24:56])
	copy(n.leftHash[:], r[80:112])
	copy(n.rightHash[:], r[112:144])
	n.selectedSec, n.selectedNano, n.observed = int64(binary.BigEndian.Uint64(r[56:64])), binary.BigEndian.Uint32(r[64:68]), int64(binary.BigEndian.Uint64(r[72:80]))
	if n.bit > 256 || n.bit < 256 && (n.left == 0 || n.right == 0) || n.bit == 256 && (n.left != 0 || n.right != 0 || n.selectedNano >= 1e9 || n.observed < 0) {
		return pricingClockNode{}, ErrPricingClockCorrupt
	}
	return n, nil
}

func (x *pricingClockIndex) write(at uint64, n pricingClockNode) (digest [32]byte, err error) {
	if at < pricingClockNodeBytes || at%pricingClockNodeBytes != 0 || at > uint64(x.size) {
		return digest, ErrPricingClockCorrupt
	}
	if int64(at)+pricingClockNodeBytes > x.max {
		return digest, ErrPricingClockLimit
	}
	r := &x.raw
	clear(r[:])
	binary.BigEndian.PutUint16(r[:2], n.bit)
	binary.BigEndian.PutUint64(r[8:16], n.left)
	binary.BigEndian.PutUint64(r[16:24], n.right)
	copy(r[24:56], n.key[:])
	copy(r[80:112], n.leftHash[:])
	copy(r[112:144], n.rightHash[:])
	binary.BigEndian.PutUint64(r[56:64], uint64(n.selectedSec))
	binary.BigEndian.PutUint32(r[64:68], n.selectedNano)
	binary.BigEndian.PutUint64(r[72:80], uint64(n.observed))
	binary.BigEndian.PutUint64(x.offset[:], at)
	x.mac.Reset()
	x.mac.Write(x.offset[:])
	x.mac.Write(r[:160])
	copy(digest[:], x.mac.Sum(digest[:0]))
	copy(r[160:], digest[:])
	if _, err = x.file.WriteAt(r[:], int64(at)); err != nil {
		return digest, err
	}
	if int64(at) == x.size {
		x.size += pricingClockNodeBytes
	}
	return digest, nil
}

func (x *pricingClockIndex) find(key [32]byte, path *[256]pricingClockStep) (at uint64, digest [32]byte, leaf pricingClockNode, depth int, err error) {
	at, digest, previous := x.root, x.rootHash, -1
	if at == 0 {
		return 0, digest, leaf, 0, nil
	}
	for depth <= 256 {
		n, readErr := x.read(at, digest)
		if readErr != nil {
			return at, digest, leaf, depth, readErr
		}
		if int(n.bit) <= previous {
			return at, digest, leaf, depth, ErrPricingClockCorrupt
		}
		if n.bit == 256 {
			return at, digest, n, depth, nil
		}
		if depth == 256 {
			break
		}
		if path != nil {
			path[depth] = pricingClockStep{at: at, node: n}
		}
		depth++
		previous = int(n.bit)
		at, digest = n.left, n.leftHash
		if pricingClockBranch(key, n.bit) {
			at, digest = n.right, n.rightHash
		}
	}
	return at, digest, leaf, depth, ErrPricingClockCorrupt
}

func (x *pricingClockIndex) clockAnchor(id string) (pricingClockObservation, bool, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.err != nil {
		return pricingClockObservation{}, false, x.err
	}
	_, _, n, _, err := x.find(sha256.Sum256([]byte(id)), nil)
	if err != nil {
		x.err = err
		return pricingClockObservation{}, false, err
	}
	if x.root == 0 || n.key != sha256.Sum256([]byte(id)) {
		needed := int64(2 * pricingClockNodeBytes)
		if x.root == 0 {
			needed = pricingClockNodeBytes
		}
		if x.size > x.max-needed {
			return pricingClockObservation{}, false, ErrPricingClockLimit
		}
		return pricingClockObservation{}, false, nil
	}
	return pricingClockObservation{SelectedAt: time.Unix(n.selectedSec, int64(n.selectedNano)).UTC(), ObservedAt: x.origin.Add(time.Duration(n.observed))}, true, nil
}

func (x *pricingClockIndex) mergeClockAnchor(id string, candidate pricingClockObservation) (resultErr error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.err != nil {
		return x.err
	}
	// A partial write or authentication failure poisons this live root. No
	// later prepare may treat the failed merge as an absent first observation.
	defer func() {
		if resultErr != nil && !errors.Is(resultErr, ErrPricingClockLimit) {
			x.err = resultErr
		}
	}()
	key := sha256.Sum256([]byte(id))
	var path [256]pricingClockStep
	at, digest, leaf, depth, err := x.find(key, &path)
	if err != nil {
		return err
	}
	if x.root != 0 && leaf.key == key && !candidate.SelectedAt.After(time.Unix(leaf.selectedSec, int64(leaf.selectedNano))) {
		return nil
	}
	observed := candidate.ObservedAt.Sub(x.origin)
	if observed < 0 || candidate.SelectedAt.IsZero() {
		return ErrPricingClockCorrupt
	}
	n := pricingClockNode{bit: 256, key: key, selectedSec: candidate.SelectedAt.Unix(), selectedNano: uint32(candidate.SelectedAt.Nanosecond()), observed: int64(observed)}
	if x.root == 0 {
		if x.size > x.max-pricingClockNodeBytes {
			return ErrPricingClockLimit
		}
		at = uint64(x.size)
		digest, err = x.write(at, n)
	} else if leaf.key == key {
		digest, err = x.write(at, n)
	} else {
		if x.size > x.max-2*pricingClockNodeBytes {
			return ErrPricingClockLimit
		}
		split := pricingClockSplit(key, leaf.key)
		// Find the first subtree whose branching bit follows the new split.
		insertDepth := 0
		for insertDepth < depth && path[insertDepth].node.bit < split {
			insertDepth++
		}

		if insertDepth == 0 {
			at, digest = x.root, x.rootHash
		} else {
			parent := path[insertDepth-1].node
			at, digest = parent.left, parent.leftHash
			if pricingClockBranch(key, parent.bit) {
				at, digest = parent.right, parent.rightHash
			}
		}
		newAt := uint64(x.size)
		newHash, writeErr := x.write(newAt, n)
		if writeErr != nil {
			return writeErr
		}
		branch := pricingClockNode{bit: split, left: newAt, right: at, leftHash: newHash, rightHash: digest}
		if pricingClockBranch(key, split) {
			branch.left, branch.right, branch.leftHash, branch.rightHash = at, newAt, digest, newHash
		}
		at = uint64(x.size)
		digest, err = x.write(at, branch)
		depth = insertDepth
	}
	if err != nil {
		return err
	}
	for depth > 0 {
		depth--
		parent := path[depth]
		if pricingClockBranch(key, parent.node.bit) {
			parent.node.right, parent.node.rightHash = at, digest
		} else {
			parent.node.left, parent.node.leftHash = at, digest
		}
		at = parent.at
		digest, err = x.write(at, parent.node)
		if err != nil {
			return err
		}
	}
	x.root, x.rootHash = at, digest
	return nil
}

func (x *pricingClockIndex) close() error {
	if x == nil {
		return nil
	}
	if x.file != nil {
		if err := x.file.Close(); err != nil {
			return err
		}
	}
	x.fileRetired = true
	clear(x.key[:])
	clear(x.rootHash[:])
	clear(x.raw[:])
	x.mac = nil
	fileErr := x.owner.Remove()
	current, err := os.Lstat(x.dir)
	if errors.Is(err, os.ErrNotExist) {
		return fileErr
	}
	if err != nil {
		return errors.Join(fileErr, err)
	}
	if x.dirInfo == nil || !os.SameFile(x.dirInfo, current) {
		return errors.Join(fileErr, ownedfile.ErrReplaced)
	}
	return errors.Join(fileErr, os.Remove(x.dir))
}
