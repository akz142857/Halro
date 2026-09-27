package bolt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/akz142857/Halro/internal/metadatajournal"
	bbolt "go.etcd.io/bbolt"
)

// Tx is a bbolt transaction seen through the recorder.
//
// Every write path in this package takes one of these rather than a *bbolt.Tx,
// which is what makes the journal unbypassable: there is no handle to the
// database that records nothing. The method names are bbolt's own, so the
// callbacks read the way they always did and the difference is in the type
// rather than in the code.
//
// Read paths take one too. They could have kept *bbolt.Tx, but then every
// helper shared between a read and a write path would need two versions, and
// the one that took the raw transaction would be a way to write without
// recording — reachable by accident rather than by decision.
type Tx struct {
	tx *bbolt.Tx
	// recorder is nil for a read-only transaction, and for the two writable
	// transactions that deliberately do not record: schema migration and
	// initial bucket creation. Both are whole-projection events, published as
	// a new journal epoch rather than described operation by operation — see
	// metadatajournal's package comment.
	recorder *recorder
}

// Bucket returns a top-level bucket, or nil if it does not exist. Callers rely
// on the nil, the way they do with bbolt.
func (t *Tx) Bucket(name []byte) *Bucket {
	bucket := t.tx.Bucket(name)
	if bucket == nil {
		return nil
	}
	return &Bucket{tx: t, bucket: bucket, path: []string{string(name)}}
}

func (t *Tx) CreateBucketIfNotExists(name []byte) (*Bucket, error) {
	bucket, err := t.tx.CreateBucketIfNotExists(name)
	if err != nil {
		return nil, err
	}
	path := []string{string(name)}
	if err := t.record(metadatajournal.OpCreateBucket, path, nil, nil, nil); err != nil {
		return nil, err
	}
	return &Bucket{tx: t, bucket: bucket, path: path}, nil
}

func (t *Tx) CreateBucket(name []byte) (*Bucket, error) {
	bucket, err := t.tx.CreateBucket(name)
	if err != nil {
		return nil, err
	}
	path := []string{string(name)}
	if err := t.record(metadatajournal.OpCreateBucket, path, nil, nil, nil); err != nil {
		return nil, err
	}
	return &Bucket{tx: t, bucket: bucket, path: path}, nil
}

func (t *Tx) DeleteBucket(name []byte) error {
	if err := t.tx.DeleteBucket(name); err != nil {
		return err
	}
	return t.record(metadatajournal.OpDeleteBucket, []string{string(name)}, nil, nil, nil)
}

func (t *Tx) ForEach(fn func(name []byte, bucket *Bucket) error) error {
	return t.tx.ForEach(func(name []byte, raw *bbolt.Bucket) error {
		return fn(name, &Bucket{tx: t, bucket: raw, path: []string{string(name)}})
	})
}

// ID, WriteTo and CopyFile are read-only views of the transaction itself, used
// by the batch metrics and by snapshot/backup. Nothing they do changes a key,
// so nothing they do is recorded.
func (t *Tx) ID() int                            { return t.tx.ID() }
func (t *Tx) WriteTo(w io.Writer) (int64, error) { return t.tx.WriteTo(w) }
func (t *Tx) CopyFile(path string, mode os.FileMode) error {
	return t.tx.CopyFile(path, mode)
}

func (t *Tx) record(kind metadatajournal.OpKind, path []string, key, previous, value []byte) error {
	if t.recorder == nil {
		return nil
	}
	return t.recorder.add(kind, path, key, previous, value)
}

// Bucket is a bbolt bucket that knows where it is, so a write to a nested
// bucket records the path that reaches it rather than only its leaf name.
type Bucket struct {
	tx     *Tx
	bucket *bbolt.Bucket
	path   []string
}

func (b *Bucket) Get(key []byte) []byte { return b.bucket.Get(key) }

func (b *Bucket) Put(key, value []byte) error {
	previous := slices.Clone(b.bucket.Get(key))
	if err := b.bucket.Put(key, value); err != nil {
		return err
	}
	return b.tx.record(metadatajournal.OpPut, b.path, key, previous, value)
}

func (b *Bucket) Delete(key []byte) error {
	previous := slices.Clone(b.bucket.Get(key))
	if err := b.bucket.Delete(key); err != nil {
		return err
	}
	return b.tx.record(metadatajournal.OpDelete, b.path, key, previous, nil)
}

func (b *Bucket) Cursor() *Cursor { return &Cursor{bucket: b, cursor: b.bucket.Cursor()} }

// Cursor preserves bbolt's traversal API without handing writable callers a
// raw Delete method that bypasses the metadata recorder.
type Cursor struct {
	bucket *Bucket
	cursor *bbolt.Cursor
	key    []byte
	value  []byte
}

func (c *Cursor) remember(key, value []byte) ([]byte, []byte) {
	c.key, c.value = key, value
	return key, value
}

func (c *Cursor) First() ([]byte, []byte) { return c.remember(c.cursor.First()) }
func (c *Cursor) Last() ([]byte, []byte)  { return c.remember(c.cursor.Last()) }
func (c *Cursor) Next() ([]byte, []byte)  { return c.remember(c.cursor.Next()) }
func (c *Cursor) Prev() ([]byte, []byte)  { return c.remember(c.cursor.Prev()) }
func (c *Cursor) Seek(prefix []byte) ([]byte, []byte) {
	return c.remember(c.cursor.Seek(prefix))
}

func (c *Cursor) Delete() error {
	if c.key == nil {
		return errors.New("cursor is not positioned on a key")
	}
	key, previous := slices.Clone(c.key), slices.Clone(c.value)
	if err := c.cursor.Delete(); err != nil {
		return err
	}
	c.key, c.value = nil, nil
	return c.bucket.tx.record(metadatajournal.OpDelete, c.bucket.path, key, previous, nil)
}

func (b *Bucket) ForEach(fn func(key, value []byte) error) error { return b.bucket.ForEach(fn) }

func (b *Bucket) Stats() bbolt.BucketStats { return b.bucket.Stats() }

func (b *Bucket) Bucket(name []byte) *Bucket {
	nested := b.bucket.Bucket(name)
	if nested == nil {
		return nil
	}
	return &Bucket{tx: b.tx, bucket: nested, path: append(slices.Clone(b.path), string(name))}
}

func (b *Bucket) CreateBucketIfNotExists(name []byte) (*Bucket, error) {
	nested, err := b.bucket.CreateBucketIfNotExists(name)
	if err != nil {
		return nil, err
	}
	path := append(slices.Clone(b.path), string(name))
	if err := b.tx.record(metadatajournal.OpCreateBucket, path, nil, nil, nil); err != nil {
		return nil, err
	}
	return &Bucket{tx: b.tx, bucket: nested, path: path}, nil
}

func (b *Bucket) DeleteBucket(name []byte) error {
	if err := b.bucket.DeleteBucket(name); err != nil {
		return err
	}
	return b.tx.record(metadatajournal.OpDeleteBucket, append(slices.Clone(b.path), string(name)), nil, nil, nil)
}

// recorder collects one transaction's replicated operations and watches for the
// one thing a transaction may not do: write both the replicated set and the
// node-local set (§5.2, §6.1.2).
//
// It classifies as it goes rather than at the end, so an unclassified bucket
// fails the write that touched it and names it — instead of failing a
// transaction that has already done its work and leaving the caller to find out
// which of its twenty puts was the problem.
type recorder struct {
	ops                  []metadatajournal.Op
	requiresConfirmation bool
	// The two bucket sets a mixed-write check needs. Sets rather than counts:
	// the exception in §5.2 is about which buckets, not how many.
	replicated map[string]struct{}
	local      map[string]struct{}
}

func newRecorder() *recorder {
	return &recorder{
		replicated: make(map[string]struct{}, 4),
		local:      make(map[string]struct{}, 2),
	}
}

func (r *recorder) add(kind metadatajournal.OpKind, path []string, key, previous, value []byte) error {
	class, err := classify(path, key)
	if err != nil {
		return err
	}
	switch {
	case class == classJournalBookkeeping:
		// The entry writes these itself, on the raw transaction. Reaching here
		// means a caller wrote the journal's own position through the recorder,
		// which would have a frame describe itself.
		return fmt.Errorf("the metadata journal's own position is not a caller's to write: %q", key)
	case class.replicated():
		r.replicated[writeIdentity(path, key)] = struct{}{}
	default:
		r.local[writeIdentity(path, key)] = struct{}{}
		return nil
	}
	r.ops = append(r.ops, metadatajournal.Op{
		Kind: kind, Path: slices.Clone(path),
		// bbolt's byte slices are only valid for the life of the transaction,
		// and a caller's value slice may be reused after it returns. The frame
		// is written before the commit but the copy costs nothing next to the
		// fsync, and sharing would be a bug that only appears under load.
		Key: slices.Clone(key), Value: slices.Clone(value),
	})
	if metadataOpRequiresConfirmation(kind, path, previous, value) {
		r.requiresConfirmation = true
	}
	return nil
}

// metadataOpRequiresConfirmation identifies only authority-withdrawing
// mutations: losing one during failover would reopen access. Additive or
// ordinary metadata remains asynchronous.
func metadataOpRequiresConfirmation(kind metadatajournal.OpKind, path []string, previous, value []byte) bool {
	if len(path) != 1 {
		return false
	}
	bucket := path[0]
	if kind == metadatajournal.OpDelete {
		switch bucket {
		case string(bucketCredentials), string(bucketGatewayKeys), string(bucketGatewayKeyHash),
			string(bucketAdminUsers), string(bucketAdminMFAAuthenticators), string(bucketAdminMFARecoveryCodes):
			return true
		}
		return false
	}
	if kind != metadatajournal.OpPut {
		return false
	}
	switch bucket {
	case string(bucketGatewayKeys):
		var record struct {
			Enabled   bool `json:"enabled"`
			DeletedAt any  `json:"deleted_at"`
		}
		// Existing keys may lose scopes or receive an earlier expiry. Treat every
		// update as authority-sensitive; key writes are operator-scale and a
		// field-by-field partial order would be fragile as the model grows.
		return json.Unmarshal(value, &record) == nil && (!record.Enabled || record.DeletedAt != nil || len(previous) != 0)
	case string(bucketProjects):
		var record struct {
			Enabled   bool `json:"enabled"`
			DeletedAt any  `json:"deleted_at"`
		}
		return json.Unmarshal(value, &record) == nil && (!record.Enabled || record.DeletedAt != nil || len(previous) != 0)
	case string(bucketTokenGuardPolicies), string(bucketRedactionPolicies):
		// These records are authorization filters. A field-by-field partial
		// order is brittle: adding a rule, lowering a limit, enabling a policy
		// or changing action can all tighten access. They are operator-scale
		// writes, so conservatively confirm every put rather than risk teaching
		// a future field that its loss is safe.
		return true
	case string(bucketCredentials):
		var record struct {
			Revision uint64 `json:"revision"`
		}
		// Revision 1 creates authority; later puts rotate or replace it.
		return json.Unmarshal(value, &record) == nil && record.Revision > 1
	case string(bucketAdminUsers):
		var record struct {
			Role string `json:"role"`
		}
		return json.Unmarshal(value, &record) == nil && (record.Role == "read_only" || len(previous) != 0)
	case string(bucketAdminMFAAuthenticators):
		var record struct {
			Status string `json:"status"`
		}
		return json.Unmarshal(value, &record) == nil && record.Status == "revoked"
	case string(bucketAdminMFARecoveryCodes):
		var record struct {
			UsedAt any `json:"used_at"`
		}
		return json.Unmarshal(value, &record) == nil && record.UsedAt != nil
	}
	return false
}

func (r *recorder) finish() error {
	return mixedWriteAllowed(r.replicated, r.local)
}

// ErrJournalUnavailable says a write that must be recorded arrived before the
// journal was attached, or after it failed.
//
// It is fail-closed on purpose. The alternative — commit the bbolt transaction
// and carry on — is a write that exists in the projection and in no log, which
// is precisely the state the journal exists to make impossible, and it would be
// invisible until something tried to replicate or replay it.
var ErrJournalUnavailable = errors.New("metadata journal is not available")
