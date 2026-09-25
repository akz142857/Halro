package audit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reaching the audit log's ordering checks.
//
// `scan` tests three things about every frame before it authenticates one: the
// header, the sequence, and the previous-frame hash. The sequence and chain
// branches had no test at all (260918-PV-F-18). The existing tampering test
// flips a payload byte, which leaves both of them satisfied and fails at the
// HMAC — so it proves the MAC works and says nothing about whether the log
// notices a *missing* record.
//
// That distinction is the whole point of the two checks. An adversary who can
// write in the data directory but holds no key is stopped by the MAC. The
// sequence and chain are for what is left: deleting a frame, reordering two,
// and — for anyone who does hold the key — re-signing part of the history
// without re-signing the rest. Nothing about a valid MAC on each surviving
// frame makes a history with a hole in it acceptable.
//
// Building these cases means writing frames the log itself would produce, so
// the tests hold the key and use the package's own encoder. A test that could
// only corrupt bytes could never reach past the MAC.

// auditFrames splits a log file into its frames, so a test can rebuild one
// without them.
func auditFrames(t *testing.T, path string) [][]byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var frames [][]byte
	for offset := 0; offset < len(contents); {
		if offset+frameHeaderSize > len(contents) {
			t.Fatalf("a partial frame at offset %d", offset)
		}
		header := contents[offset : offset+frameHeaderSize]
		payloadLength := int(uint32(header[16])<<24 | uint32(header[17])<<16 |
			uint32(header[18])<<8 | uint32(header[19]))
		size := frameHeaderSize + payloadLength + frameMACSize
		if offset+size > len(contents) {
			t.Fatalf("a truncated frame at offset %d", offset)
		}
		frames = append(frames, contents[offset:offset+size])
		offset += size
	}
	return frames
}

// writeAuditLog builds a log of n events and returns its path and key.
func writeAuditLog(t *testing.T, events int) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	key := randomKey(t)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	for index := range events {
		if _, err := log.Append(context.Background(), validEvent(index, "key.create")); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	return path, key
}

// TestARemovedRecordIsCaughtBySequenceNotByTheMAC.
//
// The case the finding named: every surviving frame authenticates perfectly,
// because none of them was touched. What is wrong with the file is what is no
// longer in it, and only the sequence can see that.
func TestARemovedRecordIsCaughtBySequenceNotByTheMAC(t *testing.T) {
	path, key := writeAuditLog(t, 5)
	frames := auditFrames(t, path)
	if len(frames) != 5 {
		t.Fatalf("wrote 5 events and read %d frames", len(frames))
	}
	// Excise the third. The first two still chain correctly, so verification
	// gets as far as the fourth before anything is wrong.
	var rebuilt []byte
	for index, frame := range frames {
		if index == 2 {
			continue
		}
		rebuilt = append(rebuilt, frame...)
	}
	if err := os.WriteFile(path, rebuilt, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Verify(path, key)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a log with a record removed verified: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid sequence") {
		t.Fatalf("the gap was caught by something other than the sequence check: %v", err)
	}
}

// TestReorderedRecordsAreCaught. Two frames swapped leaves every MAC valid and
// every record present; what changed is the order they claim to have happened
// in, which is exactly what an audit trail is for.
func TestReorderedRecordsAreCaught(t *testing.T) {
	path, key := writeAuditLog(t, 4)
	frames := auditFrames(t, path)
	frames[1], frames[2] = frames[2], frames[1]
	var rebuilt []byte
	for _, frame := range frames {
		rebuilt = append(rebuilt, frame...)
	}
	if err := os.WriteFile(path, rebuilt, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Verify(path, key)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a reordered log verified: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid sequence") {
		t.Fatalf("reordering was caught by something other than the sequence check: %v", err)
	}
}

// TestARewrittenRecordBreaksTheChainEvenWhenReSigned.
//
// This is the check that does not overlap with the MAC at all. An adversary who
// holds the audit key can re-sign whatever they write — so the MAC alone cannot
// stop them replacing a record. The chain can, unless they also re-sign every
// frame after it: the next frame carries the hash of the one it followed, and
// the replacement hashes differently.
//
// The test forges the way such an adversary would, with the real key and the
// package's own encoder, and changes one record in place.
func TestARewrittenRecordBreaksTheChainEvenWhenReSigned(t *testing.T) {
	path, key := writeAuditLog(t, 4)
	frames := auditFrames(t, path)

	// Rebuild frame 2 with different content, correctly signed and at the same
	// sequence, chained onto the same predecessor.
	previous := sha256.Sum256(frames[0])
	// A different, entirely valid event: the forgery has to survive decoding
	// and validation, or it would be caught before the chain is ever consulted
	// — which is a weaker result and not the one this test is about.
	replacement, err := json.Marshal(validEvent(99, "credential.delete"))
	if err != nil {
		t.Fatal(err)
	}
	forged, forgedHash := encodeFrame(key, 2, previous, replacement)
	if forgedHash == sha256.Sum256(frames[1]) {
		t.Fatal("the forged frame is identical to the original; the test proves nothing")
	}
	rebuilt := append([]byte(nil), frames[0]...)
	rebuilt = append(rebuilt, forged...)
	rebuilt = append(rebuilt, frames[2]...)
	rebuilt = append(rebuilt, frames[3]...)
	if err := os.WriteFile(path, rebuilt, 0o600); err != nil {
		t.Fatal(err)
	}

	_, verifyErr := Verify(path, key)
	if !errors.Is(verifyErr, ErrCorrupt) {
		t.Fatalf("a re-signed rewrite verified: %v", verifyErr)
	}
	// Not the MAC: the forged frame's MAC is valid, and so is every other
	// frame's. What fails is frame 3, which still carries the hash of the
	// record that was replaced.
	if !strings.Contains(verifyErr.Error(), "broken hash chain") {
		t.Fatalf("the rewrite was caught by something other than the chain: %v", verifyErr)
	}
}

// TestAValidlySignedFrameAtTheWrongSequenceIsRefused pins the branch itself
// rather than a scenario, so the ordering of the checks inside scan cannot
// change without somebody noticing: the sequence is tested before the MAC, and
// a frame that is perfectly signed still has to be in the right place.
func TestAValidlySignedFrameAtTheWrongSequenceIsRefused(t *testing.T) {
	path, key := writeAuditLog(t, 2)
	frames := auditFrames(t, path)
	previous := sha256.Sum256(frames[0])
	// Sequence 3 where 2 belongs, correctly signed, correctly chained.
	forged, _ := encodeFrame(key, 3, previous, framePayload(t, frames[1]))
	if err := os.WriteFile(path, append(append([]byte(nil), frames[0]...), forged...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Verify(path, key)
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "invalid sequence") {
		t.Fatalf("a correctly signed frame at the wrong sequence verified: %v", err)
	}
}

// TestOpenRefusesAGappedLogRatherThanRepairingIt.
//
// Open repairs a torn final frame, because that is a crash. A gap in the middle
// is not a crash — no append can produce one — so the repair must not extend to
// it. An instance that silently truncated to the last good record would be
// deleting the evidence of whatever removed the record.
func TestOpenRefusesAGappedLogRatherThanRepairingIt(t *testing.T) {
	path, key := writeAuditLog(t, 5)
	frames := auditFrames(t, path)
	var rebuilt []byte
	for index, frame := range frames {
		if index == 2 {
			continue
		}
		rebuilt = append(rebuilt, frame...)
	}
	if err := os.WriteFile(path, rebuilt, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	log, err := Open(path, key)
	if err == nil {
		log.Close()
		t.Fatal("a gapped log opened")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("the refused open changed the file: %d bytes became %d", len(before), len(after))
	}
}

// framePayload returns a frame's payload bytes.
func framePayload(t *testing.T, frame []byte) []byte {
	t.Helper()
	length := int(uint32(frame[16])<<24 | uint32(frame[17])<<16 | uint32(frame[18])<<8 | uint32(frame[19]))
	return append([]byte(nil), frame[frameHeaderSize:frameHeaderSize+length]...)
}
