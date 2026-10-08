package audit

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Measures the native cache after authenticated Open and a GC. Construction
// still scans history; this local retained-heap observation is not RSS or
// Linux/production capacity acceptance.
func BenchmarkAuditNativeCacheRetained(b *testing.B) {
	for _, scenario := range []struct {
		rows, width int
	}{{4096, 0}, {4096, 4096}, {16384, 4096}} {
		b.Run(fmt.Sprintf("rows_%d_width_%d", scenario.rows, scenario.width), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "native.wal")
			key := make([]byte, 32)
			seedAuditCacheBenchmark(b, path, key, scenario.rows, scenario.width)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				b.StartTimer()
				log, err := Open(path, key)
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				runtime.GC()
				runtime.ReadMemStats(&after)
				b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "retained_B")
				runtime.KeepAlive(log)
				if err := log.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}

func seedAuditCacheBenchmark(b *testing.B, path string, key []byte, rows, width int) {
	b.Helper()
	log, err := Open(path, key)
	if err != nil {
		b.Fatal(err)
	}
	defer log.Close()
	for start := 0; start < rows; start += 64 {
		batch := make([]Event, 0, 64)
		for i := start; i < start+64 && i < rows; i++ {
			e := sequencedEvent(i)
			e.EventID += strings.Repeat("x", width)
			batch = append(batch, e)
		}
		if _, err := log.AppendBatch(context.Background(), batch); err != nil {
			b.Fatal(err)
		}
	}
}
