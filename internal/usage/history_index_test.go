package usage

import (
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

// flushedFailureFixture is failureFixture written to the index, with the
// sequence keys of req_ok's summary and its one attempt.
func flushedFailureFixture(t *testing.T) (aggregate *Aggregate, summaryKey, attemptKey []byte) {
	t.Helper()
	aggregate = failureFixture(t)
	if err := aggregate.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := aggregate.History().db.View(func(tx *bbolt.Tx) error {
		summaryKey = append([]byte(nil), tx.Bucket(historyRequestIndex).Get([]byte("req_ok"))...)
		return walkPrefixForward(tx.Bucket(historyRequestAttempts), "req_ok", func(sequence uint64) error {
			attemptKey = sequenceKey(sequence)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if len(summaryKey) != 8 || attemptKey == nil {
		t.Fatalf("fixture keys summary=%x attempt=%x", summaryKey, attemptKey)
	}
	return aggregate, summaryKey, attemptKey
}

func putIndexEntry(t *testing.T, aggregate *Aggregate, bucket, key []byte) {
	t.Helper()
	if err := aggregate.History().db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).Put(key, nil)
	}); err != nil {
		t.Fatal(err)
	}
}

// Index entries carry no MAC of their own. One added on disk, naming an intact
// row that does not belong under it, must fail the read rather than put a
// successful request in the failed list, a successful attempt among the
// anomalies, or another project's attempt in this project's list.
func TestHistoryIndexEntryMustAgreeWithItsRow(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		aggregate, summaryKey, _ := flushedFailureFixture(t)
		putIndexEntry(t, aggregate, historyFailed, summaryKey)
		if _, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 100}); !errors.Is(err, errHistoryRowTampered) {
			t.Fatalf("a successful request filed as failed was read: err=%v", err)
		}
	})
	t.Run("anomalies", func(t *testing.T) {
		aggregate, _, attemptKey := flushedFailureFixture(t)
		putIndexEntry(t, aggregate, historyAnomalies, attemptKey)
		at := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
		if _, err := aggregate.Dashboard(at, Period{}); !errors.Is(err, errHistoryRowTampered) {
			t.Fatalf("a successful attempt filed as an anomaly was read: err=%v", err)
		}
	})
	t.Run("project", func(t *testing.T) {
		aggregate, _, attemptKey := flushedFailureFixture(t)
		putIndexEntry(t, aggregate, historyProjectAttempts, compositeKey("other_project", binaryUint64(attemptKey)))
		if _, err := aggregate.QueryAttempts(AttemptQuery{Limit: 100, ProjectID: "other_project"}); !errors.Is(err, errHistoryRowTampered) {
			t.Fatalf("another project's attempt was listed: err=%v", err)
		}
	})
	t.Run("request", func(t *testing.T) {
		aggregate, _, attemptKey := flushedFailureFixture(t)
		putIndexEntry(t, aggregate, historyRequestAttempts, compositeKey("req_failed", binaryUint64(attemptKey)))
		if _, err := aggregate.QueryAttempts(AttemptQuery{Limit: 100, RequestID: "req_failed"}); !errors.Is(err, errHistoryRowTampered) {
			t.Fatalf("another request's attempt was listed: err=%v", err)
		}
	})
}

func binaryUint64(key []byte) uint64 {
	var sequence uint64
	for _, b := range key {
		sequence = sequence<<8 | uint64(b)
	}
	return sequence
}

// A malformed key where a page resumes is a read error, not a panic.
func TestHistoryPageResumingAtAMalformedKeyFails(t *testing.T) {
	aggregate, _, _ := flushedFailureFixture(t)
	putIndexEntry(t, aggregate, historyFailed, []byte{0xff, 0xff, 0xff, 0xff})
	_, err := aggregate.QueryFailedRequests(FailureQuery{Limit: 100, BeforeSequence: 1 << 40})
	if err == nil {
		t.Fatal("a malformed sequence key was walked past")
	}
}

// Ids are free text, so one may begin with another id and the separator. Its
// longer keys sort among the shorter id's and are skipped; they must not end
// the walk before the shorter id's later entries.
func TestHistoryPrefixWalkSkipsALongerIDSharingThePrefix(t *testing.T) {
	store := newTestHistory(t)
	keys := [][]byte{
		compositeKey("a", 1),
		compositeKey("a\x00x", 2),
		compositeKey("a", 0x7900000000000000),
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		for _, key := range keys {
			if err := tx.Bucket(historyRequestAttempts).Put(key, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var forward, backward []uint64
	if err := store.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(historyRequestAttempts)
		if err := walkPrefixForward(bucket, "a", func(sequence uint64) error {
			forward = append(forward, sequence)
			return nil
		}); err != nil {
			return err
		}
		return walkPrefixBackward(bucket, "a", 0, func(sequence uint64) error {
			backward = append(backward, sequence)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if len(forward) != 2 || forward[0] != 1 || forward[1] != 0x7900000000000000 {
		t.Fatalf("forward = %x", forward)
	}
	if len(backward) != 2 || backward[0] != 0x7900000000000000 || backward[1] != 1 {
		t.Fatalf("backward = %x", backward)
	}
}

// The watermark a rollup read reports is the one its rows were committed at,
// not what the aggregate has applied since.
func TestRollupRangeReportsTheWatermarkOfItsRows(t *testing.T) {
	aggregate, _, _ := flushedFailureFixture(t)
	committed := aggregate.Watermark()
	next := committed.Sequence + 1
	if err := aggregate.Apply(ledger.Record{Generation: 1, Sequence: next, Offset: int64(next * 100), Event: ledger.Event{
		EventID: "late_accepted", Kind: ledger.EventRequestAccepted, RequestID: "req_late",
		ProjectID: "project", PeriodID: "period", RequestedModel: "chat", OccurredAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	if aggregate.Watermark() == committed {
		t.Fatal("the fixture did not advance the applied watermark")
	}
	watermark, err := aggregate.History().RollupRange("", "", func(domain.RollupKey, domain.DailyRollup) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if watermark != committed {
		t.Fatalf("rollup watermark = %#v, committed = %#v", watermark, committed)
	}
}
