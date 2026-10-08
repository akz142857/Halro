package authenticatedstate

import (
	"bytes"
	"context"
)

// SeekOptions selects the nearest authenticated key at/after Key, or at/before
// Key when Reverse is true. Exclusive omits equality. The fixed-root caller
// supplies its namespace bounds; this operation retains no history or cursor.
type SeekOptions struct {
	Key                []byte
	Reverse, Exclusive bool
}

// CommittedBytes is this pin's authenticated page-pack extent. It describes
// committed bytes, not physical allocation, RSS, or the publisher's later roots.
func (s *Snapshot) CommittedBytes(ctx context.Context) (size int64, resultErr error) {
	permit, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		s.end(ctx, permit, &resultErr)
		if resultErr != nil {
			size = 0
		}
	}()
	return int64(s.current.extent), nil
}

// Seek proves the nearest ordered row or absence from authenticated child
// ranges. It loads one node per level and one value, without scanning preceding
// rows. Missing/corrupt referenced content returns no partial row and retires
// the same pin/Store through the ordinary integrity gate.
func (s *Snapshot) Seek(ctx context.Context, o SeekOptions) (row Row, found bool, resultErr error) {
	if !validKey(o.Key) {
		return Row{}, false, ErrLimit
	}
	permit, err := s.begin(ctx)
	if err != nil {
		return Row{}, false, err
	}
	defer func() {
		s.end(ctx, permit, &resultErr)
		if resultErr != nil {
			row = Row{}
			found = false
		}
	}()
	if s.current.count == 0 {
		return Row{}, false, nil
	}
	q := query{ctx: ctx, snapshot: s, maxReads: maxHeight + 1}
	return q.seek(o)
}

// Seek reads the transaction's current candidate, including earlier mutations.
// Work and returned bytes share the transaction's cumulative limits; an ignored
// integrity or limit failure aborts publication just as Get and Range do.
func (t *Transaction) Seek(o SeekOptions) (Row, bool, error) {
	if err := t.enter(); err != nil {
		return Row{}, false, err
	}
	defer t.mu.Unlock()
	if !validKey(o.Key) || t.readCalls >= t.limits.calls {
		return Row{}, false, t.fail(ErrLimit)
	}
	t.readCalls++
	q := t.query()
	defer func() { t.reads = q.reads }()
	row, found, err := q.seek(o)
	if err != nil {
		return Row{}, false, t.fail(err)
	}
	if len(row.Key)+len(row.Value) > t.limits.returned-t.returnedBytes {
		return Row{}, false, t.fail(ErrLimit)
	}
	t.returnedBytes += len(row.Key) + len(row.Value)
	return row, found, nil
}

func (q *query) seek(o SeekOptions) (Row, bool, error) {
	s := q.snapshot
	if s.current.count == 0 {
		return Row{}, false, nil
	}
	d := descriptor{count: s.current.count, height: s.current.height, ref: s.current.root}
	eligible := func(k []byte) bool {
		n := bytes.Compare(k, o.Key)
		if o.Reverse {
			return n < 0 || n == 0 && !o.Exclusive
		}
		return n > 0 || n == 0 && !o.Exclusive
	}
	for {
		n, err := q.load(d)
		if err != nil {
			return Row{}, false, err
		}
		if n.height == 1 {
			if !eligible(n.key) {
				return Row{}, false, nil
			}
			value, err := q.value(n.value)
			if err != nil {
				return Row{}, false, err
			}
			return Row{Key: bytes.Clone(n.key), Value: value}, true, nil
		}
		next := -1
		if o.Reverse {
			for i := len(n.children) - 1; i >= 0; i-- {
				if eligible(n.children[i].first) {
					next = i
					break
				}
			}
		} else {
			for i := range n.children {
				if eligible(n.children[i].last) {
					next = i
					break
				}
			}
		}
		if next < 0 {
			return Row{}, false, nil
		}
		d = n.children[next]
	}
}
