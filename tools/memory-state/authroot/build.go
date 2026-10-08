package authroot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
)

const maxBatchRows = 32
const maxBatchBytes = 8192

type mutation struct {
	key, value []byte
	remove     bool
}

// mutationHash is a synthetic test input chain, NOT a native Ledger/ordering
// cursor or proof that any Halro authority persisted these facts.
func mutationHash(previous [32]byte, sequence uint64, m mutation) [32]byte {
	h := sha256.New()
	h.Write([]byte("halro:authroot-experiment:mutation:v1"))
	h.Write(previous[:])
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], sequence)
	h.Write(b[:])
	kind := byte(0)
	if m.remove {
		kind = 1
	}
	h.Write([]byte{kind, byte(len(m.key))})
	h.Write(m.key)
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(m.value)))
	h.Write(length[:])
	h.Write(m.value)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func (t *diskTree) applyBatch(ctx context.Context, input []mutation) (bool, error) {
	if len(input) == 0 || len(input) > maxBatchRows {
		return false, errLimit
	}
	// Copy a bounded batch before entering the writer so reusable input buffers
	// cannot change an accepted mutation between hashing and writing it.
	owned := make([]mutation, 0, len(input))
	size := 0
	for _, m := range input {
		if !validKey(m.key) || len(m.value) > maxValueBytes || (m.remove && len(m.value) != 0) {
			return false, errLimit
		}
		size += len(m.key) + len(m.value)
		if size > maxBatchBytes {
			return false, errLimit
		}
		owned = append(owned, mutation{bytes.Clone(m.key), bytes.Clone(m.value), m.remove})
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.poisoned {
		return false, errCorrupt
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if t.current.revision == math.MaxUint64 || t.current.sequence > math.MaxUint64-uint64(len(owned)) {
		return false, errLimit
	}
	next := t.current
	changed := false
	for _, m := range owned {
		if err := ctx.Err(); err != nil {
			t.poisoned = true
			return false, err
		}
		candidate, modified, err := t.applyOne(ctx, next, m)
		if err != nil {
			t.poisoned = true
			return false, err
		}
		candidate.sequence = next.sequence + 1
		candidate.factHash = mutationHash(next.factHash, candidate.sequence, m)
		next = candidate
		changed = changed || modified
	}
	next.revision++
	if err := ctx.Err(); err != nil {
		t.poisoned = true
		return false, err
	}
	if err := t.phase("pages-written"); err != nil {
		t.poisoned = true
		return false, err
	}
	if err := t.publish(next); err != nil {
		t.poisoned = true
		return false, err
	}
	return changed, nil
}

// bulkBuild accepts a strictly sorted, caller-verified stream into a freshly
// initialized empty experiment. Source verification/native positions belong to
// the eventual production protocol; this does not certify arbitrary live DBs.
// At most one leaf and 8 child descriptors per tree level are retained.
func (t *diskTree) bulkBuild(ctx context.Context, nextEntry func() (entry, bool, error)) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.poisoned {
		return errCorrupt
	}
	if t.current.count != 0 || t.current.sequence != 0 || t.current.revision != 1 {
		return errCorrupt
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var levels [maxDepth][maxEntries]child
	var sizes [maxDepth]int
	var push func(int, child) error
	push = func(level int, c child) error {
		if level >= maxDepth {
			return errLimit
		}
		levels[level][sizes[level]] = c
		sizes[level]++
		if sizes[level] < maxEntries {
			return nil
		}
		if c.height >= maxDepth {
			return errLimit
		}
		parent, err := t.writeChildren(levels[level][:])
		if err != nil {
			return err
		}
		clear(levels[level][:])
		sizes[level] = 0
		return push(level+1, parent)
	}
	leaf := make([]entry, 0, maxEntries)
	var previous []byte
	candidate := t.current
	flush := func() error {
		if len(leaf) == 0 {
			return nil
		}
		c, err := t.writePage(page{scope: t.scope, kind: leafKind, count: uint64(len(leaf)), entries: leaf})
		if err != nil {
			return err
		}
		clear(leaf)
		leaf = leaf[:0]
		return push(0, c)
	}
	build := func() error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			e, ok, err := nextEntry()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			if !validKey(e.key) || len(e.value) > maxValueBytes || (previous != nil && bytes.Compare(previous, e.key) >= 0) || candidate.sequence == math.MaxUint64 {
				return errCorrupt
			}
			e = entry{bytes.Clone(e.key), bytes.Clone(e.value)}
			previous = append(previous[:0], e.key...)
			candidate.sequence++
			candidate.factHash = mutationHash(candidate.factHash, candidate.sequence, mutation{key: e.key, value: e.value})
			leaf = append(leaf, e)
			if len(leaf) == maxEntries {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if err := flush(); err != nil {
			return err
		}
		// Fold trailing groups from low to high. A sole descriptor becomes the root
		// only if no higher group remains; otherwise append it after prior groups.
		for level := 0; level < maxDepth; level++ {
			n := sizes[level]
			if n == 0 {
				continue
			}
			higher := false
			for i := level + 1; i < maxDepth; i++ {
				higher = higher || sizes[i] != 0
			}
			var c child
			if n == 1 {
				c = levels[level][0]
			} else {
				if level == maxDepth-1 {
					return errLimit
				}
				var err error
				c, err = t.writeChildren(levels[level][:n])
				if err != nil {
					return err
				}
			}
			clear(levels[level][:])
			sizes[level] = 0
			if !higher {
				candidate.count, candidate.ref, candidate.height = c.count, c.ref, c.height
				break
			}
			if err := push(level+1, c); err != nil {
				return err
			}
		}
		if candidate.count != candidate.sequence {
			return errCorrupt
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := t.phase("pages-written"); err != nil {
			return err
		}
		candidate.revision++
		return t.publish(candidate)
	}
	if err := build(); err != nil {
		t.poisoned = true
		return err
	}
	return nil
}
