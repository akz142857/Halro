package bolt

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"
)

func BenchmarkDeploymentPricePageHistoryGrowth(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s, base := streamPriceFixture(b, count, 256)
			selected := base.Add(time.Duration(count/2) * time.Second)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			page, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", "", 50, selected)
			if err != nil || len(page.Items) != 50 {
				b.Fatal(page, err)
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			resident := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			runtime.KeepAlive(page)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				page, err = s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", "", 50, selected)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(resident), "resident_delta_B")
			b.ReportMetric(float64(len(page.Items)), "returned_rows")
			runtime.KeepAlive(page)
		})
	}
}
