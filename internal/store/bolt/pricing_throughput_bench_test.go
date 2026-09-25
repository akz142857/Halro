package bolt

// Throughput harnesses for the ceiling described in ADR 0012, "Amendment
// 2026-08-07". These exist so the ceiling is a regression gate rather than a
// one-off observation: the amendment's original numbers came from throwaway
// harnesses, which is why nobody noticed the ceiling for as long as they did.
//
// Read the numbers as a shape, not a budget. On darwin, os.File.Sync issues
// F_FULLFSYNC, so every durability figure here is a pessimistic bound and none
// of them transfer to a Linux NVMe host.
//
//	go test ./internal/store/bolt/ -run '^$' -bench 'Metadata|PricePin' -benchtime 200x
//
// Concurrency is driven by an explicit worker pool rather than b.RunParallel,
// which multiplies its parallelism by GOMAXPROCS — "workers=1" under RunParallel
// is ten concurrent writers on a ten-core host, which is exactly the confusion
// these harnesses exist to prevent.

import (
	"context"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var benchmarkWorkerCounts = []int{1, 8, 64}

// BenchmarkMetadataWriteTransaction measures one durable metadata write at
// several concurrency levels, for both the single-transaction entry and the
// coalescing layer above it. The single entry does not coalesce, so its rate is
// flat in the worker count — that flatness, next to the Ledger's group commit,
// is the whole finding of the amendment.
//
// It measures Store.update and Store.batch rather than bbolt's own db.Update
// and db.Batch, which is what the tuning now has to be balanced against: each
// transaction here also appends and fsyncs a journal frame, so the fsync the
// 250µs window trades against is a different and larger number than the one the
// amendment measured. Re-run the sweep before changing metadataBatchDelay.
func BenchmarkMetadataWriteTransaction(b *testing.B) {
	value := make([]byte, 512)
	for _, workers := range benchmarkWorkerCounts {
		for _, mode := range []string{"update", "batch"} {
			b.Run(fmt.Sprintf("%s/workers=%d", mode, workers), func(b *testing.B) {
				store := benchmarkStore(b)
				// An authoritative bucket, so every write in the sweep records
				// a frame. Benchmarking a node-local bucket would measure the
				// path with the journal switched off and report a number no
				// real write can reach.
				bucket := bucketRedactionPolicies
				write := store.update
				if mode == "batch" {
					write = store.batch
				}
				runConcurrent(b, workers, "tx/s", func(index int) error {
					var key [8]byte
					binary.BigEndian.PutUint64(key[:], uint64(index))
					return write(func(tx *Tx) error {
						return tx.Bucket(bucket).Put(key[:], value)
					})
				})
			})
		}
	}
}

// BenchmarkDeploymentPricePinCeiling measures the durable pricing path a Gateway
// attempt actually walks — shared gate, prepare, commit — against a single
// deployment. Before the amendment this was flat in the worker count, because an
// exclusive gate meant same-deployment attempts could not overlap at all.
//
// It is also the bbolt half of the end-to-end baseline §16.3 of the HA design
// asks for. The other half is internal/budget's BenchmarkRequestLifecycle,
// which walks one request's five Ledger events and deliberately touches no
// bbolt — which is why the amendment's 7051/s cannot serve as the denominator
// on its own. Since the metadata journal, every one of these two writes also
// appends and fsyncs a frame, so this number now includes the cost the journal
// added to the request path rather than only the cost bbolt had.
func BenchmarkDeploymentPricePinCeiling(b *testing.B) {
	for _, workers := range benchmarkWorkerCounts {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			store, err := openForTest(b, filepath.Join(b.TempDir(), "metadata.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			seedPricingDeployment(b, store, "dep_bench", 400_000, 1_600_000, 0)
			if _, err := store.CreateDeploymentPriceVersion(ctx, newStoredPrice("price_bench", "dep_bench", time.Now().UTC().Add(-time.Hour))); err != nil {
				b.Fatal(err)
			}

			runConcurrent(b, workers, "attempts/s", func(index int) error {
				attemptID := fmt.Sprintf("att_bench_%d", index)
				// The Gateway takes the gate, then reads the clock; the
				// benchmark mirrors that order so the measured span is the one
				// the ceiling formula describes.
				unlock := store.LockDeploymentPricingShared("dep_bench")
				defer unlock()
				selectedAt := time.Now().UTC()
				_, _, intent, err := store.PrepareDeploymentPricePin(
					ctx, "dep_bench", attemptID, selectedAt,
					2*time.Second, 30*time.Second,
				)
				if err != nil {
					return err
				}
				_, err = store.CommitDeploymentPricePin(ctx, intent.AttemptID, intent.SnapshotSHA256, uint64(index+1), time.Now().UTC())
				return err
			})
		})
	}
}

// runConcurrent spreads b.N operations over exactly workers goroutines and
// reports the achieved rate. The rate, not ns/op, is the number the amendment
// argues about: ns/op under a shared batch window describes latency, and the
// claim being regression-tested is about throughput.
func runConcurrent(b *testing.B, workers int, unit string, work func(index int) error) {
	b.Helper()
	var next atomic.Int64
	var failures atomic.Int64
	var group sync.WaitGroup
	b.ResetTimer()
	start := time.Now()
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1)) - 1
				if index >= b.N {
					return
				}
				if err := work(index); err != nil {
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
	if elapsed > 0 {
		b.ReportMetric(float64(b.N)/elapsed.Seconds(), unit)
	}
}

// BenchmarkMetadataBatchDelay is the harness behind the metadataBatchDelay
// choice. Batching is not free at low concurrency: a lone writer waits out the
// batch window before its fsync even starts, so too generous a delay makes an
// uncontended write slower than db.Update would have been. Run this before
// changing the constant — the right value is host-dependent, and the tradeoff it
// balances is single-writer latency against many-writer throughput.
//
//	go test ./internal/store/bolt/ -run '^$' -bench BatchDelay -benchtime 200x
func BenchmarkMetadataBatchDelay(b *testing.B) {
	value := make([]byte, 512)
	for _, delay := range []time.Duration{0, 250 * time.Microsecond, 500 * time.Microsecond, time.Millisecond, 2 * time.Millisecond, 10 * time.Millisecond} {
		for _, workers := range []int{1, 8} {
			b.Run(fmt.Sprintf("delay=%s/workers=%d", delay, workers), func(b *testing.B) {
				store := benchmarkStore(b)
				store.batches.delay = delay
				bucket := bucketRedactionPolicies
				runConcurrent(b, workers, "tx/s", func(index int) error {
					var key [8]byte
					binary.BigEndian.PutUint64(key[:], uint64(index))
					return store.batch(func(tx *Tx) error {
						return tx.Bucket(bucket).Put(key[:], value)
					})
				})
			})
		}
	}
}
