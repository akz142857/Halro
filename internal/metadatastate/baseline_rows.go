package metadatastate

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	bbolt "go.etcd.io/bbolt"
)

const maxBaselineDepth = 32

// RowKey preserves bbolt byte order and bucket hierarchy without ambiguous path
// separators. A bucket marker sorts before every descendant and commits its
// existence and sequence, including empty buckets. Keys and bucket names are
// arbitrary bytes; zero is escaped, not interpreted as a slash or terminator.
func RowKey(path [][]byte, key []byte, bucket bool) ([]byte, error) {
	if len(path) == 0 || len(path) > maxBaselineDepth || bucket && len(key) != 0 || !bucket && len(key) == 0 {
		return nil, authenticatedstate.ErrLimit
	}
	b := []byte{1} // codec version
	add := func(value []byte) error {
		if len(value) == 0 {
			return authenticatedstate.ErrLimit
		}
		for _, c := range value {
			if len(b) > authenticatedstate.MaxKeyBytes-4 {
				return authenticatedstate.ErrLimit
			}
			b = append(b, c)
			if c == 0 {
				b = append(b, 255)
			}
		}
		b = append(b, 0, 0)
		return nil
	}
	for _, component := range path {
		if err := add(component); err != nil {
			return nil, err
		}
	}
	if bucket {
		b = append(b, 0)
	} else {
		if err := add(key); err != nil {
			return nil, err
		}
		b = append(b, 1)
	}
	if len(b) > authenticatedstate.MaxKeyBytes {
		return nil, authenticatedstate.ErrLimit
	}
	return b, nil
}

type baselineFrame struct {
	bucket  *bbolt.Bucket
	cursor  *bbolt.Cursor
	started bool
}
type baselineIterator struct {
	ctx    context.Context
	tx     *bbolt.Tx
	frames [maxBaselineDepth + 1]baselineFrame
	path   [maxBaselineDepth][]byte
	names  [maxBaselineDepth]string
	depth  int
	done   bool
}

func newBaselineIterator(ctx context.Context, tx *bbolt.Tx) *baselineIterator {
	i := &baselineIterator{ctx: ctx, tx: tx}
	i.frames[0].cursor = tx.Cursor()
	return i
}
func (i *baselineIterator) next() (authenticatedstate.Row, bool, error) {
	for !i.done {
		if err := i.ctx.Err(); err != nil {
			return authenticatedstate.Row{}, false, err
		}
		frame := &i.frames[i.depth]
		var k, v []byte
		if frame.started {
			k, v = frame.cursor.Next()
		} else {
			k, v = frame.cursor.First()
			frame.started = true
		}
		if k == nil {
			if i.depth == 0 {
				i.done = true
				break
			}
			i.frames[i.depth] = baselineFrame{}
			i.depth--
			i.path[i.depth], i.names[i.depth] = nil, ""
			continue
		}
		var child *bbolt.Bucket
		if i.depth == 0 {
			child = i.tx.Bucket(k)
		} else {
			child = frame.bucket.Bucket(k)
		}
		if child != nil {
			if i.depth >= maxBaselineDepth {
				return authenticatedstate.Row{}, false, authenticatedstate.ErrLimit
			}
			i.path[i.depth], i.names[i.depth] = k, string(k)
			if _, err := boltstore.MetadataBaselineClass(i.names[:i.depth+1], nil); err != nil {
				return authenticatedstate.Row{}, false, err
			}
			key, err := RowKey(i.path[:i.depth+1], nil, true)
			if err != nil {
				return authenticatedstate.Row{}, false, err
			}
			i.depth++
			i.frames[i.depth] = baselineFrame{bucket: child, cursor: child.Cursor()}
			return authenticatedstate.Row{Key: key, Value: binary.BigEndian.AppendUint64(nil, child.Sequence())}, true, nil
		}
		if i.depth == 0 {
			return authenticatedstate.Row{}, false, ErrSource
		}
		if _, err := boltstore.MetadataBaselineClass(i.names[:i.depth], k); err != nil {
			return authenticatedstate.Row{}, false, err
		}
		if len(v) > authenticatedstate.MaxValueBytes {
			return authenticatedstate.Row{}, false, authenticatedstate.ErrLimit
		}
		key, err := RowKey(i.path[:i.depth], k, false)
		if err != nil {
			return authenticatedstate.Row{}, false, err
		}
		// The source transaction remains open during the fixed-depth stream; the
		// authenticated builder copies this one bounded value before Next.
		return authenticatedstate.Row{Key: key, Value: v}, true, nil
	}
	return authenticatedstate.Row{}, false, nil
}

func verifyMetadataIdentity(tx *bbolt.Tx, expected boltstore.MetadataInfo, instanceID string) error {
	meta := tx.Bucket([]byte("meta"))
	if meta == nil || uint64(tx.ID()) != expected.TxID || string(meta.Get([]byte("instance_id"))) != instanceID {
		return ErrSource
	}
	decode := func(name string, optional bool) (uint64, error) {
		raw := meta.Get([]byte(name))
		if optional && raw == nil {
			return 0, nil
		}
		if len(raw) != 8 {
			return 0, fmt.Errorf("%w: invalid %s", ErrSource, name)
		}
		return binary.BigEndian.Uint64(raw), nil
	}
	schema, err := decode("schema_version", false)
	if err != nil {
		return err
	}
	epoch, err := decode("metadata_journal_epoch", true)
	if err != nil {
		return err
	}
	sequence, err := decode("applied_journal_sequence", true)
	if err != nil {
		return err
	}
	feature := meta.Get([]byte("ledger_feature_epoch"))
	if feature != nil && len(feature) != 1 {
		return ErrSource
	}
	var featureEpoch uint8
	if len(feature) == 1 {
		featureEpoch = feature[0]
	}
	actual := boltstore.MetadataInfo{SchemaVersion: schema, TxID: uint64(tx.ID()), MinimumLedgerReaderVersion: string(meta.Get([]byte("minimum_ledger_reader_version"))), LedgerFeatureEpoch: featureEpoch, MetadataJournalEpoch: epoch, MetadataJournalSequence: sequence}
	if actual != expected {
		return fmt.Errorf("%w: archived metadata differs from authenticated manifest", ErrSource)
	}
	return nil
}
