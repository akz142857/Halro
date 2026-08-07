package ledger

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// downgradeFrameToPeriodEpoch rewrites one epoch-4 frame as an epoch-3 frame:
// drop the 64-byte previous-hash/MAC tail, set the epoch byte to 3, recompute
// the CRC. Every step uses public information — the CRC is keyless and covers
// exactly the same bytes in both epochs, and an epoch-4 payload already
// carries the period identity epoch 3 requires. That is the whole point: an
// attacker with nothing but write access to the file can strip authentication
// off history unless the reader refuses to accept a frame whose epoch went
// backwards.
func downgradeFrameToPeriodEpoch(t *testing.T, frame []byte) []byte {
	t.Helper()
	if frame[4] != frameVersionLedgerIntegrity {
		t.Fatalf("frame is epoch %d, want %d", frame[4], frameVersionLedgerIntegrity)
	}
	payloadLength := binary.BigEndian.Uint32(frame[16:20])
	payload := frame[chainHeaderSize : chainHeaderSize+int(payloadLength)]
	downgraded := make([]byte, 0, frameHeaderSize+len(payload))
	downgraded = append(downgraded, frame[:frameHeaderSize]...)
	downgraded = append(downgraded, payload...)
	downgraded[4] = frameVersionPeriod
	checksum := crc32.NewIEEE()
	checksum.Write(downgraded[4:20])
	checksum.Write(payload)
	binary.BigEndian.PutUint32(downgraded[20:24], checksum.Sum32())
	return downgraded
}

// TestEpochDowngradeIsRejected covers the gap that per-frame MACs and the
// chain link both miss. Both of those only run on epoch-4 frames, so rewriting
// a frame as epoch 3 does not defeat them — it removes them from the picture
// entirely, and the file that comes back reads as a legitimately old ledger
// that predates ADR 0016. Only the epoch itself, which a writer never moves
// backwards, distinguishes the two.
func TestEpochDowngradeIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	for _, id := range []string{"evt_1", "evt_2", "evt_3"} {
		if _, err := log.Append(context.Background(), validReservation(id, "attempt_"+id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Keep the first frame honest and downgrade the rest, which is the shape
	// that matters: a file with no epoch-4 frames at all could be an honest
	// pre-upgrade ledger, but a file that has them and then goes backwards
	// cannot be anything but rewritten.
	firstLength := int(binary.BigEndian.Uint32(raw[16:20]))
	firstEnd := chainHeaderSize + firstLength
	rebuilt := append([]byte{}, raw[:firstEnd]...)
	for offset := firstEnd; offset < len(raw); {
		length := int(binary.BigEndian.Uint32(raw[offset+16 : offset+20]))
		end := offset + chainHeaderSize + length
		rebuilt = append(rebuilt, downgradeFrameToPeriodEpoch(t, raw[offset:end])...)
		offset = end
	}
	if err := os.WriteFile(path, rebuilt, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := VerifyChain(path, testChainKey); !errors.Is(err, ErrTampered) {
		t.Fatalf("downgraded suffix: err=%v, want ErrTampered", err)
	}
	// A keyless reader has no MAC to check and no chain to walk, so this is
	// the only defence it has at all. It must hold there too, or offline
	// tooling and any build without the key silently certifies the rewrite.
	if _, _, err := Inspect(path); !errors.Is(err, ErrTampered) {
		t.Fatalf("downgraded suffix read without a key: err=%v, want ErrTampered", err)
	}
}

// TestForgedLegacyFrameAppendedAfterChainIsRejected is the same weakness used
// constructively rather than destructively: instead of rewriting history, add
// to it. An epoch-3 frame hand-built onto the end of an authenticated log
// needs no key, and since the chain only advances across epoch-4 frames, the
// verifier previously walked right past it and still reported the chain as
// verified — the forged event replayed as fact with no signal at all.
func TestForgedLegacyFrameAppendedAfterChainIsRejected(t *testing.T) {
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

	forged := validReservation("evt_forged", "attempt_forged")
	forged.OccurredAt = time.Now().UTC()
	forged.ReservationMicrosUSD = MicrosUSD(999999)
	payload, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, frameHeaderSize+len(payload))
	copy(frame[:4], frameMagic)
	frame[4] = frameVersionPeriod
	frame[5] = byte(EventReservationCreated)
	binary.BigEndian.PutUint64(frame[8:16], 2)
	binary.BigEndian.PutUint32(frame[16:20], uint32(len(payload)))
	copy(frame[frameHeaderSize:], payload)
	checksum := crc32.NewIEEE()
	checksum.Write(frame[4:20])
	checksum.Write(payload)
	binary.BigEndian.PutUint32(frame[20:24], checksum.Sum32())
	if err := os.WriteFile(path, append(raw, frame...), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := VerifyChain(path, testChainKey); !errors.Is(err, ErrTampered) {
		t.Fatalf("forged legacy frame appended after an epoch-4 chain: err=%v, want ErrTampered", err)
	}
}
