package ledger

// Throughput harness for the group-commit property ADR 0012's "Amendment
// 2026-08-07" measures the metadata store against. The WAL coalesces concurrent
// appenders into one fsync; the amendment's finding is that the pricing protocol
// prevented same-deployment attempts from ever being in flight together, so they
// could never share a batch and this scaling was unreachable.
//
// Committed so the property is a regression gate rather than a one-off
// observation. Read the numbers as a shape, not a budget: on darwin os.File.Sync
// issues F_FULLFSYNC, so these are pessimistic bounds that do not transfer to a
// Linux NVMe host.
//
//	go test ./internal/ledger/ -run '^$' -bench ConcurrentAppend -benchtime 300x

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkConcurrentAppend reports both the achieved append rate and the mean
// number of events per fsync. The batch size is the interesting half: a rate that
// rises while the batch size stays at 1.0 would mean something other than group
// commit was responsible.
func BenchmarkConcurrentAppend(b *testing.B) {
	for _, appenders := range []int{1, 8, 64, 256} {
		b.Run(fmt.Sprintf("appenders=%d", appenders), func(b *testing.B) {
			status := NewStatus()
			log, err := OpenWithOptions(filepath.Join(b.TempDir(), "usage.wal"), status, Options{ChainKey: benchChainKey()})
			if err != nil {
				b.Fatal(err)
			}
			defer log.Close()
			ctx := context.Background()

			before := log.Stats()
			var next atomic.Int64
			var failures atomic.Int64
			var group sync.WaitGroup
			b.ResetTimer()
			start := time.Now()
			for appender := 0; appender < appenders; appender++ {
				group.Add(1)
				go func() {
					defer group.Done()
					for {
						index := int(next.Add(1)) - 1
						if index >= b.N {
							return
						}
						event := validReservation(fmt.Sprintf("evt_bench_%d", index), fmt.Sprintf("att_bench_%d", index))
						if _, err := log.Append(ctx, event); err != nil {
							if failures.Add(1) == 1 {
								b.Error(err)
							}
							return
						}
					}
				}()
			}
			group.Wait()
			elapsed := time.Since(start)
			b.StopTimer()

			after := log.Stats()
			if elapsed > 0 {
				b.ReportMetric(float64(b.N)/elapsed.Seconds(), "events/s")
			}
			if batches := after.Batches - before.Batches; batches > 0 {
				b.ReportMetric(float64(after.Records-before.Records)/float64(batches), "events/batch")
			}
		})
	}
}

func benchChainKey() []byte {
	key := make([]byte, 32)
	for index := range key {
		key[index] = 0x24
	}
	return key
}
