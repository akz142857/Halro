package diskset

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDisposableSetDeepPathsAndOldValidNodesCannotInventAbsence(t *testing.T) {
	ctx := context.Background()
	s, err := Create(ctx, filepath.Join(t.TempDir(), "deep-set"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	// The zero key follows 256 increasing branches. No recursion, decoded
	// history map or unbounded node cache is needed to follow the final path.
	if err := s.Add(ctx, [32]byte{}); err != nil {
		t.Fatal(err)
	}
	for bit := 0; bit < 256; bit++ {
		var key [32]byte
		key[bit/8] = 1 << (7 - bit%8)
		if err := s.Add(ctx, key); err != nil {
			t.Fatal(bit, err)
		}
	}
	for bit := -1; bit < 256; bit++ {
		var key [32]byte
		if bit >= 0 {
			key[bit/8] = 1 << (7 - bit%8)
		}
		if found, err := s.Contains(ctx, key); err != nil || !found {
			t.Fatal(bit, found, err)
		}
	}
	if found, err := s.Contains(ctx, sha256.Sum256([]byte("absent"))); err != nil || found {
		t.Fatal(found, err)
	}
	// Save an authentic parent at its original offset, then mutate that same
	// parent through another insertion. Even with its still-valid MAC, replay
	// of the old parent must fail the independently held child/root hash.
	oldRoot := s.root
	var old [nodeBytes]byte
	if _, err := s.file.ReadAt(old[:], int64(oldRoot.offset)); err != nil {
		t.Fatal(err)
	}
	added := sha256.Sum256([]byte("new-member"))
	if err := s.Add(ctx, added); err != nil {
		t.Fatal(err)
	}
	if _, err := s.file.WriteAt(old[:], int64(oldRoot.offset)); err != nil {
		t.Fatal(err)
	}
	if found, err := s.Contains(ctx, added); !errors.Is(err, ErrCorrupt) || found {
		t.Fatal("old valid node invented absence", found, err)
	}
}

func TestDisposableSetCapCancellationAndExactRetirement(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bounded-set")
	s, err := Create(ctx, path, 3*nodeBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, sha256.Sum256([]byte("one"))); err != nil {
		t.Fatal(err)
	}
	before := s.size
	if err := s.Add(ctx, sha256.Sum256([]byte("two"))); !errors.Is(err, ErrLimit) || s.size != before {
		t.Fatal("partial append exceeded cap", s.size, before, err)
	}
	if _, err := s.Contains(ctx, sha256.Sum256([]byte("one"))); !errors.Is(err, ErrCorrupt) {
		t.Fatal("poisoned partial job remained usable", err)
	}
	if err := os.Rename(path, path+".owned"); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(); err == nil {
		t.Fatal("missing original name became a retirement receipt")
	}
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(); err == nil {
		t.Fatal("foreign replacement removed")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "foreign" {
		t.Fatal(data, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".owned", path); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Create(canceled, path, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled creation performed IO", err)
	}
}

func TestDisposableCounterDuplicatesStayFixedAndOldCountsCannotReplay(t *testing.T) {
	ctx := context.Background()
	s, err := Create(ctx, filepath.Join(t.TempDir(), "counts"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	for bit := -1; bit < 256; bit++ {
		var key [32]byte
		if bit >= 0 {
			key[bit/8] = 1 << (7 - bit%8)
		}
		if n, err := s.Increment(ctx, key); err != nil || n != 1 {
			t.Fatal(bit, n, err)
		}
	}
	originalSize := s.size
	for repeat := uint64(2); repeat <= 200; repeat++ {
		if n, err := s.Increment(ctx, [32]byte{}); err != nil || n != repeat {
			t.Fatal(n, err)
		}
	}
	if s.size != originalSize {
		t.Fatal("duplicate counters grew the file", originalSize, s.size)
	}
	var old [nodeBytes]byte
	if _, err := s.file.ReadAt(old[:], int64(s.root.offset)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Increment(ctx, [32]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.file.WriteAt(old[:], int64(s.root.offset)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Increment(ctx, [32]byte{}); !errors.Is(err, ErrCorrupt) || n != 0 {
		t.Fatal("old authentic count was accepted", n, err)
	}
}

func TestDisposableCounterOverflowAndCancellationPrecedeMutation(t *testing.T) {
	ctx := context.Background()
	s, err := Create(ctx, filepath.Join(t.TempDir(), "counts"), 3*nodeBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	key := sha256.Sum256([]byte("one"))
	if n, err := s.Increment(ctx, key); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	before := s.root
	if n, err := s.Increment(cancelled, key); !errors.Is(err, context.Canceled) || n != 0 || before != s.root {
		t.Fatal(n, err)
	}
	leaf, err := s.read(ctx, s.root)
	if err != nil {
		t.Fatal(err)
	}
	leaf.count = ^uint64(0)
	s.root, err = s.write(ctx, s.root.offset, leaf)
	if err != nil {
		t.Fatal(err)
	}
	before = s.root
	if n, err := s.Increment(ctx, key); !errors.Is(err, ErrLimit) || n != 0 || before != s.root {
		t.Fatal(n, err)
	}
	if _, err := s.Increment(ctx, sha256.Sum256([]byte("two"))); !errors.Is(err, ErrCorrupt) {
		t.Fatal("overflow did not poison operation", err)
	}
}
