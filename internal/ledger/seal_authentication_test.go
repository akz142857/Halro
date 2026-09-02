package ledger

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// Sealing moved the archive out of the open path, and with it the guarantee
// that starting the process examined every byte the balances rest on. Open now
// checks a sealed generation for presence and size, which is what makes a roll
// worth doing — but "not examined at startup" must not become "not examined
// before it is believed".
//
// The line these tests draw: a sealed generation is authenticated whenever it
// is replayed, because replaying it is the only way it can reach a balance.

// tamperWithSealedFrame edits the first frame's payload and repairs its CRC,
// which is what separates this from an ordinary bit flip: the cheap check
// passes, so only the frame's MAC can still object.
func tamperWithSealedFrame(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < chainHeaderSize+8 {
		t.Fatalf("sealed generation is only %d bytes", len(raw))
	}
	payloadLength := int(binary.BigEndian.Uint32(raw[16:20]))
	if payloadLength <= 0 || chainHeaderSize+payloadLength > len(raw) {
		t.Fatalf("first frame declares a %d byte payload", payloadLength)
	}
	payload := raw[chainHeaderSize : chainHeaderSize+payloadLength]
	payload[payloadLength/2] ^= 0x20
	checksum := crc32.NewIEEE()
	checksum.Write(raw[4:20])
	checksum.Write(payload)
	binary.BigEndian.PutUint32(raw[20:24], checksum.Sum32())
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A sealed generation whose frames were edited and CRCs repaired replays into
// nothing: the MAC chain refuses it. Before this, replay scanned sealed
// generations with no verifier at all, so the same file replayed cleanly.
func TestReplayRefusesATamperedSealedGeneration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 12)
	result, err := log.Roll()
	if err != nil {
		t.Fatal(err)
	}
	appendReservations(t, log, 13, 3)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	sealed := filepath.Join(directory, result.Sealed.File)
	tamperWithSealedFrame(t, sealed)
	// The manifest is repaired too, the way an attacker who can write the data
	// directory would: size and checksum are made to agree with the edited
	// file, so presence-and-size at open has nothing to object to.
	repairManifestChecksum(t, directory, result.Sealed.Generation, sealed)

	reopened, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatalf("open refused a directory whose active file is intact: %v", err)
	}
	defer reopened.Close()

	_, replayErr := reopened.Replay(Watermark{}, func(Record) error { return nil })
	if !errors.Is(replayErr, ErrTampered) && !errors.Is(replayErr, ErrCorrupt) {
		t.Fatalf("replay accepted a tampered sealed generation: %v", replayErr)
	}
}

// And a manifest edited to forget older generations is refused outright. The
// files are still on disk; what changed is the record of which of them the
// balances are built from, and a shorter history that reports itself healthy is
// the failure mode sealing could most easily have introduced.
func TestAManifestThatDropsGenerationsIsRefused(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ledger.wal")
	log := openChained(t, path, nil)
	appendReservations(t, log, 1, 6)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendReservations(t, log, 7, 6)
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	appendReservations(t, log, 13, 2)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	manifestPath := filepath.Join(directory, segmentManifestName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest segmentManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Segments) != 2 {
		t.Fatalf("fixture sealed %d generations, want 2", len(manifest.Segments))
	}
	manifest.Segments = manifest.Segments[1:]
	edited, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("open accepted a manifest missing its first generation: %v", err)
	}
}

func repairManifestChecksum(t *testing.T, directory string, generation uint64, file string) {
	t.Helper()
	manifestPath := filepath.Join(directory, segmentManifestName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest segmentManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	checksum, length, err := hashFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for index, segment := range manifest.Segments {
		if segment.Generation != generation {
			continue
		}
		segment.PlainChecksum, segment.StoredChecksum = checksum, checksum
		segment.Length, segment.StoredLength = length, length
		manifest.Segments[index] = segment
	}
	edited, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, edited, 0o600); err != nil {
		t.Fatal(err)
	}
}
