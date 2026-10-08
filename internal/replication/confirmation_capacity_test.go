package replication

import "testing"

// Keep this workload identical before/after replacing per-index ACK state.
// It measures one large authenticated, contiguous, unconfirmed suffix; the
// journal and sender are deliberately outside this tracker-only comparison.
func BenchmarkConfirmationTrackerUnconfirmedGrowth(b *testing.B) {
	const count = 100000
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		tracker, err := NewConfirmationTracker("production-a", "inc_01", 7, 0, []string{"halro-1", "halro-2"}, false)
		if err != nil {
			b.Fatal(err)
		}
		if err := tracker.Register(testConfirmationFrame(1, KindLeadershipEstablished)); err != nil {
			b.Fatal(err)
		}
		frame := testConfirmationFrame(2, KindData)
		for index := uint64(2); index <= count; index++ {
			frame.Index, frame.StoreSequenceFirst, frame.StoreSequenceLast = index, index, index
			if err := tracker.Register(frame); err != nil {
				b.Fatal(err)
			}
		}
		if got, err := tracker.Acknowledge(testAcknowledgement("halro-2", count)); err != nil || got != count {
			b.Fatal(got, err)
		}
	}
}
