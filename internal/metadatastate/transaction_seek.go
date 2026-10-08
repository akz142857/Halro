package metadatastate

import (
	"bytes"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// Entry is one immediate member of a Metadata bucket. Bucket entries carry
// no value, matching bbolt's key/bucket namespace without exposing a raw cursor.
type Entry = boltstore.FacadeEntry

// Seek finds the next immediate bucket member. Empty key selects First/Last;
// otherwise Reverse and Exclusive provide Prev/Next/Seek. Authenticated seeks
// skip a nested bucket's entire descendants, and the finite overlay supplies
// staged writes. Work and returned bytes share the transaction's hard limits.
func (t *Transaction) Seek(path []string, key []byte, reverse, exclusive bool) (Entry, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.enter(); err != nil {
		return Entry{}, false, err
	}
	if _, found, err := t.read(path, nil, true); err != nil || !found {
		return Entry{}, false, err
	}
	p, err := facadePath(path)
	if err != nil {
		return Entry{}, false, t.fail(err)
	}
	lo, hi, err := BucketRange(p)
	if err != nil {
		return Entry{}, false, t.fail(err)
	}
	selector := lo
	exclude := true
	if len(key) == 0 {
		if reverse {
			selector = hi
		}
	} else {
		selector, err = RowKey(p, key, false)
		if err != nil {
			return Entry{}, false, t.fail(err)
		}
		if !reverse && !exclusive || reverse && exclusive {
			selector[len(selector)-1] = 0
		}
		exclude = exclusive
	}
	eligible := func(k []byte) bool {
		if len(key) == 0 {
			return true
		}
		n := bytes.Compare(k, key)
		if reverse {
			return n < 0 || n == 0 && !exclusive
		}
		return n > 0 || n == 0 && !exclusive
	}
	better := func(a, b []byte) bool {
		if reverse {
			return bytes.Compare(a, b) > 0
		}
		return bytes.Compare(a, b) < 0
	}
	var best Entry
	foundBest := false
	consider := func(candidate []byte, bucket bool) error {
		if !eligible(candidate) || foundBest && !better(candidate, best.Key) {
			return nil
		}
		var value []byte
		var found bool
		var err error
		if bucket {
			_, found, err = t.read(append(append([]string(nil), path...), string(candidate)), nil, true)
		} else {
			value, found, err = t.read(path, candidate, false)
		}
		if err != nil {
			return err
		}
		if found {
			best = Entry{Key: bytes.Clone(candidate), Value: value, Bucket: bucket}
			foundBest = true
		}
		return nil
	}
	for {
		if err := t.enter(); err != nil {
			return Entry{}, false, err
		}
		row, found, err := t.pin.Seek(t.ctx, authenticatedstate.SeekOptions{Key: selector, Reverse: reverse, Exclusive: exclude})
		if err != nil {
			return Entry{}, false, t.fail(err)
		}
		if !found || bytes.Compare(row.Key, lo) <= 0 || bytes.Compare(row.Key, hi) >= 0 {
			break
		}
		rawPath, rawKey, marker, err := decodeRowKey(row.Key)
		if err != nil || len(rawPath) < len(path) {
			return Entry{}, false, t.fail(ErrSource)
		}
		for i := range path {
			if !bytes.Equal(rawPath[i], p[i]) {
				return Entry{}, false, t.fail(ErrSource)
			}
		}
		bucket := len(rawPath) > len(path)
		candidate := rawKey
		if bucket {
			candidate = rawPath[len(path)]
		} else if marker {
			return Entry{}, false, t.fail(ErrSource)
		}
		if err := consider(candidate, bucket); err != nil {
			return Entry{}, false, err
		}
		if foundBest {
			break
		}
		if bucket {
			child := append(append([][]byte(nil), p...), candidate)
			childLo, childHi, err := BucketRange(child)
			if err != nil {
				return Entry{}, false, t.fail(err)
			}
			if reverse {
				selector, exclude = childLo, true
			} else {
				selector, exclude = childHi, false
			}
		} else {
			selector, exclude = row.Key, true
		}
	}
	for _, op := range t.ops {
		switch {
		case op.Kind == metadatajournal.OpPut && len(op.Path) == len(path) && hasPathPrefix(op.Path, path):
			if err := consider(op.Key, false); err != nil {
				return Entry{}, false, err
			}
		case op.Kind == metadatajournal.OpCreateBucket && len(op.Path) == len(path)+1 && hasPathPrefix(op.Path, path):
			if err := consider([]byte(op.Path[len(path)]), true); err != nil {
				return Entry{}, false, err
			}
		}
	}
	if foundBest {
		if len(best.Key) > metadatajournal.MaxPayloadSize-t.returnedBytes {
			return Entry{}, false, t.fail(authenticatedstate.ErrLimit)
		}
		t.returnedBytes += len(best.Key)
	}
	if t.authority != nil {
		if err := t.authority(); err != nil {
			return Entry{}, false, t.fail(err)
		}
	}
	return best, foundBest, nil
}
