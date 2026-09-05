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

// downgradeFrameToPeriodEpoch rewrites one authenticated frame as an epoch-3 frame:
// drop the 64-byte previous-hash/MAC tail, set the epoch byte to 3, recompute
// the CRC. Every step uses public information — the CRC is keyless and covers
// exactly the same bytes in both epochs, and an epoch-4 payload already
// carries the period identity epoch 3 requires. That is the whole point: an
// attacker with nothing but write access to the file can strip authentication
// off history unless the reader refuses to accept an unauthenticated frame
// once the authenticated epoch has started.
func downgradeFrameToPeriodEpoch(t *testing.T, frame []byte) []byte {
	t.Helper()
	if !authenticatedFrameEpoch(frame[4]) {
		t.Fatalf("frame is epoch %d, want an authenticated epoch", frame[4])
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
// that predates ADR 0016. What distinguishes the two is that the rewrite
// leaves an unauthenticated frame after an authenticated one, which no writer
// produces.
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

// TestInterleavedLegacyEpochsStillReplay is the shape a real pre-upgrade
// ledger actually has, and the reason the epoch rule has to be narrow. Before
// ADR 0016 the frame epoch was chosen per event by its shape rather than by
// the build: an accounting event carrying a lease mode wrote epoch 2 while the
// plain event beside it wrote epoch 1, so the two interleave throughout the
// file. A rule that simply required a non-decreasing epoch would reject every
// history that predates the guarantee — refusing to start on exactly the
// installs the guarantee was added to protect.
func TestInterleavedLegacyEpochsStillReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	now := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	accepted, err := json.Marshal(Event{
		EventID: "legacy_1", Kind: EventRequestAccepted, RequestID: "r",
		ProjectID: "p", PeriodID: "2026-08-04", OccurredAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := json.Marshal(Event{
		EventID: "legacy_2", Kind: EventReservationCreated, RequestID: "r", AttemptID: "a",
		ProjectID: "p", PeriodID: "2026-08-04", OccurredAt: now,
		ReservationMicrosUSD: MicrosUSD(100), LeaseMode: LeaseModeMetered,
	})
	if err != nil {
		t.Fatal(err)
	}

	var frames []byte
	// v1, v2, v1, v2 — the epoch goes backwards twice, and every one of these
	// frames is honest.
	frames = append(frames, encodeFrameVersion(frameVersionLegacy, 1, EventRequestAccepted, accepted)...)
	frames = append(frames, encodeFrameVersion(frameVersionCurrent, 2, EventReservationCreated, reservation)...)
	frames = append(frames, encodeFrameVersion(frameVersionLegacy, 3, EventRequestAccepted, accepted)...)
	frames = append(frames, encodeFrameVersion(frameVersionCurrent, 4, EventReservationCreated, reservation)...)
	if err := os.WriteFile(path, frames, 0o600); err != nil {
		t.Fatal(err)
	}

	watermark, partial, err := Inspect(path)
	if err != nil || partial {
		t.Fatalf("an interleaved legacy ledger must still replay: partial=%t err=%v", partial, err)
	}
	if watermark.Sequence != 4 {
		t.Fatalf("watermark sequence=%d, want 4", watermark.Sequence)
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
