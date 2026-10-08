package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	bolt "go.etcd.io/bbolt"
)

const sourceCatalogRecordLimit = 64 << 10

var sourceBucket = []byte("backup-sources")

// SourceCatalog owns a private, disposable sorted directory. Calls are serial;
// Walk callbacks must not reenter it. It authenticates operation-local facts,
// not persistent source contents or a saved snapshot. Create consumes it and
// closes/removes it before publishing the archive. Otherwise the caller closes it.
type SourceCatalog struct {
	index   *observationIndex
	walking bool
}

func NewSourceCatalog(directory string) (*SourceCatalog, error) {
	return NewSourceCatalogWithLimit(directory, 0)
}

// NewSourceCatalogWithLimit optionally caps the disposable catalog data file.
// A zero limit preserves the legacy unlimited behavior.
func NewSourceCatalogWithLimit(directory string, maxBytes int64) (*SourceCatalog, error) {
	return NewSourceCatalogWithContext(context.Background(), directory, maxBytes)
}

func NewSourceCatalogWithContext(ctx context.Context, directory string, maxBytes int64) (*SourceCatalog, error) {
	index, err := newObservationIndexForWorkspace(ctx, directory, maxBytes)
	if err != nil {
		return nil, err
	}
	if err := index.db.Update(func(tx *bolt.Tx) error { _, err := tx.CreateBucket(sourceBucket); return err }); err != nil {
		return nil, errors.Join(err, index.Close())
	}
	return &SourceCatalog{index: index}, nil
}

func (c *SourceCatalog) signature(key, body []byte) []byte {
	mac := hmac.New(sha256.New, c.index.key[:])
	_, _ = mac.Write([]byte("halro:backup-source:v1\x00"))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(key)))
	_, _ = mac.Write(length[:])
	_, _ = mac.Write(key)
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

func (c *SourceCatalog) decode(key, value []byte) (SourceFile, error) {
	if len(key) == 0 || len(key) > bolt.MaxKeySize || len(value) < 8+sha256.Size || len(key)+len(value) > sourceCatalogRecordLimit {
		return SourceFile{}, errObservationIndex
	}
	body := value[:len(value)-sha256.Size]
	if !hmac.Equal(c.signature(key, body), value[len(body):]) {
		return SourceFile{}, errObservationIndex
	}
	ordinal := binary.BigEndian.Uint64(body[:8])
	if ordinal == 0 || ordinal > c.index.count || len(body) == 8 {
		return SourceFile{}, errObservationIndex
	}
	return SourceFile{ArchivePath: string(key), LocalPath: string(body[8:])}, nil
}

// Add rejects duplicate archive names. AddShared allows an exact identical
// reference, for multiple provider resources naming the same immutable object.
func (c *SourceCatalog) Add(source SourceFile) error       { return c.add(source, false) }
func (c *SourceCatalog) AddShared(source SourceFile) error { return c.add(source, true) }
func (c *SourceCatalog) add(source SourceFile, shared bool) error {
	if c == nil || c.index == nil {
		return errors.New("backup source catalog is absent")
	}
	if c.walking {
		return errors.New("backup source catalog walk is active")
	}
	if err := c.index.check(); err != nil {
		return err
	}
	clean, err := safeArchivePath(source.ArchivePath)
	if err != nil {
		return err
	}
	if clean == "manifest.json" {
		return fmt.Errorf("reserved backup path %q", clean)
	}
	if len(clean) > bolt.MaxKeySize || source.LocalPath == "" || len(clean)+len(source.LocalPath)+8+sha256.Size > sourceCatalogRecordLimit || c.index.count == math.MaxUint64 {
		return errors.New("backup source exceeds catalog record limit")
	}
	key := []byte(clean)
	inserted := false
	err = c.index.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(sourceBucket)
		if bucket == nil {
			return errObservationIndex
		}
		if value := bucket.Get(key); value != nil {
			old, err := c.decode(key, value)
			if err != nil {
				return err
			}
			if shared && old == source {
				return nil
			}
			return fmt.Errorf("duplicate backup path %q", clean)
		}
		body := make([]byte, 8+len(source.LocalPath), 8+len(source.LocalPath)+sha256.Size)
		binary.BigEndian.PutUint64(body[:8], c.index.count+1)
		copy(body[8:], source.LocalPath)
		value := append(body, c.signature(key, body)...)
		if err := bucket.Put(key, value); err != nil {
			return err
		}
		inserted = true
		return nil
	})
	if err != nil {
		return err
	}
	if inserted {
		c.index.count++
	}
	return c.index.check()
}

// Walk releases every read transaction before the consumer performs file IO.
// One authenticated <=64KiB record and one cursor are live at a time. Callback
// effects are tentative until the final identity and native-count checks pass.
func (c *SourceCatalog) Walk(ctx context.Context, consume func(SourceFile) error) error {
	if c == nil || c.index == nil || consume == nil {
		return errors.New("backup source catalog and consumer are required")
	}
	if c.walking {
		return errors.New("backup source catalog walk is active")
	}
	if err := c.index.check(); err != nil {
		return err
	}
	c.walking = true
	defer func() { c.walking = false }()
	var after []byte
	var count uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.index.check(); err != nil {
			return err
		}
		var source SourceFile
		var next []byte
		err := c.index.db.View(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(sourceBucket)
			if bucket == nil {
				return errObservationIndex
			}
			cursor := bucket.Cursor()
			key, value := cursor.First()
			if after != nil {
				key, value = cursor.Seek(after)
				if bytes.Equal(key, after) {
					key, value = cursor.Next()
				}
			}
			if key == nil {
				return nil
			}
			var err error
			source, err = c.decode(key, value)
			if err != nil {
				return err
			}
			next = bytes.Clone(key)
			return nil
		})
		if err != nil {
			return err
		}
		if next == nil {
			break
		}
		if count >= c.index.count {
			return errObservationIndex
		}
		if err := consume(source); err != nil {
			return err
		}
		count++
		after = next
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if count != c.index.count {
		return errObservationIndex
	}
	return c.index.check()
}
func (c *SourceCatalog) Close() error {
	if c == nil {
		return nil
	}
	if c.walking {
		return errors.New("backup source catalog walk is active")
	}
	return c.index.Close()
}
