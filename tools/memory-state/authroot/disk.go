package authroot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/store/lock"
)

const packHeaderBytes = 32

// diskTree is an experiment-owned directory, one append-only pack and one root.
// It has a single writer, a fixed pack-byte ceiling, and no GC or compaction.
// A publication error poisons writes/snapshot acquisition until reopen.
type diskTree struct {
	mu               sync.RWMutex
	ioMu             sync.RWMutex
	cache            atomic.Pointer[packCache]
	pack             *os.File
	guard            *lock.Lock
	dir              string
	id               [16]byte
	scope            uint16
	key              [32]byte
	current          root
	nextOffset       int64
	maxBytes         int64
	poisoned, closed bool
	hook             func(string) error // fault injection only; never part of root authority
}

func createDiskTree(dir string, key []byte, scope uint16, maxBytes int64) (*diskTree, error) {
	if len(key) != 32 || scope == 0 || maxBytes < packHeaderBytes {
		return nil, errLimit
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	if err := durable.SyncDirectory(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	guard, err := lock.Acquire(dir)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "pages"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		guard.Close()
		return nil, err
	}
	t := &diskTree{pack: f, guard: guard, dir: dir, scope: scope, nextOffset: packHeaderBytes, maxBytes: maxBytes}
	copy(t.key[:], key)
	if _, err = rand.Read(t.id[:]); err != nil {
		t.close()
		return nil, err
	}
	header := append([]byte{}, []byte("HMPPACK1")...)
	header = binary.BigEndian.AppendUint16(header, scope)
	header = append(header, make([]byte, 6)...)
	header = append(header, t.id[:]...)
	if _, err = f.WriteAt(header, 0); err == nil {
		err = f.Sync()
	}
	if err != nil {
		t.close()
		return nil, err
	}
	empty := root{scope: scope, generation: 1, revision: 1, ref: emptyRef(scope)}
	if err = t.publish(empty); err != nil {
		t.close()
		return nil, err
	}
	return t, nil
}

func openDiskTree(dir string, key []byte, scope uint16, maxBytes int64) (*diskTree, error) {
	if len(key) != 32 || scope == 0 || maxBytes < packHeaderBytes {
		return nil, errLimit
	}
	guard, err := lock.AcquireExistingReadOnly(dir)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			guard.Close()
		}
	}()
	// Root length is checked before allocating or reading caller-controlled bytes.
	rf, err := os.Open(filepath.Join(dir, "root"))
	if err != nil {
		return nil, err
	}
	info, err := rf.Stat()
	if err != nil || info.Size() != rootBytes {
		rf.Close()
		return nil, errCorrupt
	}
	var encoded [rootBytes]byte
	_, err = io.ReadFull(rf, encoded[:])
	closeErr := rf.Close()
	if err != nil {
		return nil, errCorrupt
	}
	if closeErr != nil {
		return nil, closeErr
	}
	r, err := verifyRoot(encoded[:], key)
	if err != nil || r.scope != scope {
		return nil, errCorrupt
	}
	f, err := os.OpenFile(filepath.Join(dir, "pages"), os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*diskTree, error) { f.Close(); return nil, err }
	var header [packHeaderBytes]byte
	if _, err = f.ReadAt(header[:], 0); err != nil || !bytes.Equal(header[:8], []byte("HMPPACK1")) || binary.BigEndian.Uint16(header[8:10]) != scope || !bytes.Equal(header[10:16], make([]byte, 6)) {
		return fail(errCorrupt)
	}
	info, err = f.Stat()
	if err != nil {
		return fail(err)
	}
	if info.Size() < packHeaderBytes || info.Size() > maxBytes {
		return fail(errLimit)
	}
	t := &diskTree{pack: f, guard: guard, dir: dir, scope: scope, current: r, nextOffset: info.Size(), maxBytes: maxBytes}
	copy(t.key[:], key)
	copy(t.id[:], header[16:])
	if r.count != 0 {
		if r.ref.pack != t.id {
			return fail(errCorrupt)
		}
		q := query{ctx: context.Background(), reader: reader{t, r}}
		if _, err = q.load(r.ref, r.count, r.height, nil, nil, 1); err != nil {
			return fail(err)
		}
	}
	// Unpublished append tails/staging files are not promoted or trusted. We keep
	// their bytes charged against the fixed ceiling; this prototype has no GC.
	success = true
	return t, nil
}

func (t *diskTree) readPage(r pageRef, dst []byte) (int, error) {
	if !r.valid() || r.pack != t.id || r.offset < packHeaderBytes || len(dst) != int(r.length) {
		return 0, errCorrupt
	}
	t.ioMu.RLock()
	defer t.ioMu.RUnlock()
	n, err := t.pack.ReadAt(dst, int64(r.offset))
	if err == nil {
		if cache := t.cache.Load(); cache != nil {
			if adviceErr := cache.afterRead(t.pack, int64(r.offset), n); adviceErr != nil {
				return n, adviceErr
			}
		}
	}
	return n, err
}
func (t *diskTree) snapshot() (reader, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.closed || t.poisoned {
		return reader{}, errCorrupt
	}
	return reader{t, t.current}, nil
}
func (t *diskTree) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	t.ioMu.Lock()
	defer t.ioMu.Unlock()
	clear(t.key[:])
	return errors.Join(t.pack.Close(), t.guard.Close())
}
func (t *diskTree) phase(name string) error {
	if t.hook != nil {
		return t.hook(name)
	}
	return nil
}

func (t *diskTree) writePage(p page) (child, error) {
	b, err := encodePage(p)
	if err != nil {
		return child{}, err
	}
	if t.nextOffset > t.maxBytes-int64(len(b)) {
		return child{}, errLimit
	}
	offset := t.nextOffset
	n, err := t.pack.WriteAt(b, offset)
	t.nextOffset += int64(n)
	if err != nil {
		return child{}, err
	}
	if n != len(b) {
		return child{}, io.ErrShortWrite
	}
	if cache := t.cache.Load(); cache != nil {
		if err := cache.afterWrite(t.pack, t.nextOffset, false); err != nil {
			return child{}, err
		}
	}
	lo, hi := p.bounds()
	return child{bytes.Clone(lo), bytes.Clone(hi), p.count, p.height(), refFor(t.id, uint64(offset), b)}, nil
}

func (t *diskTree) publish(r root) error {
	b, err := signRoot(r, t.key[:])
	if err != nil {
		return err
	}
	if err = t.pack.Sync(); err != nil {
		return err
	}
	if cache := t.cache.Load(); cache != nil {
		if err = cache.afterWrite(t.pack, t.nextOffset, true); err != nil {
			return err
		}
	}
	if err = t.phase("pages-synced"); err != nil {
		return err
	}
	f, err := os.CreateTemp(t.dir, "root-stage-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	n, writeErr := f.Write(b)
	if writeErr != nil {
		f.Close()
		return writeErr
	}
	if n != len(b) {
		f.Close()
		return io.ErrShortWrite
	}
	if err = t.phase("root-written"); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = t.phase("root-synced"); err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(t.dir, "root")); err != nil {
		return err
	}
	if err = t.phase("root-renamed"); err != nil {
		return err
	}
	if err = durable.SyncDirectory(t.dir); err != nil {
		return err
	}
	if err = t.phase("directory-synced"); err != nil {
		return err
	}
	t.current = r // visibility follows the successful durable publication
	return nil
}

// update is the one-mutation wrapper. The boolean describes content change;
// even a no-op consumes one synthetic input sequence in the experiment.
func (t *diskTree) update(ctx context.Context, key, value []byte, remove bool) (bool, error) {
	if remove {
		value = nil
	}
	return t.applyBatch(ctx, []mutation{{key: key, value: value, remove: remove}})
}

// applyOne is called only by the serialized builder. base is an authenticated
// published root or a private root built from already-verified paths in this
// batch. Neither intermediate roots nor their positions become visible.
func (t *diskTree) applyOne(ctx context.Context, base root, m mutation) (root, bool, error) {
	q := query{ctx: ctx, reader: reader{t, base}}
	var replacement []child
	var changed bool
	var err error
	if base.count == 0 {
		if m.remove {
			return base, false, nil
		}
		var c child
		c, err = t.writePage(page{scope: t.scope, kind: leafKind, count: 1, entries: []entry{{m.key, m.value}}})
		replacement = []child{c}
		changed = true
	} else {
		replacement, changed, err = t.change(&q, base.ref, base.count, base.height, nil, nil, m.key, m.value, m.remove, 1)
	}
	if err != nil {
		return root{}, false, err
	}
	if !changed {
		return base, false, nil
	}
	next := base
	next.count = 0
	next.ref = emptyRef(t.scope)
	next.height = 0
	if len(replacement) > 0 {
		c := replacement[0]
		if len(replacement) == 2 {
			if replacement[0].height >= maxDepth || replacement[1].height >= maxDepth {
				return root{}, false, errLimit
			}
			c, err = t.writeChildren(replacement)
			if err != nil {
				return root{}, false, err
			}
		}
		next.count, next.ref, next.height = c.count, c.ref, c.height
	}
	return next, true, nil
}

func (t *diskTree) writeChildren(children []child) (child, error) {
	var count uint64
	for _, c := range children {
		if c.count > math.MaxUint64-count {
			return child{}, errCorrupt
		}
		count += c.count
	}
	return t.writePage(page{scope: t.scope, kind: branchKind, count: count, children: children})
}

func (t *diskTree) change(q *query, ref pageRef, count uint64, height uint16, first, last, key, value []byte, remove bool, depth int) ([]child, bool, error) {
	p, err := q.load(ref, count, height, first, last, depth)
	if err != nil {
		return nil, false, err
	}
	lo, hi := p.bounds()
	unchanged := func() ([]child, bool, error) { return []child{{lo, hi, p.count, p.height(), ref}}, false, nil }
	if p.kind == leafKind {
		i := 0
		for i < len(p.entries) && bytes.Compare(p.entries[i].key, key) < 0 {
			i++
		}
		exists := i < len(p.entries) && bytes.Equal(p.entries[i].key, key)
		if remove && !exists {
			return unchanged()
		}
		if !remove && exists && bytes.Equal(p.entries[i].value, value) {
			return unchanged()
		}
		entries := make([]entry, 0, maxEntries+1)
		entries = append(entries, p.entries[:i]...)
		if !remove {
			entries = append(entries, entry{bytes.Clone(key), bytes.Clone(value)})
		}
		tail := i
		if exists {
			tail++
		}
		entries = append(entries, p.entries[tail:]...)
		if len(entries) == 0 {
			return nil, true, nil
		}
		cut := len(entries)
		if cut > maxEntries {
			cut /= 2
		}
		c, err := t.writePage(page{scope: t.scope, kind: leafKind, count: uint64(cut), entries: entries[:cut]})
		if err != nil {
			return nil, false, err
		}
		out := []child{c}
		if cut < len(entries) {
			c, err = t.writePage(page{scope: t.scope, kind: leafKind, count: uint64(len(entries) - cut), entries: entries[cut:]})
			if err != nil {
				return nil, false, err
			}
			out = append(out, c)
		}
		return out, true, nil
	}
	i := 0
	for i < len(p.children)-1 && bytes.Compare(key, p.children[i].last) > 0 {
		i++
	}
	c := p.children[i]
	if remove && (bytes.Compare(key, c.first) < 0 || bytes.Compare(key, c.last) > 0) {
		return unchanged()
	}
	replacement, changed, err := t.change(q, c.ref, c.count, c.height, c.first, c.last, key, value, remove, depth+1)
	if err != nil {
		return nil, false, err
	}
	if !changed {
		return unchanged()
	}
	children := make([]child, 0, maxEntries+1)
	children = append(children, p.children[:i]...)
	children = append(children, replacement...)
	children = append(children, p.children[i+1:]...)
	if len(children) == 0 {
		return nil, true, nil
	}
	if len(children) == 1 {
		return children, true, nil
	} // inherited authenticated commitment, shorter path
	cut := len(children)
	if cut > maxEntries {
		cut /= 2
	}
	c, err = t.writeChildren(children[:cut])
	if err != nil {
		return nil, false, err
	}
	out := []child{c}
	if cut < len(children) {
		c, err = t.writeChildren(children[cut:])
		if err != nil {
			return nil, false, err
		}
		out = append(out, c)
	}
	return out, true, nil
}
