package bolt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	tx     *bbolt.Tx
	facade FacadeTransaction
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
	if t.facade != nil {
		path := []string{string(name)}
		found, err := t.facade.BucketExists(path)
		if err == nil && !found {
			return nil
		}
		return &Bucket{tx: t, path: path}
	}
	bucket := t.tx.Bucket(name)
	if bucket == nil {
		return nil
	}
	return &Bucket{tx: t, bucket: bucket, path: []string{string(name)}}
}

func (t *Tx) CreateBucketIfNotExists(name []byte) (*Bucket, error) {
	if t.facade != nil {
		path := []string{string(name)}
		if err := t.facade.CreateBucket(path); err != nil {
			return nil, err
		}
		if err := t.record(metadatajournal.OpCreateBucket, path, nil, nil, nil); err != nil {
			return nil, err
		}
		return &Bucket{tx: t, path: path}, nil
	}
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
	if t.facade != nil {
		found, err := t.facade.BucketExists([]string{string(name)})
		if err != nil {
			return nil, err
		}
		if found {
			return nil, bbolt.ErrBucketExists
		}
		return t.CreateBucketIfNotExists(name)
	}
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
	if t.facade != nil {
		path := []string{string(name)}
		if err := t.facade.DeleteBucket(path); err != nil {
			return err
		}
		return t.record(metadatajournal.OpDeleteBucket, path, nil, nil, nil)
	}
	if err := t.tx.DeleteBucket(name); err != nil {
		return err
	}
	return t.record(metadatajournal.OpDeleteBucket, []string{string(name)}, nil, nil, nil)
}

func (t *Tx) ForEach(fn func(name []byte, bucket *Bucket) error) error {
	if t.facade != nil {
		names := make([]string, 0, len(bucketClasses)+1)
		names = append(names, "meta")
		for name := range bucketClasses {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			b := t.Bucket([]byte(name))
			if err := t.facade.Error(); err != nil {
				return err
			}
			if b != nil {
				if err := fn([]byte(name), b); err != nil {
					return err
				}
			}
		}
		return t.facade.Error()
	}
	return t.tx.ForEach(func(name []byte, raw *bbolt.Bucket) error {
		return fn(name, &Bucket{tx: t, bucket: raw, path: []string{string(name)}})
	})
}

// ID and WriteTo are read-only views of the transaction itself, used
// by the batch metrics and by snapshot/backup. Nothing they do changes a key,
// so nothing they do is recorded.
func (t *Tx) ID() int {
	if t.facade != nil {
		revision, err := t.facade.Revision()
		if err != nil || revision == 0 || revision > uint64(math.MaxInt) {
			return 0
		}
		// bbolt readers see the last committed transaction, while a writer
		// sees its proposed successor. The authenticated backend pins the old
		// immutable root during preparation; preserve the same DAO fence here.
		if t.recorder != nil {
			if revision == uint64(math.MaxInt) {
				return 0
			}
			revision++
		}
		return int(revision)
	}
	return t.tx.ID()
}
func (t *Tx) WriteTo(w io.Writer) (int64, error) {
	if t.facade != nil {
		return 0, ErrFacadeMaintenance
	}
	return t.tx.WriteTo(w)
}

func (t *Tx) record(kind metadatajournal.OpKind, path []string, key, previous, value []byte) error {
	if t.recorder == nil {
		return nil
	}
	if err := t.recorder.add(kind, path, key, previous, value); err != nil {
		return err
	}
	if len(path) == 1 && path[0] == string(bucketDeploymentPriceProposals) && (kind == metadatajournal.OpPut || kind == metadatajournal.OpDelete) {
		return t.updateProposalOrder(key, previous, value)
	}
	return nil
}

// Bucket is a bbolt bucket that knows where it is, so a write to a nested
// bucket records the path that reaches it rather than only its leaf name.
type Bucket struct {
	tx     *Tx
	bucket *bbolt.Bucket
	path   []string
}

func (b *Bucket) Get(key []byte) []byte {
	if b.tx.facade != nil {
		value, found, err := b.tx.facade.Get(b.path, key)
		if err != nil || !found {
			return nil
		}
		return value
	}
	return b.bucket.Get(key)
}

// Local writes are still classified and checked by record/finish, but their
// previous payload never enters a frame or a confirmation decision. Cloning a
// historical Usage checkpoint just to discard it defeats finite rebuilds.
// Preserve the existing owned copy for every replicated/unknown classification.
func (b *Bucket) previousForRecording(key, raw []byte) []byte {
	if b.tx.recorder == nil {
		return nil
	}
	if class, err := classify(b.path, key); err == nil && (class == classNodeDerived || class == classNodeLocal) {
		return nil
	}
	return slices.Clone(raw)
}

func (b *Bucket) Put(key, value []byte) error {
	previous := b.previousForRecording(key, b.Get(key))
	if b.tx.facade != nil {
		if err := b.tx.facade.Put(b.path, key, value); err != nil {
			return err
		}
		return b.tx.record(metadatajournal.OpPut, b.path, key, previous, value)
	}
	if err := b.bucket.Put(key, value); err != nil {
		return err
	}
	return b.tx.record(metadatajournal.OpPut, b.path, key, previous, value)
}

func (b *Bucket) Delete(key []byte) error {
	previous := b.previousForRecording(key, b.Get(key))
	if b.tx.facade != nil {
		if err := b.tx.facade.Delete(b.path, key); err != nil {
			return err
		}
		return b.tx.record(metadatajournal.OpDelete, b.path, key, previous, nil)
	}
	if err := b.bucket.Delete(key); err != nil {
		return err
	}
	return b.tx.record(metadatajournal.OpDelete, b.path, key, previous, nil)
}

func (b *Bucket) Cursor() *Cursor {
	if b.tx.facade != nil {
		return &Cursor{bucket: b}
	}
	return &Cursor{bucket: b, cursor: b.bucket.Cursor()}
}

// Cursor preserves bbolt's traversal API without handing writable callers a
// raw Delete method that bypasses the metadata recorder.
type Cursor struct {
	bucket *Bucket
	cursor *bbolt.Cursor
	key    []byte
	value  []byte
	edge   int8 // -1 before first; +1 after last; 0 inside or unpositioned
}

func (c *Cursor) remember(key, value []byte) ([]byte, []byte) {
	c.key, c.value = key, value
	c.edge = 0
	return key, value
}

func (c *Cursor) First() ([]byte, []byte) {
	if c.bucket.tx.facade != nil {
		return c.facadeSeek(nil, false, false)
	}
	return c.remember(c.cursor.First())
}
func (c *Cursor) Last() ([]byte, []byte) {
	if c.bucket.tx.facade != nil {
		return c.facadeSeek(nil, true, false)
	}
	return c.remember(c.cursor.Last())
}
func (c *Cursor) Next() ([]byte, []byte) {
	if c.bucket.tx.facade != nil {
		if c.key == nil {
			if c.edge == -1 {
				return c.First()
			}
			return nil, nil
		}
		return c.facadeSeek(c.key, false, true)
	}
	return c.remember(c.cursor.Next())
}
func (c *Cursor) Prev() ([]byte, []byte) {
	if c.bucket.tx.facade != nil {
		if c.key == nil {
			if c.edge == 1 {
				return c.Last()
			}
			return nil, nil
		}
		return c.facadeSeek(c.key, true, true)
	}
	return c.remember(c.cursor.Prev())
}
func (c *Cursor) Seek(prefix []byte) ([]byte, []byte) {
	if c.bucket.tx.facade != nil {
		return c.facadeSeek(prefix, false, false)
	}
	return c.remember(c.cursor.Seek(prefix))
}

func (c *Cursor) facadeSeek(key []byte, reverse, exclusive bool) ([]byte, []byte) {
	entry, found, err := c.bucket.tx.facade.Seek(c.bucket.path, key, reverse, exclusive)
	if err != nil || !found {
		c.key, c.value, c.edge = nil, nil, 0
		if err == nil {
			c.edge = 1
			if reverse {
				c.edge = -1
			}
		}
		return nil, nil
	}
	return c.remember(entry.Key, entry.Value)
}

func (c *Cursor) Delete() error {
	if c.bucket.tx.facade != nil {
		if c.key == nil {
			return bbolt.ErrKeyRequired
		}
		return c.bucket.Delete(c.key)
	}
	if c.key == nil {
		return errors.New("cursor is not positioned on a key")
	}
	key, previous := slices.Clone(c.key), c.bucket.previousForRecording(c.key, c.value)
	if err := c.cursor.Delete(); err != nil {
		return err
	}
	c.key, c.value = nil, nil
	return c.bucket.tx.record(metadatajournal.OpDelete, c.bucket.path, key, previous, nil)
}

func (b *Bucket) ForEach(fn func(key, value []byte) error) error {
	if b.tx.facade != nil {
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if err := fn(k, v); err != nil {
				return err
			}
		}
		return b.tx.facade.Error()
	}
	return b.bucket.ForEach(fn)
}

func (b *Bucket) Stats() bbolt.BucketStats {
	if b.tx.facade != nil {
		var stats bbolt.BucketStats
		_ = b.ForEach(func(k, v []byte) error { stats.KeyN++; return nil })
		return stats
	}
	return b.bucket.Stats()
}

func (b *Bucket) Bucket(name []byte) *Bucket {
	if b.tx.facade != nil {
		path := append(slices.Clone(b.path), string(name))
		found, err := b.tx.facade.BucketExists(path)
		if err == nil && !found {
			return nil
		}
		return &Bucket{tx: b.tx, path: path}
	}
	nested := b.bucket.Bucket(name)
	if nested == nil {
		return nil
	}
	return &Bucket{tx: b.tx, bucket: nested, path: append(slices.Clone(b.path), string(name))}
}

func (b *Bucket) CreateBucketIfNotExists(name []byte) (*Bucket, error) {
	if b.tx.facade != nil {
		path := append(slices.Clone(b.path), string(name))
		if err := b.tx.facade.CreateBucket(path); err != nil {
			return nil, err
		}
		if err := b.tx.record(metadatajournal.OpCreateBucket, path, nil, nil, nil); err != nil {
			return nil, err
		}
		return &Bucket{tx: b.tx, path: path}, nil
	}
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
	if b.tx.facade != nil {
		path := append(slices.Clone(b.path), string(name))
		if err := b.tx.facade.DeleteBucket(path); err != nil {
			return err
		}
		return b.tx.record(metadatajournal.OpDeleteBucket, path, nil, nil, nil)
	}
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
	replicated                map[string]struct{}
	local                     map[string]struct{}
	priceDeployments          map[string]struct{}
	updatingPriceRevisions    bool
	pricingPendingDelta       int64
	updatingPricingPending    bool
	proposalDeployments       map[string]struct{}
	updatingProposalRevisions bool
	updatingProposalOrder     bool
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
	if err := r.trackPriceMutation(kind, path, previous, value); err != nil {
		return err
	}
	if err := r.trackProposalMutation(kind, path, previous, value); err != nil {
		return err
	}
	if err := r.trackPricingPendingMutation(kind, path, previous, value); err != nil {
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
			string(bucketAdminUsers), string(bucketAdminSessions), string(bucketAdminMFAAuthenticators),
			string(bucketAdminMFARecoveryCodes), string(bucketAdminMFAChallenges):
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
		// Creation of a pending authenticator adds no authority. Every later
		// mutation is security-sensitive: it may activate/revoke the factor or
		// advance LastAcceptedTimeStep, whose loss would permit TOTP replay.
		return len(previous) != 0 || json.Unmarshal(value, &record) == nil && record.Status == "revoked"
	case string(bucketAdminMFARecoveryCodes):
		var record struct {
			UsedAt any `json:"used_at"`
		}
		return json.Unmarshal(value, &record) == nil && record.UsedAt != nil
	case string(bucketAdminMFAChallenges):
		// A new challenge is additive. Updates claim it or consume an attempt;
		// losing either transition on promotion reopens a one-time credential.
		return len(previous) != 0
	case string(bucketProviderResources):
		var record struct {
			CreationStatus string `json:"creation_status"`
		}
		// in_flight is the last durable boundary before Provider I/O. It must
		// belong to the confirmed prefix before the upstream can observe the
		// call, otherwise promotion can recover the old reserved record and
		// dispatch the same idempotency key again.
		return json.Unmarshal(value, &record) == nil && record.CreationStatus == "in_flight"
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
