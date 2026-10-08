package authroot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
)

// pageSource must copy at most len(dst) bytes, without allocating from a length
// stored in a page. Physical transactions/handles end before this call returns.
type pageSource interface {
	readPage(pageRef, []byte) (int, error)
}
type reader struct {
	source pageSource
	root   root
}
type query struct {
	ctx    context.Context
	reader reader
	reads  int
}

func (q *query) load(ref pageRef, count uint64, height uint16, first, last []byte, depth int) (page, error) {
	if err := q.ctx.Err(); err != nil {
		return page{}, err
	}
	if depth > maxDepth || depth > int(q.reader.root.height) || q.reads >= maxPageReads {
		return page{}, errLimit
	}
	if !ref.valid() {
		return page{}, errCorrupt
	}
	q.reads++
	var buf [maxPageBytes]byte
	n, err := q.reader.source.readPage(ref, buf[:ref.length])
	if err := q.ctx.Err(); err != nil {
		return page{}, err
	}
	if err != nil || n != int(ref.length) {
		return page{}, errCorrupt
	}
	b := buf[:n]
	if sha256.Sum256(b) != ref.digest {
		return page{}, errCorrupt
	}
	p, err := decodePage(b, q.reader.root.scope)
	if err != nil || p.count != count || p.height() != height {
		return page{}, errCorrupt
	}
	lo, hi := p.bounds()
	if first != nil && (!bytes.Equal(lo, first) || !bytes.Equal(hi, last)) {
		return page{}, errCorrupt
	}
	return p, nil
}

// get proves absence by the authenticated branch boundaries or a verified leaf.
// Missing/corrupt referenced bytes never become a NotFound result.
func (r reader) get(ctx context.Context, key []byte) ([]byte, bool, error) {
	if !validKey(key) {
		return nil, false, errLimit
	}
	if !r.root.valid() {
		return nil, false, errCorrupt
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if r.root.count == 0 {
		return nil, false, nil
	}
	q := query{ctx: ctx, reader: r}
	ref, count, height := r.root.ref, r.root.count, r.root.height
	var first, last []byte
	for depth := 1; ; depth++ {
		p, err := q.load(ref, count, height, first, last, depth)
		if err != nil {
			return nil, false, err
		}
		if p.kind == leafKind {
			for _, e := range p.entries {
				c := bytes.Compare(e.key, key)
				if c == 0 {
					return bytes.Clone(e.value), true, nil
				}
				if c > 0 {
					break
				}
			}
			return nil, false, nil
		}
		matched := false
		for _, c := range p.children {
			if bytes.Compare(key, c.first) >= 0 && bytes.Compare(key, c.last) <= 0 {
				ref, count, height, first, last = c.ref, c.count, c.height, c.first, c.last
				matched = true
				break
			}
		}
		if !matched {
			return nil, false, nil
		}
	}
}

type rangeResult struct {
	rows  []entry
	after []byte
	done  bool
	reads int
}

// scan is inclusive at lo, exclusive at hi and after. Continuations are only
// valid against this exact root. Production cursors must also authenticate the
// root identity, scope and query; that protocol is outside this experiment.
func (r reader) scan(ctx context.Context, lo, hi, after []byte, rows, byteBudget int) (rangeResult, error) {
	var result rangeResult
	if rows <= 0 || rows > maxRangeRows || byteBudget <= 0 || byteBudget > maxRangeBytes || !validKey(lo) || !validKey(hi) || bytes.Compare(lo, hi) >= 0 || (after != nil && !validKey(after)) {
		return result, errLimit
	}
	if !r.root.valid() {
		return rangeResult{}, errCorrupt
	}
	if err := ctx.Err(); err != nil {
		return rangeResult{}, err
	}
	if r.root.count == 0 {
		return rangeResult{done: true}, nil
	}
	q := query{ctx: ctx, reader: r}
	used := 0
	var visit func(pageRef, uint64, uint16, []byte, []byte, int) error
	visit = func(ref pageRef, count uint64, height uint16, first, last []byte, depth int) error {
		p, err := q.load(ref, count, height, first, last, depth)
		if err != nil {
			return err
		}
		if p.kind == branchKind {
			for _, c := range p.children {
				if bytes.Compare(c.last, lo) < 0 || bytes.Compare(c.first, hi) >= 0 || (after != nil && bytes.Compare(c.last, after) <= 0) {
					continue
				}
				if err := visit(c.ref, c.count, c.height, c.first, c.last, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		for _, e := range p.entries {
			if bytes.Compare(e.key, lo) < 0 || bytes.Compare(e.key, hi) >= 0 || (after != nil && bytes.Compare(e.key, after) <= 0) {
				continue
			}
			size := len(e.key) + len(e.value)
			if len(result.rows) == rows || size > byteBudget-used {
				if len(result.rows) == 0 {
					return errLimit
				}
				return io.EOF // another authenticated matching row exists; continuation required
			}
			result.rows = append(result.rows, entry{bytes.Clone(e.key), bytes.Clone(e.value)})
			used += size
		}
		return nil
	}
	err := visit(r.root.ref, r.root.count, r.root.height, nil, nil, 1)
	result.reads = q.reads
	if err != nil && err != io.EOF {
		return rangeResult{}, err
	} // do not return partial business answers on corruption
	result.done = err == nil
	if len(result.rows) > 0 {
		result.after = bytes.Clone(result.rows[len(result.rows)-1].key)
	}
	return result, nil
}
