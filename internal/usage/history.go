package usage

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/boltreopen"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

// The usage history index (docs/todo/memory-bounded-redesign.zh-CN.md §11).
//
// history.db holds every settled attempt, every finished request summary, the
// hourly and daily rollups, and the in-flight state needed to resume, all
// derived from the Ledger and written in batches off the request path. Each
// batch commits its rows together with the Ledger watermark they describe, so
// the index never claims more of the WAL than it holds, and a start resumes by
// replaying only the suffix past that watermark.
//
// It is a derivative: whenever its version, its key or its position cannot be
// trusted it is discarded and rebuilt from the Ledger, which stays the
// accounting authority.

// historyFormatVersion is bumped whenever a stored shape changes. A file
// written under another version is rebuilt rather than read.
// Version 2: the latency histograms gained three buckets past 120s.
const historyFormatVersion = 2

const historyMACSize = sha256.Size

var (
	historyMeta             = []byte("meta")
	historyAttempts         = []byte("attempts")
	historyRequests         = []byte("requests")
	historyRequestIndex     = []byte("request_index")
	historyRequestAttempts  = []byte("request_attempts")
	historyFailed           = []byte("failed")
	historyAnomalies        = []byte("anomalies")
	historyProjectAttempts  = []byte("project_attempts")
	historyHourly           = []byte("hourly")
	historyRollups          = []byte("rollups")
	historyPendingRequests  = []byte("pending_requests")
	historyPendingStarted   = []byte("pending_started")
	historySuccesses        = []byte("successes")
	historyAllBuckets       = [][]byte{historyMeta, historyAttempts, historyRequests, historyRequestIndex, historyRequestAttempts, historyFailed, historyAnomalies, historyProjectAttempts, historyHourly, historyRollups, historyPendingRequests, historyPendingStarted, historySuccesses}
	historyKeyVersion       = []byte("version")
	historyKeyCheck         = []byte("key_check")
	historyKeyState         = []byte("state")
	historyKeyCheckLabel    = []byte("halro history key check v1")
	errHistoryRowTampered   = errors.New("usage history row failed authentication")
	errHistoryStateMissing  = errors.New("usage history state is missing")
	historyRequestSeparator = byte(0)
)

// HistoryState is the small part of the aggregate that is not a row: where the
// index stands, the running totals and counters, and the tail of event IDs the
// duplicate check needs across a restart.
type HistoryState struct {
	Watermark ledger.Watermark `json:"watermark"`
	Totals    Bucket           `json:"totals"`
	Metrics   Metrics          `json:"metrics"`
	EventIDs  []string         `json:"event_ids,omitempty"`
}

// hourlyRow is one UTC hour: the attempt-level bucket plus the request latency
// histogram the dashboard's percentiles are read from.
type hourlyRow struct {
	Bucket                 Bucket                     `json:"bucket"`
	RequestLatencyBuckets  [latencyBucketCount]uint64 `json:"request_latency_buckets"`
	RequestLatencyOverflow uint64                     `json:"request_latency_overflow"`
}

// pendingRequest is one request still in flight: its summary so far, and when
// each deployment, route and provider served a successful attempt for it,
// which onboarding counts only once the request itself has succeeded.
type pendingRequest struct {
	Summary   RequestSummary       `json:"summary"`
	Successes map[string]time.Time `json:"successes,omitempty"`
}

// The kinds a success is recorded under, as a key prefix in the successes
// table. One row per configured object, so the table grows with
// configuration rather than with traffic.
const (
	successDeployment = "d:"
	successRoute      = "r:"
	successProvider   = "p:"
)

func successKey(kind, id string) string {
	if id == "" {
		return ""
	}
	return kind + id
}

// historyBatch is one write: everything Apply produced since the previous
// batch, and the state as of its last record.
type historyBatch struct {
	state         HistoryState
	attempts      []AttemptEvent
	summaries     []RequestSummary
	hourly        map[int64]hourlyRow
	rollup        map[string]domain.DailyRollup
	pendingPut    map[string]pendingRequest
	pendingDelete map[string]struct{}
	startedPut    map[string]time.Time
	startedDelete map[string]struct{}
	successes     map[string]time.Time
}

func (b *historyBatch) records() int {
	return len(b.attempts) + len(b.summaries)
}

// HistoryStore is history.db.
type HistoryStore struct {
	db  *boltreopen.DB
	key []byte
	// commits and commitNanos time the batch transactions, fsync included,
	// so a write that grows with the file shows up as a rising average.
	commits     atomic.Uint64
	commitNanos atomic.Int64
	// rowAuthFailures counts reads that met a row failing authentication.
	// The read errors rather than skipping the row; this is what makes the
	// failure visible to an operator, who rebuilds the index from the Ledger.
	rowAuthFailures atomic.Uint64
}

// HistoryWriteStats describes what writing the index costs.
type HistoryWriteStats struct {
	Commits       uint64
	CommitSeconds float64
	// PageWrites and WriteSeconds are bbolt's own counters: pages written to
	// the file and the time spent writing them, fsync excluded.
	PageWrites    int64
	WriteSeconds  float64
	FreePages     int
	PendingPages  int
	FreelistBytes int
	FileBytes     int64
	// RowAuthFailures counts reads that met a row failing authentication.
	RowAuthFailures uint64
}

func (s *HistoryStore) WriteStats() HistoryWriteStats {
	stats := s.db.Stats()
	result := HistoryWriteStats{
		Commits: s.commits.Load(), CommitSeconds: time.Duration(s.commitNanos.Load()).Seconds(),
		PageWrites: stats.TxStats.GetWrite(), WriteSeconds: stats.TxStats.GetWriteTime().Seconds(),
		FreePages: stats.FreePageN, PendingPages: stats.PendingPageN, FreelistBytes: stats.FreelistInuse,
		RowAuthFailures: s.rowAuthFailures.Load(),
	}
	_ = s.db.View(func(tx *bbolt.Tx) error {
		result.FileBytes = tx.Size()
		return nil
	})
	return result
}

// OpenHistory opens or creates history.db. rebuilt reports that the file it
// found could not be trusted — another format version or another key — and was
// emptied, so the caller rebuilds it from the Ledger.
func OpenHistory(path string, key []byte) (store *HistoryStore, rebuilt bool, err error) {
	if len(key) != 32 {
		return nil, false, errors.New("usage history key must be 32 bytes")
	}
	db, err := boltreopen.Open(path, bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, false, fmt.Errorf("open usage history: %w", err)
	}
	store = &HistoryStore{db: db, key: bytes.Clone(key)}
	check := store.mac(historyMeta, historyKeyCheck, historyKeyCheckLabel)
	err = db.Update(func(tx *bbolt.Tx) error {
		meta := tx.Bucket(historyMeta)
		if meta != nil {
			version := meta.Get(historyKeyVersion)
			stored := meta.Get(historyKeyCheck)
			if len(version) == 8 && binary.BigEndian.Uint64(version) == historyFormatVersion &&
				hmac.Equal(stored, check) {
				return nil
			}
			rebuilt = true
		}
		return store.resetTx(tx, check)
	})
	if err != nil {
		db.Close()
		return nil, false, fmt.Errorf("initialize usage history: %w", err)
	}
	return store, rebuilt, nil
}

func (s *HistoryStore) Close() error {
	clear(s.key)
	return s.db.Close()
}

// Reset discards every row, for a caller that found the index ahead of the
// Ledger or otherwise unusable.
func (s *HistoryStore) Reset() error {
	check := s.mac(historyMeta, historyKeyCheck, historyKeyCheckLabel)
	return s.db.Update(func(tx *bbolt.Tx) error { return s.resetTx(tx, check) })
}

func (s *HistoryStore) resetTx(tx *bbolt.Tx, check []byte) error {
	for _, name := range historyAllBuckets {
		if tx.Bucket(name) != nil {
			if err := tx.DeleteBucket(name); err != nil {
				return err
			}
		}
		if _, err := tx.CreateBucket(name); err != nil {
			return err
		}
	}
	meta := tx.Bucket(historyMeta)
	if err := meta.Put(historyKeyVersion, sequenceKey(historyFormatVersion)); err != nil {
		return err
	}
	return meta.Put(historyKeyCheck, check)
}

// mac authenticates one stored value under its bucket and key, so a row cannot
// be edited, nor moved to another key or bucket, without failing its check.
func (s *HistoryStore) mac(bucket, key, value []byte) []byte {
	mac := hmac.New(sha256.New, s.key)
	var length [8]byte
	for _, part := range [][]byte{bucket, key, value} {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		mac.Write(length[:])
		mac.Write(part)
	}
	return mac.Sum(nil)
}

func (s *HistoryStore) putSealed(bucket *bbolt.Bucket, name, key []byte, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put(key, append(encoded, s.mac(name, key, encoded)...))
}

// getSealed decodes one authenticated row. A row whose MAC does not match is an
// error, never skipped: a report that silently left rows out would be wrong in
// a way nobody could see.
func (s *HistoryStore) getSealed(name, key, raw []byte, target any) error {
	if len(raw) < historyMACSize {
		return s.tampered()
	}
	body, tag := raw[:len(raw)-historyMACSize], raw[len(raw)-historyMACSize:]
	if !hmac.Equal(tag, s.mac(name, key, body)) {
		return s.tampered()
	}
	return json.Unmarshal(body, target)
}

// tampered counts a row that failed authentication and returns the error the
// read fails with.
func (s *HistoryStore) tampered() error {
	s.rowAuthFailures.Add(1)
	return errHistoryRowTampered
}

func sequenceKey(sequence uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, sequence)
	return key
}

// isAnomaly is what files an attempt under the anomalies index, and what a
// read through that index checks the authenticated row against: the index
// entries carry no MAC of their own, so an entry is only believed when the row
// it names agrees with it.
func isAnomaly(attempt AttemptEvent) bool {
	return attempt.Status != "success" || attempt.RetryCount > 0 || attempt.FallbackCount > 0
}

func compositeKey(prefix string, sequence uint64) []byte {
	key := make([]byte, 0, len(prefix)+9)
	key = append(key, prefix...)
	key = append(key, historyRequestSeparator)
	return binary.BigEndian.AppendUint64(key, sequence)
}

// loadState reads the index's position and the in-flight state it resumes
// from. errHistoryStateMissing means an empty index.
func (s *HistoryStore) loadState() (HistoryState, map[string]pendingRequest, map[string]time.Time, error) {
	var state HistoryState
	pending := map[string]pendingRequest{}
	started := map[string]time.Time{}
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(historyMeta).Get(historyKeyState)
		if raw == nil {
			return errHistoryStateMissing
		}
		if err := s.getSealed(historyMeta, historyKeyState, raw, &state); err != nil {
			return err
		}
		if err := tx.Bucket(historyPendingRequests).ForEach(func(key, value []byte) error {
			var request pendingRequest
			if err := s.getSealed(historyPendingRequests, key, value, &request); err != nil {
				return err
			}
			pending[string(key)] = request
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket(historyPendingStarted).ForEach(func(key, value []byte) error {
			var at time.Time
			if err := s.getSealed(historyPendingStarted, key, value, &at); err != nil {
				return err
			}
			started[string(key)] = at
			return nil
		})
	})
	return state, pending, started, err
}

// commit writes one batch in one transaction: rows, secondary indexes, rollup
// increments, in-flight state, and the state that names the watermark they
// cover. Either all of it lands or none of it does.
func (s *HistoryStore) commit(batch *historyBatch) error {
	started := time.Now()
	defer func() {
		s.commits.Add(1)
		s.commitNanos.Add(int64(time.Since(started)))
	}()
	return s.db.Update(func(tx *bbolt.Tx) error {
		attempts := tx.Bucket(historyAttempts)
		requestAttempts := tx.Bucket(historyRequestAttempts)
		projectAttempts := tx.Bucket(historyProjectAttempts)
		anomalies := tx.Bucket(historyAnomalies)
		for index := range batch.attempts {
			attempt := &batch.attempts[index]
			key := sequenceKey(attempt.Sequence)
			if err := s.putSealed(attempts, historyAttempts, key, attempt); err != nil {
				return err
			}
			if err := requestAttempts.Put(compositeKey(attempt.RequestID, attempt.Sequence), nil); err != nil {
				return err
			}
			if err := projectAttempts.Put(compositeKey(attempt.ProjectID, attempt.Sequence), nil); err != nil {
				return err
			}
			if isAnomaly(*attempt) {
				if err := anomalies.Put(key, nil); err != nil {
					return err
				}
			}
		}
		requests := tx.Bucket(historyRequests)
		requestIndex := tx.Bucket(historyRequestIndex)
		failed := tx.Bucket(historyFailed)
		for index := range batch.summaries {
			summary := &batch.summaries[index]
			key := sequenceKey(summary.Sequence)
			if err := s.putSealed(requests, historyRequests, key, summary); err != nil {
				return err
			}
			if err := requestIndex.Put([]byte(summary.RequestID), key); err != nil {
				return err
			}
			if summary.Outcome != "success" {
				if err := failed.Put(key, nil); err != nil {
					return err
				}
			}
		}
		if err := s.mergeHourly(tx.Bucket(historyHourly), batch.hourly); err != nil {
			return err
		}
		if err := s.applyRollupDelta(tx.Bucket(historyRollups), batch.rollup); err != nil {
			return err
		}
		pending := tx.Bucket(historyPendingRequests)
		for requestID := range batch.pendingDelete {
			if err := pending.Delete([]byte(requestID)); err != nil {
				return err
			}
		}
		for requestID, request := range batch.pendingPut {
			if err := s.putSealed(pending, historyPendingRequests, []byte(requestID), request); err != nil {
				return err
			}
		}
		started := tx.Bucket(historyPendingStarted)
		for attemptID := range batch.startedDelete {
			if err := started.Delete([]byte(attemptID)); err != nil {
				return err
			}
		}
		for attemptID, at := range batch.startedPut {
			if err := s.putSealed(started, historyPendingStarted, []byte(attemptID), at); err != nil {
				return err
			}
		}
		successes := tx.Bucket(historySuccesses)
		for name, at := range batch.successes {
			key := []byte(name)
			if raw := successes.Get(key); raw != nil {
				var stored time.Time
				if err := s.getSealed(historySuccesses, key, raw, &stored); err != nil {
					return err
				}
				if !at.After(stored) {
					continue
				}
			}
			if err := s.putSealed(successes, historySuccesses, key, at); err != nil {
				return err
			}
		}
		meta := tx.Bucket(historyMeta)
		if raw := meta.Get(historyKeyState); raw != nil {
			var current HistoryState
			if err := s.getSealed(historyMeta, historyKeyState, raw, &current); err != nil {
				return err
			}
			if batch.state.Watermark.Sequence < current.Watermark.Sequence {
				return errors.New("usage history watermark cannot move backwards")
			}
		}
		return s.putSealed(meta, historyMeta, historyKeyState, batch.state)
	})
}

func (s *HistoryStore) mergeHourly(bucket *bbolt.Bucket, delta map[int64]hourlyRow) error {
	for hour, increment := range delta {
		key := sequenceKey(uint64(hour))
		row := increment
		if raw := bucket.Get(key); raw != nil {
			var stored hourlyRow
			if err := s.getSealed(historyHourly, key, raw, &stored); err != nil {
				return err
			}
			if err := stored.add(increment); err != nil {
				return err
			}
			row = stored
		}
		if err := s.putSealed(bucket, historyHourly, key, row); err != nil {
			return err
		}
	}
	return nil
}

func (r *hourlyRow) add(other hourlyRow) error {
	if err := r.Bucket.add(other.Bucket); err != nil {
		return err
	}
	for index := range r.RequestLatencyBuckets {
		if err := addUint64(&r.RequestLatencyBuckets[index], other.RequestLatencyBuckets[index]); err != nil {
			return err
		}
	}
	return addUint64(&r.RequestLatencyOverflow, other.RequestLatencyOverflow)
}

// add folds another bucket's counts into this one. Hour and the percentile
// fields are not counts and are left alone: percentiles are read off a
// histogram, never summed.
func (b *Bucket) add(other Bucket) error {
	for _, pair := range []struct {
		target *int64
		delta  int64
	}{
		{&b.Requests, other.Requests},
		{&b.RequestErrors, other.RequestErrors},
		{&b.RequestLatencySamples, other.RequestLatencySamples},
		{&b.Attempts, other.Attempts},
		{&b.InputTokens, other.InputTokens},
		{&b.OutputTokens, other.OutputTokens},
		{&b.EstimatedInputTokens, other.EstimatedInputTokens},
		{&b.EstimatedOutputTokens, other.EstimatedOutputTokens},
		{&b.CostMicrosUSD, other.CostMicrosUSD},
		{&b.EstimatedCostMicrosUSD, other.EstimatedCostMicrosUSD},
		{&b.UnknownAttempts, other.UnknownAttempts},
		{&b.Errors, other.Errors},
		{&b.LatencyMillis, other.LatencyMillis},
	} {
		if err := addInt64(pair.target, pair.delta); err != nil {
			return err
		}
	}
	return nil
}

// applyRollupDelta folds one increment into the stored daily rows, keeping each
// accounting day's per-dimension key count bounded.
//
// New values are admitted in ledger order — the earliest sequence that reached
// each row — and everything past the cap is merged into a single
// domain.RollupOtherKey row. Ledger order is what makes the cap deterministic:
// the incremental path sees the day in many increments and a rebuild sees it in
// one, and both admit the same values because both walk the same events in the
// same order.
func (s *HistoryStore) applyRollupDelta(bucket *bbolt.Bucket, rollup map[string]domain.DailyRollup) error {
	if len(rollup) == 0 {
		return nil
	}
	type pending struct {
		key domain.RollupKey
		row domain.DailyRollup
	}
	groups := map[string][]pending{}
	for encoded, row := range rollup {
		parsed, err := domain.DecodeRollupKey(encoded)
		if err != nil {
			return err
		}
		if parsed.PeriodID != row.PeriodID || parsed.TimezoneVersion != row.TimezoneVersion {
			return fmt.Errorf("usage rollup row %q does not match its key", encoded)
		}
		prefix := domain.RollupDimensionPrefix(parsed.PeriodID, parsed.TimezoneVersion, parsed.Dimension)
		groups[prefix] = append(groups[prefix], pending{key: parsed, row: row})
	}
	prefixes := make([]string, 0, len(groups))
	for prefix := range groups {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		admitted := map[string]struct{}{}
		cursor := bucket.Cursor()
		seek := []byte(prefix)
		for key, _ := cursor.Seek(seek); key != nil && bytes.HasPrefix(key, seek); key, _ = cursor.Next() {
			if value := string(key[len(prefix):]); value != domain.RollupOtherKey {
				admitted[value] = struct{}{}
			}
		}
		items := groups[prefix]
		sort.Slice(items, func(i, j int) bool {
			if items[i].row.FirstSequence != items[j].row.FirstSequence {
				return items[i].row.FirstSequence < items[j].row.FirstSequence
			}
			return items[i].key.DimensionKey < items[j].key.DimensionKey
		})
		for _, item := range items {
			key := item.key
			if key.Dimension != domain.RollupDimensionTotal && key.DimensionKey != domain.RollupOtherKey {
				if _, known := admitted[key.DimensionKey]; !known {
					if len(admitted) >= domain.MaxRollupKeysPerDimension {
						key.DimensionKey = domain.RollupOtherKey
					} else {
						admitted[key.DimensionKey] = struct{}{}
					}
				}
			}
			encoded := []byte(key.Encode())
			row := item.row
			if raw := bucket.Get(encoded); raw != nil {
				var stored domain.DailyRollup
				if err := s.getSealed(historyRollups, encoded, raw, &stored); err != nil {
					return err
				}
				if err := stored.Add(item.row); err != nil {
					return err
				}
				row = stored
			}
			if err := s.putSealed(bucket, historyRollups, encoded, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// Reopen closes history.db and opens it again, giving back the memory bbolt's
// freelist accumulates with every write (see boltreopen). It reports false,
// having done nothing, while a read or a batch is running.
func (s *HistoryStore) Reopen() (bool, error) {
	return s.db.Reopen()
}
