package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

const maxQueryPages = 4096
const maxRangeRows = 64
const maxRangeBytes = 8 << 20

type query struct {
	ctx             context.Context
	snapshot        *Snapshot
	reads, maxReads int
	pending         *txPending // a transaction's unwritten nodes, read from memory
}

func (q *query) read(r ref, max uint32) ([]byte, error) {
	if e := q.ctx.Err(); e != nil {
		return nil, e
	}
	if q.reads >= q.maxReads {
		return nil, ErrLimit
	}
	if !r.valid(q.snapshot.current.extent, max) {
		return nil, ErrCorrupt
	}
	q.reads++
	b := make([]byte, int(r.length))
	n, e := q.snapshot.file.ReadAt(b, int64(r.offset))
	if e != nil || n != len(b) {
		return nil, errors.Join(ErrCorrupt, e)
	}
	if e = q.ctx.Err(); e != nil {
		return nil, e
	}
	if sha256.Sum256(b) != r.digest {
		return nil, ErrCorrupt
	}
	return b, nil
}
func (q *query) load(d descriptor) (node, error) {
	var n node
	if q.pending != nil && isPending(d.ref) {
		if e := q.pendingRead(); e != nil {
			return node{}, e
		}
		var ok bool
		if n, ok = q.pending.node(d.ref); !ok {
			return node{}, ErrCorrupt
		}
	} else {
		b, e := q.read(d.ref, maxNodeBytes)
		if e != nil {
			return node{}, e
		}
		if n, e = decodeNode(b, q.snapshot.current.extent); e != nil {
			return node{}, ErrCorrupt
		}
	}
	if n.count != d.count || n.height != d.height {
		return node{}, ErrCorrupt
	}
	lo, hi := n.bounds()
	if d.first != nil && (!bytes.Equal(lo, d.first) || !bytes.Equal(hi, d.last)) {
		return node{}, ErrCorrupt
	}
	return n, nil
}

// pendingRead counts an in-memory read like a pack read, keeping the same
// per-transaction read limits.
func (q *query) pendingRead() error {
	if e := q.ctx.Err(); e != nil {
		return e
	}
	if q.reads >= q.maxReads {
		return ErrLimit
	}
	q.reads++
	return nil
}
func (q *query) value(r ref) ([]byte, error) {
	var b []byte
	if q.pending != nil && isPending(r) {
		if e := q.pendingRead(); e != nil {
			return nil, e
		}
		var ok bool
		if b, ok = q.pending.value(r); !ok {
			return nil, ErrCorrupt
		}
	} else {
		var e error
		if b, e = q.read(r, MaxValueBytes+1); e != nil {
			return nil, e
		}
	}
	if b[0] != 0 {
		return nil, ErrCorrupt
	}
	return b[1:], nil
}
func (s *Snapshot) begin(ctx context.Context) (resourcebudget.Permit, error) {
	if !s.mu.TryLock() {
		return resourcebudget.Permit{}, ErrBusy
	}
	return s.beginLocked(ctx)
}

// beginLocked consumes the already-held reader mutex on success and unlocks
// it on failure. Audit uses it to start its clock before admission/header IO.
func (s *Snapshot) beginLocked(ctx context.Context) (resourcebudget.Permit, error) {
	if e := ctx.Err(); e != nil {
		s.mu.Unlock()
		return resourcebudget.Permit{}, e
	}
	if e := s.checkLocked(); e != nil {
		s.errorState = e
		if errors.Is(e, ErrCorrupt) && s.onCorrupt != nil {
			s.onCorrupt(e)
		}
		s.mu.Unlock()
		return resourcebudget.Permit{}, e
	}
	permit, e := s.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: ReadRAMBytes})
	if e != nil {
		s.mu.Unlock()
	}
	return permit, e
}
func (s *Snapshot) end(ctx context.Context, permit resourcebudget.Permit, resultErr *error) {
	if *resultErr == nil {
		*resultErr = ctx.Err()
	}
	if *resultErr == nil {
		*resultErr = s.checkLocked()
	}
	if errors.Is(*resultErr, ErrCorrupt) || errors.Is(*resultErr, ErrAuditDeadline) || errors.Is(*resultErr, ErrAuditStalled) {
		s.errorState = *resultErr
		if s.onCorrupt != nil {
			s.onCorrupt(*resultErr)
		}
	}
	*resultErr = errors.Join(*resultErr, permit.Release())
	s.mu.Unlock()
}

// Get proves absence by the authenticated ordered tree. A missing leaf or blob
// is a sticky integrity failure, never a successful false result. Every call
// rereads the authenticated path; no unverified DB/cache supplies lookup hints.
func (s *Snapshot) Get(ctx context.Context, key []byte) (value []byte, found bool, resultErr error) {
	if !validKey(key) {
		return nil, false, ErrLimit
	}
	permit, e := s.begin(ctx)
	if e != nil {
		return nil, false, e
	}
	defer func() {
		s.end(ctx, permit, &resultErr)
		if resultErr != nil {
			value = nil
			found = false
		}
	}()
	return s.getLocked(ctx, key)
}

// getLocked is shared with whole-image export, which holds one reader permit
// and mutex through destination writes so Store.Close joins the entire job.
func (s *Snapshot) getLocked(ctx context.Context, key []byte) ([]byte, bool, error) {
	if s.current.count == 0 {
		return nil, false, nil
	}
	q := query{ctx: ctx, snapshot: s, maxReads: maxHeight + 1}
	d := descriptor{count: s.current.count, height: s.current.height, ref: s.current.root}
	for {
		n, e := q.load(d)
		if e != nil {
			return nil, false, e
		}
		if n.height == 1 {
			if !bytes.Equal(n.key, key) {
				return nil, false, nil
			}
			v, e := q.value(n.value)
			return v, e == nil, e
		}
		matched := false
		for _, c := range n.children {
			if bytes.Compare(key, c.first) >= 0 && bytes.Compare(key, c.last) <= 0 {
				d = c
				matched = true
				break
			}
		}
		if !matched {
			return nil, false, nil
		}
	}
}

type RangeOptions struct {
	Lo, Hi                      []byte
	Cursor                      string
	MaxRows, MaxBytes, MaxPages int
}
type Page struct {
	Rows   []Row
	Cursor string
	Done   bool
	Reads  int
}

func rangeDigest(lo, hi []byte) [32]byte {
	b := putKey([]byte("halro:authenticated-state:range:v1\x00"), lo)
	b = putKey(b, hi)
	return sha256.Sum256(b)
}
func (s *Snapshot) cursor(lo, hi, after []byte) string {
	b := append([]byte{}, s.headerDigest[:]...)
	d := rangeDigest(lo, hi)
	b = append(b, d[:]...)
	b = putKey(b, after)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("halro:authenticated-state:cursor:v1\x00"))
	mac.Write(b)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(b))
}
func (s *Snapshot) after(o RangeOptions) ([]byte, error) {
	if o.Cursor == "" {
		return nil, nil
	}
	if len(o.Cursor) > base64.RawURLEncoding.EncodedLen(66+MaxKeyBytes+32) {
		return nil, ErrCursor
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(o.Cursor)
	if e != nil || len(b) < 99 || len(b) > 66+MaxKeyBytes+32 {
		return nil, ErrCursor
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("halro:authenticated-state:cursor:v1\x00"))
	mac.Write(b[:len(b)-32])
	d := rangeDigest(o.Lo, o.Hi)
	if !hmac.Equal(mac.Sum(nil), b[len(b)-32:]) || !bytes.Equal(b[:32], s.headerDigest[:]) || !bytes.Equal(b[32:64], d[:]) || int(binary.BigEndian.Uint16(b[64:66])) != len(b)-98 {
		return nil, ErrCursor
	}
	after := b[66 : len(b)-32]
	if !validKey(after) || len(o.Lo) > 0 && bytes.Compare(after, o.Lo) < 0 || len(o.Hi) > 0 && bytes.Compare(after, o.Hi) >= 0 {
		return nil, ErrCursor
	}
	return bytes.Clone(after), nil
}

// Range returns a complete bounded prefix of [Lo,Hi); nil bounds mean infinity.
// Continuations authenticate the exact root, bounds and last returned key. Tree
// traversal supplies completeness, without a normal DB cursor. Any integrity
// failure discards the entire page. It never holds a physical DB transaction.
func (s *Snapshot) Range(ctx context.Context, o RangeOptions) (result Page, resultErr error) {
	if o.MaxRows <= 0 || o.MaxRows > maxRangeRows || o.MaxBytes <= 0 || o.MaxBytes > maxRangeBytes || o.MaxPages <= 0 || o.MaxPages > maxQueryPages || len(o.Lo) > MaxKeyBytes || len(o.Hi) > MaxKeyBytes || len(o.Lo) > 0 && len(o.Hi) > 0 && bytes.Compare(o.Lo, o.Hi) >= 0 {
		return Page{}, ErrLimit
	}
	permit, e := s.begin(ctx)
	if e != nil {
		return Page{}, e
	}
	defer func() {
		s.end(ctx, permit, &resultErr)
		if resultErr != nil {
			result = Page{}
		}
	}()
	return s.rangeLocked(ctx, o)
}

func (s *Snapshot) rangeLocked(ctx context.Context, o RangeOptions) (result Page, resultErr error) {
	after, e := s.after(o)
	if e != nil {
		return Page{}, e
	}
	if s.current.count == 0 {
		return Page{Done: true}, nil
	}
	q := query{ctx: ctx, snapshot: s, maxReads: o.MaxPages}
	used := 0
	matches := func(k []byte) bool {
		return (len(o.Lo) == 0 || bytes.Compare(k, o.Lo) >= 0) && (len(o.Hi) == 0 || bytes.Compare(k, o.Hi) < 0) && (after == nil || bytes.Compare(k, after) > 0)
	}
	var walk func(descriptor) error
	walk = func(d descriptor) error {
		n, e := q.load(d)
		if e != nil {
			return e
		}
		if n.height > 1 {
			for _, c := range n.children {
				if len(o.Lo) > 0 && bytes.Compare(c.last, o.Lo) < 0 || len(o.Hi) > 0 && bytes.Compare(c.first, o.Hi) >= 0 || after != nil && bytes.Compare(c.last, after) <= 0 {
					continue
				}
				if e := walk(c); e != nil {
					return e
				}
			}
			return nil
		}
		if !matches(n.key) {
			return nil
		}
		size := len(n.key) + int(n.value.length) - 1
		if len(result.Rows) == o.MaxRows || size > o.MaxBytes-used {
			if len(result.Rows) == 0 {
				return ErrLimit
			}
			return io.EOF
		}
		v, e := q.value(n.value)
		if e != nil {
			return e
		}
		result.Rows = append(result.Rows, Row{bytes.Clone(n.key), v})
		used += size
		return nil
	}
	e = walk(descriptor{count: s.current.count, height: s.current.height, ref: s.current.root})
	if e != nil && e != io.EOF {
		return Page{}, e
	}
	result.Done = e == nil
	result.Reads = q.reads
	if !result.Done {
		result.Cursor = s.cursor(o.Lo, o.Hi, result.Rows[len(result.Rows)-1].Key)
	}
	return result, nil
}
