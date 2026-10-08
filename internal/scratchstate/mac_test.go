package scratchstate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"testing"
)

// Isolates authentication CPU with identical key/input bytes. This does not
// measure FD IO, a complete reducer, or migration capacity.
func BenchmarkScratchMACImplementations(b *testing.B) {
	s := &Store{key: [32]byte{1, 2, 3}}
	var input [MaxKeyBytes]byte
	for _, n := range []int{64, pageBytes - 32, MaxKeyBytes} {
		b.Run(fmt.Sprintf("owner-workspace/%d", n), func(b *testing.B) {
			owner := &Store{key: s.key}
			owner.initializeMAC()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				owner.keyedMAC(nil, input[:n])
			}
		})
		b.Run(fmt.Sprintf("fixed/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s.keyedMAC(nil, input[:n])
			}
		})
		b.Run(fmt.Sprintf("retained-standard/%d", n), func(b *testing.B) {
			h := hmac.New(sha256.New, s.key[:])
			var result [sha256.Size]byte
			h.Reset()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h.Reset()
				h.Write(input[:n])
				h.Sum(result[:0])
			}
		})
	}
}

func TestScratchFixedMACMatchesStandardEveryKeyLengthAndPage(t *testing.T) {
	s := &Store{}
	for i := range s.key {
		s.key[i] = byte(i*7 + 3)
	}
	s.initializeMAC()
	var input [MaxKeyBytes]byte
	for i := range input {
		input[i] = byte(i*11 + 5)
	}
	for n := 0; n <= len(input); n++ {
		for _, prefix := range [][]byte{nil, []byte("scratch-priority-v1")} {
			h := hmac.New(sha256.New, s.key[:])
			h.Write(prefix)
			h.Write(input[:n])
			actual := s.keyedMAC(prefix, input[:n])
			if !hmac.Equal(actual[:], h.Sum(nil)) {
				t.Fatal("standard MAC differs", n, len(prefix))
			}
		}
	}
	for n := 0; n <= pageBytes; n++ {
		h := hmac.New(sha256.New, s.key[:])
		h.Write(input[:n])
		actual := s.pageMAC(input[:n])
		if !hmac.Equal(actual[:], h.Sum(nil)) {
			t.Fatal("page MAC differs", n)
		}
	}
}

func TestScratchFixedMACDoesNotAllocateOrRetainWorkspace(t *testing.T) {
	s := &Store{key: [32]byte{1, 2, 3}}
	s.initializeMAC()
	var key [MaxKeyBytes]byte
	for _, n := range []int{0, 32, pageBytes - 32, 512, 513, MaxKeyBytes} {
		if got := testing.AllocsPerRun(100, func() { s.keyedMAC([]byte("scratch-priority-v1"), key[:n]) }); got != 0 {
			t.Fatal("fixed MAC allocated", n, got)
		}
	}
}

func TestScratchActualFDPageBuffersDoNotAllocateOrAlias(t *testing.T) {
	s, _ := testStore(t, 4<<20)
	if err := s.Update(context.Background(), func(tx *Transaction) error {
		if err := tx.Put([]byte("a"), []byte("one")); err != nil {
			return err
		}
		return tx.Put([]byte("b"), []byte("two"))
	}); err != nil {
		t.Fatal(err)
	}
	page, err := s.readPage(0, s.root)
	if err != nil {
		t.Fatal(err)
	}
	original := page
	child := decodeLink(page[32:72])
	if child.empty() {
		child = decodeLink(page[72:112])
	}
	if _, err := s.readPage(0, child); err != nil {
		t.Fatal(err)
	}
	if page != original {
		t.Fatal("later actual FD read changed an earlier page")
	}
	for _, operation := range []struct {
		name string
		run  func()
	}{
		{"read", func() {
			if actual, err := s.readPage(0, s.root); err != nil || actual != original {
				t.Fatal("actual page read differs", err)
			}
		}},
		{"write", func() {
			if actual, err := s.writePage(0, s.root.id, original[:pageBytes-32]); err != nil || actual != s.root {
				t.Fatal("actual page write differs", actual, s.root, err)
			}
		}},
	} {
		if got := testing.AllocsPerRun(100, operation.run); got > maximumPageIOAllocations {
			t.Fatal("actual fixed FD page IO allocated", operation.name, got)
		}
	}
	var persisted [pageBytes]byte
	if _, err := s.files[0].ReadAt(persisted[:], int64(s.root.id)*pageBytes); err != nil {
		t.Fatal(err)
	}
	h := hmac.New(sha256.New, s.key[:])
	h.Write(persisted[:pageBytes-32])
	if !bytes.Equal(h.Sum(nil), persisted[pageBytes-32:]) {
		t.Fatal("fixed FD write changed standard page MAC format")
	}
}
