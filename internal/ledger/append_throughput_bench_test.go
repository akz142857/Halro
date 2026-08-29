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
//
// BenchmarkReportedCeilingTracksAchieved below asks a different question against
// the shipped configuration, and the two are separate because the options
// differ: this one runs the package defaults, where FlushInterval is 0 and the
// writer never lingers.

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

// BenchmarkReportedCeilingTracksAchieved compares the ceiling the Settings card
// reports against the rate this host actually sustained, under the configuration
// a shipped instance runs: MaxBatch 128 and a 2 ms FlushInterval.
//
// The card used to derive its ceiling from the durability barrier alone, which
// measured 55% high at one appender on the host this was written against: a
// batch there occupies 6.7 ms, of which the fsync is 4.4 ms and the rest is
// collectBatch waiting out its flush interval for an appender that never
// arrives. Records over AppendStats.BatchDuration includes that wait, and the
// reported/achieved metric below is 1.0 when the two agree.
//
// The configuration is the whole point of it being a second benchmark: the one
// above runs the package defaults, where FlushInterval is 0 and there is no
// linger to miss. A benchmark cannot fail, so this reports rather than asserts —
// what it guards is that a change to the derivation shows up as a ratio moving
// off 1.0, next to the batch size that explains why.
//
//	go test ./internal/ledger/ -run '^$' -bench ReportedCeiling -benchtime 2000x
func BenchmarkReportedCeilingTracksAchieved(b *testing.B) {
	for _, appenders := range []int{1, 8, 64, 128, 256} {
		b.Run(fmt.Sprintf("appenders=%d", appenders), func(b *testing.B) {
			status := NewStatus()
			log, err := OpenWithOptions(filepath.Join(b.TempDir(), "usage.wal"), status, Options{
				ChainKey: benchChainKey(),
				// internal/config's defaults, so this measures what an operator runs.
				QueueCapacity: 4096, MaxBatch: 128, FlushInterval: 2 * time.Millisecond,
			})
			if err != nil {
				b.Fatal(err)
			}
			defer log.Close()
			ctx := context.Background()

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
						event := validReservation(fmt.Sprintf("evt_ceiling_%d", index), fmt.Sprintf("att_ceiling_%d", index))
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

			stats := log.Stats()
			if stats.Batches == 0 || elapsed <= 0 || stats.BatchDuration <= 0 {
				b.Skip("nothing was committed, so there is nothing to compare")
			}
			achieved := float64(stats.Records) / elapsed.Seconds()
			reported := float64(stats.Records) / stats.BatchDuration.Seconds()
			b.ReportMetric(float64(stats.Records)/float64(stats.Batches), "events/batch")
			b.ReportMetric(achieved, "achieved-events/s")
			b.ReportMetric(reported/achieved, "reported/achieved")
			// The half the fix removed: what the card would have said had it kept
			// deriving the ceiling from the barrier. Reported beside the honest
			// ratio so the gap stays visible rather than remembered.
			barrier := float64(stats.Records) / float64(stats.Batches) * float64(stats.Syncs) / stats.SyncDuration.Seconds()
			b.ReportMetric(barrier/achieved, "barrier-derived/achieved")
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

// The batch and sync counters exist so an operator can tell an fsync-bound host
// from an under-concurrent one. A counter that silently stays at zero would be
// worse than no counter at all, so this asserts they actually move, and that
// records/batches really is a group-commit size rather than a per-record count.
func TestAppendStatsRecordDurabilityWork(t *testing.T) {
	status := NewStatus()
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "usage.wal"), status, Options{ChainKey: benchChainKey()})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	ctx := context.Background()

	const appends = 24
	var group sync.WaitGroup
	for index := 0; index < appends; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			event := validReservation(fmt.Sprintf("evt_stats_%d", index), fmt.Sprintf("att_stats_%d", index))
			if _, err := log.Append(ctx, event); err != nil {
				t.Error(err)
			}
		}(index)
	}
	group.Wait()

	stats := log.Stats()
	if stats.Records != appends {
		t.Fatalf("recorded %d appends, want %d", stats.Records, appends)
	}
	if stats.Batches == 0 || stats.Batches > stats.Records {
		t.Fatalf("batches=%d is not a group-commit count for %d records", stats.Batches, stats.Records)
	}
	if stats.Syncs == 0 {
		t.Fatal("no durability barriers were counted")
	}
	if stats.SyncDuration <= 0 {
		t.Fatalf("sync duration is %s; the fsync cost every ceiling here is bounded by went unmeasured", stats.SyncDuration)
	}
	// The writer's own busy time is what the reported ceiling divides by, so a
	// counter stuck at zero would report an instance as serving nothing. It has
	// to contain the barrier it wraps: the fsync happens inside the timed
	// region, and a BatchDuration below SyncDuration would mean the two are
	// measuring different work.
	if stats.BatchDuration <= 0 {
		t.Fatal("no batch wall time was counted, so no throughput can be reported from it")
	}
	if stats.BatchDuration < stats.SyncDuration {
		t.Fatalf("batch duration %s is below the sync duration %s it contains", stats.BatchDuration, stats.SyncDuration)
	}
	// One barrier per committed batch: if these ever diverge, the mean derived in
	// Prometheus from _sum/_count stops describing one fsync.
	if stats.Syncs != stats.Batches {
		t.Fatalf("syncs=%d batches=%d", stats.Syncs, stats.Batches)
	}
}
