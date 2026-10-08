package authenticatedstate

import (
	"bytes"
	"context"
)

// floor proves the greatest key <= target by descending the rightmost child
// whose authenticated first key is <= target. Neighbor bounds and complete
// child ordering are checked by load/decode; no ordinary index supplies a hint.
func (q *query) floor(target []byte, maxBytes int) (Row, bool, error) {
	if q.snapshot.current.count == 0 {
		return Row{}, false, nil
	}
	d := descriptor{count: q.snapshot.current.count, height: q.snapshot.current.height, ref: q.snapshot.current.root}
	for {
		n, err := q.load(d)
		if err != nil {
			return Row{}, false, err
		}
		if n.height == 1 {
			if bytes.Compare(n.key, target) > 0 {
				return Row{}, false, nil
			}
			if len(n.key)+int(n.value.length)-1 > maxBytes {
				return Row{}, false, ErrLimit
			}
			value, err := q.value(n.value)
			if err != nil {
				return Row{}, false, err
			}
			return Row{Key: bytes.Clone(n.key), Value: value}, true, nil
		}
		matched := false
		for i := len(n.children) - 1; i >= 0; i-- {
			if bytes.Compare(n.children[i].first, target) <= 0 {
				d, matched = n.children[i], true
				break
			}
		}
		if !matched {
			return Row{}, false, nil
		}
	}
}

// Floor returns the greatest complete row whose key is <= target, inclusive.
// A missing or altered selected page/value is corruption, never absence. The
// result may precede the caller's scope; the caller must check the returned key.
func (s *Snapshot) Floor(ctx context.Context, target []byte) (row Row, found bool, resultErr error) {
	if !validKey(target) {
		return Row{}, false, ErrLimit
	}
	permit, err := s.begin(ctx)
	if err != nil {
		return Row{}, false, err
	}
	defer func() {
		s.end(ctx, permit, &resultErr)
		if resultErr != nil {
			row, found = Row{}, false
		}
	}()
	q := query{ctx: ctx, snapshot: s, maxReads: maxHeight + 1}
	return q.floor(target, MaxKeyBytes+MaxValueBytes)
}

// Floor uses the same callback lifetime and cumulative read budget as Get and
// Range. Ignoring its error still aborts the complete transaction.
func (t *Transaction) Floor(target []byte) (Row, bool, error) {
	if err := t.enter(); err != nil {
		return Row{}, false, err
	}
	defer t.mu.Unlock()
	if !validKey(target) || t.readCalls >= t.limits.calls {
		return Row{}, false, t.fail(ErrLimit)
	}
	t.readCalls++
	q := t.query()
	row, found, err := q.floor(target, t.limits.returned-t.returnedBytes)
	t.reads = q.reads
	if err != nil {
		return Row{}, false, t.fail(err)
	}
	if found {
		t.returnedBytes += len(row.Key) + len(row.Value)
	}
	return row, found, nil
}
