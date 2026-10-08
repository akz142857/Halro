package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const maxUpdateRows = 64
const maxUpdateBytes = 8 << 20
const maxUpdateReads = 4096
const maxReadCalls = 256

// The ordinary profile remains unchanged. The explicit native-frame profile
// admits up to 1024 operations creating 32 ancestors each plus bookkeeping.
// These cumulative work limits do not retain a collection of historical rows.
type updateLimits struct{ mutations, bytes, pages, calls, returned int }

var ordinaryUpdateLimits = updateLimits{maxUpdateRows, maxUpdateBytes, maxUpdateReads, maxReadCalls, maxUpdateBytes}

const MaxNativeFrameMutations = 1024*maxHeight + 2

var nativeFrameUpdateLimits = updateLimits{MaxNativeFrameMutations, maxUpdateBytes * maxHeight * 2, MaxNativeFrameMutations * (4*maxHeight + 8), MaxNativeFrameMutations * 4, maxUpdateBytes * 4}

type UpdateReceipt struct {
	Cut                     [32]byte
	Revision, Rows          uint64
	ReadPages, WrittenNodes int
	AppendedBytes           int64
}

// Transaction is a zero-queue, callback-scoped view over verified old paths and
// the private new paths of this batch. Methods cannot outlive Update; ignored
// method errors abort the complete batch. Ordinary DB hints are never read.
type Transaction struct {
	mu                                                         sync.Mutex
	store                                                      *Store
	ctx                                                        context.Context
	candidate                                                  commitment
	cut                                                        [32]byte
	revision                                                   uint64
	start                                                      uint64
	reads, readCalls, returnedBytes, writes, inputBytes, nodes int
	limits                                                     updateLimits
	closed                                                     bool
	failure                                                    atomic.Pointer[storeFault]
}

func (t *Transaction) fail(e error) error {
	if e != nil {
		t.failure.CompareAndSwap(nil, &storeFault{e})
	}
	return e
}
func (t *Transaction) enter() error {
	if !t.mu.TryLock() {
		return t.fail(ErrBusy)
	}
	if t.closed {
		t.mu.Unlock()
		return ErrClosed
	}
	if f := t.failure.Load(); f != nil {
		t.mu.Unlock()
		return f.err
	}
	if e := t.ctx.Err(); e != nil {
		t.mu.Unlock()
		return t.fail(e)
	}
	return nil
}
func (t *Transaction) view() *Snapshot {
	return &Snapshot{file: t.store.pack, current: t.candidate, key: t.store.key}
}
func (t *Transaction) query() *query {
	return &query{ctx: t.ctx, snapshot: t.view(), reads: t.reads, maxReads: t.limits.pages}
}
func (t *Transaction) get(key []byte) ([]byte, bool, error) {
	if !validKey(key) || t.readCalls >= t.limits.calls {
		return nil, false, ErrLimit
	}
	t.readCalls++
	if t.candidate.count == 0 {
		return nil, false, nil
	}
	q := t.query()
	defer func() { t.reads = q.reads }()
	d := descriptor{count: t.candidate.count, height: t.candidate.height, ref: t.candidate.root}
	for {
		n, e := q.load(d)
		if e != nil {
			return nil, false, e
		}
		if n.height == 1 {
			if !bytes.Equal(n.key, key) {
				return nil, false, nil
			}
			if int(n.value.length)-1 > t.limits.returned-t.returnedBytes {
				return nil, false, ErrLimit
			}
			v, e := q.value(n.value)
			if e != nil {
				return nil, false, e
			}
			t.returnedBytes += len(v)
			return v, true, nil
		}
		found := false
		for _, c := range n.children {
			if bytes.Compare(key, c.first) >= 0 && bytes.Compare(key, c.last) <= 0 {
				d = c
				found = true
				break
			}
		}
		if !found {
			return nil, false, nil
		}
	}
}
func (t *Transaction) Get(key []byte) ([]byte, bool, error) {
	if e := t.enter(); e != nil {
		return nil, false, e
	}
	defer t.mu.Unlock()
	v, found, e := t.get(key)
	if e != nil {
		return nil, false, t.fail(e)
	}
	return v, found, nil
}
func validRange(o RangeOptions) bool {
	return o.MaxRows > 0 && o.MaxRows <= maxRangeRows && o.MaxBytes > 0 && o.MaxBytes <= maxRangeBytes && o.MaxPages > 0 && o.MaxPages <= maxQueryPages && len(o.Lo) <= MaxKeyBytes && len(o.Hi) <= MaxKeyBytes && (len(o.Lo) == 0 || len(o.Hi) == 0 || bytes.Compare(o.Lo, o.Hi) < 0)
}
func (t *Transaction) Range(o RangeOptions) (Page, error) {
	if e := t.enter(); e != nil {
		return Page{}, e
	}
	defer t.mu.Unlock()
	if !validRange(o) || t.readCalls >= t.limits.calls || o.MaxBytes > t.limits.returned-t.returnedBytes || o.MaxPages > t.limits.pages-t.reads {
		return Page{}, t.fail(ErrLimit)
	}
	t.readCalls++
	view := t.view()
	b, e := (cowRoot{cut: t.cut, revision: t.revision, content: t.candidate}).encode(t.store.key)
	if e != nil {
		return Page{}, t.fail(e)
	}
	view.headerDigest = sha256.Sum256(b)
	page, e := view.rangeLocked(t.ctx, o)
	t.reads += page.Reads
	if e != nil {
		return Page{}, t.fail(e)
	}
	for _, r := range page.Rows {
		t.returnedBytes += len(r.Key) + len(r.Value)
	}
	return page, nil
}
func (t *Transaction) append(raw []byte) (ref, error) {
	if e := t.ctx.Err(); e != nil {
		return ref{}, e
	}
	if uint64(len(raw)) > uint64(t.store.ceiling)-t.candidate.extent || uint64(len(raw)) > uint64(t.store.appendLimit)-(t.candidate.extent-t.start) {
		return ref{}, ErrLimit
	}
	r := ref{offset: t.candidate.extent, length: uint32(len(raw)), digest: sha256.Sum256(raw)}
	n, e := t.store.pack.WriteAt(raw, int64(r.offset))
	t.candidate.extent += uint64(n)
	if e != nil {
		return ref{}, e
	}
	if n != len(raw) {
		return ref{}, io.ErrShortWrite
	}
	return r, nil
}
func (t *Transaction) writeNode(n node) (descriptor, error) {
	raw, e := encodeNode(n, t.candidate.extent)
	if e != nil {
		return descriptor{}, e
	}
	r, e := t.append(raw)
	if e != nil {
		return descriptor{}, e
	}
	t.nodes++
	lo, hi := n.bounds()
	return descriptor{bytes.Clone(lo), bytes.Clone(hi), n.count, n.height, r}, nil
}
func (t *Transaction) leaf(key, value []byte) (descriptor, error) {
	blob := make([]byte, len(value)+1)
	copy(blob[1:], value)
	v, e := t.append(blob)
	if e != nil {
		return descriptor{}, e
	}
	return t.writeNode(node{key: key, value: v, count: 1, height: 1})
}
func (t *Transaction) branch(children []descriptor) (descriptor, error) {
	if len(children) == 0 || len(children) > fanout {
		return descriptor{}, ErrLimit
	}
	var count uint64
	var highest uint16
	owned := append([]descriptor{}, children...)
	for _, c := range owned {
		if c.count > math.MaxUint64-count {
			return descriptor{}, ErrLimit
		}
		count += c.count
		if c.height > highest {
			highest = c.height
		}
	}
	if highest >= maxHeight {
		return descriptor{}, ErrLimit
	}
	// Pad short inherited children with authenticated unary branches. Retaining
	// child levels on deletion avoids a new unbalanced path on every churn cycle.
	for i := range owned {
		for owned[i].height < highest {
			c := owned[i]
			p, e := t.writeNode(node{children: []descriptor{c}, count: c.count, height: c.height + 1})
			if e != nil {
				return descriptor{}, e
			}
			owned[i] = p
		}
	}
	return t.writeNode(node{children: owned, count: count, height: highest + 1})
}
func (t *Transaction) rewrite(q *query, d descriptor, key, value []byte, remove bool) ([]descriptor, bool, error) {
	n, e := q.load(d)
	if e != nil {
		return nil, false, e
	}
	lo, hi := n.bounds()
	d.first = lo
	d.last = hi
	unchanged := func() ([]descriptor, bool, error) { return []descriptor{d}, false, nil }
	if n.height == 1 {
		exists := bytes.Equal(n.key, key)
		if remove && !exists {
			return unchanged()
		}
		if exists {
			old, e := q.value(n.value)
			if e != nil {
				return nil, false, e
			}
			if !remove && bytes.Equal(old, value) {
				return unchanged()
			}
			if remove {
				return nil, true, nil
			}
		}
		fresh, e := t.leaf(key, value)
		if e != nil {
			return nil, false, e
		}
		if exists {
			return []descriptor{fresh}, true, nil
		}
		if bytes.Compare(key, n.key) < 0 {
			return []descriptor{fresh, d}, true, nil
		}
		return []descriptor{d, fresh}, true, nil
	}
	index := len(n.children) - 1
	inside := false
	for i, c := range n.children {
		if bytes.Compare(key, c.last) <= 0 {
			index = i
			inside = bytes.Compare(key, c.first) >= 0
			break
		}
	}
	if remove && !inside {
		return unchanged()
	}
	replacement, changed, e := t.rewrite(q, n.children[index], key, value, remove)
	if e != nil {
		return nil, false, e
	}
	if !changed {
		return unchanged()
	}
	children := make([]descriptor, 0, fanout+1)
	children = append(children, n.children[:index]...)
	children = append(children, replacement...)
	children = append(children, n.children[index+1:]...)
	if len(children) == 0 {
		return nil, true, nil
	}
	// Keep surviving children at this branch's original level; only the top root
	// is allowed to collapse after a deletion.
	for i := range children {
		for children[i].height < n.height-1 {
			c := children[i]
			p, e := t.writeNode(node{children: []descriptor{c}, count: c.count, height: c.height + 1})
			if e != nil {
				return nil, false, e
			}
			children[i] = p
		}
	}
	if len(children) <= fanout {
		c, e := t.branch(children)
		return []descriptor{c}, true, e
	}
	split := len(children) / 2
	left, e := t.branch(children[:split])
	if e != nil {
		return nil, false, e
	}
	right, e := t.branch(children[split:])
	if e != nil {
		return nil, false, e
	}
	return []descriptor{left, right}, true, nil
}
func (t *Transaction) mutate(key, value []byte, remove bool) error {
	if !validKey(key) || len(value) > MaxValueBytes || remove && len(value) != 0 || t.writes >= t.limits.mutations || len(key)+len(value) > t.limits.bytes-t.inputBytes {
		return ErrLimit
	}
	t.writes++
	t.inputBytes += len(key) + len(value)
	key = bytes.Clone(key)
	value = bytes.Clone(value)
	q := t.query()
	defer func() { t.reads = q.reads }()
	var replacement []descriptor
	changed := false
	var e error
	if t.candidate.count == 0 {
		if remove {
			return nil
		}
		c, e := t.leaf(key, value)
		if e != nil {
			return e
		}
		replacement = []descriptor{c}
		changed = true
	} else {
		replacement, changed, e = t.rewrite(q, descriptor{count: t.candidate.count, height: t.candidate.height, ref: t.candidate.root}, key, value, remove)
		if e != nil {
			return e
		}
	}
	if !changed {
		return nil
	}
	if len(replacement) == 0 {
		t.candidate.count = 0
		t.candidate.height = 0
		t.candidate.root = ref{digest: emptyDigest(t.candidate.context)}
		return nil
	}
	root := replacement[0]
	if len(replacement) > 1 {
		root, e = t.branch(replacement)
		if e != nil {
			return e
		}
	}
	// Collapse a unary root through verified nodes. Unrelated subtrees retain
	// their authenticated references; this does not scan the complete key set.
	for root.height > 1 {
		q.snapshot.current = t.candidate
		n, e := q.load(root)
		if e != nil {
			return e
		}
		if len(n.children) != 1 {
			break
		}
		root = n.children[0]
	}
	t.candidate.count = root.count
	t.candidate.height = root.height
	t.candidate.root = root.ref
	return nil
}
func (t *Transaction) Put(key, value []byte) error {
	if e := t.enter(); e != nil {
		return e
	}
	defer t.mu.Unlock()
	return t.fail(t.mutate(key, value, false))
}
func (t *Transaction) Delete(key []byte) error {
	if e := t.enter(); e != nil {
		return e
	}
	defer t.mu.Unlock()
	return t.fail(t.mutate(key, nil, true))
}

// Update publishes content and the caller-established durable native cut in one
// atomic root replacement. Success requires all page/root/directory barriers.
// It provides component durability, not source eligibility or HA confirmation.
func (s *Store) Update(ctx context.Context, cut [32]byte, apply func(*Transaction) error) (UpdateReceipt, error) {
	return s.update(ctx, cut, ordinaryUpdateLimits, apply, nil, false)
}

// UpdateNativeFrame prepares one complete bounded native frame before invoking
// its durable append barrier. Pages are synced first; the barrier must durably
// record exactly the independently authenticated cut before root publication.
// It is not a role/source authorization API. Callback and barrier lifetimes are
// joined, and any uncertainty revokes the writer. Ordinary Update stays at 64
// mutations. The fixed per-job RAM and configured disk/append ceilings remain.
func (s *Store) UpdateNativeFrame(ctx context.Context, cut [32]byte, apply func(*Transaction) error, durableFrame func() error) (UpdateReceipt, error) {
	if durableFrame == nil {
		return UpdateReceipt{}, ErrLimit
	}
	return s.update(ctx, cut, nativeFrameUpdateLimits, apply, durableFrame, false)
}

// UpdateDerived publishes an independently authorized derived-row maintenance
// step at the exact existing native cut. It cannot advance or roll back that
// cut. The caller must validate its archive/retention permission in the barrier;
// component authentication alone is not permission to remove business rows.
// The complete step shares the finite native-frame work profile and fixed RAM.
func (s *Store) UpdateDerived(ctx context.Context, expectedCut [32]byte, apply func(*Transaction) error, authorizedStep func() error) (UpdateReceipt, error) {
	if authorizedStep == nil {
		return UpdateReceipt{}, ErrLimit
	}
	return s.update(ctx, expectedCut, nativeFrameUpdateLimits, apply, authorizedStep, true)
}

func (s *Store) update(ctx context.Context, cut [32]byte, limits updateLimits, apply func(*Transaction) error, durableFrame func() error, sameCut bool) (result UpdateReceipt, resultErr error) {
	if !s.mu.TryLock() {
		return result, ErrBusy
	}
	defer s.mu.Unlock()
	if s.readOnly || s.physicalRecoveryPending {
		return result, ErrRecovery
	}
	if s.closing || s.closed {
		return result, ErrClosed
	}
	if s.repack != nil {
		return result, ErrBusy
	}
	if s.physical != nil && physicalUpdate(ctx, s) == nil {
		return result, resourcebudget.ErrPhysicalAllocation
	}
	if e := s.fault(); e != nil {
		return result, e
	}
	if cut == ([32]byte{}) || !sameCut && cut == s.current.cut || sameCut && cut != s.current.cut || s.physicalRevision == math.MaxUint64 || apply == nil {
		return result, ErrLimit
	}
	if e := ctx.Err(); e != nil {
		return result, e
	}
	if e := errors.Join(s.checkPack(), s.checkRoot()); e != nil {
		s.poison(e)
		return result, e
	}
	permit, e := s.budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{RAM: UpdateRAMBytes})
	if e != nil {
		return result, e
	}
	candidate := s.current.content
	candidate.extent = s.physicalExtent
	tx := &Transaction{store: s, ctx: ctx, candidate: candidate, start: s.physicalExtent, cut: cut, revision: s.physicalRevision + 1, limits: limits}
	publishing := false
	callbackReturned := false
	finished := false
	defer func() {
		tx.mu.Lock()
		tx.closed = true
		tx.mu.Unlock()
		if !callbackReturned || !finished && resultErr == nil {
			s.poison(ErrRecovery)
		}
		if resultErr != nil && (tx.candidate.extent != tx.start || publishing || errors.Is(resultErr, ErrCorrupt)) {
			s.poison(resultErr)
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			s.retiredUpdate = permit
			s.retirementErr = errors.Join(s.retirementErr, resultErr)
			s.poison(resultErr)
		} else {
			resultErr = errors.Join(resultErr, permit.Release())
		}
		if resultErr != nil {
			result = UpdateReceipt{}
		}
	}()
	callbackErr := apply(tx)
	callbackReturned = true
	tx.mu.Lock()
	tx.closed = true
	methodFailure := tx.failure.Load()
	tx.mu.Unlock()
	if methodFailure != nil {
		callbackErr = errors.Join(callbackErr, methodFailure.err)
	}
	if callbackErr != nil {
		return result, callbackErr
	}
	if e = ctx.Err(); e != nil {
		return result, e
	}
	if e = s.phase("pages-written"); e != nil {
		return result, e
	}
	next := cowRoot{cut, s.physicalRevision + 1, tx.candidate}
	publishing = true
	if durableFrame != nil {
		if e = s.pack.Sync(); e != nil {
			return result, e
		}
		if e = ctx.Err(); e != nil {
			return result, e
		}
		if e = durableFrame(); e != nil {
			return result, e
		}
		if e = ctx.Err(); e != nil {
			return result, e
		}
	}
	if e = s.publish(ctx, next); e != nil {
		return result, e
	}
	finished = true
	return UpdateReceipt{cut, next.revision, next.content.count, tx.reads, tx.nodes, int64(tx.candidate.extent - tx.start)}, nil
}
