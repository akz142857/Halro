package ledger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	bbolt "go.etcd.io/bbolt"
)

func TestPendingLeaseStatisticsMatchStateAndTimeOrderAtEveryCut(t *testing.T) {
	p := projectionTestStore(t)
	s := NewState()
	ctx := context.Background()
	seq := uint64(0)
	// Native times outside UnixNano's range and ties exercise time ordering.
	var events []Event
	for i, year := range []int{2500, 1500, 2500} {
		life := projectionTestLifecycle(i+1, false)
		for j := range life {
			life[j].OccurredAt = time.Date(year, 1, 2, 12, 0, 0, 9, time.FixedZone("east", 3600))
		}
		events = append(events, life[:3]...)
	}
	for _, id := range []int{2, 1, 3} {
		life := projectionTestLifecycle(id, false)
		// Settlement time need not be the original reservation time.
		events = append(events, life[3:]...)
	}
	for _, e := range events {
		seq++
		r := projectionTestRecord(seq, e)
		if err := s.Apply(r); err != nil {
			t.Fatal(err)
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
		for _, now := range []time.Time{time.Date(1400, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2600, 1, 1, 0, 0, 0, 0, time.UTC)} {
			got, err := p.ReadPendingLeaseStats(ctx, now)
			want, stateErr := s.ReadPendingLeaseStats(ctx, now)
			oldCount, oldAge := s.PendingLeaseStats(now)
			if err != nil || stateErr != nil || got != want || got.Count != int64(oldCount) || got.OldestAge != oldAge {
				t.Fatalf("cut=%d got=%#v want=%#v err=%v/%v", seq, got, want, err, stateErr)
			}
		}
	}
}

func TestPendingLeaseStatisticsFinalizedPendingDuplicatesAndBatchRollback(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	seq := projectionApplyEvents(t, p, projectionTestAttribution(), 0)
	life := projectionTestLifecycle(2, true)
	seq = projectionApplyEvents(t, p, life[:3], seq)
	seq = projectionApplyEvents(t, p, life[4:], seq)
	before, err := p.ReadPendingLeaseStats(ctx, now)
	if err != nil || before.Count != 1 || before.Watermark.Sequence != seq {
		t.Fatal(before, err)
	}
	seq = projectionApplyEvents(t, p, life[1:2], seq)
	v, err := p.ReadPendingLeaseStats(ctx, now)
	if err != nil || v.Count != 1 || v.OldestAge != before.OldestAge {
		t.Fatal("duplicate altered pending stats", v, err)
	}
	other := projectionTestLifecycle(3, false)
	batch := []Record{projectionTestRecord(seq+1, other[0]), projectionTestRecord(seq+2, other[1]), projectionTestRecord(seq+3, other[1])}
	batch[2].Event.EventID = "different-reservation-for-same-attempt"
	if err := p.ApplyBatch(ctx, batch); err == nil {
		t.Fatal("double reservation batch accepted")
	}
	rolled, err := p.ReadPendingLeaseStats(ctx, now)
	if err != nil || rolled != v {
		t.Fatal("rollback left time/count changes", rolled, v, err)
	}
	seq = projectionApplyEvents(t, p, life[3:4], seq)
	seq = projectionApplyEvents(t, p, life[3:4], seq)
	v, err = p.ReadPendingLeaseStats(ctx, now)
	if err != nil || v.Count != 0 || v.OldestAge != 0 || v.Watermark.Sequence != seq {
		t.Fatal("late/duplicate settlement did not clear one pending lease", v, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadPendingLeaseStats(ctx, now); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal(err)
	}
}

func TestPendingLeaseStatisticsCorruptionCannotBecomeZeroOrAdvanceApply(t *testing.T) {
	for _, mode := range []string{"missing_count", "negative_count", "implausible_count", "missing_bucket", "empty_index", "dangling", "wrong_time", "wrong_id", "zero_count_with_index", "overflow_count"} {
		t.Run(mode, func(t *testing.T) {
			p := projectionTestStore(t)
			ctx := context.Background()
			life := projectionTestLifecycle(1, false)
			seq := projectionApplyEvents(t, p, life[:2], 0)
			if err := p.db.Update(func(tx *bbolt.Tx) error {
				b := tx.Bucket([]byte("pending_by_time"))
				switch mode {
				case "missing_count":
					return tx.Bucket([]byte("meta")).Delete([]byte("pending_count"))
				case "negative_count":
					return projectionPut(tx, "meta", []byte("pending_count"), int64(-1))
				case "implausible_count":
					return projectionPut(tx, "meta", []byte("pending_count"), int64(100))
				case "overflow_count":
					return projectionPut(tx, "meta", []byte("pending_count"), int64(math.MaxInt64))
				case "missing_bucket":
					return tx.DeleteBucket([]byte("pending_by_time"))
				case "empty_index":
					return b.Delete(projectionPendingTimeKey(life[1]))
				case "dangling":
					return tx.Bucket([]byte("pending")).Delete(projectionIDKey(life[1].AttemptID))
				case "wrong_time":
					if err := b.Delete(projectionPendingTimeKey(life[1])); err != nil {
						return err
					}
					e := life[1]
					e.OccurredAt = e.OccurredAt.Add(-time.Hour)
					return b.Put(projectionPendingTimeKey(e), []byte(e.AttemptID))
				case "wrong_id":
					return b.Put(projectionPendingTimeKey(life[1]), []byte("att_other"))
				case "zero_count_with_index":
					return projectionPut(tx, "meta", []byte("pending_count"), int64(0))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if v, err := p.ReadPendingLeaseStats(ctx, time.Now()); !errors.Is(err, ErrProjectionCorrupt) || !reflect.DeepEqual(v, PendingLeaseStatistics{}) {
				t.Fatal("corruption credited pending metrics", v, err)
			}
			if mode == "missing_count" || mode == "missing_bucket" || mode == "overflow_count" {
				other := projectionTestLifecycle(2, false)
				if err := p.ApplyBatch(ctx, []Record{projectionTestRecord(seq+1, other[0]), projectionTestRecord(seq+2, other[1])}); !errors.Is(err, ErrProjectionCorrupt) {
					t.Fatal(err)
				}
				head, _, err := p.ReadPosition(ctx)
				if err != nil || head.Sequence != seq {
					t.Fatal("failed derived index write advanced cut", head, err)
				}
			}
		})
	}
}

func TestPendingLeaseStatisticsConcurrentApplyHasOneAccountingCut(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		seq := uint64(0)
		for i := 1; i <= 40; i++ {
			for _, e := range projectionTestLifecycle(i, false) {
				seq++
				if err := p.Apply(ctx, projectionTestRecord(seq, e)); err != nil {
					done <- err
					return
				}
			}
		}
		done <- nil
	}()
	var readErr error
	for {
		v, err := p.ReadPendingLeaseStats(ctx, time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
		phase := v.Watermark.Sequence % 5
		want := int64(0)
		if phase == 2 || phase == 3 {
			want = 1
		}
		if (err != nil || v.Count != want) && readErr == nil {
			readErr = fmt.Errorf("torn pending cut: %#v %v", v, err)
		}
		select {
		case err := <-done:
			if err != nil || readErr != nil {
				t.Fatal(err, readErr)
			}
			return
		default:
		}
	}
}

func BenchmarkPendingLeaseStatistics(b *testing.B) {
	for _, count := range []int{1, 4096} {
		for _, backend := range []string{"state", "projection"} {
			b.Run(fmt.Sprintf("%s/%d", backend, count), func(b *testing.B) {
				ctx := context.Background()
				p := projectionTestStore(b)
				s := NewState()
				var batch []Record
				sequence := uint64(0)
				flush := func() {
					if len(batch) != 0 {
						if err := p.ApplyBatch(ctx, batch); err != nil {
							b.Fatal(err)
						}
						batch = batch[:0]
					}
				}
				for i := 1; i <= count; i++ {
					for _, e := range projectionTestLifecycle(i, false)[:3] {
						sequence++
						r := projectionTestRecord(sequence, e)
						if err := s.Apply(r); err != nil {
							b.Fatal(err)
						}
						batch = append(batch, r)
						if len(batch) == projectionBatchRows {
							flush()
						}
					}
				}
				flush()
				var reader AccountingObservationReader = s
				if backend == "projection" {
					reader = p
				}
				now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					v, err := reader.ReadPendingLeaseStats(ctx, now)
					if err != nil || v.Count != int64(count) {
						b.Fatal(v, err)
					}
				}
			})
		}
	}
}
