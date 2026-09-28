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

// This measures authenticated 64-event pagination at the beginning, middle,
// and end of a segmented retained chain. Fixture writes are not timed.
func BenchmarkTransitionJournalPage(b *testing.B) {
	path, key, state := durablePublisherFixture(b)
	journalPath := TransitionJournalPath(path)
	journal, err := OpenTransitionJournal(journalPath, key, state)
	if err != nil {
		b.Fatal(err)
	}
	journal.segmentLimit = 8 << 10
	const transitions = 2048
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
	recovered, err := OpenTransitionJournal(journalPath, key, state)
	if err != nil {
		b.Fatal(err)
	}
	defer recovered.Close()
	if len(recovered.segments) < 100 {
		b.Fatalf("fixture has only %d segments", len(recovered.segments))
	}
	storage, err := recovered.storage()
	if err != nil {
		b.Fatal(err)
	}
	for _, position := range []struct {
		name  string
		after uint64
	}{
		{name: "first", after: 0},
		{name: "middle", after: transitions / 2},
		{name: "last", after: transitions - 64},
	} {
		digest := ""
		if position.after > 0 {
			if err := recovered.visitRecords(position.after, position.after, func(record transitionJournalRecord, _ *transitionJournalRecord) error {
				digest = publicTransitionDigest(record.MAC)
				return nil
			}); err != nil {
				b.Fatal(err)
			}
		}
		b.Run(position.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				page, err := recovered.Page(position.after, digest, 64)
				if err != nil {
					b.Fatal(err)
				}
				if len(page.Events) != 64 {
					b.Fatalf("page has %d events", len(page.Events))
				}
			}
			b.ReportMetric(float64(storage.Segments), "segments")
			b.ReportMetric(float64(storage.Bytes), "journal_bytes")
		})
	}
}
