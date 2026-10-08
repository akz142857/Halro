package ledger

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replicationrange"
)

// Native bytes are built with the same encoders as historical writers. The
// fixture deliberately permits legacy sequence gaps; authenticated chain state
// and global native sequence are separate authorities.
func ledgerRangeHistory(t *testing.T, epochs [][]byte, compressed bool) (string, [][][]byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	var segments []Segment
	var hash [32]byte
	var sequence uint64
	var highest byte
	frames := make([][][]byte, len(epochs))
	for gen, versions := range epochs {
		start := hash
		var raw []byte
		first := sequence + 1
		for _, version := range versions {
			sequence++
			event := validReservation(fmt.Sprintf("evt_%d", sequence), fmt.Sprintf("att_%d", sequence))
			if version == frameVersionCurrent {
				event.LeaseMode = "reserved"
			}
			payload, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var frame []byte
			if authenticatedFrameEpoch(version) {
				frame, hash = encodeChainFrameVersion(version, testChainKey, sequence, event.Kind, payload, hash)
				if version > highest {
					highest = version
				}
			} else {
				frame = encodeFrameVersion(version, sequence, event.Kind, payload)
			}
			frames[gen] = append(frames[gen], frame)
			raw = append(raw, frame...)
		}
		if gen == len(epochs)-1 {
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			break
		}
		segment := Segment{Generation: uint64(gen + 1), File: fmt.Sprintf("ledger-%d.wal", gen+1), FirstSequence: first, LastSequence: sequence, Length: int64(len(raw)), StoredLength: int64(len(raw)), StartHash: hex.EncodeToString(start[:]), EndHash: hex.EncodeToString(hash[:]), EndEpoch: highest}
		stored := raw
		if compressed {
			var zipped bytes.Buffer
			writer := gzip.NewWriter(&zipped)
			if _, err := writer.Write(raw); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			stored = zipped.Bytes()
			segment.Compressed = true
			segment.File += ".gz"
			segment.StoredLength = int64(len(stored))
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), segment.File), stored, 0o600); err != nil {
			t.Fatal(err)
		}
		segments = append(segments, segment)
	}
	if len(segments) > 0 {
		if err := saveSegmentManifest(filepath.Dir(path), segmentManifest{Segments: segments}); err != nil {
			t.Fatal(err)
		}
	}
	return path, frames
}

func ledgerRangeReader(t *testing.T, path string, limit int) *ReplicationRangeReader {
	t.Helper()
	r, err := NewReplicationRangeReader(path, testChainKey, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func TestLedgerRangeCursorExactForwardJumpOverlapAndGenerations(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("compressed-%t", compressed), func(t *testing.T) {
			path, frames := ledgerRangeHistory(t, [][]byte{{4, 4, 5}, {5, 5}, {5, 5}}, compressed)
			r := ledgerRangeReader(t, path, 1<<20)
			for _, query := range [][3]uint64{{1, 1, 1}, {1, 3, 3}, {1, 2, 3}, {1, 2, 2}, {2, 4, 5}, {3, 7, 7}, {3, 6, 7}, {1, 1, 3}} {
				want, err := ReadReplicationFrames(path, testChainKey, query[0], query[1], query[2])
				if err != nil {
					t.Fatal(err)
				}
				got, err := r.Read(query[0], query[1], query[2])
				if err != nil || !bytes.Equal(got, want) {
					t.Fatal(query, err)
				}
			}
			if got, err := r.Read(2, 4, 4); err != nil || !bytes.Equal(got, frames[1][0]) {
				t.Fatal(err)
			}
			if _, err := r.Read(2, 3, 4); err == nil {
				t.Fatal("foreign-generation sequence accepted")
			}
		})
	}
}

func TestLedgerRangeCursorValidatesSealedBoundaryAndGzipTrailer(t *testing.T) {
	for _, defect := range []string{"start-hash", "end-hash", "first", "last", "length", "epoch", "partial", "extra-frame", "gzip-trailer"} {
		t.Run(defect, func(t *testing.T) {
			path, _ := ledgerRangeHistory(t, [][]byte{{4, 5}, {5}}, defect == "gzip-trailer")
			manifest, err := loadSegmentManifest(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			segment := &manifest.Segments[0]
			switch defect {
			case "start-hash":
				segment.StartHash = hex.EncodeToString(bytes.Repeat([]byte{1}, 32))
			case "end-hash":
				segment.EndHash = hex.EncodeToString(bytes.Repeat([]byte{1}, 32))
			case "first":
				segment.FirstSequence++
			case "last":
				segment.LastSequence++
			case "length":
				segment.Length++
			case "epoch":
				segment.EndEpoch = 4
			case "partial", "extra-frame", "gzip-trailer":
				file := filepath.Join(filepath.Dir(path), segment.File)
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if defect == "partial" {
					raw = raw[:len(raw)-1]
					segment.StoredLength--
				} else if defect == "extra-frame" {
					event := validReservation("extra", "att_extra")
					payload, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					head, err := decodeChainHash(segment.EndHash)
					if err != nil {
						t.Fatal(err)
					}
					frame, _ := encodeChainFrameVersion(5, testChainKey, 3, event.Kind, payload, head)
					raw = append(raw, frame...)
					segment.StoredLength = int64(len(raw))
				} else {
					raw[len(raw)-8] ^= 1
				}
				if err := os.WriteFile(file, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := saveSegmentManifest(filepath.Dir(path), manifest); err != nil {
				t.Fatal(err)
			}
			r := ledgerRangeReader(t, path, 1<<20)
			if _, err := r.Read(2, 3, 3); err == nil {
				t.Fatal("bad predecessor accepted")
			}
			if r.state.generation != 1 || r.state.offset != 0 || r.state.verifier.sawFrames || r.stream != nil {
				t.Fatal("failed scan retained prefix authority")
			}
			// Directly requesting the declared last frame must also finish the
			// sealed stream, rather than publish success before its EOF/trailer.
			if defect != "last" {
				if _, err := r.Read(1, 1, 2); err == nil {
					t.Fatal("sealed last-frame request skipped boundary verification")
				}
			}
		})
	}
}

func TestLedgerRangeCursorLegacyAndEpochTransitionsAcrossGenerations(t *testing.T) {
	path, _ := ledgerRangeHistory(t, [][]byte{{1, 2, 1}, {4, 5}, {5}}, true)
	r := ledgerRangeReader(t, path, 1<<20)
	if _, err := r.Read(3, 6, 6); err != nil {
		t.Fatal(err)
	}
	for _, versions := range [][][]byte{{{4}, {3}}, {{5}, {4}}} {
		path, _ := ledgerRangeHistory(t, versions, false)
		r := ledgerRangeReader(t, path, 1<<20)
		if _, err := r.Read(1, 1, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Read(2, 2, 2); !errors.Is(err, ErrTampered) {
			t.Fatal("downgrade accepted", err)
		}
	}
}

func TestLedgerRangeCursorPrefixTrustResetReplacementAndPartialTail(t *testing.T) {
	path, frames := ledgerRangeHistory(t, [][]byte{{4, 4, 4}}, false)
	r := ledgerRangeReader(t, path, 1<<20)
	if _, err := r.Read(1, 1, 1); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), raw...)
	raw[chainHeaderSize] ^= 1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Read(1, 2, 2); err != nil || !bytes.Equal(got, frames[0][1]) {
		t.Fatal("forward read changed prefix trust contract", err)
	}
	r.Reset()
	if _, err := r.Read(1, 3, 3); err == nil {
		t.Fatal("reset skipped corrupt old prefix")
	}
	if err := os.WriteFile(path, append(original, []byte("torn")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(1, 1, 1); err != nil {
		t.Fatal("later partial tail hid good prefix", err)
	}
	if _, err := r.Read(1, 4, 4); err == nil {
		t.Fatal("requested torn tail accepted")
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(1, 1, 1); err != nil {
		t.Fatal(err)
	}
	replacement := path + ".new"
	if err := os.WriteFile(replacement, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(1, 2, 2); err == nil {
		t.Fatal("replacement reused old prefix authority")
	}
}

func TestLedgerRangeCursorFollowsLiveRollAndCompact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	t.Cleanup(func() { _ = log.Close() })
	appendReservations(t, log, 1, 2)
	r := ledgerRangeReader(t, path, 1<<20)
	if _, err := r.Read(1, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(1, 2, 2); err != nil {
		t.Fatal("Roll invalidated readable bytes", err)
	}
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(1, 2, 2); err != nil {
		t.Fatal("Compact invalidated readable bytes", err)
	}
	appendReservations(t, log, 3, 1)
	if _, err := r.Read(2, 3, 3); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerRangeCursorBoundCloseResetAndCompressedStreamReuse(t *testing.T) {
	versions := bytes.Repeat([]byte{4}, 100)
	path, frames := ledgerRangeHistory(t, [][]byte{versions, {}}, true)
	limit := 0
	for _, frame := range frames[0] {
		if len(frame) > limit {
			limit = len(frame)
		}
	}
	r := ledgerRangeReader(t, path, limit)
	if _, err := r.Read(1, 1, 2); err == nil || r.stream != nil {
		t.Fatal("output limit failed to refund stream", err)
	}
	if _, err := r.Read(1, 1, 1); err != nil {
		t.Fatal(err)
	}
	stream := r.stream
	counter := &ledgerCountingStream{ReadSeekCloser: stream}
	r.stream = counter
	for i := uint64(2); i <= 100; i++ {
		if _, err := r.Read(1, i, i); err != nil || r.stream != counter {
			t.Fatal("forward compressed stream reopened", i, err)
		}
	}
	if counter.bytes != int64(len(bytes.Join(frames[0][1:], nil))) {
		t.Fatal("compressed plaintext prefix rescanned", counter.bytes)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := r.Read(1, 1, 1); err != nil && !errors.Is(err, replicationrange.ErrClosed) {
					t.Error(err)
				}
				r.Reset()
			}
		}()
	}
	r.Close()
	wg.Wait()
	if _, err := r.Read(1, 1, 1); !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal(err)
	}
	if r.stream != nil || !bytes.Equal(r.key, make([]byte, 32)) {
		t.Fatal("Close retained stream or key")
	}
}

type ledgerCountingStream struct {
	io.ReadSeekCloser
	bytes int64
}

func (s *ledgerCountingStream) Read(value []byte) (int, error) {
	n, err := s.ReadSeekCloser.Read(value)
	s.bytes += int64(n)
	return n, err
}

func TestLedgerRangeCursorDoesNotRebuildIndexForAbsentGeneration(t *testing.T) {
	path, _ := ledgerRangeHistory(t, [][]byte{{frameVersionCurrent}}, false)
	r := ledgerRangeReader(t, path, 1<<20)
	index := r.directory
	if _, err := r.Read(99, 1, 1); err == nil {
		t.Fatal("absent generation was accepted")
	}
	if r.directory != index {
		t.Fatal("ordinary absent-generation request rebuilt the directory locator")
	}
}

func TestLedgerRangeReaderIndexCeilingSurvivesManifestReplacement(t *testing.T) {
	path, _ := ledgerRangeHistory(t, [][]byte{{4, 4}}, false)
	indexBudget := int64(segmentDirectoryHeaderBytes + segmentDirectorySlotBytes)
	r, err := NewReplicationRangeReaderWithIndexLimit(path, testChainKey, 1<<20, indexBudget)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	replacementPath, _ := ledgerRangeHistory(t, [][]byte{{4}, {4}, {}}, false)
	manifest, err := os.ReadFile(filepath.Join(filepath.Dir(replacementPath), segmentManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), segmentManifestName), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.findGeneration(2); err == nil {
		t.Fatal("manifest replacement rebuilt a locator beyond the reader disk ceiling")
	}
	if r.directory == nil || r.directory.maxBytes != indexBudget {
		t.Fatal("failed rebuild discarded the reader's configured ceiling")
	}
}

func TestLedgerRangeCursorPendingRenameSides(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("after-%t", after), func(t *testing.T) {
			path, frames := ledgerRangeHistory(t, [][]byte{{4}, {4}}, false)
			manifest, err := loadSegmentManifest(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			pending := manifest.Segments[0]
			rolled := filepath.Join(filepath.Dir(path), pending.File)
			pending.PlainChecksum, _, err = hashFile(rolled)
			if err != nil {
				t.Fatal(err)
			}
			if !after {
				if err := os.Rename(rolled, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := saveSegmentManifest(filepath.Dir(path), segmentManifest{Pending: &pending}); err != nil {
				t.Fatal(err)
			}
			r := ledgerRangeReader(t, path, 1<<20)
			if got, err := r.Read(1, 1, 1); err != nil || !bytes.Equal(got, frames[0][0]) {
				t.Fatal(err)
			}
		})
	}
}

func TestLedgerRangeCursorLegacySequenceGapsAndObservableShrink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	event := validReservation("legacy", "att")
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	first := encodeFrameVersion(1, 2, event.Kind, payload)
	last := encodeFrameVersion(1, 7, event.Kind, payload)
	if err := os.WriteFile(path, append(first, last...), 0o600); err != nil {
		t.Fatal(err)
	}
	r := ledgerRangeReader(t, path, 1<<20)
	if got, err := r.Read(1, 2, 7); err != nil || !bytes.Equal(got, append(first, last...)) {
		t.Fatal(err)
	}
	if _, err := r.Read(1, 3, 7); err == nil {
		t.Fatal("missing legacy first accepted")
	}
	// Real old Roll uses startSequence+1 for its descriptor rather than the
	// first frame's actual sequence. Preserve that historical representation.
	rolled := filepath.Join(filepath.Dir(path), "ledger-1.wal")
	if err := os.Rename(path, rolled); err != nil {
		t.Fatal(err)
	}
	frame, _ := encodeChainFrame(testChainKey, 8, event.Kind, payload, [32]byte{})
	if err := os.WriteFile(path, frame, 0o600); err != nil {
		t.Fatal(err)
	}
	zero := hex.EncodeToString(make([]byte, 32))
	if err := saveSegmentManifest(filepath.Dir(path), segmentManifest{Segments: []Segment{{Generation: 1, File: filepath.Base(rolled), FirstSequence: 1, LastSequence: 7, Length: int64(len(first) + len(last)), StoredLength: int64(len(first) + len(last)), StartHash: zero, EndHash: zero}}}); err != nil {
		t.Fatal(err)
	}
	r.Reset()
	if got, err := r.Read(2, 8, 8); err != nil || !bytes.Equal(got, frame) {
		t.Fatal("old sealed legacy gap rejected", err)
	}
	if err := os.Remove(segmentManifestPath(filepath.Dir(path))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(first, last...), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Reset()
	if _, err := r.Read(1, 2, 2); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(1, 7, 7); err == nil || r.stream != nil {
		t.Fatal("shrink retained old authority", err)
	}
}

func TestLedgerNativeFrameConsumerRunsOnlyAfterValidation(t *testing.T) {
	event := validReservation("evt", "att")
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	frame, _ := encodeChainFrame(testChainKey, 1, event.Kind, payload, [32]byte{})
	for _, defect := range []string{"none", "CRC", "MAC", "JSON", "kind", "period"} {
		t.Run(defect, func(t *testing.T) {
			raw := append([]byte(nil), frame...)
			switch defect {
			case "CRC":
				raw[len(raw)-1] ^= 1
			case "MAC":
				raw[frameHeaderSize+chainPreviousHashSize] ^= 1
			case "JSON":
				raw, _ = encodeChainFrame(testChainKey, 1, event.Kind, []byte("{"), [32]byte{})
			case "kind":
				raw, _ = encodeChainFrame(testChainKey, 1, EventRequestAccepted, payload, [32]byte{})
			case "period":
				event.PeriodTimezone = ""
				missing, _ := json.Marshal(event)
				raw, _ = encodeChainFrame(testChainKey, 1, event.Kind, missing, [32]byte{})
			}
			var delivered []byte
			_, partial, err := scanFromFrames(bytes.NewReader(raw), 1, 0, 0, 0, nil, func(_ Record, parts nativeFrameParts) error {
				delivered = append(delivered, parts.header...)
				delivered = append(delivered, parts.chainTail...)
				delivered = append(delivered, parts.payload...)
				return nil
			}, &chainVerifier{key: testChainKey})
			if defect == "none" {
				if err != nil || partial || !bytes.Equal(delivered, raw) {
					t.Fatal(err, partial)
				}
			} else if err == nil || len(delivered) != 0 {
				t.Fatal("invalid native frame delivered", err)
			}
		})
	}
	_, _, err = scanFromFrames(bytes.NewReader(frame), 1, 0, 0, 0, nil, func(Record, nativeFrameParts) error { return os.ErrPermission }, &chainVerifier{key: testChainKey})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal("consumer failure lost", err)
	}
}

type ledgerChangingStream struct {
	io.ReadSeekCloser
	change func()
}

func (s *ledgerChangingStream) Read(value []byte) (int, error) {
	n, err := s.ReadSeekCloser.Read(value)
	if s.change != nil {
		change := s.change
		s.change = nil
		change()
	}
	return n, err
}

func TestLedgerRangeCursorRefusesReplacementDuringAuthenticatedRead(t *testing.T) {
	path, _ := ledgerRangeHistory(t, [][]byte{{4, 4}, {4}}, true)
	r := ledgerRangeReader(t, path, 1<<20)
	if _, err := r.Read(1, 1, 1); err != nil {
		t.Fatal(err)
	}
	r.stream = &ledgerChangingStream{ReadSeekCloser: r.stream, change: func() {
		raw, err := os.ReadFile(r.streamPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.streamPath+".new", raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(r.streamPath+".new", r.streamPath); err != nil {
			t.Fatal(err)
		}
	}}
	if got, err := r.Read(1, 2, 2); err == nil || got != nil || r.stream != nil {
		t.Fatal("path replacement during read accepted", err)
	}
	if _, err := r.Read(1, 2, 2); err != nil {
		t.Fatal("retry could not authenticate replacement from root", err)
	}
}

func BenchmarkLedgerReplicationRangeRecovery(b *testing.B) {
	for _, count := range []int{100, 1000} {
		path := filepath.Join(b.TempDir(), "ledger.wal")
		frames := make([][]byte, count)
		var hash [32]byte
		file, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		for i := range frames {
			event := validReservation(fmt.Sprintf("evt_%d", i), fmt.Sprintf("att_%d", i))
			event.OccurredAt = time.Unix(123, 0).UTC()
			payload, err := json.Marshal(event)
			if err != nil {
				b.Fatal(err)
			}
			frames[i], hash = encodeChainFrame(testChainKey, uint64(i+1), event.Kind, payload, hash)
			if _, err := file.Write(frames[i]); err != nil {
				b.Fatal(err)
			}
		}
		if err := file.Close(); err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("FullScan/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				for i, want := range frames {
					got, err := ReadReplicationFrames(path, testChainKey, 1, uint64(i+1), uint64(i+1))
					if err != nil || !bytes.Equal(got, want) {
						b.Fatal(err)
					}
				}
			}
		})
		b.Run(fmt.Sprintf("PrefixCursor/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				r, err := NewReplicationRangeReader(path, testChainKey, 1<<20)
				if err != nil {
					b.Fatal(err)
				}
				for i, want := range frames {
					got, err := r.Read(1, uint64(i+1), uint64(i+1))
					if err != nil || !bytes.Equal(got, want) {
						r.Close()
						b.Fatal(err)
					}
				}
				r.Close()
			}
		})
	}
}
