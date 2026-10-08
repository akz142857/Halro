package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func projectionTestStore(t testing.TB) *LedgerProjection {
	t.Helper()
	p, err := createLedgerProjection(filepath.Join(t.TempDir(), "projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	p.ready = true
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}
func projectionTestRecord(sequence uint64, event Event) Record {
	return Record{Generation: 1, Sequence: sequence, Offset: int64(sequence * 100), Epoch: 5, Event: event, Hash: sha256Sum([]byte(fmt.Sprint(sequence)))}
}
func projectionTestLifecycle(id int, attributed bool) []Event {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	base := Event{RequestID: fmt.Sprintf("req_%06d", id), ProjectID: "prj_1", PeriodID: fmt.Sprintf("2026-10-%02d", id%3+1), PeriodTimezone: "UTC", PeriodTimezoneVersion: uint64(id%2 + 1), OccurredAt: now}
	if attributed {
		base.WorkUnitID = "wku_1"
		base.RunID = "run_1"
	}
	var result []Event
	for _, kind := range []EventKind{EventRequestAccepted, EventReservationCreated, EventAttemptStarted, EventAttemptSettled, EventRequestFinalized} {
		e := base
		e.Kind = kind
		e.EventID = fmt.Sprintf("evt_%06d_%d", id, kind)
		if kind >= EventReservationCreated && kind <= EventAttemptSettled {
			e.AttemptID = fmt.Sprintf("att_%06d", id)
		}
		if kind == EventReservationCreated {
			e.ReservationMicrosUSD = MicrosUSD(int64(id%100 + 1))
		}
		if kind == EventAttemptSettled {
			e.CommittedMicrosUSD = MicrosUSD(int64(id % 80))
			e.ProviderInputTokens = int64(id % 10)
			e.ProviderOutputTokens = int64(id % 4)
			e.Outcome = "success"
		}
		result = append(result, e)
	}
	return result
}
func projectionTestAttribution() []Event {
	now := time.Date(2026, 10, 2, 11, 0, 0, 0, time.UTC)
	wu := governanceEvent("evt_wu", EventWorkUnitCreated, now)
	wu.WorkUnitID = "wku_1"
	run := governanceEvent("evt_run", EventRunCreated, now)
	run.WorkUnitID = "wku_1"
	run.RunID = "run_1"
	run.RunExpiresAt = now.Add(24 * time.Hour)
	run.RunBudgetMicrosUSD = 1_000_000
	return []Event{wu, run}
}
func projectionApplyEvents(t testing.TB, p *LedgerProjection, events []Event, start uint64) uint64 {
	t.Helper()
	for _, event := range events {
		start++
		if err := p.Apply(context.Background(), projectionTestRecord(start, event)); err != nil {
			t.Fatalf("apply %d %v: %v", start, event.Kind, err)
		}
	}
	return start
}

func TestLedgerProjectionEventFirstPositionIsAtomicAndStable(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	events := projectionTestLifecycle(1, false)
	bad := []Record{projectionTestRecord(1, events[0]), projectionTestRecord(2, events[2])}
	if err := p.ApplyBatch(ctx, bad); err == nil {
		t.Fatal("start without reservation accepted")
	}
	if _, found, err := p.ReadEventPosition(ctx, events[0].EventID); err != nil || found {
		t.Fatal("rolled back event position survived", found, err)
	}
	first := projectionTestRecord(1, events[0])
	if err := p.Apply(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, projectionTestRecord(2, events[0])); err != nil {
		t.Fatal(err)
	}
	position, found, err := p.ReadEventPosition(ctx, events[0].EventID)
	if err != nil || !found || position != (Watermark{first.Generation, first.Offset, first.Sequence}) {
		t.Fatal("duplicate replaced first position", position, found, err)
	}
	head, _, err := p.ReadPosition(ctx)
	if err != nil || head.Sequence != 2 {
		t.Fatal("duplicate did not advance head", head, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ReadEventPosition(ctx, events[0].EventID); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal(err)
	}
}

func TestLedgerProjectionEventFirstPositionRejectsPartialIndex(t *testing.T) {
	for _, bucket := range []string{"events", "event_positions"} {
		t.Run(bucket, func(t *testing.T) {
			p := projectionTestStore(t)
			ctx := context.Background()
			event := projectionTestLifecycle(1, false)[0]
			if err := p.Apply(ctx, projectionTestRecord(1, event)); err != nil {
				t.Fatal(err)
			}
			if err := p.db.Update(func(tx *bbolt.Tx) error {
				return tx.Bucket([]byte(bucket)).Delete(projectionIDKey(event.EventID))
			}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := p.ReadEventPosition(ctx, event.EventID); !errors.Is(err, ErrProjectionCorrupt) {
				t.Fatal("partial index became absence", err)
			}
			if err := p.Apply(ctx, projectionTestRecord(2, event)); !errors.Is(err, ErrProjectionCorrupt) {
				t.Fatal("partial index repaired without authentication", err)
			}
			head, _, err := p.ReadPosition(ctx)
			if err != nil || head.Sequence != 1 {
				t.Fatal("failed duplicate advanced head", head, err)
			}
		})
	}
}

func TestLedgerProjectionRequestAttemptsAreScopedBoundedAndKeepTerminalAssociations(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	seq := uint64(0)
	for request := 0; request < 2; request++ {
		events := projectionTestLifecycle(request+1, false)
		seq = projectionApplyEvents(t, p, events[:1], seq)
		for attempt := 0; attempt < 3; attempt++ {
			rows := append([]Event(nil), events[1:4]...)
			for n := range rows {
				rows[n].AttemptID = fmt.Sprintf("attempt_%d_%d", request, attempt)
				rows[n].EventID = fmt.Sprintf("event_%d_%d_%d", request, attempt, n)
			}
			if attempt == 2 {
				rows = rows[:1]
			}
			seq = projectionApplyEvents(t, p, rows, seq)
		}
	}
	requestID := projectionTestLifecycle(1, false)[0].RequestID
	otherID := projectionTestLifecycle(2, false)[0].RequestID
	var cursor ProjectionCursor
	count, pending := 0, 0
	for {
		page, err := p.RequestAttemptPage(ctx, requestID, cursor, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
		if err != nil || page.Scanned > 1 || len(page.Items) > 1 {
			t.Fatal(page, err)
		}
		for _, row := range page.Items {
			if row.Lease.Event.RequestID != requestID {
				t.Fatal("association leaked another request", row)
			}
			count++
			if row.Pending {
				pending++
			} else if row.Settlement == nil {
				t.Fatal("lost terminal association", row)
			}
		}
		cursor = page.Next
		if page.Done {
			break
		}
	}
	if count != 3 || pending != 1 {
		t.Fatal(count, pending)
	}
	if _, err := p.RequestAttemptPage(ctx, otherID, cursor, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20}); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal("cross-request cursor accepted", err)
	}
	if _, err := p.RequestAttemptPage(ctx, requestID, ProjectionCursor{}, ProjectionPageLimits{Rows: 1, Bytes: 1}); !errors.Is(err, ErrProjectionPageLimit) {
		t.Fatal(err)
	}
	// A syntactically valid but missing terminal/pending partner is an error.
	if err := p.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("settled")).Delete(projectionIDKey("attempt_0_0")) }); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RequestAttemptPage(ctx, requestID, ProjectionCursor{}, ProjectionPageLimits{Rows: 3, Bytes: 1 << 20}); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal("missing attempt state became empty history", err)
	}
}

func TestLedgerProjectionRecoveryScanFreezesBothInputsBeforeWrites(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	events := projectionTestLifecycle(1, false)
	seq := projectionApplyEvents(t, p, events[:2], 0)
	scan, err := p.BeginRecoveryScan(ctx)
	if err != nil || scan.Head.Sequence != seq {
		t.Fatal(scan, err)
	}
	projectionApplyEvents(t, p, projectionTestLifecycle(2, false)[:2], seq)
	for _, kind := range []string{"pending", "requests"} {
		count := 0
		cursor := scan.Pending
		if kind == "requests" {
			cursor = scan.Requests
		}
		for {
			var next ProjectionCursor
			var done bool
			if kind == "pending" {
				page, err := p.PendingLeasePage(ctx, cursor, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
				if err != nil {
					t.Fatal(err)
				}
				count += len(page.Items)
				next, done = page.Next, page.Done
			} else {
				page, err := p.UnfinalizedRequestPage(ctx, cursor, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
				if err != nil {
					t.Fatal(err)
				}
				count += len(page.Items)
				next, done = page.Next, page.Done
			}
			cursor = next
			if done {
				break
			}
		}
		if count != 1 {
			t.Fatal("new input entered frozen recovery", kind, count)
		}
	}
}

func TestLedgerProjectionDifferentialLifecycleAndIdempotency(t *testing.T) {
	p := projectionTestStore(t)
	state := NewState()
	ctx := context.Background()
	seq := uint64(0)
	apply := func(e Event) {
		t.Helper()
		seq++
		r := projectionTestRecord(seq, e)
		if err := state.Apply(r); err != nil {
			t.Fatal(err)
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range projectionTestAttribution() {
		apply(e)
	}
	for i := 1; i <= 120; i++ {
		for _, e := range projectionTestLifecycle(i, i%2 == 0) {
			apply(e)
			if e.Kind == EventAttemptSettled {
				apply(e)
			}
		}
		key := BalanceKey{"prj_1", fmt.Sprintf("2026-10-%02d", i%3+1), uint64(i%2 + 1)}
		got, err := p.ReadBalance(ctx, key)
		if err != nil || got != state.Balance(key.ProjectID, key.PeriodID, key.TimezoneVersion) {
			t.Fatalf("balance %#v err %v", got, err)
		}
		id := fmt.Sprintf("att_%06d", i)
		lease, found, err := p.ReadLease(ctx, id)
		wantLease, wantFound := state.AccountingLease(id)
		if err != nil || found != wantFound || !reflect.DeepEqual(lease, wantLease) {
			t.Fatalf("lease differs: %v", err)
		}
		settled, found, err := p.ReadSettlement(ctx, id)
		wantSettled, wantFound := state.SettledAttempt(id)
		if err != nil || found != wantFound || !reflect.DeepEqual(settled, wantSettled) {
			t.Fatalf("settlement differs: %v", err)
		}
	}
	run, found, err := p.ReadRun(ctx, "run_1")
	wantRun, wantFound := state.Run("run_1")
	if err != nil || found != wantFound || !reflect.DeepEqual(run, wantRun) {
		t.Fatalf("run differs: %#v vs %#v %v", run, wantRun, err)
	}
	head, _, err := p.ReadPosition(ctx)
	if err != nil || head != state.Watermark() {
		t.Fatal(head, err)
	}
	page, err := p.PendingLeasePage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 10, Bytes: 1 << 20})
	if err != nil || !page.Done || len(page.Items) != 0 {
		t.Fatalf("pending %#v %v", page, err)
	}
	requests, err := p.UnfinalizedRequestPage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 10, Bytes: 1 << 20})
	if err != nil || !requests.Done || len(requests.Items) != 0 {
		t.Fatalf("active %#v %v", requests, err)
	}
	cursor := ProjectionCursor{}
	count := 0
	for {
		page, err := p.SettlementPage(ctx, cursor, ProjectionPageLimits{Rows: 7, Bytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		count += len(page.Items)
		cursor = page.Next
		if page.Done {
			break
		}
	}
	if count != 120 {
		t.Fatal(count)
	}
}

func TestLedgerProjectionBatchRollbackAndPhysicalDuplicate(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	events := projectionTestLifecycle(1, false)
	records := []Record{projectionTestRecord(1, events[0]), projectionTestRecord(2, events[1]), projectionTestRecord(3, events[2])}
	records[2].Event.AttemptID = "missing"
	if err := p.ApplyBatch(ctx, records); err == nil {
		t.Fatal("invalid batch committed")
	}
	head, _, err := p.ReadPosition(ctx)
	if err != nil || head != (Watermark{}) {
		t.Fatal(head, err)
	}
	if _, found, err := p.ReadRequest(ctx, events[0].RequestID); err != nil || found {
		t.Fatal(found, err)
	}
	records[2].Event = events[2]
	if err := p.ApplyBatch(ctx, records); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, records[2]); err != nil {
		t.Fatalf("exact committed physical retry: %v", err)
	}
	wrong := records[2]
	wrong.Hash[0] ^= 1
	if err := p.Apply(ctx, wrong); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal(err)
	}
	wrong = projectionTestRecord(4, events[2])
	wrong.Event.ProviderInputTokens = 5
	if err := p.Apply(ctx, wrong); err == nil {
		t.Fatal("event ID changed content accepted")
	}
	head, _, err = p.ReadPosition(ctx)
	if err != nil || head.Sequence != 3 {
		t.Fatal(head, err)
	}
	// Overflow after the reducer has already changed its disposable pending
	// maps must not publish any part of the mutation to disk.
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		return projectionPut(tx, "balances", projectionBalanceKey(BalanceKey{"prj_1", events[1].PeriodID, events[1].PeriodTimezoneVersion}), Balance{ReservedMicrosUSD: math.MaxInt64})
	}); err != nil {
		t.Fatal(err)
	}
	e := events[1]
	e.EventID = "overflow"
	e.AttemptID = "overflow"
	if err := p.Apply(ctx, projectionTestRecord(4, e)); err == nil {
		t.Fatal("overflow accepted")
	}
	if _, found, err := p.ReadLease(ctx, "overflow"); err != nil || found {
		t.Fatal(found, err)
	}
}

func TestLedgerProjectionFinalizedRequestRetainsLatePendingAttribution(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	seq := projectionApplyEvents(t, p, projectionTestAttribution(), 0)
	events := projectionTestLifecycle(2, true)
	seq = projectionApplyEvents(t, p, events[:3], seq)
	seq = projectionApplyEvents(t, p, events[4:], seq)
	active, err := p.UnfinalizedRequestPage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || len(active.Items) != 0 {
		t.Fatal(active, err)
	}
	pending, err := p.PendingLeasePage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || len(pending.Items) != 1 || !pending.Items[0].Started {
		t.Fatal(pending, err)
	}
	request, found, err := p.ReadRequest(ctx, events[0].RequestID)
	if err != nil || !found || request.Finalized == nil || request.Accepted.Sequence != 3 {
		t.Fatal(request, found, err)
	}
	wrong := events[3]
	wrong.RequestID = "other"
	wrong.EventID = "wrong-request"
	if err := p.Apply(ctx, projectionTestRecord(seq+1, wrong)); err == nil {
		t.Fatal("late settlement changed request")
	}
	wrong = events[3]
	wrong.RunID = "run_other"
	wrong.EventID = "wrong-run"
	if err := p.Apply(ctx, projectionTestRecord(seq+1, wrong)); err == nil {
		t.Fatal("late settlement changed run")
	}
	seq = projectionApplyEvents(t, p, events[3:4], seq)
	settled, found, err := p.ReadSettlement(ctx, events[3].AttemptID)
	if err != nil || !found || settled.Settlement.RunID != "run_1" {
		t.Fatal(settled, found, err)
	}
	run, _, err := p.ReadRun(ctx, "run_1")
	if err != nil || run.ReservedMicrosUSD != 0 || run.CommittedMicrosUSD != 2 {
		t.Fatal(run, err)
	}
	pending, err = p.PendingLeasePage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || len(pending.Items) != 0 {
		t.Fatal(pending, err)
	}
	// A later deterministic duplicate consumes a physical record without
	// reactivating the finalized request or duplicating its committed balance.
	seq = projectionApplyEvents(t, p, events[3:4], seq)
	head, _, err := p.ReadPosition(ctx)
	if err != nil || head.Sequence != seq {
		t.Fatal(head, err)
	}
}

func TestLedgerProjectionGovernanceIdempotencyDoesNotInventSecondaryIndex(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	events := projectionTestAttribution()
	seq := projectionApplyEvents(t, p, events, 0)
	duplicate := events[1]
	duplicate.EventID = "other_physical_event"
	duplicate.RunID = "run_should_not_exist"
	seq = projectionApplyEvents(t, p, []Event{duplicate}, seq)
	firstAlias, found, err := p.ReadEventPosition(ctx, duplicate.EventID)
	if err != nil || !found || firstAlias.Sequence != seq {
		t.Fatal("new alias has no first occurrence", firstAlias, found, err)
	}
	if _, found, err := p.ReadRun(ctx, duplicate.RunID); err != nil || found {
		t.Fatal(found, err)
	}
	if err := p.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket([]byte("runs_by_work_unit")).Stats().KeyN != 1 {
			return errors.New("invented secondary index")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The alias event digest was committed even though the lifecycle effect
	// was already represented by its idempotency key.
	seq = projectionApplyEvents(t, p, []Event{duplicate}, seq)
	if first, found, err := p.ReadEventPosition(ctx, duplicate.EventID); err != nil || !found || first != firstAlias {
		t.Fatal("alias duplicate replaced first occurrence", first, found, err)
	}
	for i := 2; i <= domain.MaxRunsPerWorkUnit; i++ {
		e := events[1]
		e.EventID = fmt.Sprint("run", i)
		e.RunID = fmt.Sprint("run_", i)
		e.Operation = e.EventID
		seq = projectionApplyEvents(t, p, []Event{e}, seq)
	}
	e := events[1]
	e.EventID = "over-limit"
	e.RunID = "over-limit"
	e.Operation = e.EventID
	if err := p.Apply(ctx, projectionTestRecord(seq+1, e)); err == nil {
		t.Fatal("run limit ignored")
	}
}

func TestLedgerProjectionRecoveryPagesReleaseReaderAndFreezeAdmissionPrefix(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	seq := uint64(0)
	for i := 1; i <= 7; i++ {
		seq = projectionApplyEvents(t, p, projectionTestLifecycle(i, false)[:2], seq)
	}
	limits := ProjectionPageLimits{Rows: 2, Bytes: 1 << 20}
	page, err := p.PendingLeasePage(ctx, ProjectionCursor{}, limits)
	if err != nil || len(page.Items) != 2 || page.Done {
		t.Fatal(page, err)
	}
	cursor := page.Next
	if p.db.Stats().OpenTxN != 0 {
		t.Fatal("page retained read transaction")
	}
	// Complete an unvisited pending row and add a new lease after
	// the frozen head. Neither may be presented as unresolved old work.
	seq = projectionApplyEvents(t, p, projectionTestLifecycle(4, false)[3:4], seq)
	seq = projectionApplyEvents(t, p, projectionTestLifecycle(99, false)[:2], seq)
	ids := []string{page.Items[0].Reservation.AttemptID, page.Items[1].Reservation.AttemptID}
	for {
		page, err = p.PendingLeasePage(ctx, cursor, limits)
		if err != nil {
			t.Fatal(err)
		}
		if page.Scanned > 2 || page.Bytes > limits.Bytes {
			t.Fatal(page)
		}
		for _, item := range page.Items {
			ids = append(ids, item.Reservation.AttemptID)
		}
		cursor = page.Next
		if page.Done {
			break
		}
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"att_000001", "att_000002", "att_000003", "att_000005", "att_000006", "att_000007"}) {
		t.Fatal(ids)
	}
	other := projectionTestStore(t)
	if _, err := other.PendingLeasePage(ctx, cursor, limits); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal(err)
	}
	if _, err := p.UnfinalizedRequestPage(ctx, cursor, limits); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal(err)
	}
	if _, err := p.PendingLeasePage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 1, Bytes: 1}); !errors.Is(err, ErrProjectionPageLimit) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.PendingLeasePage(canceled, ProjectionCursor{}, limits); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLedgerProjectionDecodeIndexErrorsAndCloseAreExplicit(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	projectionApplyEvents(t, p, projectionTestLifecycle(1, false)[:2], 0)
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("balances")).Put(projectionBalanceKey(BalanceKey{"prj_1", "2026-10-02", 2}), []byte("null"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadBalance(ctx, BalanceKey{"prj_1", "2026-10-02", 2}); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal(err)
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("requests")).Delete(projectionIDKey("req_000001")) }); err != nil {
		t.Fatal(err)
	}
	if page, err := p.UnfinalizedRequestPage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20}); !errors.Is(err, ErrProjectionCorrupt) || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _, err := p.ReadLease(ctx, "att_000001")
				if err != nil && !errors.Is(err, ErrProjectionUnavailable) {
					t.Error(err)
				}
			}
		}()
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if _, found, err := p.ReadLease(ctx, "att_000001"); !errors.Is(err, ErrProjectionUnavailable) || found {
		t.Fatal(found, err)
	}
}

func TestLedgerProjectionBuildAuthenticatesCompleteHistoryAndRefusesExistingOutput(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "ledger.wal")
	log := openChained(t, path, nil)
	for i := 1; i <= 20; i++ {
		for _, event := range projectionTestLifecycle(i, false) {
			if _, err := log.Append(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		if i == 6 {
			if _, err := log.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	expected, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	output := filepath.Join(root, "projection.db")
	p, err := BuildLedgerProjection(ctx, path, testChainKey, output, expected)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	head, hash, err := p.ReadPosition(ctx)
	if err != nil || head != expected.Head || hash != expected.ChainHash {
		t.Fatal(head, hash, err)
	}
	if _, err := BuildLedgerProjection(ctx, path, testChainKey, output, expected); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	if _, found, err := p.ReadSettlement(ctx, "att_000020"); err != nil || !found {
		t.Fatal(found, err)
	}
	bad := expected
	bad.ChainHash[0] ^= 1
	rejected := filepath.Join(root, "rejected.db")
	if _, err := BuildLedgerProjection(ctx, path, testChainKey, rejected, bad); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal(err)
	}
	if _, err := os.Stat(rejected); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	// Changing the old sealed content and repairing its checksum still cannot
	// make a new derived candidate activate: the WAL HMAC remains authoritative.
	manifest, err := loadSegmentManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(root, manifest.Segments[0].File)
	tamperWithSealedFrame(t, sealed)
	repairManifestChecksum(t, root, 1, sealed)
	if _, err := BuildLedgerProjection(ctx, path, testChainKey, rejected, expected); !errors.Is(err, ErrTampered) {
		t.Fatal(err)
	}
	if _, err := os.Stat(rejected); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestLedgerProjectionBuildCancellationAndIncompleteTailDiscardCandidate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ledger.wal")
	log := openChained(t, path, nil)
	for i := 1; i <= 80; i++ {
		if _, err := log.Append(context.Background(), validReservation(fmt.Sprint("evt_", i), fmt.Sprint("att_", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	expected, _, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("hal")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "projection.db")
	if _, err := BuildLedgerProjection(context.Background(), path, testChainKey, output, expected); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildLedgerProjection(ctx, path, testChainKey, output, expected); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func BenchmarkLedgerProjectionRetainedHistory(b *testing.B) {
	for _, count := range []int{100, 10000} {
		for _, disk := range []bool{false, true} {
			name := "Memory"
			if disk {
				name = "Disk"
			}
			b.Run(fmt.Sprintf("%s/%d", name, count), func(b *testing.B) {
				var retained uint64
				b.ReportAllocs()
				for range b.N {
					runtime.GC()
					var before runtime.MemStats
					runtime.ReadMemStats(&before)
					var state *State
					var p *LedgerProjection
					if disk {
						var err error
						p, err = createLedgerProjection(filepath.Join(b.TempDir(), "projection.db"))
						if err != nil {
							b.Fatal(err)
						}
						p.ready = true
					} else {
						state = NewState()
					}
					var batch []Record
					seq := uint64(0)
					flush := func() {
						if len(batch) == 0 {
							return
						}
						if disk {
							if err := p.ApplyBatch(context.Background(), batch); err != nil {
								b.Fatal(err)
							}
						} else {
							for _, r := range batch {
								if err := state.Apply(r); err != nil {
									b.Fatal(err)
								}
							}
						}
						clear(batch)
						batch = batch[:0]
					}
					for i := 1; i <= count; i++ {
						for _, event := range projectionTestLifecycle(i, false) {
							seq++
							batch = append(batch, projectionTestRecord(seq, event))
							if len(batch) == projectionBatchRows {
								flush()
							}
						}
					}
					flush()
					batch = nil
					runtime.GC()
					var after runtime.MemStats
					runtime.ReadMemStats(&after)
					if after.HeapAlloc > before.HeapAlloc {
						retained += after.HeapAlloc - before.HeapAlloc
					}
					if disk {
						balance, err := p.ReadBalance(context.Background(), BalanceKey{"prj_1", "2026-10-02", 2})
						if err != nil || balance.ReservedMicrosUSD != 0 {
							b.Fatal(balance, err)
						}
						if err := p.Close(); err != nil {
							b.Fatal(err)
						}
					}
					runtime.KeepAlive(state)
					runtime.KeepAlive(p)
				}
				b.ReportMetric(float64(retained)/float64(b.N), "retained-B/op")
			})
		}
	}
}

// Key tuples must not alias when user-controlled components contain delimiters.
func TestLedgerProjectionCompositeKeysAreUnambiguous(t *testing.T) {
	if bytes.Equal(projectionTuple("a\x00b", "c"), projectionTuple("a", "b\x00c")) {
		t.Fatal("tuple collision")
	}
}

func TestLedgerProjectionFreeAndUnknownLeaseDifferential(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []LeaseMode{LeaseModeFree, LeaseModeUnknownAllowed} {
		t.Run(string(mode), func(t *testing.T) {
			p := projectionTestStore(t)
			state := NewState()
			events := projectionTestLifecycle(1, false)
			var snapshot domain.PriceSnapshot
			if mode == LeaseModeUnknownAllowed {
				snapshot = domain.NewUnknownPriceSnapshot(events[0].OccurredAt)
				events[1].ReservationMicrosUSD = nil
				events[3].CommittedMicrosUSD = nil
				events[1].UnknownPolicyEvidence = &domain.UnknownPricePolicyEvidence{PolicyVersion: "v1", ProjectID: "prj_1", TokenGuardStatus: "disabled", ReasonCode: "cost_governance_disabled", InstanceExplicitOptIn: true, CostGovernanceDisabled: true}
			} else {
				now := events[0].OccurredAt
				price := domain.DeploymentPriceVersion{ID: "price", DeploymentID: "dep", Version: 1, Revision: 1, BillingMode: domain.BillingModeFree, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1, EffectiveFrom: now.Add(-time.Hour), CreatedBy: "test", CreatedAt: now.Add(-time.Hour), Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted, ReceivedAt: now.Add(-time.Hour), ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Reference: "test", AssertedWithoutArchive: true}}
				var err error
				snapshot, err = domain.NewVersionedPriceSnapshot(price, now)
				if err != nil {
					t.Fatal(err)
				}
				events[1].ReservationMicrosUSD = MicrosUSD(0)
				events[3].CommittedMicrosUSD = MicrosUSD(0)
			}
			events[1].LeaseMode = mode
			events[1].PriceSnapshot = &snapshot
			events[1].TokenGuardPricingViewDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			events[3].LeaseMode = mode
			events[3].PriceSnapshot = &snapshot
			for i, e := range events {
				r := projectionTestRecord(uint64(i+1), e)
				if err := state.Apply(r); err != nil {
					t.Fatal(err)
				}
				if err := p.Apply(ctx, r); err != nil {
					t.Fatal(err)
				}
				got, err := p.ReadPendingLeaseStats(ctx, e.OccurredAt.Add(time.Hour))
				want, stateErr := state.ReadPendingLeaseStats(ctx, e.OccurredAt.Add(time.Hour))
				if err != nil || stateErr != nil || got != want {
					t.Fatal("free/unknown lease lost pending observation", got, want, err, stateErr)
				}
			}
			key := BalanceKey{"prj_1", events[0].PeriodID, events[0].PeriodTimezoneVersion}
			actual, err := p.ReadBalance(ctx, key)
			if err != nil || actual != state.Balance(key.ProjectID, key.PeriodID, key.TimezoneVersion) {
				t.Fatal(actual, err)
			}
			actualSettlement, found, err := p.ReadSettlement(ctx, events[3].AttemptID)
			want, _ := state.SettledAttempt(events[3].AttemptID)
			if err != nil || !found || !reflect.DeepEqual(actualSettlement, want) {
				t.Fatal(actualSettlement, found, err)
			}
		})
	}
}

func TestLedgerProjectionBatchBoundsAndCancellationBeforeCommit(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	events := projectionTestLifecycle(1, false)
	records := make([]Record, projectionBatchRows+1)
	for i := range records {
		e := events[0]
		e.EventID = fmt.Sprint("bound-", i)
		e.RequestID = fmt.Sprint("bound-", i)
		records[i] = projectionTestRecord(uint64(i+1), e)
	}
	if err := p.ApplyBatch(ctx, records); !errors.Is(err, ErrProjectionPageLimit) {
		t.Fatal(err)
	}
	// Each event is legal in size but the aggregate batch exceeds its ceiling.
	for i := range records[:projectionBatchRows] {
		records[i].Event.RequestedModel = string(bytes.Repeat([]byte{'x'}, 150<<10))
	}
	if err := p.ApplyBatch(ctx, records[:projectionBatchRows]); !errors.Is(err, ErrProjectionPageLimit) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.ApplyBatch(canceled, records[:1]); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	position, _, err := p.ReadPosition(ctx)
	if err != nil || position != (Watermark{}) {
		t.Fatal(position, err)
	}
}

func TestLedgerProjectionUnrelatedAttemptFieldCannotMoveSettlementPrefix(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	seq := uint64(0)
	for i := 1; i <= 2; i++ {
		seq = projectionApplyEvents(t, p, projectionTestLifecycle(i, false)[:4], seq)
	}
	page, err := p.SettlementPage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || len(page.Items) != 1 || page.Done {
		t.Fatal(page, err)
	}
	finalized := projectionTestLifecycle(2, false)[4]
	finalized.AttemptID = "att_000002"
	seq = projectionApplyEvents(t, p, []Event{finalized}, seq)
	page, err = p.SettlementPage(ctx, page.Next, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
	if err != nil || len(page.Items) != 1 || page.Items[0].Settlement.AttemptID != "att_000002" {
		t.Fatalf("old settlement disappeared after unrelated finalization: %#v %v", page, err)
	}
	// Event.Validate also allows an accepted request to carry an unrelated
	// AttemptID. Its request event must not load or mutate that attempt's state.
	seq = projectionApplyEvents(t, p, projectionTestLifecycle(3, false)[:2], seq)
	accepted := projectionTestLifecycle(4, false)[0]
	accepted.AttemptID = "att_000003"
	seq = projectionApplyEvents(t, p, []Event{accepted}, seq)
	pending, err := p.PendingLeasePage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 2, Bytes: 1 << 20})
	if err != nil || len(pending.Items) != 1 || pending.Items[0].Reservation.RequestID != "req_000003" {
		t.Fatal(pending, err)
	}
}

func TestLedgerProjectionPreservesWideNativeIdentitiesAndScopedAssociations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	state := NewState()
	// Each identifier is legal in the authoritative WAL, yet wider than the
	// storage library's individual key limit. Two association components are
	// independently wide; reducing only EventID would not fix this history.
	requestID := string(bytes.Repeat([]byte{'r'}, bbolt.MaxKeySize+1))
	projectID := string(bytes.Repeat([]byte{'p'}, bbolt.MaxKeySize+1))
	attempts := []string{string(bytes.Repeat([]byte{'a'}, bbolt.MaxKeySize+1)), string(bytes.Repeat([]byte{'b'}, bbolt.MaxKeySize+1))}
	events := projectionTestLifecycle(1, false)
	events[0].EventID = string(bytes.Repeat([]byte{'e'}, bbolt.MaxKeySize+1))
	appendEvent := func(e Event) {
		e.RequestID, e.ProjectID = requestID, projectID
		record, _, err := log.AppendRecordWithReceipt(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		if err := state.Apply(record); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(events[0])
	for i, id := range attempts {
		for j, e := range events[1:4] {
			if i == 1 && j > 0 {
				break
			}
			e.AttemptID = id
			e.EventID = fmt.Sprintf("wide_%d_%d", i, j)
			appendEvent(e)
		}
	}
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	p, err := BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "projection.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got, found, err := p.ReadRequest(ctx, requestID); err != nil || !found || got.Accepted.Event.EventID != events[0].EventID {
		t.Fatal(found, err)
	}
	if _, found, err := p.ReadEventPosition(ctx, events[0].EventID); err != nil || !found {
		t.Fatal(found, err)
	}
	for _, id := range attempts {
		got, found, err := p.ReadLease(ctx, id)
		want, exists := state.AccountingLease(id)
		if err != nil || found != exists || !reflect.DeepEqual(got, want) {
			t.Fatal("wide lease differs", found, err)
		}
	}
	settled, found, err := p.ReadSettlement(ctx, attempts[0])
	want, wantFound := state.SettledAttempt(attempts[0])
	if err != nil || found != wantFound || !reflect.DeepEqual(settled, want) {
		t.Fatal(found, err)
	}
	page, err := p.RequestAttemptPage(ctx, requestID, ProjectionCursor{}, ProjectionPageLimits{Rows: 1, Bytes: 8 << 20})
	if err != nil || len(page.Items) != 1 || page.Done {
		t.Fatal("first scoped page", err)
	}
	page, err = p.RequestAttemptPage(ctx, requestID, page.Next, ProjectionPageLimits{Rows: 1, Bytes: 8 << 20})
	if err != nil || len(page.Items) != 1 || !page.Done {
		t.Fatal("second scoped page", err)
	}
	pending, err := p.PendingLeasePage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 1, Bytes: 8 << 20})
	if err != nil || len(pending.Items) != 1 || pending.Items[0].Reservation.AttemptID != attempts[1] {
		t.Fatal("wide pending identity lost", err)
	}
	history, err := p.SettlementPage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 1, Bytes: 8 << 20})
	if err != nil || len(history.Items) != 1 || history.Items[0].Settlement.AttemptID != attempts[0] {
		t.Fatal("wide settlement identity lost", err)
	}
	balance, err := p.ReadBalance(ctx, BalanceKey{ProjectID: projectID, PeriodID: events[0].PeriodID, TimezoneVersion: events[0].PeriodTimezoneVersion})
	if err != nil || balance != state.Balance(projectID, events[0].PeriodID, events[0].PeriodTimezoneVersion) {
		t.Fatal("wide balance differs", balance, err)
	}
	if err := p.db.View(func(tx *bbolt.Tx) error {
		for _, name := range projectionBuckets {
			if err := tx.Bucket([]byte(name)).ForEach(func(k, v []byte) error {
				if len(k) > 128 {
					return fmt.Errorf("unbounded %s key: %d", name, len(k))
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
