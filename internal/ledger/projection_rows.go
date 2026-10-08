package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"

	bbolt "go.etcd.io/bbolt"
)

// projectionRows is the bounded dependency interface of the existing reducer.
// Both backends preserve absence separately from a failed read. Raw Bolt rows
// are borrowed for the transaction; authenticated rows are verified copies.
type projectionRows interface {
	get(bucket string, key []byte) ([]byte, bool, error)
	put(bucket string, key, value []byte) error
	delete(bucket string, key []byte) error
	floor(bucket string, key []byte) ([]byte, []byte, bool, error)
}

type boltProjectionRows struct{ tx *bbolt.Tx }

func (b boltProjectionRows) bucket(name string) (*bbolt.Bucket, error) {
	bucket := b.tx.Bucket([]byte(name))
	if bucket == nil {
		return nil, ErrProjectionCorrupt
	}
	return bucket, nil
}
func (b boltProjectionRows) get(name string, key []byte) ([]byte, bool, error) {
	bucket, err := b.bucket(name)
	if err != nil {
		return nil, false, err
	}
	value := bucket.Get(key)
	return value, value != nil, nil
}
func (b boltProjectionRows) put(name string, key, value []byte) error {
	bucket, err := b.bucket(name)
	if err != nil {
		return err
	}
	return bucket.Put(key, value)
}
func (b boltProjectionRows) delete(name string, key []byte) error {
	bucket, err := b.bucket(name)
	if err != nil {
		return err
	}
	return bucket.Delete(key)
}
func (b boltProjectionRows) floor(name string, key []byte) ([]byte, []byte, bool, error) {
	bucket, err := b.bucket(name)
	if err != nil {
		return nil, nil, false, err
	}
	cursor := bucket.Cursor()
	k, v := cursor.Seek(key)
	if k == nil || bytes.Compare(k, key) > 0 {
		k, v = cursor.Prev()
	}
	return k, v, k != nil, nil
}

func projectionRowsGet[T any](tx projectionRows, bucket string, key []byte) (T, bool, error) {
	var value T
	if len(key) == 0 {
		return value, false, nil
	}
	raw, found, err := tx.get(bucket, key)
	if err != nil || !found {
		return value, false, err
	}
	value, err = decodeProjectionValue[T](raw)
	if err != nil {
		return value, false, fmt.Errorf("%w: %s: %v", ErrProjectionCorrupt, bucket, err)
	}
	return value, true, nil
}
func projectionRowsPut(tx projectionRows, bucket string, key []byte, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > projectionMaxValue {
		return ErrProjectionPageLimit
	}
	return tx.put(bucket, key, raw)
}
