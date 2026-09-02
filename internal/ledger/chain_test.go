package ledger

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// TestForgedPayloadWithRecomputedCRCIsRejectedByMAC is the exact attack ADR
// 0016 exists to stop: CRC32 is public and keyless, so anyone who can write
// to the WAL can edit a settled frame's payload and recompute a CRC that
// matches. Without a MAC, that forged frame replays as fact. With one, CRC
// still passes (it was recomputed correctly) but the MAC — which the
// forger has no key for — does not, and the failure must be reported as
// tampering, not corruption.
func TestForgedPayloadWithRecomputedCRCIsRejectedByMAC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	if _, err := log.Append(context.Background(), validReservation("evt_1", "attempt_1")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payloadLength := binary.BigEndian.Uint32(raw[16:20])
	payloadStart := chainHeaderSize
	payload := raw[payloadStart : payloadStart+int(payloadLength)]
	// Forge the reservation amount: find the digits of "100" in the JSON
	// payload and change them to "999", keeping the payload valid JSON (so
	// the forgery is only caught by the MAC, not by json.Unmarshal choking
	// on malformed bytes first).
	marker := []byte(`"reservation_micros_usd":100`)
	index := bytes.Index(payload, marker)
	if index < 0 {
		t.Fatalf("did not find the reservation amount in the payload: %q", payload)
	}
	copy(payload[index+len(marker)-3:index+len(marker)], []byte("999"))
	checksum := crc32.NewIEEE()
	checksum.Write(raw[4:20])
	checksum.Write(raw[payloadStart : payloadStart+int(payloadLength)])
	binary.BigEndian.PutUint32(raw[20:24], checksum.Sum32())
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := VerifyChain(path, testChainKey); !errors.Is(err, ErrTampered) {
		t.Fatalf("forged payload with recomputed CRC: err=%v, want ErrTampered", err)
	}
	// The same forged file, read without a key, is indistinguishable from an
	// honest frame — CRC alone cannot tell them apart. That gap is the
	// finding this ADR exists to close for keyed readers, and confirming it
	// stays open for keyless ones proves the test above is checking the MAC,
	// not accidentally checking CRC.
	if _, partial, err := Inspect(path); err != nil || partial {
		t.Fatalf("checksum-only read unexpectedly rejected the forged frame: partial=%t err=%v", partial, err)
	}
}

// TestDeletedMiddleFrameIsRejectedByChain is the reason a per-frame MAC is
// not enough on its own (ADR 0016, "why sequence numbers are not enough"):
// the scanner already rejects a sequence that goes backwards, but excising
// one complete frame from the middle of the file — the expensive settlement,
// say — leaves every remaining frame's own CRC and MAC individually valid.
// Only the chain link (this frame's previous-hash no longer matching what
// the prior frame actually produced) catches it.
func TestDeletedMiddleFrameIsRejectedByChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	first, err := log.Append(context.Background(), validReservation("evt_1", "attempt_1"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := log.Append(context.Background(), validReservation("evt_2", "attempt_2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), validReservation("evt_3", "attempt_3")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Excise the second frame's bytes entirely, splicing the third frame
	// directly after the first. The third frame's own CRC and MAC are
	// unchanged and still self-consistent — only its previous-hash now
	// disagrees with what the (now-missing) second frame would have produced.
	spliced := append([]byte{}, raw[:first.Offset]...)
	spliced = append(spliced, raw[second.Offset:]...)
	if err := os.WriteFile(path, spliced, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := VerifyChain(path, testChainKey); !errors.Is(err, ErrTampered) {
		t.Fatalf("deleted middle frame: err=%v, want ErrTampered", err)
	}
	// Sequence numbers alone would not catch this: the scanner only rejects a
	// sequence going backwards or repeating, and 1, 3 is still increasing.
	if _, _, err := Inspect(path); err != nil {
		t.Fatalf("sequence-only read (no key) unexpectedly rejected 1,3 as non-monotonic: %v", err)
	}
}

// TestMasterKeyRotationPreservesVerificationOfPriorFrames is the ADR's
// stated reason the ledger key is loaded from an encrypted envelope rather
// than re-derived from the Master Key on every open: derivation would make
// rotation silently invalidate every historical frame. Rotation here is
// simulated directly at the ledger layer — re-wrapping the same 32-byte key
// under a different envelope must not change what it authenticates.
func TestMasterKeyRotationPreservesVerificationOfPriorFrames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	if _, err := log.Append(context.Background(), validReservation("evt_1", "attempt_1")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), validReservation("evt_2", "attempt_2")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	// "Rotation" at this layer means the same key bytes surviving a round
	// trip through a different envelope — the envelope changes, the key does
	// not, so frames written before the rotation must still verify.
	reopened, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	head, _, ok := reopened.ChainHead()
	sequence := head.Sequence
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if !ok || sequence != 2 {
		t.Fatalf("chain head after reopen with the same key: sequence=%d ok=%t", sequence, ok)
	}
	report, partial, err := VerifyChain(path, testChainKey)
	if err != nil || partial {
		t.Fatalf("VerifyChain after rotation-equivalent reopen: err=%v partial=%t", err, partial)
	}
	if report.Authenticated != 2 || !report.ChainVerified {
		t.Fatalf("report=%#v", report)
	}

	// A different key (standing in for a rotation that actually re-derived
	// instead of re-wrapping) must fail — this is the negative control that
	// proves the positive case above is really checking the key, not just
	// succeeding unconditionally.
	wrongKey := append([]byte(nil), testChainKey...)
	wrongKey[0] ^= 0xff
	if _, _, err := VerifyChain(path, wrongKey); !errors.Is(err, ErrTampered) {
		t.Fatalf("verification under the wrong key: err=%v, want ErrTampered", err)
	}
}
