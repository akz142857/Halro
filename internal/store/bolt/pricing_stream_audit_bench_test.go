package bolt

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"
)

// Heap retention and cumulative scan allocation are different measurements.
// Setup is excluded; these local samples are not RSS/cgroup capacity evidence.
func BenchmarkPricingDiskTimeline(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s, base := streamPriceFixture(b, count, 4096)
			at := base.Add(time.Duration(count) * time.Second)
			b.Run("cold", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					s.invalidateDeploymentPricingTimeline("dep_stream")
					if _, err := s.SelectDeploymentPriceVersion(context.Background(), "dep_stream", at); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("hot", func(b *testing.B) {
				if _, err := s.SelectDeploymentPriceVersion(context.Background(), "dep_stream", at); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := s.SelectDeploymentPriceVersion(context.Background(), "dep_stream", at); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("retained", func(b *testing.B) {
				s.invalidateDeploymentPricingTimeline("dep_stream")
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				if _, err := s.SelectDeploymentPriceVersion(context.Background(), "dep_stream", at); err != nil {
					b.Fatal(err)
				}
				runtime.GC()
				runtime.ReadMemStats(&after)
				runtime.KeepAlive(s)
				b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "retained-B")
			})
		})
	}
}
