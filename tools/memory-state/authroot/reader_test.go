package authroot

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"
)

type fixtureSource struct {
	pages map[uint64][]byte
	reads int
}

func (s *fixtureSource) readPage(r pageRef, dst []byte) (int, error) {
	s.reads++
	b, ok := s.pages[r.offset]
	if !ok {
		return 0, errors.New("missing")
	}
	return copy(dst, b), nil
}
func (s *fixtureSource) add(t *testing.T, p page) pageRef {
	t.Helper()
	b, err := encodePage(p)
	if err != nil {
		t.Fatal(err)
	}
	offset := uint64(len(s.pages)+1) * maxPageBytes
	s.pages[offset] = b
	return refFor([16]byte{}, offset, b)
}
func fixture(t *testing.T) (reader, *fixtureSource, []byte) {
	t.Helper()
	s := &fixtureSource{pages: make(map[uint64][]byte)}
	var children []child
	for group := 0; group < 4; group++ {
		p := page{scope: 7, kind: leafKind, count: 4}
		for i := 0; i < 4; i++ {
			k := []byte(fmt.Sprintf("k%02d", group*4+i))
			p.entries = append(p.entries, entry{k, []byte("value:" + string(k))})
		}
		ref := s.add(t, p)
		first, last := p.bounds()
		children = append(children, child{first, last, p.count, p.height(), ref})
	}
	ref := s.add(t, page{scope: 7, kind: branchKind, count: 16, children: children})
	key := bytes.Repeat([]byte{0x53}, 32)
	encoded, err := signRoot(root{scope: 7, height: 2, generation: 1, revision: 2, count: 16, ref: ref}, key)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifyRoot(encoded, key)
	if err != nil {
		t.Fatal(err)
	}
	return reader{s, verified}, s, encoded
}

func TestAuthenticatedQueriesAndPagination(t *testing.T) {
	r, _, _ := fixture(t)
	ctx := context.Background()
	for _, key := range []string{"k00", "k07", "k15"} {
		v, found, err := r.get(ctx, []byte(key))
		if err != nil || !found || string(v) != "value:"+key {
			t.Fatalf("get %s: %q %v %v", key, v, found, err)
		}
	}
	for _, key := range []string{"a", "k031", "z"} {
		_, found, err := r.get(ctx, []byte(key))
		if err != nil || found {
			t.Fatalf("absence %s: %v %v", key, found, err)
		}
	}
	var after []byte
	var keys []string
	for i := 0; i < 16; i++ {
		result, err := r.scan(ctx, []byte("k02"), []byte("k14"), after, 3, 60)
		if err != nil {
			t.Fatal(err)
		}
		if result.reads > maxPageReads || len(result.rows) > 3 {
			t.Fatal("budget exceeded")
		}
		for _, e := range result.rows {
			keys = append(keys, string(e.key))
		}
		if result.done {
			break
		}
		if len(result.rows) == 0 || bytes.Equal(after, result.after) {
			t.Fatal("no pagination progress")
		}
		after = result.after
	}
	if len(keys) != 12 {
		t.Fatalf("missing rows: %v", keys)
	}
	for i, k := range keys {
		if k != fmt.Sprintf("k%02d", i+2) {
			t.Fatalf("range omitted/duplicated row: %v", keys)
		}
	}
}

func TestCorruptionNeverReturnsFalseEmptyOrPartial(t *testing.T) {
	for _, mode := range []string{"delete", "value", "hide-row", "truncate", "foreign-scope"} {
		t.Run(mode, func(t *testing.T) {
			r, s, _ := fixture(t)
			offset := uint64(3) * maxPageBytes
			switch mode {
			case "delete":
				delete(s.pages, offset)
			case "value":
				s.pages[offset][len(s.pages[offset])-1] ^= 1
			case "hide-row":
				p, err := decodePage(s.pages[offset], 7)
				if err != nil {
					t.Fatal(err)
				}
				p.entries = p.entries[1:]
				p.count--
				s.pages[offset], err = encodePage(p)
				if err != nil {
					t.Fatal(err)
				}
			case "truncate":
				s.pages[offset] = s.pages[offset][:5]
			case "foreign-scope":
				s.pages[offset][7] = 8
			}
			_, found, err := r.get(context.Background(), []byte("k09"))
			if !errors.Is(err, errCorrupt) || found {
				t.Fatalf("corruption became absence: %v %v", found, err)
			}
			result, err := r.scan(context.Background(), []byte("k00"), []byte("k99"), nil, 32, 8192)
			if !errors.Is(err, errCorrupt) || len(result.rows) != 0 {
				t.Fatalf("corruption returned partial rows: %+v %v", result, err)
			}
		})
	}
}

func TestRootAuthenticationAndNoncanonicalDecode(t *testing.T) {
	_, _, encoded := fixture(t)
	key := bytes.Repeat([]byte{0x53}, 32)
	for i := range encoded {
		bad := bytes.Clone(encoded)
		bad[i] ^= 1
		if _, err := verifyRoot(bad, key); err == nil {
			t.Fatalf("accepted changed root byte %d", i)
		}
	}
	for _, bad := range [][]byte{encoded[:len(encoded)-1], append(bytes.Clone(encoded), 0)} {
		if _, err := verifyRoot(bad, key); err == nil {
			t.Fatal("accepted length")
		}
	}
	// Even a correctly MACed unknown critical version must fail.
	bad := bytes.Clone(encoded)
	binary.BigEndian.PutUint16(bad[8:10], pageFormat+1)
	mac := hmac.New(sha256.New, key)
	mac.Write(bad[:len(bad)-32])
	copy(bad[len(bad)-32:], mac.Sum(nil))
	if _, err := verifyRoot(bad, key); err == nil {
		t.Fatal("accepted unknown version")
	}
}

func TestPageLimitsAndSemanticValidation(t *testing.T) {
	r, s, _ := fixture(t)
	b := s.pages[r.root.ref.offset]
	for _, mutate := range []func([]byte){
		func(b []byte) { binary.BigEndian.PutUint16(b[18:20], 65535) },
		func(b []byte) { b[5] = 1 },
		func(b []byte) { b[4] = 9 },
		func(b []byte) { binary.BigEndian.PutUint64(b[10:18], math.MaxUint64) },
	} {
		bad := bytes.Clone(b)
		mutate(bad)
		if _, err := decodePage(bad, 7); err == nil {
			t.Fatal("accepted invalid page")
		}
	}
	if _, err := decodePage(append(bytes.Clone(b), 0), 7); err == nil {
		t.Fatal("accepted trailing data")
	}
	if _, err := encodePage(page{scope: 7, kind: leafKind, count: 2, entries: []entry{{[]byte("x"), nil}, {[]byte("x"), nil}}}); err == nil {
		t.Fatal("accepted duplicate")
	}
	p, err := decodePage(b, 7)
	if err != nil {
		t.Fatal(err)
	}
	p.children[1].first = p.children[0].last
	if _, err := encodePage(p); err == nil {
		t.Fatal("accepted overlapping children")
	}
	if (pageRef{offset: math.MaxInt64, length: 1}).valid() {
		t.Fatal("accepted overflow ref")
	}
	result, err := r.scan(context.Background(), []byte("a"), []byte("z"), nil, 1, 1)
	if !errors.Is(err, errLimit) || len(result.rows) != 0 {
		t.Fatal("too-small budget must fail with no answer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.get(ctx, []byte("k00")); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
}

func TestBranchCommitmentCheckedAgainstReachedChild(t *testing.T) {
	r, s, _ := fixture(t)
	p, err := decodePage(s.pages[r.root.ref.offset], 7)
	if err != nil {
		t.Fatal(err)
	}
	// A trusted-format producer error must still fail when that child is reached.
	p.children[0].first = []byte("a")
	ref := s.add(t, p)
	r.root.ref = ref
	if _, _, err := r.get(context.Background(), []byte("k01")); !errors.Is(err, errCorrupt) {
		t.Fatalf("accepted wrong child bounds: %v", err)
	}
}

func FuzzPageDecoder(f *testing.F) {
	b, _ := encodePage(page{scope: 7, kind: leafKind, count: 1, entries: []entry{{[]byte("k"), []byte("v")}}})
	f.Add(b)
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{255}, maxPageBytes+1))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := decodePage(b, 7)
		if err != nil {
			return
		}
		encoded, err := encodePage(p)
		if err != nil || !bytes.Equal(encoded, b) {
			t.Fatal("accepted noncanonical encoding")
		}
	})
}

func TestTraversalDepthIsBounded(t *testing.T) {
	r, s, _ := fixture(t)
	q := query{ctx: context.Background(), reader: r}
	if _, err := q.load(r.root.ref, r.root.count, r.root.height, nil, nil, maxDepth+1); !errors.Is(err, errLimit) {
		t.Fatalf("unbounded depth: %v", err)
	}
	if s.reads != 0 {
		t.Fatalf("read beyond depth budget: %d", s.reads)
	}
	q.reads = maxPageReads
	if _, err := q.load(r.root.ref, r.root.count, r.root.height, nil, nil, 1); !errors.Is(err, errLimit) {
		t.Fatalf("unbounded reads: %v", err)
	}
	bad := r.root
	bad.height = maxDepth + 1
	if bad.valid() {
		t.Fatal("accepted oversized root height")
	}
}

type cancelSource struct {
	fixture *fixtureSource
	cancel  context.CancelFunc
}

func (s cancelSource) readPage(ref pageRef, dst []byte) (int, error) {
	n, err := s.fixture.readPage(ref, dst)
	s.cancel()
	return n, err
}
func TestCancellationDuringReadDoesNotReturnAnswer(t *testing.T) {
	r, s, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.source = cancelSource{s, cancel}
	if v, found, err := r.get(ctx, []byte("k00")); !errors.Is(err, context.Canceled) || found || v != nil {
		t.Fatalf("cancelled read answered: %q %v %v", v, found, err)
	}
}

type noReadSource struct{}

func (noReadSource) readPage(pageRef, []byte) (int, error) {
	panic("canonical empty root must not read physical pages")
}
func TestCanonicalEmptyRoot(t *testing.T) {
	previous, _, _ := fixture(t)
	empty := root{scope: 7, generation: previous.root.generation, revision: previous.root.revision + 1, count: 0, ref: emptyRef(7)}
	key := bytes.Repeat([]byte{0x53}, 32)
	encoded, err := signRoot(empty, key)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifyRoot(encoded, key)
	if err != nil {
		t.Fatal(err)
	}
	r := reader{noReadSource{}, verified}
	if v, found, err := r.get(context.Background(), []byte("k00")); err != nil || found || v != nil {
		t.Fatalf("empty get: %q %v %v", v, found, err)
	}
	result, err := r.scan(context.Background(), []byte("a"), []byte("z"), nil, 32, 8192)
	if err != nil || !result.done || len(result.rows) != 0 || result.reads != 0 {
		t.Fatalf("empty scan: %+v %v", result, err)
	}
	if emptyRef(7) == emptyRef(8) {
		t.Fatal("empty commitment not bound to component")
	}
	for _, mutate := range []func(*root){
		func(r *root) { r.ref = pageRef{} },
		func(r *root) { r.ref = emptyRef(8) },
		func(r *root) { r.ref = previous.root.ref },
		func(r *root) { r.count = 1 },
	} {
		bad := empty
		mutate(&bad)
		if _, err := signRoot(bad, key); err == nil {
			t.Fatal("signed noncanonical empty")
		}
	}
	for i := range encoded {
		bad := bytes.Clone(encoded)
		bad[i] ^= 1
		if _, err := verifyRoot(bad, key); err == nil {
			t.Fatalf("accepted changed empty root byte %d", i)
		}
	}
}
