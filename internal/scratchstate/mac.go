package scratchstate

import (
	"crypto/hmac"
	"crypto/sha256"
)

// Only Create initializes this constant-size workspace with its fresh owner
// key. All actual pages still undergo FD reads and full HMAC verification.
// Keeping the standard HMAC's fixed pad states avoids hashing the same two
// key pads for every tree traversal; no page, row or verification is cached.
func (s *Store) initializeMAC() {
	s.mac = hmac.New(sha256.New, s.key[:])
	s.mac.Reset()
	var warm [sha256.Size]byte
	s.mac.Sum(warm[:0])
	s.mac.Reset()
	clear(warm[:])
}

// A bare, uninitialized library fixture retains the original fixed-stack
// implementation. Actual Create owners use the standard workspace above.
func (s *Store) keyedMAC(first, second []byte) [32]byte {
	if s.mac != nil && len(first)+len(second) <= len(s.macInput) {
		s.mac.Reset()
		// An owner's fixed buffer avoids making the caller's stack page escape
		// through hash.Hash on every authenticated FD read.
		n := copy(s.macInput[:], first)
		n += copy(s.macInput[n:], second)
		s.mac.Write(s.macInput[:n])
		clear(s.macInput[:n])
		s.mac.Sum(s.macOutput[:0])
		var result [sha256.Size]byte
		copy(result[:], s.macOutput[:])
		clear(s.macOutput[:])
		return result
	}
	if len(first)+len(second) <= 512 {
		var inner [64 + 512]byte
		return s.macBuffer(inner[:], first, second)
	}
	return s.wideKeyMAC(first, second)
}

func (s *Store) wideKeyMAC(first, second []byte) [32]byte {
	var inner [64 + MaxKeyBytes + 64]byte
	return s.macBuffer(inner[:], first, second)
}

func (s *Store) macBuffer(inner, first, second []byte) [32]byte {
	for i := 0; i < 64; i++ {
		inner[i] = 0x36
		if i < len(s.key) {
			inner[i] ^= s.key[i]
		}
	}
	copy(inner[64:], first)
	copy(inner[64+len(first):], second)
	digest := sha256.Sum256(inner[:64+len(first)+len(second)])
	var outer [64 + sha256.Size]byte
	for i := 0; i < 64; i++ {
		outer[i] = 0x5c
		if i < len(s.key) {
			outer[i] ^= s.key[i]
		}
	}
	copy(outer[64:], digest[:])
	result := sha256.Sum256(outer[:])
	clear(inner)
	clear(outer[:])
	clear(digest[:])
	return result
}
