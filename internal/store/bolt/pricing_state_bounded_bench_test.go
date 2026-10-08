package bolt

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// Isolate identifier growth from native business records and durable fsync.
// The existing pin-ceiling harness separately exercises the real price path.
func BenchmarkPricingIdentifierStateGrowth(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := openJournalledStore(b, b.TempDir()+"/metadata.db")
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			x, err := s.pricingClockOwner()
			if err != nil {
				b.Fatal(err)
			}
			base := time.Now().UTC()
			b.ResetTimer()
			for round := 0; round < b.N; round++ {
				for i := 0; i < count; i++ {
					id := fmt.Sprint("dep_growth_", i)
					s.deploymentPricingState(id)
					if err := x.mergeClockAnchor(id, pricingClockObservation{SelectedAt: base.Add(time.Duration(round) * time.Second), ObservedAt: x.origin.Add(time.Duration(round) * time.Second)}); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.StopTimer()
			runtime.GC()
			runtime.ReadMemStats(&after)
			b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "retained-heap-B")
			b.ReportMetric(float64(int64(after.StackInuse)-int64(before.StackInuse)), "retained-stack-B")
			b.ReportMetric(float64(x.size), "clock-file-B")
			runtime.KeepAlive(s)
		})
	}
}
