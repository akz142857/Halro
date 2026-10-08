package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ownedfile"
	bolt "go.etcd.io/bbolt"
)

// Observations precede the size-limited manifest, so their count has no RAM
// bound. A private append-only disk dictionary stores these authenticated facts.
// It is rebuilt per Verify and grants no saved-index or archive authority.
const backupManifestSizeLimit = 1 << 20
const observationHeaderBytes = 8 + 8 + sha256.Size

var observationBucket = []byte("observed")
var errObservationIndex = errors.New("backup observation index changed")

type observationIndex struct {
	db       *bolt.DB
	owner    *ownedfile.Owner
	identity os.FileInfo
	key      [32]byte
	count    uint64
	closed   bool
	path     string
	release  func() error
	closeErr error
}

// Single-operation owner: all Add/Get/Close calls are serial in Verify. No
// shared cache or goroutine survives the operation. NoSync is for this disposable
// derivative only; it never publishes the archive or replaces its AEAD ending.
func newObservationIndex(directory string) (*observationIndex, error) {
	return newObservationIndexWithLimit(directory, 0)
}

func newObservationIndexWithLimit(directory string, maxBytes int64) (result *observationIndex, err error) {
	return newObservationIndexForWorkspace(context.Background(), directory, maxBytes)
}

func newObservationIndexForWorkspace(ctx context.Context, directory string, maxBytes int64) (result *observationIndex, err error) {
	return newObservationIndexWithCreate(ctx, directory, maxBytes, ownedfile.Create)
}

func newObservationIndexWithCreate(ctx context.Context, directory string, maxBytes int64, create func(string, int, os.FileMode) (*os.File, *ownedfile.Owner, error)) (result *observationIndex, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	budget := backupWorkspace(ctx)
	if budget != nil {
		if maxBytes != 0 && maxBytes != budget.bytes {
			return nil, errors.New("backup index changed its admitted file ceiling")
		}
		maxBytes = budget.bytes
	}
	if maxBytes < 0 || uint64(maxBytes) > uint64(int(^uint(0)>>1)) {
		return nil, errors.New("backup observation index limit is invalid")
	}
	release, err := budget.acquire(ctx)
	if err != nil {
		return nil, err
	}
	index := &observationIndex{release: release}
	defer func() {
		if result == nil {
			if errors.Is(err, ownedfile.ErrRetirement) {
				index.closeErr = errors.Join(index.closeErr, err)
			}
			err = errors.Join(err, index.Close())
		}
	}()
	if _, err = rand.Read(index.key[:]); err != nil {
		return nil, err
	}
	name, err := id.New("halro-backup-observation")
	if err != nil {
		return nil, err
	}
	filename := filepath.Join(directory, name+".db")
	index.path = filename
	handedToEngine := false
	options := &bolt.Options{
		NoSync: true, NoFreelistSync: true, FreelistType: bolt.FreelistArrayType,
		OpenFile: func(path string, flags int, mode os.FileMode) (*os.File, error) {
			file, owner, err := create(path, flags, mode)
			if err != nil {
				return nil, err
			}
			index.owner = owner
			index.identity, err = file.Stat()
			if err != nil {
				closeErr := file.Close()
				if closeErr != nil {
					closeErr = errors.Join(ownedfile.ErrRetirement, closeErr)
				}
				return nil, errors.Join(err, closeErr)
			}
			handedToEngine = true
			return file, nil
		},
	}
	if maxBytes > 0 {
		options.MaxSize = int(maxBytes)
	}
	index.db, err = bolt.Open(filename, 0600, options)
	if err != nil {
		// Failed bbolt Open discards its internal Close error. Once its
		// mmap lifecycle began, no nil DB proves physical retirement.
		if handedToEngine {
			err = errors.Join(err, ownedfile.ErrRetirement)
		}
		return nil, err
	}
	err = index.db.Update(func(tx *bolt.Tx) error { _, err := tx.CreateBucket(observationBucket); return err })
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return index, nil
}

func (i *observationIndex) check() error {
	if i.closed || i.db == nil {
		return errors.New("backup observation index is closed")
	}
	named, err := os.Lstat(i.db.Path())
	if err != nil || !os.SameFile(i.identity, named) {
		return errors.Join(errObservationIndex, err)
	}
	return nil
}
func (i *observationIndex) mac(pathDigest [32]byte, value []byte) [32]byte {
	mac := hmac.New(sha256.New, i.key[:])
	_, _ = mac.Write([]byte("halro:backup-observation:v1\x00"))
	_, _ = mac.Write(pathDigest[:])
	_, _ = mac.Write(value)
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}
func (i *observationIndex) Add(file File) error {
	if err := i.check(); err != nil {
		return err
	}
	// A longer name cannot be declared in the existing size-limited manifest.
	// Reject that impossible match before allocating an index record for it.
	if len(file.Path) > backupManifestSizeLimit || file.Size < 0 || !validSHA256(file.SHA256) || i.count == math.MaxUint64 {
		return errors.New("invalid backup observation")
	}
	digest := sha256.Sum256([]byte(file.Path))
	value := make([]byte, observationHeaderBytes+len(file.Path)+sha256.Size)
	binary.BigEndian.PutUint64(value[:8], i.count+1)
	binary.BigEndian.PutUint64(value[8:16], uint64(file.Size))
	_, _ = hex.Decode(value[16:observationHeaderBytes], []byte(file.SHA256))
	copy(value[observationHeaderBytes:], file.Path)
	signature := i.mac(digest, value[:len(value)-sha256.Size])
	copy(value[len(value)-sha256.Size:], signature[:])
	err := i.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(observationBucket)
		if bucket == nil {
			return errObservationIndex
		}
		if bucket.Get(digest[:]) != nil {
			return fmt.Errorf("duplicate backup entry %q", file.Path)
		}
		return bucket.Put(digest[:], value)
	})
	if err != nil {
		return err
	}
	i.count++
	return i.check()
}
func (i *observationIndex) Get(name string) (File, bool, error) {
	if err := i.check(); err != nil {
		return File{}, false, err
	}
	if len(name) > backupManifestSizeLimit {
		return File{}, false, errors.New("backup path exceeds manifest limit")
	}
	digest := sha256.Sum256([]byte(name))
	var result File
	found := false
	err := i.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(observationBucket)
		if bucket == nil {
			return errObservationIndex
		}
		value := bucket.Get(digest[:])
		if value == nil {
			return nil
		}
		if len(value) < observationHeaderBytes+sha256.Size || len(value) > observationHeaderBytes+backupManifestSizeLimit+sha256.Size {
			return errObservationIndex
		}
		body := value[:len(value)-sha256.Size]
		expected := i.mac(digest, body)
		if !hmac.Equal(expected[:], value[len(body):]) {
			return errObservationIndex
		}
		ordinal := binary.BigEndian.Uint64(body[:8])
		size := binary.BigEndian.Uint64(body[8:16])
		if ordinal == 0 || ordinal > i.count || size > math.MaxInt64 || string(body[observationHeaderBytes:]) != name {
			return errObservationIndex
		}
		result = File{Path: name, Size: int64(size), SHA256: hex.EncodeToString(body[16:observationHeaderBytes])}
		found = true
		return nil
	})
	if err != nil {
		return File{}, false, err
	}
	if err := i.check(); err != nil {
		return File{}, false, err
	}
	return result, found, nil
}
func (i *observationIndex) Close() error {
	if i == nil {
		return nil
	}
	if i.closed {
		return i.closeErr
	}
	i.closed = true
	if i.db != nil {
		closeErr := i.db.Close()
		if closeErr != nil {
			closeErr = errors.Join(ownedfile.ErrRetirement, closeErr)
		}
		i.closeErr = errors.Join(i.closeErr, closeErr)
		if i.closeErr == nil {
			i.db = nil
		}
	}
	clear(i.key[:])
	if i.closeErr == nil && i.owner != nil {
		// A disappeared name can be a renamed live file, not deleted bytes.
		// Only a matching name followed by our unlink can retire this fee.
		named, err := os.Lstat(i.path)
		if err != nil || i.identity == nil || !os.SameFile(i.identity, named) {
			i.closeErr = errors.Join(ownedfile.ErrReplaced, errObservationIndex, err)
		} else {
			i.closeErr = i.owner.Remove()
		}
	}
	if i.closeErr == nil && i.release != nil {
		i.closeErr = i.release()
		if i.closeErr == nil {
			i.release = nil
		}
	}
	return i.closeErr
}

// The old manifest itself is capped at 1 MiB. Sorting bounded integer offsets
// detects duplicate expected paths without mutating its returned order or the
// immutable observation records. Matching never deletes disk facts or trusts a
// mutable "consumed" flag that an old index value could roll back.
func matchManifestObservations(manifest Manifest, index *observationIndex) error {
	if index.count != uint64(len(manifest.Files)) {
		return errors.New("backup file set does not match manifest")
	}
	order := make([]int, len(manifest.Files))
	for n := range order {
		order[n] = n
	}
	sort.Slice(order, func(a, b int) bool { return manifest.Files[order[a]].Path < manifest.Files[order[b]].Path })
	for n, position := range order {
		expected := manifest.Files[position]
		if n > 0 && expected.Path == manifest.Files[order[n-1]].Path {
			return fmt.Errorf("duplicate backup manifest path %q", expected.Path)
		}
		actual, exists, err := index.Get(expected.Path)
		if err != nil {
			return err
		}
		if !exists || actual != expected {
			return fmt.Errorf("backup checksum mismatch for %q", expected.Path)
		}
	}
	return index.check()
}

// Walk's order is private digest-key order, not archive path order. Each
// callback runs after View retirement, with one authenticated owned record.
func (i *observationIndex) Walk(ctx context.Context, consume func(File) error) error {
	var after []byte
	var count uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := i.check(); err != nil {
			return err
		}
		var next []byte
		var name string
		err := i.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(observationBucket)
			if b == nil {
				return errObservationIndex
			}
			c := b.Cursor()
			k, v := c.First()
			if after != nil {
				k, v = c.Seek(after)
				if bytes.Equal(k, after) {
					k, v = c.Next()
				}
			}
			if k == nil {
				return nil
			}
			if len(k) != sha256.Size || len(v) < observationHeaderBytes+sha256.Size || len(v) > observationHeaderBytes+backupManifestSizeLimit+sha256.Size {
				return errObservationIndex
			}
			var digest [32]byte
			copy(digest[:], k)
			body := v[:len(v)-sha256.Size]
			expected := i.mac(digest, body)
			if !hmac.Equal(expected[:], v[len(body):]) {
				return errObservationIndex
			}
			name = string(body[observationHeaderBytes:])
			if sha256.Sum256([]byte(name)) != digest {
				return errObservationIndex
			}
			next = bytes.Clone(k)
			return nil
		})
		if err != nil {
			return err
		}
		if next == nil {
			break
		}
		file, found, err := i.Get(name)
		if err != nil {
			return err
		}
		if !found || count >= i.count {
			return errObservationIndex
		}
		if err := consume(file); err != nil {
			return err
		}
		count++
		after = next
	}
	if count != i.count {
		return errObservationIndex
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return i.check()
}
