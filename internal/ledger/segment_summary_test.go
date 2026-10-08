package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Use the public manifest descriptors only as the test oracle. Production
// metrics and candidate selection must not materialize this history slice.
func assertSegmentSummary(t *testing.T, log *Log) SegmentSummary {
	t.Helper()
	var want SegmentSummary
	for _, segment := range segmentsForTest(t, log) {
		want.Generations++
		want.PlainBytes += segment.Length
		want.StoredBytes += segment.StoredLength
	}
	if got := log.SealedSummary(); got != want {
		t.Fatalf("summary=%+v manifest=%+v", got, want)
	}
	return want
}

func TestLogGenerationForSequenceUsesOrdinalIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()

	for generation, count := range []int{2, 2} {
		appendReservations(t, log, generation*10+1, count)
		if _, err := log.Roll(); err != nil {
			t.Fatal(err)
		}
	}
	appendReservations(t, log, 50, 1)

	for _, sample := range []struct {
		sequence uint64
		want     uint64
		found    bool
	}{
		{sequence: 0},
		{sequence: 1, want: 1, found: true},
		{sequence: 2, want: 1, found: true},
		{sequence: 3, want: 2, found: true},
		{sequence: 4, want: 2, found: true},
		{sequence: 5, want: 3, found: true},
		{sequence: 6},
	} {
		got, found, err := log.GenerationForSequence(sample.sequence)
		if err != nil || got != sample.want || found != sample.found {
			t.Errorf("sequence %d: generation=%d found=%t err=%v; want generation=%d found=%t", sample.sequence, got, found, err, sample.want, sample.found)
		}
	}

	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openChained(t, path, nil)
	defer reopened.Close()
	if got, found, err := reopened.GenerationForSequence(4); err != nil || !found || got != 2 {
		t.Fatalf("reopened sequence 4: generation=%d found=%t err=%v", got, found, err)
	}
}

func TestLogGenerationForSequenceReturnsLegacySegmentEnvelope(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ledger.wal")
	event := validReservation("legacy-sequence-gap", "legacy-sequence-gap-attempt")
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	first := encodeFrameVersion(1, 2, event.Kind, payload)
	last := encodeFrameVersion(1, 7, event.Kind, payload)
	raw := append(first, last...)
	if err := os.WriteFile(filepath.Join(directory, "ledger-1.wal"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	zero := hex.EncodeToString(make([]byte, 32))
	digest := sha256.Sum256(raw)
	if err := saveSegmentManifest(directory, segmentManifest{Segments: []Segment{{
		Generation: 1, File: "ledger-1.wal", FirstSequence: 1, LastSequence: 7,
		Length: int64(len(raw)), StoredLength: int64(len(raw)), PlainChecksum: hex.EncodeToString(digest[:]),
		StartHash: zero, EndHash: zero,
	}}}); err != nil {
		t.Fatal(err)
	}
	log, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	for _, sequence := range []uint64{1, 3, 6, 7} {
		generation, found, err := log.GenerationForSequence(sequence)
		if err != nil || !found || generation != 1 {
			t.Fatalf("legacy descriptor envelope sequence=%d generation=%d found=%t err=%v", sequence, generation, found, err)
		}
	}
	if _, found, err := log.GenerationForSequence(8); err != nil || found {
		t.Fatalf("sequence above legacy descriptor found=%t err=%v", found, err)
	}
}

func TestSegmentSummaryTracksRollCompactAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()
	if got := assertSegmentSummary(t, log); got != (SegmentSummary{}) {
		t.Fatal(got)
	}
	if _, found, err := log.NextCompactableSegment(math.MaxUint64); err != nil || found {
		t.Fatalf("empty candidate found=%t err=%v", found, err)
	}
	for generation := 1; generation <= 3; generation++ {
		appendReservations(t, log, 10*generation, 4)
		if _, err := log.Roll(); err != nil {
			t.Fatal(err)
		}
		if got := assertSegmentSummary(t, log); got.Generations != uint64(generation) {
			t.Fatal(got)
		}
	}
	before := assertSegmentSummary(t, log)
	if result, err := log.Roll(); err != nil || result.Rolled || log.SealedSummary() != before {
		t.Fatalf("empty roll result=%+v err=%v summary=%+v", result, err, log.SealedSummary())
	}
	if _, found, err := log.NextCompactableSegment(3); err != nil || found {
		t.Fatalf("unarchived candidate found=%t err=%v", found, err)
	}
	first, found, err := log.NextCompactableSegment(4)
	if err != nil || !found || first.Generation != 1 {
		t.Fatalf("candidate=%+v found=%t err=%v", first, found, err)
	}
	compacted, err := log.Compact(first.Generation)
	if err != nil {
		t.Fatal(err)
	}
	after := assertSegmentSummary(t, log)
	if after.Generations != before.Generations || after.PlainBytes != before.PlainBytes || after.StoredBytes != before.StoredBytes-first.StoredLength+compacted.StoredLength {
		t.Fatalf("compaction totals before=%+v after=%+v", before, after)
	}
	if _, err := log.Compact(first.Generation); err != nil || log.SealedSummary() != after {
		t.Fatalf("duplicate compaction err=%v summary=%+v", err, log.SealedSummary())
	}
	if _, found, err := log.NextCompactableSegment(4); err != nil || found {
		t.Fatalf("compressed candidate found=%t err=%v", found, err)
	}
	second, found, err := log.NextCompactableSegment(8)
	if err != nil || !found || second.Generation != 2 {
		t.Fatalf("second candidate=%+v found=%t err=%v", second, found, err)
	}
	second.File = "caller-change"
	if candidate, _, _ := log.NextCompactableSegment(8); candidate.File == second.File {
		t.Fatal("returned descriptor aliases native directory")
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := log.NextCompactableSegment(math.MaxUint64); err == nil {
		t.Fatal("closed ledger offered a compaction candidate")
	}
	reopened := openChained(t, path, nil)
	defer reopened.Close()
	if got := assertSegmentSummary(t, reopened); got != after {
		t.Fatalf("reopen summary=%+v want=%+v", got, after)
	}
}

func TestSegmentSummaryFailedPublicationDoesNotAdvanceCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()
	appendReservations(t, log, 1, 3)
	partial := filepath.Join(filepath.Dir(path), segmentManifestName+".partial")
	block := func() {
		t.Helper()
		if err := os.Mkdir(partial, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	unblock := func() {
		t.Helper()
		if err := os.Remove(partial); err != nil {
			t.Fatal(err)
		}
	}
	block()
	if _, err := log.Roll(); err == nil {
		t.Fatal("blocked manifest publication succeeded")
	}
	if log.Generation() != 1 || assertSegmentSummary(t, log) != (SegmentSummary{}) {
		t.Fatal("failed intent advanced generation or summary")
	}
	unblock()
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	before := assertSegmentSummary(t, log)
	block()
	if _, err := log.Compact(1); err == nil {
		t.Fatal("blocked compaction publication succeeded")
	}
	if got := assertSegmentSummary(t, log); got != before {
		t.Fatalf("failed compaction summary=%+v want=%+v", got, before)
	}
	if _, found, err := log.NextCompactableSegment(3); err != nil || !found {
		t.Fatalf("failed compaction lost original candidate: %t %v", found, err)
	}
	unblock()
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	assertSegmentSummary(t, log)
}

func TestSegmentSummaryRejectsOverflowAndNegativeLengths(t *testing.T) {
	for name, sample := range map[string]struct {
		summary SegmentSummary
		segment Segment
	}{
		"count":                    {summary: SegmentSummary{Generations: math.MaxUint64}},
		"plain overflow":           {summary: SegmentSummary{PlainBytes: math.MaxInt64}, segment: Segment{Length: 1}},
		"stored overflow":          {summary: SegmentSummary{StoredBytes: math.MaxInt64}, segment: Segment{StoredLength: 1}},
		"negative plain":           {segment: Segment{Length: -1}},
		"negative stored":          {segment: Segment{StoredLength: -1}},
		"negative existing plain":  {summary: SegmentSummary{PlainBytes: -1}},
		"negative existing stored": {summary: SegmentSummary{StoredBytes: -1}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := appendSegmentSummary(sample.summary, sample.segment)
			if !errors.Is(err, ErrCorrupt) || got != (SegmentSummary{}) {
				t.Fatalf("summary=%+v err=%v", got, err)
			}
		})
	}
}

func TestSegmentSummaryConcurrentReadersObserveCommittedTotals(t *testing.T) {
	log := openChained(t, filepath.Join(t.TempDir(), "ledger.wal"), nil)
	defer log.Close()
	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				s := log.SealedSummary()
				if s.Generations > 6 || s.StoredBytes < 0 || s.PlainBytes < s.StoredBytes || (s.Generations == 0 && (s.PlainBytes != 0 || s.StoredBytes != 0)) {
					t.Errorf("inconsistent concurrent summary=%+v", s)
					return
				}
				if _, _, err := log.NextCompactableSegment(math.MaxUint64); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	defer func() { close(done); readers.Wait() }()
	for generation := 1; generation <= 6; generation++ {
		appendReservations(t, log, generation*10, 4)
		if _, err := log.Roll(); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Compact(uint64(generation)); err != nil {
			t.Fatal(err)
		}
	}
	assertSegmentSummary(t, log)
}

var summaryBenchmarkSink SegmentSummary
var candidateBenchmarkSink Segment

func BenchmarkSegmentSummaryReads(b *testing.B) {
	for _, count := range []int{0, 1_000, 100_000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			// Synthetic directory allocated before timing. This measures only
			// read API allocations; it is not a Runtime capacity acceptance.
			directory := b.TempDir()
			source := segmentManifestSource{each: func(emit func(Segment) error) error {
				for i := 0; i < count; i++ {
					if err := emit(Segment{Generation: uint64(i + 1), LastSequence: uint64(i + 1), Length: 100, StoredLength: 100}); err != nil {
						return err
					}
				}
				return nil
			}}
			if _, err := saveSegmentManifestSourceWithSync(directory, source, func(string) error { return nil }); err != nil {
				b.Fatal(err)
			}
			index, err := buildSegmentDirectoryIndex(directory, b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := index.Close(); err != nil {
					b.Error(err)
				}
			})
			log := &Log{segmentDirectory: index, file: new(os.File), generation: uint64(count + 1), status: NewStatus(), segmentSummary: index.summary}
			b.Run("summary", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					summaryBenchmarkSink = log.SealedSummary()
				}
			})
			b.Run("candidate-none", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					candidateBenchmarkSink, _, _ = log.NextCompactableSegment(0)
				}
			})
		})
	}
}

func segmentsForTest(t testing.TB, log *Log) []Segment {
	t.Helper()
	var segments []Segment
	err := log.WalkSegments(context.Background(), func(segment Segment) error {
		segments = append(segments, segment)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return segments
}
