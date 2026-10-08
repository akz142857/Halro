package bolt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

// BenchmarkMemoryLayout measures real metadata journal writes competing with a
// synthetic derived projection. It is a layout experiment, not the production
// projection/recovery protocol. All directories and keys are test-owned.
//
// The projection is prefilled, then overwrites 100 rows per transaction. The
// existing node-derived checkpoint bucket hosts the test fixture without adding
// a production bucket or changing schema/classification. Separate mode adapts a
// raw derived-only DB to the same callback; it never writes authority there.
//
// A pinned read transaction is released at workload completion or 300 ms,
// whichever happens first. This bounds the experiment even if a writer waits
// for mmap growth. No mmap preallocation or durability bypass hides that wait.
//
// Run selected cases with a fixed operation count, for example:
// go test -count=1 ./internal/store/bolt -run '^$' -bench 'BenchmarkMemoryLayout/rows=10000$/layout=same/entry=update/workers=1/reader=projection$' -benchtime=100x
func BenchmarkMemoryLayout(b *testing.B) {
	for _, rows := range []int{10000, 100000} {
		for _, layout := range []string{"same", "separate"} {
			for _, entry := range []string{"update", "batch"} {
				for _, workers := range []int{1, 8} {
					for _, reader := range []string{"none", "metadata", "projection"} {
						name := fmt.Sprintf("rows=%d/layout=%s/entry=%s/workers=%d/reader=%s", rows, layout, entry, workers, reader)
						b.Run(name, func(b *testing.B) { benchmarkMemoryLayout(b, rows, layout, entry, workers, reader) })
					}
				}
			}
		}
	}
}

// Fixed-size live history with real delete/append transactions and concurrent
// random point reads. All readers derive the live interval from the position in
// their own short transaction; no query silently accepts a missing row.
func BenchmarkMemoryLayoutMixedChurn(b *testing.B) {
	for _, rows := range []int{10000, 100000} {
		for _, layout := range []string{"same", "separate"} {
			b.Run(fmt.Sprintf("rows=%d/layout=%s", rows, layout), func(b *testing.B) {
				benchmarkMemoryLayoutWorkload(b, rows, layout, "update", 1, "none", true)
			})
		}
	}
}

// BenchmarkMemoryLayoutPricePinCompetition adds the real two-transaction
// pricing pin path to the fixed-history layout comparison. It is still a
// controlled Store fixture rather than end-to-end Gateway/ledger admission.
func BenchmarkMemoryLayoutPricePinCompetition(b *testing.B) {
	for _, rows := range []int{10000, 100000} {
		for _, layout := range []string{"same", "separate"} {
			b.Run(fmt.Sprintf("rows=%d/layout=%s", rows, layout), func(b *testing.B) {
				benchmarkMemoryLayoutPricePinCompetition(b, rows, layout)
			})
		}
	}
}

func benchmarkMemoryLayoutPricePinCompetition(b *testing.B, rows int, layout string) {
	b.StopTimer()
	store := benchmarkStore(b)
	projection := store.db
	writeProjection := store.update
	if layout == "separate" {
		var err error
		projection, err = bbolt.Open(filepath.Join(b.TempDir(), "derived.db"), 0o600, &bbolt.Options{FreelistType: bbolt.FreelistMapType})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = projection.Close() })
		writeProjection = func(fn func(*Tx) error) error {
			return projection.Update(func(tx *bbolt.Tx) error { return fn(&Tx{tx: tx}) })
		}
	}
	if err := writeProjection(func(tx *Tx) error {
		root, err := tx.CreateBucketIfNotExists(bucketUsageCheckpointSegments)
		if err != nil {
			return err
		}
		fixture, err := root.CreateBucketIfNotExists(memoryLayoutRoot)
		if err != nil {
			return err
		}
		for _, name := range [][]byte{memoryLayoutHistory, memoryLayoutDigests, memoryLayoutPosition} {
			if _, err := fixture.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 1024)
	for i := 8; i < len(payload); i++ {
		payload[i] = byte(i % 251)
	}
	initialDigest := sha256.Sum256(payload)
	for first := 0; first < rows; first += 1000 {
		if err := writeProjection(func(tx *Tx) error {
			fixture := tx.Bucket(bucketUsageCheckpointSegments).Bucket(memoryLayoutRoot)
			for row := first; row < min(first+1000, rows); row++ {
				key := encodeUint64(uint64(row + 1))
				if err := fixture.Bucket(memoryLayoutHistory).Put(key, payload); err != nil {
					return err
				}
				if err := fixture.Bucket(memoryLayoutDigests).Put(key, initialDigest[:]); err != nil {
					return err
				}
			}
			return fixture.Bucket(memoryLayoutPosition).Put([]byte("mutations"), encodeUint64(0))
		}); err != nil {
			b.Fatal(err)
		}
	}
	seedPricingDeployment(b, store, "dep_layout", 400_000, 1_600_000, 0)
	if _, err := store.CreateDeploymentPriceVersion(context.Background(), newStoredPrice("price_layout", "dep_layout", time.Now().UTC().Add(-time.Hour))); err != nil {
		b.Fatal(err)
	}
	before, err := store.MetadataJournalState()
	if err != nil {
		b.Fatal(err)
	}
	start := make(chan struct{})
	failures := make(chan error, 6)
	var group sync.WaitGroup
	var next atomic.Int64
	priceHistograms := make([]memoryLayoutHistogram, 4)
	var projectionHistogram, queryHistogram memoryLayoutHistogram
	for worker := range priceHistograms {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			<-start
			for {
				index := int(next.Add(1)) - 1
				if index >= b.N {
					return
				}
				attemptID := fmt.Sprintf("att_layout_%08d", index)
				unlock := store.LockDeploymentPricingShared("dep_layout")
				selectedAt := time.Now().UTC()
				started := time.Now()
				_, _, intent, err := store.PrepareDeploymentPricePin(context.Background(), "dep_layout", attemptID, selectedAt, time.Minute, 30*time.Second)
				if err == nil {
					_, err = store.CommitDeploymentPricePin(context.Background(), intent.AttemptID, intent.SnapshotSHA256, uint64(index+1), time.Now().UTC())
				}
				unlock()
				priceHistograms[worker].observe(time.Since(started))
				if err != nil {
					failures <- err
					return
				}
			}
		}(worker)
	}
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for iteration := 0; iteration < b.N; iteration++ {
			started := time.Now()
			err := writeProjection(func(tx *Tx) error {
				fixture := tx.Bucket(bucketUsageCheckpointSegments).Bucket(memoryLayoutRoot)
				for offset := 0; offset < 100; offset++ {
					mutation := iteration*100 + offset
					old := encodeUint64(uint64(mutation + 1))
					if err := fixture.Bucket(memoryLayoutHistory).Delete(old); err != nil {
						return err
					}
					if err := fixture.Bucket(memoryLayoutDigests).Delete(old); err != nil {
						return err
					}
					binary.BigEndian.PutUint64(payload, uint64(mutation+1))
					value := bytes.Clone(payload)
					key := encodeUint64(uint64(rows + mutation + 1))
					digest := sha256.Sum256(value)
					if err := fixture.Bucket(memoryLayoutHistory).Put(key, value); err != nil {
						return err
					}
					if err := fixture.Bucket(memoryLayoutDigests).Put(key, digest[:]); err != nil {
						return err
					}
				}
				return fixture.Bucket(memoryLayoutPosition).Put([]byte("mutations"), encodeUint64(uint64((iteration+1)*100)))
			})
			projectionHistogram.observe(time.Since(started))
			if err != nil {
				failures <- err
				return
			}
		}
	}()
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		var random uint64 = 0x704d4d2ae9
		for query := 0; query < b.N*100; query++ {
			random ^= random << 13
			random ^= random >> 7
			random ^= random << 17
			started := time.Now()
			err := projection.View(func(tx *bbolt.Tx) error {
				fixture := tx.Bucket(bucketUsageCheckpointSegments).Bucket(memoryLayoutRoot)
				position := fixture.Bucket(memoryLayoutPosition).Get([]byte("mutations"))
				if len(position) != 8 {
					return fmt.Errorf("price-pin competition lost projection position")
				}
				mutations := binary.BigEndian.Uint64(position)
				id := mutations + 1 + random%uint64(rows)
				key := encodeUint64(id)
				value := fixture.Bucket(memoryLayoutHistory).Get(key)
				if len(value) != 1024 {
					return fmt.Errorf("price-pin competition lost live history row %d", id)
				}
				version := uint64(0)
				if id > uint64(rows) {
					version = id - uint64(rows)
				}
				if binary.BigEndian.Uint64(value) != version {
					return fmt.Errorf("price-pin competition row %d has wrong version", id)
				}
				digest := sha256.Sum256(value)
				if !bytes.Equal(fixture.Bucket(memoryLayoutDigests).Get(key), digest[:]) {
					return fmt.Errorf("price-pin competition row %d digest mismatch", id)
				}
				return nil
			})
			queryHistogram.observe(time.Since(started))
			if err != nil {
				failures <- err
				return
			}
		}
	}()
	b.ResetTimer()
	started := time.Now()
	close(start)
	group.Wait()
	elapsed := time.Since(started)
	b.StopTimer()
	close(failures)
	for err := range failures {
		b.Fatal(err)
	}
	if queryHistogram.count != uint64(b.N*100) || projectionHistogram.count != uint64(b.N) {
		b.Fatal("incomplete price-pin competition workload", queryHistogram.count, projectionHistogram.count)
	}
	var priceHistogram memoryLayoutHistogram
	for _, histogram := range priceHistograms {
		priceHistogram.count += histogram.count
		priceHistogram.max = max(priceHistogram.max, histogram.max)
		for bucket, count := range histogram.buckets {
			priceHistogram.buckets[bucket] += count
		}
	}
	if priceHistogram.count != uint64(b.N) {
		b.Fatal("incomplete durable price pin workload", priceHistogram.count)
	}
	after, err := store.MetadataJournalState()
	if err != nil {
		b.Fatal("metadata journal state unavailable", after, err)
	}
	if after.Sequence <= before.Sequence || after.Applied != after.Sequence {
		b.Fatal("price pin writes did not advance the applied journal prefix", before, after)
	}
	if err := projection.View(func(tx *bbolt.Tx) error { return verifyMemoryLayoutChurnProjection(tx, rows, b.N*100, payload[8:]) }); err != nil {
		b.Fatal(err)
	}
	var pins int
	if err := store.view(func(tx *Tx) error {
		return tx.Bucket(bucketDeploymentPricePins).ForEach(func(key, raw []byte) error {
			var pin domain.PricePinIntent
			if err := json.Unmarshal(raw, &pin); err != nil || pin.State != domain.PricePinCommitted || pin.AttemptID != string(key) {
				return fmt.Errorf("invalid durable price pin %q: %w", key, err)
			}
			pins++
			return nil
		})
	}); err != nil || pins != b.N {
		b.Fatal("price pin history is incomplete", pins, b.N, err)
	}
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "price-pins/s")
	b.ReportMetric(float64(priceHistogram.upper(99)), "price-pin-p99-upper-ns")
	b.ReportMetric(float64(priceHistogram.max), "price-pin-max-ns")
	b.ReportMetric(float64(projectionHistogram.upper(99)), "projection-p99-upper-ns")
	b.ReportMetric(float64(queryHistogram.upper(99)), "point-p99-upper-ns")
	b.ReportMetric(float64(queryHistogram.max), "point-max-ns")
	b.ReportMetric(float64(b.N*100*1024+b.N*512), "logical-new-value-bytes")
	stats := projection.Stats()
	b.ReportMetric(float64(stats.FreelistInuse), "projection-freelist-bytes")
	var dbBytes int64
	for _, db := range []*bbolt.DB{store.db, projection} {
		if db == projection && projection == store.db && dbBytes != 0 {
			continue
		}
		info, err := os.Stat(db.Path())
		if err != nil {
			b.Fatal(err)
		}
		dbBytes += info.Size()
	}
	b.ReportMetric(float64(dbBytes), "all-db-file-bytes")
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	b.ReportMetric(float64(memory.HeapAlloc), "open-scan-heap-bytes")
	reportMemoryLayoutCgroup(b)
}

type memoryLayoutHistogram struct {
	buckets [64]uint64
	count   uint64
	max     uint64
}

func (h *memoryLayoutHistogram) observe(d time.Duration) {
	n := uint64(max(d.Nanoseconds(), 0))
	h.buckets[min(bits.Len64(n), 63)]++
	h.count++
	h.max = max(h.max, n)
}

func (h memoryLayoutHistogram) upper(percent uint64) uint64 {
	target, count := (h.count*percent+99)/100, uint64(0)
	for bucket, n := range h.buckets {
		count += n
		if count >= target {
			return uint64(1)<<bucket - 1
		}
	}
	return h.max
}

var memoryLayoutRoot = []byte("memory_layout_fixture")
var memoryLayoutHistory = []byte("history")
var memoryLayoutDigests = []byte("digests")
var memoryLayoutPosition = []byte("position")

type memoryLayoutReaderResult struct {
	held    time.Duration
	pending int
	err     error
}

func pinMemoryLayoutReader(db *bbolt.DB) (func() memoryLayoutReaderResult, error) {
	tx, err := db.Begin(false)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	done := make(chan memoryLayoutReaderResult, 1)
	var once sync.Once
	release := func() {
		once.Do(func() {
			pending := db.Stats().PendingPageN
			err := tx.Rollback()
			done <- memoryLayoutReaderResult{held: time.Since(started), pending: pending, err: err}
		})
	}
	timer := time.AfterFunc(300*time.Millisecond, release)
	return func() memoryLayoutReaderResult { timer.Stop(); release(); return <-done }, nil
}

func benchmarkMemoryLayout(b *testing.B, rows int, layout, entry string, workers int, reader string) {
	benchmarkMemoryLayoutWorkload(b, rows, layout, entry, workers, reader, false)
}

func benchmarkMemoryLayoutWorkload(b *testing.B, rows int, layout, entry string, workers int, reader string, churn bool) {
	b.StopTimer()
	store := benchmarkStore(b)
	projection := store.db
	writeProjection := store.update
	if layout == "separate" {
		var err error
		projection, err = bbolt.Open(filepath.Join(b.TempDir(), "derived.db"), 0o600, &bbolt.Options{FreelistType: bbolt.FreelistMapType})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { projection.Close() })
		writeProjection = func(fn func(*Tx) error) error {
			return projection.Update(func(tx *bbolt.Tx) error { return fn(&Tx{tx: tx}) })
		}
	}
	if err := writeProjection(func(tx *Tx) error {
		root, err := tx.CreateBucketIfNotExists(bucketUsageCheckpointSegments)
		if err != nil {
			return err
		}
		fixture, err := root.CreateBucketIfNotExists(memoryLayoutRoot)
		if err != nil {
			return err
		}
		for _, name := range [][]byte{memoryLayoutHistory, memoryLayoutDigests, memoryLayoutPosition} {
			if _, err := fixture.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 1024)
	for i := 8; i < len(payload); i++ {
		payload[i] = byte(i % 251)
	}
	initialDigest := sha256.Sum256(payload)
	for first := 0; first < rows; first += 1000 {
		if err := writeProjection(func(tx *Tx) error {
			fixture := tx.Bucket(bucketUsageCheckpointSegments).Bucket(memoryLayoutRoot)
			for row := first; row < min(first+1000, rows); row++ {
				key := encodeUint64(uint64(row + 1))
				if err := fixture.Bucket(memoryLayoutHistory).Put(key, payload); err != nil {
					return err
				}
				if err := fixture.Bucket(memoryLayoutDigests).Put(key, initialDigest[:]); err != nil {
					return err
				}
			}
			return fixture.Bucket(memoryLayoutPosition).Put([]byte("mutations"), encodeUint64(0))
		}); err != nil {
			b.Fatal(err)
		}
	}
	before, err := store.MetadataJournalState()
	if err != nil {
		b.Fatal(err)
	}
	beforeStats := projection.Stats()
	beforeMetadataStats := store.db.Stats()
	var releaseReader func() memoryLayoutReaderResult
	if reader != "none" {
		db := projection
		if reader == "metadata" {
			db = store.db
		}
		releaseReader, err = pinMemoryLayoutReader(db)
		if err != nil {
			b.Fatal(err)
		}
	}
	start := make(chan struct{})
	failures := make(chan error, workers+2)
	var wg sync.WaitGroup
	var next atomic.Int64
	metadataHistograms := make([]memoryLayoutHistogram, workers)
	var projectionHistogram memoryLayoutHistogram
	var queryHistogram memoryLayoutHistogram
	writeMetadata := store.update
	if entry == "batch" {
		writeMetadata = store.batch
	}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for {
				i := int(next.Add(1)) - 1
				if i >= b.N {
					return
				}
				key, value := encodeUint64(uint64(i+1)), make([]byte, 512)
				binary.BigEndian.PutUint64(value, uint64(i+1))
				started := time.Now()
				err := writeMetadata(func(tx *Tx) error { return tx.Bucket(bucketRedactionPolicies).Put(key, value) })
				metadataHistograms[worker].observe(time.Since(started))
				if err != nil {
					failures <- err
					return
				}
			}
		}(worker)
	}
	if churn {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var random uint64 = 0x7a3849de31
			for query := 0; query < b.N*100; query++ {
				random ^= random << 13
				random ^= random >> 7
				random ^= random << 17
				started := time.Now()
				err := projection.View(func(tx *bbolt.Tx) error {
					fixture := tx.Bucket(bucketUsageCheckpointSegments).Bucket(memoryLayoutRoot)
					position := fixture.Bucket(memoryLayoutPosition).Get([]byte("mutations"))
					if len(position) != 8 {
						return fmt.Errorf("random read lost its position")
					}
					mutations := binary.BigEndian.Uint64(position)
					id := mutations + 1 + random%uint64(rows)
					key := encodeUint64(id)
					value := fixture.Bucket(memoryLayoutHistory).Get(key)
					if len(value) != 1024 {
						return fmt.Errorf("random read missing live row %d", id)
					}
					version := uint64(0)
					if id > uint64(rows) {
						version = id - uint64(rows)
					}
					if binary.BigEndian.Uint64(value) != version {
						return fmt.Errorf("random read has wrong version %d", id)
					}
					digest := sha256.Sum256(value)
					if !bytes.Equal(fixture.Bucket(memoryLayoutDigests).Get(key), digest[:]) {
						return fmt.Errorf("random read digest mismatch %d", id)
					}
					return nil
				})
				queryHistogram.observe(time.Since(started))
				if err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for iteration := 0; iteration < b.N; iteration++ {
			started := time.Now()
			err := writeProjection(func(tx *Tx) error {
				fixture := tx.Bucket(bucketUsageCheckpointSegments).Bucket(memoryLayoutRoot)
				for offset := 0; offset < 100; offset++ {
					mutation := iteration*100 + offset
					key := encodeUint64(uint64(mutation%rows + 1))
					if churn {
						old := encodeUint64(uint64(mutation + 1))
						if err := fixture.Bucket(memoryLayoutHistory).Delete(old); err != nil {
							return err
						}
						if err := fixture.Bucket(memoryLayoutDigests).Delete(old); err != nil {
							return err
						}
						key = encodeUint64(uint64(rows + mutation + 1))
					}
					binary.BigEndian.PutUint64(payload, uint64(mutation+1))
					digest := sha256.Sum256(payload)
					if err := fixture.Bucket(memoryLayoutHistory).Put(key, bytes.Clone(payload)); err != nil {
						return err
					}
					if err := fixture.Bucket(memoryLayoutDigests).Put(key, digest[:]); err != nil {
						return err
					}
				}
				return fixture.Bucket(memoryLayoutPosition).Put([]byte("mutations"), encodeUint64(uint64((iteration+1)*100)))
			})
			projectionHistogram.observe(time.Since(started))
			if err != nil {
				failures <- err
				return
			}
		}
	}()
	beforeIO := readMemoryLayoutProcessIO(b)
	b.ResetTimer()
	b.StartTimer()
	started := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(started)
	b.StopTimer()
	afterIO := readMemoryLayoutProcessIO(b)
	if beforeIO != nil {
		for key, value := range afterIO {
			if value < beforeIO[key] {
				b.Fatal("process IO counter moved backwards", key)
			}
			b.ReportMetric(float64(value-beforeIO[key]), "process-"+strings.ReplaceAll(key, "_", "-")+"-delta")
		}
	}
	var readerResult memoryLayoutReaderResult
	if releaseReader != nil {
		readerResult = releaseReader()
		if readerResult.err != nil {
			b.Fatal(readerResult.err)
		}
	}
	close(failures)
	for err := range failures {
		b.Fatal(err)
	}
	var metadataHistogram memoryLayoutHistogram
	for _, h := range metadataHistograms {
		metadataHistogram.count += h.count
		metadataHistogram.max = max(metadataHistogram.max, h.max)
		for bucket, n := range h.buckets {
			metadataHistogram.buckets[bucket] += n
		}
	}
	if metadataHistogram.count != uint64(b.N) || projectionHistogram.count != uint64(b.N) {
		b.Fatal("incomplete workload")
	}
	if churn {
		if queryHistogram.count != uint64(b.N*100) {
			b.Fatal("incomplete random query workload")
		}
		b.ReportMetric(float64(queryHistogram.count), "verified-point-queries")
		b.ReportMetric(float64(queryHistogram.upper(99)), "point-p99-upper-ns")
		b.ReportMetric(float64(queryHistogram.max), "point-max-ns")
		// Data bytes deliberately exclude index/digest/journal and bbolt pages.
		b.ReportMetric(float64(b.N*100*1024+b.N*512), "logical-new-value-bytes")
	}
	after, err := store.MetadataJournalState()
	if err != nil || after.Sequence != after.Applied || after.Sequence <= before.Sequence {
		b.Fatalf("journal not committed: %+v %v", after, err)
	}
	if entry == "update" && after.Sequence-before.Sequence != uint64(b.N) {
		b.Fatal("authoritative writes did not each produce exactly one frame")
	}
	if after.Sequence-before.Sequence > uint64(b.N) {
		b.Fatal("derived writes unexpectedly journalled")
	}
	stats := projection.Stats()
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "metadata-tx/s")
	b.ReportMetric(float64(metadataHistogram.upper(95)), "metadata-p95-upper-ns")
	b.ReportMetric(float64(metadataHistogram.upper(99)), "metadata-p99-upper-ns")
	b.ReportMetric(float64(metadataHistogram.max), "metadata-max-ns")
	b.ReportMetric(float64(projectionHistogram.upper(99)), "projection-p99-upper-ns")
	b.ReportMetric(float64(readerResult.held.Nanoseconds()), "reader-held-ns")
	b.ReportMetric(float64(readerResult.pending), "reader-pending-pages")
	b.ReportMetric(float64(stats.TxStats.PageAlloc-beforeStats.TxStats.PageAlloc), "allocated-page-bytes")
	allPageAlloc := stats.TxStats.PageAlloc - beforeStats.TxStats.PageAlloc
	if projection != store.db {
		allPageAlloc += store.db.Stats().TxStats.PageAlloc - beforeMetadataStats.TxStats.PageAlloc
	}
	b.ReportMetric(float64(allPageAlloc), "all-db-allocated-page-bytes")
	b.ReportMetric(float64(after.Sequence-before.Sequence), "journal-frames")
	b.ReportMetric(float64(stats.FreelistInuse), "freelist-bytes")
	var dbBytes int64
	for _, db := range []*bbolt.DB{store.db, projection} {
		if db == projection && projection == store.db && dbBytes != 0 {
			continue
		}
		info, err := os.Stat(db.Path())
		if err != nil {
			b.Fatal(err)
		}
		dbBytes += info.Size()
	}
	b.ReportMetric(float64(dbBytes), "db-file-bytes")
	// Verify the complete fixture and metadata, close/reopen, and verify again.
	verify := func() {
		b.Helper()
		if err := projection.View(func(tx *bbolt.Tx) error {
			if churn {
				return verifyMemoryLayoutChurnProjection(tx, rows, b.N*100, payload[8:])
			}
			return verifyMemoryLayoutProjection(tx, rows, b.N*100, payload[8:])
		}); err != nil {
			b.Fatal(err)
		}
		if err := store.view(func(tx *Tx) error {
			for index := 1; index <= b.N; index++ {
				value := tx.Bucket(bucketRedactionPolicies).Get(encodeUint64(uint64(index)))
				if len(value) != 512 || binary.BigEndian.Uint64(value) != uint64(index) {
					return fmt.Errorf("metadata write %d lost", index)
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	verify()
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	b.ReportMetric(float64(memory.HeapAlloc), "open-scan-heap-bytes")
	reportMemoryLayoutCgroup(b)
	if layout == "separate" {
		path := projection.Path()
		if err := projection.Close(); err != nil {
			b.Fatal(err)
		}
		projection, err = bbolt.Open(path, 0o600, &bbolt.Options{FreelistType: bbolt.FreelistMapType})
		if err != nil {
			b.Fatal(err)
		}
	}
	path := store.db.Path()
	if err := store.Close(); err != nil {
		b.Fatal(err)
	}
	store, err = openForTest(b, path)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	if layout == "same" {
		projection = store.db
	}
	verify()
	reopened, err := store.MetadataJournalState()
	if err != nil || reopened.Sequence != after.Sequence || reopened.Applied != after.Applied {
		b.Fatalf("reopened journal diverged: %+v %v", reopened, err)
	}
	runtime.KeepAlive(projection)
	runtime.KeepAlive(store)
}

// Linux task IO accounts storage-layer bytes charged to this process, including
// the actual metadata journal and both DBs. It is not a device write-amplification
// counter and does not include setup, final scans or close/reopen.
func readMemoryLayoutProcessIO(b *testing.B) map[string]uint64 {
	b.Helper()
	if runtime.GOOS != "linux" {
		return nil
	}
	raw, err := os.ReadFile("/proc/self/io")
	if err != nil {
		b.Fatal(err)
	}
	values := make(map[string]uint64, 7)
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			b.Fatal(err)
		}
		values[strings.TrimSuffix(fields[0], ":")] = value
	}
	for _, key := range []string{"rchar", "wchar", "syscr", "syscw", "read_bytes", "write_bytes", "cancelled_write_bytes"} {
		if _, ok := values[key]; !ok {
			b.Fatal("missing process IO counter", key)
		}
	}
	return values
}

func verifyMemoryLayoutChurnProjection(tx *bbolt.Tx, rows, mutations int, suffix []byte) error {
	fixture := tx.Bucket(bucketUsageCheckpointSegments).Bucket(memoryLayoutRoot)
	history, digests := fixture.Bucket(memoryLayoutHistory), fixture.Bucket(memoryLayoutDigests)
	cursor := history.Cursor()
	count := 0
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		id := uint64(mutations + count + 1)
		if len(key) != 8 || binary.BigEndian.Uint64(key) != id || len(value) != 1024 || !bytes.Equal(value[8:], suffix) {
			return fmt.Errorf("churn history row %d malformed", id)
		}
		version := uint64(0)
		if id > uint64(rows) {
			version = id - uint64(rows)
		}
		if binary.BigEndian.Uint64(value) != version {
			return fmt.Errorf("churn row %d version mismatch", id)
		}
		digest := sha256.Sum256(value)
		if !bytes.Equal(digests.Get(key), digest[:]) {
			return fmt.Errorf("churn row %d digest mismatch", id)
		}
		count++
	}
	digestCount := 0
	dc := digests.Cursor()
	for key, value := dc.First(); key != nil; key, value = dc.Next() {
		id := uint64(mutations + digestCount + 1)
		if len(key) != 8 || binary.BigEndian.Uint64(key) != id || len(value) != sha256.Size {
			return fmt.Errorf("churn orphan digest")
		}
		digestCount++
	}
	position := fixture.Bucket(memoryLayoutPosition).Get([]byte("mutations"))
	if count != rows || digestCount != rows || len(position) != 8 || binary.BigEndian.Uint64(position) != uint64(mutations) {
		return fmt.Errorf("churn rows or position mismatch")
	}
	return nil
}

func verifyMemoryLayoutProjection(tx *bbolt.Tx, rows, mutations int, suffix []byte) error {
	fixture := tx.Bucket(bucketUsageCheckpointSegments).Bucket(memoryLayoutRoot)
	history, digests := fixture.Bucket(memoryLayoutHistory), fixture.Bucket(memoryLayoutDigests)
	cursor := history.Cursor()
	count := 0
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		if len(key) != 8 || binary.BigEndian.Uint64(key) != uint64(count+1) || len(value) != 1024 || !bytes.Equal(value[8:], suffix) {
			return fmt.Errorf("history row %d malformed", count+1)
		}
		version := 0
		if count < mutations {
			version = count + ((mutations-1-count)/rows)*rows + 1
		}
		if binary.BigEndian.Uint64(value) != uint64(version) {
			return fmt.Errorf("history row %d has wrong version", count+1)
		}
		digest := sha256.Sum256(value)
		if !bytes.Equal(digests.Get(key), digest[:]) {
			return fmt.Errorf("digest mismatch at row %d", count+1)
		}
		count++
	}
	position := fixture.Bucket(memoryLayoutPosition).Get([]byte("mutations"))
	if count != rows || len(position) != 8 || binary.BigEndian.Uint64(position) != uint64(mutations) {
		return fmt.Errorf("rows or mutation position mismatch")
	}
	return nil
}

func reportMemoryLayoutCgroup(b *testing.B) {
	b.Helper()
	directory := os.Getenv("HALRO_BENCH_CGROUP_DIR")
	if directory == "" {
		return
	}
	for _, name := range []string{"memory.current", "memory.peak"} {
		raw, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			b.Fatal(err)
		}
		value, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(value), "cgroup-"+strings.TrimPrefix(name, "memory.")+"-bytes")
	}
	raw, err := os.ReadFile(filepath.Join(directory, "memory.events"))
	if err != nil {
		b.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[0] == "oom" || fields[0] == "oom_kill" || fields[0] == "max" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(value), "cgroup-"+fields[0]+"-events")
			if value != 0 {
				b.Fatalf("cgroup capacity event %s=%d", fields[0], value)
			}
		}
	}
}
