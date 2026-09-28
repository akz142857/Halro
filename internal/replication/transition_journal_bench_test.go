package replication

import (
	"fmt"
	"testing"
)

// This isolates authenticated startup replay. Fixture writes are outside the
// timed section, and records are spread across private journal segments.
func BenchmarkTransitionJournalRecovery(b *testing.B) {
	for _, transitions := range []int{256, 2048} {
		b.Run(fmt.Sprintf("transitions_%d", transitions), func(b *testing.B) {
			path, key, state := durablePublisherFixture(b)
			journal, err := OpenTransitionJournal(TransitionJournalPath(path), key, state)
			if err != nil {
				b.Fatal(err)
			}
			journal.segmentLimit = 8 << 10
			for range transitions {
				next := cloneMemberState(state)
				next.PromisedTerm++
				cursor, err := journal.appendIntent("promise", next)
				if err != nil {
					b.Fatal(err)
				}
				if err := journal.commit(cursor); err != nil {
					b.Fatal(err)
				}
				next.Transition = cursor
				state = next
			}
			if err := WriteState(path, state, key); err != nil {
				b.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				b.Fatal(err)
			}
			segments, err := transitionSegments(TransitionJournalPath(path))
			if err != nil || len(segments) < 2 {
				b.Fatalf("expected multi-segment fixture: segments=%d err=%v", len(segments), err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				replayed, err := OpenTransitionJournal(TransitionJournalPath(path), key, state)
				if err != nil {
					b.Fatal(err)
				}
				if err := replayed.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(segments)), "segments")
		})
	}
}
