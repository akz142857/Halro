package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	bbolt "go.etcd.io/bbolt"
)

type latencyHistogram struct {
	Count    uint64     `json:"count"`
	Min      uint64     `json:"min_nanos"`
	Max      uint64     `json:"max_nanos"`
	Total    uint64     `json:"total_nanos"`
	Buckets  [64]uint64 `json:"log2_nanosecond_buckets"`
	P95Upper uint64     `json:"p95_upper_bound_nanos"`
	P99Upper uint64     `json:"p99_upper_bound_nanos"`
}

func (h *latencyHistogram) add(elapsed time.Duration) {
	n := uint64(max(elapsed.Nanoseconds(), 0))
	if h.Count == 0 {
		h.Min = n
	} else {
		h.Min = min(h.Min, n)
	}
	h.Max = max(h.Max, n)
	h.Total += n
	h.Count++
	h.Buckets[min(bits.Len64(n), 63)]++
}

func (h *latencyHistogram) finish() {
	percentile := func(percent uint64) uint64 {
		target := (h.Count*percent + 99) / 100
		var count uint64
		for index, n := range h.Buckets {
			count += n
			if count >= target {
				return (uint64(1) << index) - 1
			}
		}
		return h.Max
	}
	h.P95Upper, h.P99Upper = percentile(95), percentile(99)
}

type layoutReport struct {
	BboltVersion    string           `json:"bbolt_version"`
	Batch           int              `json:"batch_events"`
	PayloadBytes    int              `json:"history_value_bytes"`
	MetadataWrites  int              `json:"metadata_writes"`
	Durability      string           `json:"durability"`
	Metadata        latencyHistogram `json:"metadata_latency"`
	Projection      latencyHistogram `json:"projection_latency"`
	ElapsedNanos    int64            `json:"concurrent_write_elapsed_nanos"`
	Rows            int              `json:"verified_rows"`
	Watermark       uint64           `json:"verified_watermark"`
	ScanNanos       int64            `json:"scan_elapsed_nanos"`
	ReopenNanos     int64            `json:"reopen_elapsed_nanos"`
	FileBytes       int64            `json:"db_file_bytes"`
	ProjectionStats bbolt.Stats      `json:"projection_db_stats"`
	MetadataStats   bbolt.Stats      `json:"metadata_db_stats"`
	OpenAfterScan   memoryPoint      `json:"open_databases_after_scan_gc"`
}

var bucketSettings = []byte("settings")
var bucketHistory = []byte("history")
var bucketDigests = []byte("digests")
var bucketPosition = []byte("position")

// runLayout compares only transaction isolation and storage shape. It is not a
// production projection, recovery protocol, backup, or accounting benchmark.
func runLayout(o options) (layoutReport, error) {
	r := layoutReport{BboltVersion: "unknown", Batch: o.batch, PayloadBytes: o.payloadBytes,
		MetadataWrites: o.metadataWrites, Durability: "bbolt default synchronous commits; no authoritative WAL"}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range info.Deps {
			if dependency.Path == "go.etcd.io/bbolt" {
				r.BboltVersion = dependency.Version
			}
		}
	}
	directory, err := os.MkdirTemp(o.directory, "halro-layout-")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(directory) // Owns only the newly created temporary directory.
	open := func(name string) (*bbolt.DB, error) {
		return bbolt.Open(filepath.Join(directory, name), 0o600, &bbolt.Options{FreelistType: bbolt.FreelistMapType})
	}
	metadata, err := open("metadata.db")
	if err != nil {
		return r, err
	}
	defer func() {
		if metadata != nil {
			metadata.Close()
		}
	}()
	projection := metadata
	if o.mode == "layout-separate" {
		projection, err = open("projection.db")
		if err != nil {
			return r, err
		}
		defer func() {
			if projection != nil {
				projection.Close()
			}
		}()
	}
	if err := metadata.Update(func(tx *bbolt.Tx) error { _, err := tx.CreateBucket(bucketSettings); return err }); err != nil {
		return r, err
	}
	if err := projection.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketHistory, bucketDigests, bucketPosition} {
			if _, err := tx.CreateBucket(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return r, err
	}
	payload := make([]byte, o.payloadBytes)
	for index := range payload {
		payload[index] = byte(index % 251)
	}
	digest := sha256.Sum256(payload)
	start := make(chan struct{})
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for index := 0; index < o.metadataWrites; index++ {
			var key, value [8]byte
			binary.BigEndian.PutUint64(key[:], uint64(index%16))
			binary.BigEndian.PutUint64(value[:], uint64(index))
			started := time.Now()
			err := metadata.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bucketSettings).Put(key[:], value[:]) })
			r.Metadata.add(time.Since(started))
			if err != nil {
				errors <- err
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for first := 1; first <= o.requests*5; first += o.batch {
			last := min(first+o.batch-1, o.requests*5)
			started := time.Now()
			err := projection.Update(func(tx *bbolt.Tx) error {
				history, digests := tx.Bucket(bucketHistory), tx.Bucket(bucketDigests)
				for sequence := first; sequence <= last; sequence++ {
					var key [8]byte
					binary.BigEndian.PutUint64(key[:], uint64(sequence))
					if err := history.Put(key[:], payload); err != nil {
						return err
					}
					if err := digests.Put(key[:], digest[:]); err != nil {
						return err
					}
				}
				var position [8]byte
				binary.BigEndian.PutUint64(position[:], uint64(last))
				return tx.Bucket(bucketPosition).Put([]byte("sequence"), position[:])
			})
			r.Projection.add(time.Since(started))
			if err != nil {
				errors <- err
				return
			}
		}
	}()
	started := time.Now()
	close(start)
	workers.Wait()
	r.ElapsedNanos = time.Since(started).Nanoseconds()
	close(errors)
	for err := range errors {
		if err != nil {
			return r, err
		}
	}
	r.Metadata.finish()
	r.Projection.finish()
	r.ProjectionStats, r.MetadataStats = projection.Stats(), metadata.Stats()
	separate := projection != metadata
	if separate {
		if err := projection.Close(); err != nil {
			return r, err
		}
		projection = nil
	}
	if err := metadata.Close(); err != nil {
		return r, err
	}
	metadata = nil
	started = time.Now()
	metadata, err = open("metadata.db")
	if err != nil {
		return r, err
	}
	projection = metadata
	if separate {
		projection, err = open("projection.db")
		if err != nil {
			return r, err
		}
	}
	r.ReopenNanos = time.Since(started).Nanoseconds()
	started = time.Now()
	err = projection.View(func(tx *bbolt.Tx) error {
		cursor := tx.Bucket(bucketHistory).Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			r.Rows++
			if len(key) != 8 || binary.BigEndian.Uint64(key) != uint64(r.Rows) || len(value) != len(payload) {
				return fmt.Errorf("history row %d is malformed", r.Rows)
			}
			actual := sha256.Sum256(value)
			stored := tx.Bucket(bucketDigests).Get(key)
			if len(stored) != len(actual) || string(stored) != string(actual[:]) {
				return fmt.Errorf("digest mismatch at row %d", r.Rows)
			}
		}
		position := tx.Bucket(bucketPosition).Get([]byte("sequence"))
		if len(position) != 8 {
			return fmt.Errorf("missing position")
		}
		r.Watermark = binary.BigEndian.Uint64(position)
		if r.Rows != o.requests*5 || r.Watermark != uint64(r.Rows) {
			return fmt.Errorf("rows and watermark mismatch")
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	r.ScanNanos = time.Since(started).Nanoseconds()
	if err := metadata.View(func(tx *bbolt.Tx) error {
		for slot := 0; slot < min(16, o.metadataWrites); slot++ {
			var key [8]byte
			binary.BigEndian.PutUint64(key[:], uint64(slot))
			value := tx.Bucket(bucketSettings).Get(key[:])
			last := slot + ((o.metadataWrites-1-slot)/16)*16
			if len(value) != 8 || binary.BigEndian.Uint64(value) != uint64(last) {
				return fmt.Errorf("metadata slot %d did not survive reopen", slot)
			}
		}
		return nil
	}); err != nil {
		return r, err
	}
	for _, name := range []string{"metadata.db", "projection.db"} {
		info, err := os.Stat(filepath.Join(directory, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return r, err
		}
		r.FileBytes += info.Size()
	}
	runtime.GC()
	r.OpenAfterScan = sampleMemory(o.cgroup)
	runtime.KeepAlive(metadata)
	runtime.KeepAlive(projection)
	return r, nil
}
