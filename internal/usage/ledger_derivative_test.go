package usage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func TestLedgerDerivativeNativeDiskLiveAndColdAgree(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			path := filepath.Join(root, "ledger.wal")
			key := bytes.Repeat([]byte{0x71}, 32)
			native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = native.Close() })
			fixture := finalizationFixture(late, false)
			for _, r := range fixture {
				if _, err := native.Append(ctx, r.Event); err != nil {
					t.Fatal(err)
				}
			}
			// The same deterministic settlement occurs physically again after finalization.
			var settlement ledger.Event
			for _, r := range fixture {
				if r.Event.Kind == ledger.EventAttemptSettled {
					settlement = r.Event
				}
			}
			if _, err := native.Append(ctx, settlement); err != nil {
				t.Fatal(err)
			}
			live, _, err := native.BuildProjectionFromCursor(ctx, 0, 0, filepath.Join(root, "live.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = live.Close() })
			consumer, err := NewLedgerDerivative(live, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(r ledger.Record) error {
				// Force the durable index rather than the finite ID cache to answer retry.
				if r.Sequence == 6 {
					a := consumer.Aggregate()
					a.mu.Lock()
					clear(a.eventIDs)
					a.eventIDOrder = nil
					a.mu.Unlock()
				}
				if err := consumer.ApplyContext(ctx, r); err != nil {
					return err
				}
				if consumer.currentID != "" || consumer.current != (ledger.Watermark{}) {
					return errors.New("borrowed record retained")
				}
				if r.Sequence < 6 {
					if _, found, err := live.ReadEventPosition(ctx, r.Event.EventID); err != nil || found {
						return errors.New("fixture did not exercise derivative-before-Apply")
					}
				}
				return live.Apply(ctx, r)
			})
			if err != nil {
				t.Fatal(err)
			}
			cold, _, err := native.BuildProjectionFromCursor(ctx, 1, 6, filepath.Join(root, "cold.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cold.Close() })
			restored, err := NewLedgerDerivative(cold, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, restored.Apply); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(consumer.Aggregate().Snapshot(), restored.Aggregate().Snapshot()) {
				t.Fatal("live/cold Usage disagreement")
			}
			snap := consumer.Aggregate().Snapshot()
			if snap.Totals.Requests != 1 || snap.Totals.Attempts != 1 || snap.Totals.CostMicrosUSD != 7 || consumer.Aggregate().Metrics().ActiveRequests != 0 {
				t.Fatal(snap)
			}
			if late && snap.Requests[0].Attempts != 0 {
				t.Fatal("late settlement changed terminal summary")
			}
			if err := live.Close(); err != nil {
				t.Fatal(err)
			}
			r := fixture[0]
			r.Sequence = 7
			r.Generation = 1
			r.Offset = 10000
			if err := consumer.Apply(r); err == nil {
				t.Fatal("closed owner bypassed")
			}
		})
	}
}

func TestLedgerDerivativeReplayBatchMatchesSingleApplyAndRollsBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	native, err := ledger.OpenWithOptions(filepath.Join(root, "ledger.wal"), ledger.NewStatus(), ledger.Options{ChainKey: bytes.Repeat([]byte{0x73}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	for _, record := range finalizationFixture(false, false) {
		if _, err := native.Append(ctx, record.Event); err != nil {
			t.Fatal(err)
		}
	}
	owner, _, err := native.BuildProjectionFromCursor(ctx, 1, 5, filepath.Join(root, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	var records []ledger.Record
	if _, err := native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(record ledger.Record) error {
		records = append(records, record)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	batched, batchProjection, err := NewDiskLedgerDerivative(owner, filepath.Join(root, "usage-batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { batched.Close(); _ = batchProjection.Close() }()
	bad := append([]ledger.Record(nil), records...)
	bad[len(bad)-1].Event.ProjectID = "different-project"
	if err := batched.ApplyReplayBatchContext(ctx, bad); err == nil {
		t.Fatal("invalid later replay fact was accepted")
	}
	initial, err := batchProjection.ReadStats(ctx)
	if err != nil || initial.Watermark != (ledger.Watermark{}) || initial.Totals.Requests != 0 {
		t.Fatal("failed replay batch partially committed", initial, err)
	}
	if err := batched.ApplyReplayBatchContext(ctx, records); err != nil {
		t.Fatal(err)
	}
	if err := batched.ApplyReplayBatchContext(ctx, records); err != nil {
		t.Fatal("committed replay batch was not idempotent", err)
	}

	single, singleProjection, err := NewDiskLedgerDerivative(owner, filepath.Join(root, "usage-single.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { single.Close(); _ = singleProjection.Close() }()
	for _, record := range records {
		if err := single.ApplyContext(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	got, err := batchProjection.ReadStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want, err := singleProjection.ReadStats(ctx)
	if err != nil || got.Watermark != want.Watermark || got.Totals != want.Totals || got.Metrics != want.Metrics || got.Window != want.Window {
		t.Fatal("batched and single-record reducers disagree", got, want, err)
	}
	batchPage, err := batchProjection.SummaryPage(ctx, DiskSummaryQuery{Limit: 10}, DiskUsageCursor{}, diskTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	singlePage, err := singleProjection.SummaryPage(ctx, DiskSummaryQuery{Limit: 10}, DiskUsageCursor{}, diskTestLimits)
	if err != nil || !reflect.DeepEqual(batchPage, singlePage) {
		t.Fatal("batched and single-record summaries disagree", batchPage, singlePage, err)
	}
}

func TestLedgerDerivativeLegacyTerminalBeforeCrossPeriodLease(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	native, err := ledger.OpenWithOptions(filepath.Join(root, "ledger.wal"), ledger.NewStatus(), ledger.Options{ChainKey: bytes.Repeat([]byte{0x72}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	fixture := finalizationFixture(false, false)
	final := fixture[4].Event
	final.EventID = "first_terminal"
	events := []ledger.Event{final, fixture[1].Event, fixture[2].Event, fixture[3].Event, final}
	events[4].EventID = "second_terminal"
	for i := 1; i <= 3; i++ {
		events[i].PeriodID = "2026-10-03"
		events[i].PeriodTimezoneVersion = 2
	}
	for _, event := range events {
		if _, err := native.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	owner, _, err := native.BuildProjectionFromCursor(ctx, 1, uint64(len(events)), filepath.Join(root, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if legacy, err := owner.ReadLegacyRequest(ctx, final.RequestID); err != nil || !legacy {
		t.Fatal("missing actual legacy origin", legacy, err)
	}
	memory, err := NewLedgerDerivative(owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(memory.Close)
	disk, projection, err := NewDiskLedgerDerivative(owner, filepath.Join(root, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { disk.Close(); _ = projection.Close() })
	if _, err := native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(r ledger.Record) error {
		if err := memory.Apply(r); err != nil {
			return err
		}
		return disk.Apply(r)
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := memory.Aggregate().Snapshot()
	stats, err := projection.ReadStats(ctx)
	if err != nil || stats.Totals != snapshot.Totals || stats.Metrics != memory.Aggregate().Metrics() || stats.Metrics.ActiveRequests != 0 || snapshot.Totals.Requests != 1 || snapshot.Totals.Attempts != 1 || snapshot.Totals.CostMicrosUSD != 7 {
		t.Fatal("legacy disk and memory reducers disagree", stats, snapshot, err)
	}
	page, err := projection.SummaryPage(ctx, DiskSummaryQuery{Limit: 10}, DiskUsageCursor{}, diskTestLimits)
	if err != nil || !page.Done || !reflect.DeepEqual(page.Items, snapshot.Requests) || snapshot.Requests[0].Attempts != 0 {
		t.Fatal("original terminal summary changed", page, snapshot.Requests, err)
	}
	memory.Close()
	if _, err := memory.ReadLegacyRequest(ctx, final.RequestID); !errors.Is(err, ErrLedgerDerivativeRetired) {
		t.Fatal("retired legacy reader regained authority", err)
	}
}

type derivativeFaultOwner struct {
	FinalizationReader
	head                            ledger.Watermark
	headErr, errorEvent, errorFinal error
	absent                          bool
}

func (o derivativeFaultOwner) ReadPosition(context.Context) (ledger.Watermark, [32]byte, error) {
	return o.head, [32]byte{}, o.headErr
}
func (o derivativeFaultOwner) ReadEventPosition(ctx context.Context, id string) (ledger.Watermark, bool, error) {
	if o.errorEvent != nil {
		return ledger.Watermark{}, false, o.errorEvent
	}
	if o.absent {
		return ledger.Watermark{}, false, nil
	}
	return o.FinalizationReader.ReadEventPosition(ctx, id)
}
func (o derivativeFaultOwner) ReadFinalization(ctx context.Context, id string) (ledger.Record, bool, error) {
	if o.errorFinal != nil {
		return ledger.Record{}, false, o.errorFinal
	}
	return o.FinalizationReader.ReadFinalization(ctx, id)
}

func TestLedgerDerivativeErrorsAndGapsDoNotMutateUsage(t *testing.T) {
	rows := finalizationFixture(true, true)
	ioErr := errors.New("selected index IO")
	for _, reason := range []string{"head-io", "event-io", "historical-absent", "sequence-gap", "generation-gap", "offset-regressed", "zero-position", "canceled"} {
		t.Run(reason, func(t *testing.T) {
			o := derivativeFaultOwner{FinalizationReader: fixtureFinalReader(rows), absent: true}
			row := rows[0]
			ctx := context.Background()
			switch reason {
			case "head-io":
				o.headErr = ioErr
			case "event-io":
				o.errorEvent = ioErr
			case "historical-absent":
				o.head = ledger.Watermark{Generation: 1, Sequence: 1, Offset: row.Offset}
			case "sequence-gap":
				row.Sequence = 2
			case "generation-gap":
				row.Generation = 2
			case "offset-regressed":
				o.head = ledger.Watermark{Generation: 1, Sequence: 1, Offset: row.Offset}
				row.Sequence = 2
			case "zero-position":
				row.Offset = 0
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			d, err := NewLedgerDerivative(o, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := d.Aggregate().Snapshot()
			err = d.ApplyContext(ctx, row)
			if err == nil {
				t.Fatal("bad read/position accepted")
			}
			if (reason == "head-io" || reason == "event-io") && !errors.Is(err, ioErr) {
				t.Fatal("IO error hidden", err)
			}
			if !reflect.DeepEqual(before, d.Aggregate().Snapshot()) || d.currentID != "" || d.current != (ledger.Watermark{}) {
				t.Fatal("failed apply retained/mutated state")
			}
			if _, found, err := d.ReadEventPosition(context.Background(), row.Event.EventID); reason != "event-io" && (found || err != nil) {
				t.Fatal("borrow escaped", found, err)
			}
		})
	}
}

func TestLedgerDerivativeRequiresBoundRestore(t *testing.T) {
	if _, err := NewLedgerDerivative(nil, nil); err == nil {
		t.Fatal("missing owner accepted")
	}
	owner := derivativeFaultOwner{FinalizationReader: fixtureFinalReader(finalizationFixture(true, true))}
	for _, restore := range []func(FinalizationReader) (*Aggregate, error){
		func(FinalizationReader) (*Aggregate, error) { return NewAggregate(), nil },
		func(FinalizationReader) (*Aggregate, error) { return nil, nil },
		func(FinalizationReader) (*Aggregate, error) { return NewAggregateWithFinalizationReader(owner) },
	} {
		if _, err := NewLedgerDerivative(owner, restore); err == nil {
			t.Fatal("unbound restore accepted")
		}
	}
	expected := errors.New("checkpoint load failed")
	if _, err := NewLedgerDerivative(owner, func(FinalizationReader) (*Aggregate, error) { return nil, expected }); !errors.Is(err, expected) {
		t.Fatal("restore failure hidden", err)
	}
}

func TestLedgerDerivativeCheckpointBindsReaderBeforeLateSuffix(t *testing.T) {
	rows := finalizationFixture(true, true)
	source := fixtureFinalReader(rows[:4])
	owner := derivativeFaultOwner{FinalizationReader: source, head: ledger.Watermark{Generation: 1, Sequence: 4, Offset: rows[3].Offset}}
	d, err := NewLedgerDerivative(owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows[:4] {
		if err := d.Apply(row); err != nil {
			t.Fatal(err)
		}
	}
	terminal := d.Aggregate().Snapshot().Requests
	saved := newCheckpointStore()
	saved.round(t, d.Aggregate())
	restored, err := NewLedgerDerivative(owner, func(reader FinalizationReader) (*Aggregate, error) {
		return RestoreCheckpointWithFinalizationReader(saved.head, saved.read, reader)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Apply(rows[4]); err != nil {
		t.Fatal(err)
	}
	snap := restored.Aggregate().Snapshot()
	if snap.Totals.Attempts != 1 || snap.Totals.CostMicrosUSD != 7 || restored.Aggregate().Metrics().ActiveRequests != 0 || !reflect.DeepEqual(snap.Requests, terminal) {
		t.Fatal(snap)
	}
	// A persisted terminal lookup failure cannot use a borrowed current record.
	failure := errors.New("finalization index IO")
	brokenOwner := owner
	brokenOwner.errorFinal = failure
	broken, err := NewLedgerDerivative(brokenOwner, func(reader FinalizationReader) (*Aggregate, error) {
		return RestoreCheckpointWithFinalizationReader(saved.head, saved.read, reader)
	})
	if err != nil {
		t.Fatal(err)
	}
	before := broken.Aggregate().Snapshot()
	if err := broken.Apply(rows[4]); !errors.Is(err, failure) {
		t.Fatal("terminal IO hidden", err)
	}
	if !reflect.DeepEqual(before, broken.Aggregate().Snapshot()) || broken.currentID != "" {
		t.Fatal("failed terminal lookup mutated/retained Usage")
	}
}

type derivativeBlockingOwner struct {
	derivativeFaultOwner
	entered, release chan struct{}
}

func (o derivativeBlockingOwner) ReadPosition(context.Context) (ledger.Watermark, [32]byte, error) {
	close(o.entered)
	<-o.release
	return o.head, [32]byte{}, nil
}
func TestLedgerDerivativeCloseWithdrawsJoinsAndReleasesOwner(t *testing.T) {
	rows := finalizationFixture(true, false)
	row := rows[0]
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	d, err := NewLedgerDerivative(derivativeBlockingOwner{derivativeFaultOwner: derivativeFaultOwner{FinalizationReader: fixtureFinalReader(rows), absent: true}, entered: entered, release: release}, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := d.Aggregate()
	applied := make(chan error, 1)
	go func() { applied <- d.Apply(row) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner read did not enter")
	}
	closed := make(chan struct{}, 3)
	for range 3 {
		go func() { d.Close(); closed <- struct{}{} }()
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !d.retired.Load() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("Close failed to withdraw Apply")
		}
	}
	if err := d.Apply(row); !errors.Is(err, ErrLedgerDerivativeRetired) {
		t.Fatal("retirement admitted new Apply", err)
	}
	select {
	case <-closed:
		t.Fatal("Close did not join owner IO")
	default:
	}
	d.ownerMu.RLock()
	retained := d.owner != nil
	d.ownerMu.RUnlock()
	if !retained {
		t.Fatal("owner released while admitted read remained")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-applied; err != nil {
		t.Fatal("admitted Apply interrupted by withdrawal", err)
	}
	for range 3 {
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("Close did not finish")
		}
	}
	if d.Aggregate() != nil || d.owner != nil || d.currentID != "" {
		t.Fatal("retired references retained")
	}
	if _, _, err := old.finalizationReader.ReadEventPosition(context.Background(), row.Event.EventID); !errors.Is(err, ErrLedgerDerivativeRetired) {
		t.Fatal("old aggregate reader regained authority", err)
	}
	if _, _, err := old.finalizationReader.ReadFinalization(context.Background(), row.Event.RequestID); !errors.Is(err, ErrLedgerDerivativeRetired) {
		t.Fatal("old terminal reader regained authority", err)
	}
	d.Close()
}
