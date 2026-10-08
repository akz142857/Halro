package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func TestGovernanceCapacityMatchesAuthenticatedHistoryAndLiveApply(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 7, time.UTC)
	key := bytes.Repeat([]byte{47}, 32)
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	state := NewState()
	var p *LedgerProjection
	appendEvent := func(e Event) {
		t.Helper()
		if _, _, err := log.AppendApplyRecordWithReceipt(ctx, e, func(_ context.Context, record Record) error {
			if err := state.Apply(record); err != nil {
				return err
			}
			if p != nil {
				return p.Apply(ctx, record)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	projects := []string{"prj_1", "prj_other", "prj_" + strings.Repeat("p", 32769)}
	units := make(map[string][]string)
	for pi, project := range projects {
		for i := 0; i < 5; i++ {
			unitID := fmt.Sprintf("wku_%d%d", pi, i)
			units[project] = append(units[project], unitID)
			unit := governanceEvent("evt_"+unitID, EventWorkUnitCreated, now)
			unit.ProjectID, unit.WorkUnitID = project, unitID
			appendEvent(unit)
			for r := 0; r < 7; r++ {
				run := governanceEvent(fmt.Sprintf("evt_run_%d_%d_%d", pi, i, r), EventRunCreated, now)
				run.ProjectID, run.WorkUnitID, run.RunID = project, unitID, fmt.Sprintf("run_%d%d%d", pi, i, r)
				run.RunBudgetMicrosUSD = 100
				run.RunExpiresAt = now.Add(time.Duration(r+1) * time.Hour)
				appendEvent(run)
				if r == 2 {
					closed := governanceEvent("evt_close_"+run.RunID, EventRunClosed, now.Add(time.Minute))
					closed.ProjectID, closed.WorkUnitID, closed.RunID, closed.CloseReason = project, unitID, run.RunID, "operator"
					appendEvent(closed)
				}
			}
			if i%2 == 0 {
				closed := governanceEvent("evt_closed_"+unitID, EventWorkUnitClosed, now.Add(2*time.Minute))
				closed.ProjectID, closed.WorkUnitID = project, unitID
				appendEvent(closed)
			}
		}
		if _, err := log.Roll(); err != nil {
			t.Fatal(err)
		}
	}
	report, partial, err := InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(err, partial)
	}
	p, err = BuildLedgerProjection(ctx, path, key, filepath.Join(t.TempDir(), "projection.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	compare := func() {
		t.Helper()
		for _, project := range append(projects, "prj_absent") {
			want, err := state.ReadOpenWorkUnitCount(ctx, project)
			got, readErr := p.ReadOpenWorkUnitCount(ctx, project)
			if err != nil || readErr != nil || want != got {
				t.Fatal("open counts", want, got, err, readErr)
			}
			for _, unit := range append(units[project], "wku_absent") {
				for _, at := range []time.Time{now, now.Add(time.Hour - time.Nanosecond), now.Add(time.Hour), now.Add(time.Hour + time.Nanosecond), now.Add(10 * time.Hour)} {
					want, err := state.ReadRunCapacity(ctx, project, unit, at)
					got, readErr := p.ReadRunCapacity(ctx, project, unit, at.In(time.FixedZone("offset", 8*3600)))
					if err != nil || readErr != nil || want != got {
						t.Fatal("run counts", want, got, at, err, readErr)
					}
				}
			}
		}
	}
	compare()
	// Closing a work unit does not close its existing runs. Live index removal
	// is driven by each native lifecycle fact independently.
	closeRun := governanceEvent("evt_live_close_run", EventRunClosed, now.Add(time.Minute))
	closeRun.WorkUnitID, closeRun.RunID, closeRun.CloseReason = "wku_01", "run_010", "operator"
	appendEvent(closeRun)
	closeUnit := governanceEvent("evt_live_close_unit", EventWorkUnitClosed, now.Add(2*time.Minute))
	closeUnit.WorkUnitID = "wku_01"
	appendEvent(closeUnit)
	compare()
	// The same authenticated native history is read through finite list pages.
	for _, reader := range []AccountingListReader{state, p} {
		unitsPage := collectGovernancePages(t, func(after string) (GovernanceListPage[domain.WorkUnit], error) {
			options := listTestOptions(2)
			options.After = after
			return reader.ReadWorkUnitPage(ctx, WorkUnitListQuery{GovernanceListOptions: options})
		}, func(v domain.WorkUnit) string { return v.ID })
		runsPage := collectGovernancePages(t, func(after string) (GovernanceListPage[domain.Run], error) {
			options := listTestOptions(3)
			options.After = after
			return reader.ReadRunPage(ctx, RunListQuery{GovernanceListOptions: options, At: now})
		}, func(v domain.Run) string { return v.ID })
		if len(unitsPage) != 15 || len(runsPage) != 105 {
			t.Fatal("authenticated history list lost records", len(unitsPage), len(runsPage))
		}
	}
	if err := p.db.View(func(tx *bbolt.Tx) error {
		for _, name := range []string{"open_work_units_by_project", "active_runs_by_project"} {
			c := tx.Bucket([]byte(name)).Cursor()
			for k, _ := c.First(); k != nil; k, _ = c.Next() {
				if len(k) > 76 {
					t.Fatal("wide native identity escaped fixed key format", len(k))
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGovernanceCapacityIndexesRollbackAndIdempotency(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	events := projectionTestAttribution()
	seq := projectionApplyEvents(t, p, events, 0)
	alias := events[0]
	alias.EventID, alias.WorkUnitID = "evt_alias", "wku_alias"
	seq = projectionApplyEvents(t, p, []Event{alias}, seq)
	if open, err := p.ReadOpenWorkUnitCount(ctx, "prj_1"); err != nil || open != 1 {
		t.Fatal(open, err)
	}
	closeRun := governanceEvent("evt_close_run", EventRunClosed, events[0].OccurredAt.Add(time.Minute))
	closeRun.WorkUnitID, closeRun.RunID, closeRun.CloseReason = "wku_1", "run_1", "operator"
	bad := closeRun
	bad.EventID, bad.Operation = "evt_second_close", "second.close"
	before, _, err := p.ReadPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyBatch(ctx, []Record{projectionTestRecord(seq+1, closeRun), projectionTestRecord(seq+2, bad)}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	capacity, err := p.ReadRunCapacity(ctx, "prj_1", "wku_1", events[0].OccurredAt)
	if err != nil || capacity != (RunCapacity{ActiveRuns: 1, WorkUnitRuns: 1}) {
		t.Fatal("rolled-back close removed admission index", capacity, err)
	}
	after, _, err := p.ReadPosition(ctx)
	if err != nil || before != after {
		t.Fatal("invalid batch advanced position", before, after, err)
	}
}

func TestGovernanceCapacityRejectsIndexErrorsAndUnavailableViews(t *testing.T) {
	for _, index := range []string{"open_work_units_by_project", "active_runs_by_project", "runs_by_work_unit"} {
		t.Run(index, func(t *testing.T) {
			p := projectionTestStore(t)
			events := projectionTestAttribution()
			projectionApplyEvents(t, p, events, 0)
			if err := p.db.Update(func(tx *bbolt.Tx) error {
				b := tx.Bucket([]byte(index))
				k, _ := b.Cursor().First()
				return b.Put(bytes.Clone(k), []byte("absent"))
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			if index == "open_work_units_by_project" {
				var count int64
				count, err = p.ReadOpenWorkUnitCount(context.Background(), "prj_1")
				if count != 0 {
					t.Fatal("error returned capacity", count)
				}
			} else {
				var counts RunCapacity
				counts, err = p.ReadRunCapacity(context.Background(), "prj_1", "wku_1", events[0].OccurredAt)
				if counts != (RunCapacity{}) {
					t.Fatal("error returned capacity", counts)
				}
			}
			if !errors.Is(err, ErrProjectionCorrupt) {
				t.Fatal("bad index was treated as absence", err)
			}
		})
	}
	p := projectionTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.ReadOpenWorkUnitCount(ctx, "prj_1"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := p.ReadRunCapacity(ctx, "prj_1", "wku_1", time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for _, reader := range []*LedgerProjection{p, nil} {
		if _, err := reader.ReadOpenWorkUnitCount(context.Background(), "prj_1"); !errors.Is(err, ErrProjectionUnavailable) {
			t.Fatal(err)
		}
		if _, err := reader.ReadRunCapacity(context.Background(), "prj_1", "wku_1", time.Now()); !errors.Is(err, ErrProjectionUnavailable) {
			t.Fatal(err)
		}
	}
}

func TestGovernanceCapacityExpirySeekCoversNativeTimeRange(t *testing.T) {
	for _, year := range []int{1500, 1700, 2026, 2300, 2500} {
		t.Run(fmt.Sprint(year), func(t *testing.T) {
			p := projectionTestStore(t)
			now := time.Date(year, 10, 3, 12, 0, 0, 17, time.UTC)
			unit := governanceEvent("evt_wu", EventWorkUnitCreated, now)
			unit.WorkUnitID = "wku_1"
			run := governanceEvent("evt_run", EventRunCreated, now)
			run.WorkUnitID, run.RunID, run.RunExpiresAt, run.RunBudgetMicrosUSD = "wku_1", "run_1", now.Add(time.Hour), 1
			projectionApplyEvents(t, p, []Event{unit, run}, 0)
			for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
				got, err := p.ReadRunCapacity(context.Background(), "prj_1", "wku_1", run.RunExpiresAt.Add(offset))
				want := int64(0)
				if offset < 0 {
					want = 1
				}
				if err != nil || got.ActiveRuns != want || got.WorkUnitRuns != 1 {
					t.Fatal(got, err, offset)
				}
			}
		})
	}
}

func TestGovernanceCapacitySeekSkipsExpiredAndOtherProjectPrefixes(t *testing.T) {
	p := projectionTestStore(t)
	ctx := context.Background()
	events := projectionTestAttribution()
	projectionApplyEvents(t, p, events, 0)
	run, found, err := p.ReadRun(ctx, "run_1")
	if err != nil || !found {
		t.Fatal(found, err)
	}
	// Corrupt a reference in the expired part and insert another project's
	// invalid reference. A current admission for a different work unit must
	// neither decode the expired historical suffix nor cross project scope.
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("active_runs_by_project"))
		if err := b.Put(projectionActiveRunKey(run), []byte("absent")); err != nil {
			return err
		}
		other := run
		other.ProjectID, other.ExpiresAt = "prj_other", run.ExpiresAt.Add(time.Hour)
		return b.Put(projectionActiveRunKey(other), []byte("absent"))
	}); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{run.ExpiresAt, run.ExpiresAt.Add(time.Nanosecond)} {
		got, err := p.ReadRunCapacity(ctx, "prj_1", "wku_different", at)
		if err != nil || got != (RunCapacity{}) {
			t.Fatal("current query visited expired/other-project rows", got, err)
		}
	}
	if _, err := p.ReadRunCapacity(ctx, "prj_1", "wku_different", run.CreatedAt); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal("invalid reference inside selected range was ignored", err)
	}
	if _, err := p.ReadRunCapacity(ctx, "prj_other", "wku_different", run.CreatedAt); !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal("other project did not validate its own reference", err)
	}
	// The work-unit lifetime query still visits expired runs when counting its
	// 32-run limit; the malformed active index is not used for that count.
	got, err := p.ReadRunCapacity(ctx, "prj_1", "wku_1", run.ExpiresAt)
	if err != nil || got != (RunCapacity{WorkUnitRuns: 1}) {
		t.Fatal(got, err)
	}
}

func BenchmarkGovernanceCapacityStateReads(b *testing.B) {
	for _, history := range []int{0, 1000, 10000} {
		state := NewState()
		now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
		for i := 0; i < history; i++ {
			id := fmt.Sprint("historical_", i)
			state.workUnits[id] = domain.WorkUnit{ID: id, ProjectID: "prj_1", Status: domain.WorkUnitClosed}
			state.runs[id] = domain.Run{ID: id, ProjectID: "prj_1", WorkUnitID: id, Status: domain.RunClosed}
		}
		state.workUnits["wku_live"] = domain.WorkUnit{ID: "wku_live", ProjectID: "prj_1", Status: domain.WorkUnitOpen}
		state.runs["run_live"] = domain.Run{ID: "run_live", ProjectID: "prj_1", WorkUnitID: "wku_live", Status: domain.RunActive, ExpiresAt: now.Add(time.Hour)}
		b.Run(fmt.Sprintf("count/history=%d", history), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if count, err := state.ReadOpenWorkUnitCount(context.Background(), "prj_1"); err != nil || count != 1 {
					b.Fatal(count, err)
				}
				if count, err := state.ReadRunCapacity(context.Background(), "prj_1", "wku_live", now); err != nil || count != (RunCapacity{1, 1}) {
					b.Fatal(count, err)
				}
			}
		})
		b.Run(fmt.Sprintf("legacy-list/history=%d", history), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if len(state.WorkUnits("prj_1")) != history+1 || len(state.Runs("prj_1", "")) != history+1 {
					b.Fatal("fixture membership changed")
				}
			}
		})
	}
}
