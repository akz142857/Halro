package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

// These are real MAC native facts, not synthetic projection rows. Every old
// Run holds a legal 900KiB KeyID. Hydrating all 32 exceeded the transaction's
// cumulative 8MiB read/write limits even though each native frame is legal.
func TestAuthenticatedLedgerWideRunDefinitionsAndLifetimeLimit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "native.wal")
	native := openChained(t, path, nil)
	defer native.Close()
	oracle := NewState()
	appendFact := func(e Event) Record {
		t.Helper()
		r, _, err := native.AppendRecordWithReceipt(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		if err := oracle.Apply(r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	events := projectionTestAttribution()
	events[0].KeyID = strings.Repeat("w", 900<<10)
	events[1].KeyID = strings.Repeat("r", 900<<10)
	appendFact(events[0])
	var last Record
	for i := 1; i <= domain.MaxRunsPerWorkUnit; i++ {
		e := events[1]
		e.RunID = fmt.Sprintf("run_%d", i)
		e.EventID, e.Operation = fmt.Sprint("create-", i), fmt.Sprint("create-", i)
		last = appendFact(e)
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(err, partial)
	}
	options := authenticatedLedgerTestOptions(t)
	p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "auth"), report, options)
	if err != nil {
		t.Fatal("legal wide Run history refused", err)
	}
	t.Cleanup(func() {
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
		if used := options.Budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
			t.Error("resource leak", used)
		}
	})
	check := func() {
		t.Helper()
		report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
		if err != nil || partial {
			t.Fatal(err, partial)
		}
		ref, err := BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "reference.db"), report)
		if err != nil {
			t.Fatal(err)
		}
		defer ref.Discard()
		rows := compareAuthenticatedLedgerRows(t, p, ref)
		// An independent full State oracle checks definitions and lifecycle, rather
		// than relying only on the two stores that share the partial reducer.
		for i := 1; i <= domain.MaxRunsPerWorkUnit; i++ {
			id := fmt.Sprintf("run_%d", i)
			want, found, err := oracle.ReadRun(ctx, id)
			got, actual, readErr := ref.ReadRun(ctx, id)
			if err != nil || readErr != nil || !found || !actual || !reflect.DeepEqual(want, got) {
				t.Fatal(id, err, readErr)
			}
		}
		want, found, err := oracle.ReadWorkUnit(ctx, events[0].WorkUnitID)
		got, actual, readErr := ref.ReadWorkUnit(ctx, events[0].WorkUnitID)
		if err != nil || readErr != nil || !found || !actual || !reflect.DeepEqual(want, got) {
			t.Fatal("work unit differs", err, readErr)
		}
		// New coherent reads must preserve all 32 legal wide definitions,
		// rather than imposing an 8MiB page cap on the complete domain view.
		wantView, wantFound, wantErr := oracle.ReadWorkUnitAccounting(ctx, events[0].ProjectID, events[0].WorkUnitID, true)
		actualView, actualFound, actualErr := p.ReadWorkUnitAccounting(ctx, events[0].ProjectID, events[0].WorkUnitID, true)
		if wantErr != nil || actualErr != nil || wantFound != actualFound || !reflect.DeepEqual(wantView, actualView) {
			t.Fatal("wide coherent view differs", wantErr, actualErr)
		}
		wantOpen, wantErr := oracle.ReadOpenWorkUnitCount(ctx, events[0].ProjectID)
		actualOpen, actualErr := p.ReadOpenWorkUnitCount(ctx, events[0].ProjectID)
		if wantErr != nil || actualErr != nil || wantOpen != actualOpen {
			t.Fatal("wide open count differs", wantErr, actualErr)
		}
		wantCapacity, wantErr := oracle.ReadRunCapacity(ctx, events[0].ProjectID, events[0].WorkUnitID, events[0].OccurredAt)
		actualCapacity, actualErr := p.ReadRunCapacity(ctx, events[0].ProjectID, events[0].WorkUnitID, events[0].OccurredAt)
		if wantErr != nil || actualErr != nil || wantCapacity != actualCapacity {
			t.Fatal("wide capacity differs", wantErr, actualErr)
		}
		capacity, err := ref.ReadRunCapacity(ctx, events[0].ProjectID, events[0].WorkUnitID, events[0].OccurredAt)
		if err != nil || capacity.WorkUnitRuns != domain.MaxRunsPerWorkUnit {
			t.Fatal(capacity, err)
		}
		t.Logf("native sequence=%d complete row set=%d lifetime Runs=%d", report.Head.Sequence, rows, capacity.WorkUnitRuns)
	}
	check()
	// Full-capacity aliases and physical repeats have no effect, even when the
	// alias names a missing parent/Run. The original reducer resolves idempotency
	// before object existence. A different WorkUnit has an independent allowance.
	alias := last.Event
	alias.EventID, alias.RunID, alias.WorkUnitID = "full-limit-alias", "run_absent", "wku_absent"
	appendFact(alias)
	appendFact(alias)
	unitAlias := events[0]
	unitAlias.EventID, unitAlias.WorkUnitID = "full-unit-alias", "wku_absent"
	appendFact(unitAlias)
	closeRun := governanceEvent("close-wide", EventRunClosed, last.Event.OccurredAt)
	closeRun.WorkUnitID, closeRun.RunID, closeRun.CloseReason = events[0].WorkUnitID, "run_1", "operator"
	appendFact(closeRun)
	wu := events[0]
	wu.EventID, wu.Operation, wu.WorkUnitID, wu.KeyID = "other-unit", "other-unit", "wku_other", "small-key"
	appendFact(wu)
	other := events[1]
	other.EventID, other.Operation, other.WorkUnitID, other.RunID, other.KeyID = "other-run", "other-run", "wku_other", "run_other", "small-key"
	last = appendFact(other)
	receipt, err := p.AdvanceFromNative(ctx, native, nativeRecordPrefix(last))
	if err != nil || receipt.AppliedRecords != 6 {
		t.Fatal(receipt, err)
	}
	check()
	// Closing one of the original 32 cannot free its lifetime slot. Failed Apply
	// rolls back the candidate transaction and permanently retires its public API.
	extra := events[1]
	extra.EventID, extra.Operation, extra.RunID = "over-limit", "over-limit", "run_33"
	r, _, err := native.AppendRecordWithReceipt(ctx, extra)
	if err != nil {
		t.Fatal(err)
	}
	if err := oracle.Apply(r); err == nil || !strings.Contains(err.Error(), "run limit exceeded") {
		t.Fatal("full State allowed Run 33", err)
	}
	before, err := p.ReadReplayAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = p.AdvanceFromNative(ctx, native, nativeRecordPrefix(r))
	if err == nil || !strings.Contains(err.Error(), "run limit exceeded") || receipt != (AuthenticatedLedgerAdvance{}) {
		t.Fatal(receipt, err)
	}
	if _, err := p.ReadReplayAnchor(ctx); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal("failed candidate remained queryable", err)
	}
	view, err := p.store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	key, err := authenticatedLedgerKey("meta", []byte("position"))
	if err != nil {
		t.Fatal(err)
	}
	raw, found, err := view.Get(ctx, key)
	if err != nil || !found {
		t.Fatal(err, found)
	}
	var saved projectionPosition
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if (ReplayAnchor{Head: saved.Watermark, Hash: saved.Hash, Epoch: saved.Epoch}) != before {
		t.Fatal("rejected Run changed private position")
	}

}

func TestLedgerProjectionRunCounterCorruptionRollsBack(t *testing.T) {
	for _, value := range []string{"missing", "null", "33", "-1", "1.0", "\"1\"", "01", " 1", strings.Repeat("1", 100)} {
		t.Run(value, func(t *testing.T) {
			ctx := context.Background()
			p := projectionTestStore(t)
			events := projectionTestAttribution()
			projectionApplyEvents(t, p, events, 0)
			key := projectionTuple("work_unit_run_count", events[0].WorkUnitID)
			if err := p.db.Update(func(tx *bbolt.Tx) error {
				if value == "missing" {
					return tx.Bucket([]byte("meta")).Delete(key)
				}
				return tx.Bucket([]byte("meta")).Put(key, []byte(value))
			}); err != nil {
				t.Fatal(err)
			}
			e := events[1]
			e.EventID, e.Operation, e.RunID = "next", "next", "run_next"
			before, err := p.ReadReplayAnchor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// A preceding valid fact in the batch must also roll back.
			valid := projectionTestLifecycle(1, false)[0]
			err = p.ApplyBatch(ctx, []Record{projectionTestRecord(3, valid), projectionTestRecord(4, e)})
			if !errors.Is(err, ErrProjectionCorrupt) {
				t.Fatal("bad counter accepted", err)
			}
			after, err := p.ReadReplayAnchor(ctx)
			if err != nil || after != before {
				t.Fatal("bad count advanced batch", before, after, err)
			}
			for _, id := range []string{valid.EventID, e.EventID} {
				if _, found, err := p.ReadEventPosition(ctx, id); err != nil || found {
					t.Fatal("failed batch left event", found, err)
				}
			}
			if _, found, err := p.ReadRun(ctx, e.RunID); err != nil || found {
				t.Fatal("failed batch left Run", found, err)
			}
		})
	}
}

func TestAuthenticatedLedgerRunCounterFailureRetiresCandidate(t *testing.T) {
	for _, value := range []string{"missing", "33"} {
		t.Run(value, func(t *testing.T) {
			ctx := context.Background()
			p, native, _ := authenticatedAdvanceFixture(t, 0)
			before, err := p.ReadReplayAnchor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			key, err := authenticatedLedgerKey("meta", projectionTuple("work_unit_run_count", "wku_1"))
			if err != nil {
				t.Fatal(err)
			}
			// Inject an invalid derived scalar through the test's internal writer. This
			// deliberately bypasses normal native-only construction to exercise the
			// shared reducer's failure behavior after an authenticated tree read.
			_, err = p.store.Update(ctx, sha256.Sum256([]byte("test-counter-corruption-"+value)), func(tx *authenticatedstate.Transaction) error {
				if value == "missing" {
					return tx.Delete(key)
				}
				return tx.Put(key, []byte(value))
			})
			if err != nil {
				t.Fatal(err)
			}
			e := projectionTestAttribution()[1]
			e.EventID, e.Operation, e.RunID = "run-next", "run-next", "run_next"
			r, _, err := native.AppendRecordWithReceipt(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := p.AdvanceFromNative(ctx, native, nativeRecordPrefix(r))
			if !errors.Is(err, ErrProjectionCorrupt) || receipt != (AuthenticatedLedgerAdvance{}) {
				t.Fatal(receipt, err)
			}
			if _, err := p.ReadReplayAnchor(ctx); !errors.Is(err, ErrProjectionUnavailable) {
				t.Fatal("failed candidate still public", err)
			}
			view, err := p.store.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer view.Close()
			positionKey, err := authenticatedLedgerKey("meta", []byte("position"))
			if err != nil {
				t.Fatal(err)
			}
			raw, found, err := view.Get(ctx, positionKey)
			var position projectionPosition
			if err != nil || !found || json.Unmarshal(raw, &position) != nil {
				t.Fatal(found, err)
			}
			if (ReplayAnchor{Head: position.Watermark, Hash: position.Hash, Epoch: position.Epoch}) != before {
				t.Fatal("counter failure changed position")
			}
			for _, item := range []struct{ bucket, id string }{{"runs", e.RunID}, {"events", e.EventID}, {"event_positions", e.EventID}} {
				key, err := authenticatedLedgerKey(item.bucket, projectionIDKey(item.id))
				if err != nil {
					t.Fatal(err)
				}
				if _, found, err := view.Get(ctx, key); err != nil || found {
					t.Fatal("counter failure committed a row", item.bucket, found, err)
				}
			}
			// Only the derived candidate is retired; valid native history remains
			// writable, unlike an actual native authentication failure.
			if _, _, err := native.AppendRecordWithReceipt(ctx, projectionTestLifecycle(2, false)[0]); err != nil {
				t.Fatal("healthy native poisoned", err)
			}
		})
	}
}

func TestLedgerProjectionRunCounterSeedConflictAndBatchRollback(t *testing.T) {
	ctx := context.Background()
	p := projectionTestStore(t)
	events := projectionTestAttribution()
	key := projectionTuple("work_unit_run_count", events[0].WorkUnitID)
	if err := p.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("meta")).Put(key, []byte("7")) }); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, projectionTestRecord(1, events[0])); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal("preexisting count overwritten", err)
	}
	if _, found, err := p.ReadWorkUnit(ctx, events[0].WorkUnitID); err != nil || found {
		t.Fatal(found, err)
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("meta")).Delete(key) }); err != nil {
		t.Fatal(err)
	}
	// The first Run is valid, but another Run with its ID invalidates the entire
	// batch. Neither the scalar nor either definition may survive rollback.
	conflicting := events[1]
	conflicting.EventID, conflicting.Operation = "conflicting", "conflicting"
	err := p.ApplyBatch(ctx, []Record{projectionTestRecord(1, events[0]), projectionTestRecord(2, events[1]), projectionTestRecord(3, conflicting)})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	if err := p.db.View(func(tx *bbolt.Tx) error {
		if raw := tx.Bucket([]byte("meta")).Get(key); raw != nil {
			return errors.New("rolled-back counter retained")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	projectionApplyEvents(t, p, events, 0)
	if err := p.db.View(func(tx *bbolt.Tx) error {
		count, err := projectionRunCount(boltProjectionRows{tx}, events[0].WorkUnitID)
		if err != nil || count != 1 {
			return fmt.Errorf("count after retry: %d %v", count, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
