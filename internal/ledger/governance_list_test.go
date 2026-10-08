package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func listTestOptions(rows int) GovernanceListOptions {
	return GovernanceListOptions{Limits: ProjectionPageLimits{Rows: rows, Bytes: GovernanceListMaxBytes}}
}

func collectGovernancePages[T any](t testing.TB, read func(string) (GovernanceListPage[T], error), id func(T) string) []T {
	t.Helper()
	var items []T
	after := ""
	for n := 0; n < 1000; n++ {
		page, err := read(after)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Items {
			if id(row) <= after || (len(items) > 0 && id(row) <= id(items[len(items)-1])) {
				t.Fatal("page repeated or moved backward", after, id(row))
			}
			items = append(items, row)
		}
		if page.NextID == "" {
			return items
		}
		if page.NextID <= after || (len(page.Items) > 0 && page.NextID < id(page.Items[len(page.Items)-1])) {
			t.Fatal("cursor did not advance", after, page.NextID)
		}
		after = page.NextID
	}
	t.Fatal("pagination did not terminate")
	return nil
}

func TestGovernanceListPagesMatchStateHistoryAndScopes(t *testing.T) {
	ctx := context.Background()
	p, state := projectionTestStore(t), NewState()
	now := time.Date(2026, 10, 3, 12, 0, 0, 17, time.UTC)
	projects := []string{"prj_1", "prj_other", "prj_" + strings.Repeat("p", 32769)}
	var seq uint64
	apply := func(e Event) {
		t.Helper()
		seq++
		record := projectionTestRecord(seq, e)
		if err := state.Apply(record); err != nil {
			t.Fatal(err)
		}
		if err := p.Apply(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	for pi, project := range projects {
		for i := 0; i < 6; i++ {
			unit := governanceEvent(fmt.Sprintf("evt_wu_%d_%d", pi, i), EventWorkUnitCreated, now)
			unit.ProjectID, unit.WorkUnitID = project, fmt.Sprintf("wku_%d%02d", pi, i)
			unit.OutcomeDefinitions = []domain.OutcomeDefinitionRef{{ID: "odef_1", Version: 1}}
			apply(unit)
			for j := 0; j < 4; j++ {
				run := governanceEvent(fmt.Sprintf("evt_run_%d_%d_%d", pi, i, j), EventRunCreated, now)
				run.ProjectID, run.WorkUnitID, run.RunID = project, unit.WorkUnitID, fmt.Sprintf("run_%d%02d%d", pi, i, j)
				run.RunBudgetMicrosUSD, run.RunExpiresAt = 100, now.Add(time.Duration(j+1)*time.Hour)
				apply(run)
				if j == 0 {
					closed := governanceEvent("evt_close_"+run.RunID, EventRunClosed, now.Add(time.Minute))
					closed.ProjectID, closed.WorkUnitID, closed.RunID, closed.CloseReason = project, unit.WorkUnitID, run.RunID, "operator"
					apply(closed)
				}
			}
			if i%2 == 0 {
				closed := governanceEvent("evt_close_"+unit.WorkUnitID, EventWorkUnitClosed, now.Add(time.Minute))
				closed.ProjectID, closed.WorkUnitID = project, unit.WorkUnitID
				apply(closed)
			}
		}
	}
	for _, reader := range []AccountingListReader{state, p} {
		for _, project := range append([]string{"", "prj_absent"}, projects...) {
			for _, status := range []domain.WorkUnitStatus{"", domain.WorkUnitOpen, domain.WorkUnitClosed} {
				query := WorkUnitListQuery{GovernanceListOptions: listTestOptions(2), ProjectID: project, Status: status}
				got := collectGovernancePages(t, func(after string) (GovernanceListPage[domain.WorkUnit], error) {
					query.After = after
					return reader.ReadWorkUnitPage(ctx, query)
				}, func(v domain.WorkUnit) string { return v.ID })
				want := slices.DeleteFunc(state.WorkUnits(project), func(v domain.WorkUnit) bool { return status != "" && v.Status != status })
				if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
					t.Fatal("work unit list differs from native state", project, status)
				}
			}
			for _, unit := range []string{"", "wku_001", "wku_absent"} {
				for _, status := range []domain.RunStatus{"", domain.RunActive, domain.RunClosed, domain.RunExpired} {
					query := RunListQuery{GovernanceListOptions: listTestOptions(3), ProjectID: project, WorkUnitID: unit, Status: status, At: now.Add(2 * time.Hour).In(time.FixedZone("offset", 8*3600))}
					got := collectGovernancePages(t, func(after string) (GovernanceListPage[domain.Run], error) {
						query.After = after
						return reader.ReadRunPage(ctx, query)
					}, func(v domain.Run) string { return v.ID })
					want := slices.DeleteFunc(state.Runs(project, unit), func(v domain.Run) bool { return status != "" && domain.EffectiveRunStatus(v, query.At) != status })
					if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
						t.Fatal("run list differs from native state", project, unit, status)
					}
				}
			}
		}
	}
	// Returned pointers and definition slices cannot mutate the State owner.
	page, err := state.ReadWorkUnitPage(ctx, WorkUnitListQuery{GovernanceListOptions: listTestOptions(1)})
	if err != nil {
		t.Fatal(err)
	}
	page.Items[0].OutcomeDefinitions[0].Version = 99
	*page.Items[0].ClosedAt = now
	original, found, err := state.ReadWorkUnit(ctx, page.Items[0].ID)
	if err != nil || !found || original.OutcomeDefinitions[0].Version != 1 || original.ClosedAt.Equal(now) {
		t.Fatal("page returned state-owned memory", original, err)
	}
	for _, project := range projects {
		want, err := state.ReadRunCapacity(ctx, project, "", now.Add(2*time.Hour))
		got, readErr := p.ReadRunCapacity(ctx, project, "", now.Add(2*time.Hour))
		if err != nil || readErr != nil || want != got || got.ActiveRuns != 12 || got.WorkUnitRuns != 0 {
			t.Fatal("project-only capacity differs", want, got, err, readErr)
		}
	}
}

func TestGovernanceListProjectionSparsePagesBoundScanAndDecode(t *testing.T) {
	p := projectionTestStore(t)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	var seq uint64
	for i := 0; i < 90; i++ {
		e := governanceEvent(fmt.Sprintf("evt_wu_%03d", i), EventWorkUnitCreated, now)
		e.WorkUnitID = fmt.Sprintf("wku_%03d", i)
		seq = projectionApplyEvents(t, p, []Event{e}, seq)
	}
	closed := governanceEvent("evt_closed", EventWorkUnitClosed, now.Add(time.Minute))
	closed.WorkUnitID = "wku_089"
	projectionApplyEvents(t, p, []Event{closed}, seq)
	query := WorkUnitListQuery{GovernanceListOptions: listTestOptions(1), Status: domain.WorkUnitClosed}
	page, err := p.ReadWorkUnitPage(context.Background(), query)
	if err != nil || len(page.Items) != 0 || page.NextID != "wku_067" || page.Scanned != 68 {
		t.Fatal("sparse scan is not bounded/resumable", page, err)
	}
	query.After = page.NextID
	page, err = p.ReadWorkUnitPage(context.Background(), query)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "wku_089" || page.NextID != "" {
		t.Fatal("empty continuation lost matching row", page, err)
	}
	first, _, _ := p.ReadWorkUnit(context.Background(), "wku_000")
	raw, _ := json.Marshal(first)
	query = WorkUnitListQuery{GovernanceListOptions: listTestOptions(2), Status: domain.WorkUnitClosed}
	query.Limits.Bytes = len(raw)
	page, err = p.ReadWorkUnitPage(context.Background(), query)
	if err != nil || len(page.Items) != 0 || page.Scanned != 1 || page.NextID != "wku_000" {
		t.Fatal("filtered raw input did not consume decode budget", page, err)
	}
}

func TestGovernanceListByteLimitsAndFailuresDiscardEntirePage(t *testing.T) {
	ctx := context.Background()
	p, state := projectionTestStore(t), NewState()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		e := governanceEvent(fmt.Sprintf("evt_wu_%d", i), EventWorkUnitCreated, now)
		e.WorkUnitID = fmt.Sprintf("wku_%d", i)
		record := projectionTestRecord(uint64(i+1), e)
		if err := state.Apply(record); err != nil {
			t.Fatal(err)
		}
		if err := p.Apply(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	first, _, _ := p.ReadWorkUnit(ctx, "wku_0")
	raw, _ := json.Marshal(first)
	for _, reader := range []AccountingListReader{state, p} {
		q := WorkUnitListQuery{GovernanceListOptions: listTestOptions(2)}
		q.Limits.Bytes = len(raw)
		page, err := reader.ReadWorkUnitPage(ctx, q)
		if err != nil || len(page.Items) != 1 || page.NextID != "wku_0" || page.Bytes != len(raw) {
			t.Fatal("byte-limited page not resumable", page, err)
		}
		q.After = page.NextID
		page, err = reader.ReadWorkUnitPage(ctx, q)
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != "wku_1" {
			t.Fatal(page, err)
		}
		q.After, q.Limits.Bytes = "", len(raw)-1
		page, err = reader.ReadWorkUnitPage(ctx, q)
		if !errors.Is(err, ErrProjectionPageLimit) || len(page.Items) != 0 || page.NextID != "" {
			t.Fatal(page, err)
		}
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("work_units")).Put(projectionIDKey("wku_1"), []byte("invalid JSON"))
	}); err != nil {
		t.Fatal(err)
	}
	q := WorkUnitListQuery{GovernanceListOptions: listTestOptions(1)}
	page, err := p.ReadWorkUnitPage(ctx, q)
	if err != nil || len(page.Items) != 1 || page.NextID != "wku_0" {
		t.Fatal("decoded outside current page", page, err)
	}
	q.Limits.Rows = 2
	page, err = p.ReadWorkUnitPage(ctx, q)
	if !errors.Is(err, ErrProjectionCorrupt) || len(page.Items) != 0 || page.NextID != "" || page.Bytes != 0 {
		t.Fatal("partial page survived failure", page, err)
	}
}

func TestGovernanceListIndexesRollbackAndIdempotency(t *testing.T) {
	p := projectionTestStore(t)
	events := projectionTestAttribution()
	seq := projectionApplyEvents(t, p, events, 0)
	for _, original := range events {
		alias := original
		alias.EventID += "_alias"
		if alias.Kind == EventWorkUnitCreated {
			alias.WorkUnitID = "wku_alias"
		} else {
			alias.RunID = "run_alias"
		}
		seq = projectionApplyEvents(t, p, []Event{alias}, seq)
	}
	unit := events[0]
	unit.EventID, unit.Operation, unit.WorkUnitID = "evt_new", "test.new", "wku_new"
	bad := events[1]
	bad.EventID, bad.Operation, bad.RunID, bad.WorkUnitID = "evt_bad", "test.bad", "run_bad", "wku_missing"
	before, _, _ := p.ReadPosition(context.Background())
	if err := p.ApplyBatch(context.Background(), []Record{projectionTestRecord(seq+1, unit), projectionTestRecord(seq+2, bad)}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if after, _, err := p.ReadPosition(context.Background()); err != nil || after != before {
		t.Fatal("invalid batch advanced position", after, err)
	}
	if err := p.db.View(func(tx *bbolt.Tx) error {
		for _, index := range projectionListBuckets {
			b := tx.Bucket([]byte(index))
			if b.Stats().KeyN != 1 {
				t.Fatal("alias or rolled back index survived", index, b.Stats().KeyN)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGovernanceListRejectsCorruptIndexesAndUnavailableReaders(t *testing.T) {
	for _, test := range []string{"index_value", "wrong_key", "missing_row", "foreign_scope", "empty_scoped_key", "invalid_identity", "missing_bucket"} {
		t.Run(test, func(t *testing.T) {
			p := projectionTestStore(t)
			projectionApplyEvents(t, p, projectionTestAttribution(), 0)
			if err := p.db.Update(func(tx *bbolt.Tx) error {
				b := tx.Bucket([]byte("work_units_list_project"))
				k := projectionListKey("prj_1", "wku_1")
				switch test {
				case "index_value":
					return b.Put(k, []byte("wku_absent"))
				case "wrong_key":
					return b.Put(projectionListKey("prj_1", "wku_0"), []byte("wku_1"))
				case "missing_row":
					return tx.Bucket([]byte("work_units")).Delete(projectionIDKey("wku_1"))
				case "empty_scoped_key":
					return b.Put(projectionTuple("prj_1"), []byte("wku_1"))
				case "missing_bucket":
					return tx.DeleteBucket([]byte("work_units_list_project"))
				default:
					row, _, err := projectionGet[domain.WorkUnit](tx, "work_units", projectionIDKey("wku_1"))
					if err != nil {
						return err
					}
					if test == "foreign_scope" {
						row.ProjectID = "prj_other"
					} else {
						row.ID = "bad"
					}
					raw, err := json.Marshal(row)
					if err != nil {
						return err
					}
					return tx.Bucket([]byte("work_units")).Put(projectionIDKey("wku_1"), raw)
				}
			}); err != nil {
				t.Fatal(err)
			}
			page, err := p.ReadWorkUnitPage(context.Background(), WorkUnitListQuery{GovernanceListOptions: listTestOptions(2), ProjectID: "prj_1"})
			if !errors.Is(err, ErrProjectionCorrupt) || len(page.Items) != 0 || page.NextID != "" {
				t.Fatal(page, err)
			}
		})
	}
	p := projectionTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, reader := range []AccountingListReader{NewState(), p} {
		if _, err := reader.ReadRunPage(ctx, RunListQuery{GovernanceListOptions: listTestOptions(1)}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if _, err := reader.ReadWorkUnitPage(ctx, WorkUnitListQuery{GovernanceListOptions: listTestOptions(1)}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		for _, bad := range []GovernanceListOptions{{}, {Limits: ProjectionPageLimits{Rows: 201, Bytes: 1}}, {Limits: ProjectionPageLimits{Rows: 1, Bytes: GovernanceListMaxBytes + 1}}, {After: "x\x00y", Limits: listTestOptions(1).Limits}, {After: strings.Repeat("x", 257), Limits: listTestOptions(1).Limits}} {
			if _, err := reader.ReadWorkUnitPage(context.Background(), WorkUnitListQuery{GovernanceListOptions: bad}); !errors.Is(err, ErrProjectionPageLimit) {
				t.Fatal(err)
			}
		}
		if _, err := reader.ReadRunPage(context.Background(), RunListQuery{GovernanceListOptions: listTestOptions(1), Status: "bad"}); !errors.Is(err, ErrProjectionPageLimit) {
			t.Fatal(err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for _, reader := range []AccountingListReader{p, (*LedgerProjection)(nil), (*State)(nil)} {
		if _, err := reader.ReadWorkUnitPage(context.Background(), WorkUnitListQuery{GovernanceListOptions: listTestOptions(1)}); !errors.Is(err, ErrProjectionUnavailable) {
			t.Fatal(err)
		}
		if _, err := reader.ReadRunPage(context.Background(), RunListQuery{GovernanceListOptions: listTestOptions(1)}); !errors.Is(err, ErrProjectionUnavailable) {
			t.Fatal(err)
		}
	}
}

func TestGovernanceListRunIndexErrorsDoNotBecomeEmptyHistory(t *testing.T) {
	for _, index := range []string{"runs_list", "runs_list_project", "runs_list_work_unit"} {
		for _, problem := range []string{"missing_row", "foreign_scope", "invalid_status"} {
			t.Run(index+"/"+problem, func(t *testing.T) {
				p := projectionTestStore(t)
				projectionApplyEvents(t, p, projectionTestAttribution(), 0)
				if err := p.db.Update(func(tx *bbolt.Tx) error {
					b := tx.Bucket([]byte("runs"))
					if problem == "missing_row" {
						return b.Delete(projectionIDKey("run_1"))
					}
					row, _, err := projectionGet[domain.Run](tx, "runs", projectionIDKey("run_1"))
					if err != nil {
						return err
					}
					if problem == "invalid_status" {
						row.Status = "bad"
					} else if index == "runs_list_project" {
						row.ProjectID = "prj_other"
					} else {
						row.WorkUnitID = "wku_other"
					}
					if index == "runs_list" && problem == "foreign_scope" {
						row.ID = "run_other"
					}
					raw, err := json.Marshal(row)
					if err != nil {
						return err
					}
					return b.Put(projectionIDKey("run_1"), raw)
				}); err != nil {
					t.Fatal(err)
				}
				q := RunListQuery{GovernanceListOptions: listTestOptions(2), At: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
				if index == "runs_list_project" {
					q.ProjectID = "prj_1"
				}
				if index == "runs_list_work_unit" {
					q.WorkUnitID = "wku_1"
				}
				page, err := p.ReadRunPage(context.Background(), q)
				if !errors.Is(err, ErrProjectionCorrupt) || len(page.Items) != 0 || page.NextID != "" {
					t.Fatal(page, err)
				}
			})
		}
	}
}

func BenchmarkGovernanceListStatePage(b *testing.B) {
	for _, history := range []int{0, 1000, 10000} {
		state := NewState()
		now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
		for i := 0; i < history; i++ {
			id := fmt.Sprintf("wku_%08d", i)
			state.workUnits[id] = domain.WorkUnit{ID: id, ProjectID: "prj_1", Status: domain.WorkUnitOpen, CreatedByKeyID: "key_1", CreatedAt: now, PeriodID: "2026-10-03"}
		}
		b.Run(fmt.Sprintf("page/history_%d", history), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				page, err := state.ReadWorkUnitPage(context.Background(), WorkUnitListQuery{GovernanceListOptions: listTestOptions(10)})
				if err != nil || len(page.Items) > 10 {
					b.Fatal(page, err)
				}
			}
		})
		b.Run(fmt.Sprintf("legacy/history_%d", history), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = state.WorkUnits("prj_1")
			}
		})
	}
}

func BenchmarkGovernanceListProjectionPage(b *testing.B) {
	for _, history := range []int{0, 1000, 10000} {
		p := projectionTestStore(b)
		now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
		// Pre-fill the finite native row shape without timing or native fsync.
		if err := p.db.Update(func(tx *bbolt.Tx) error {
			for i := 0; i < history; i++ {
				id := fmt.Sprintf("wku_%08d", i)
				row := domain.WorkUnit{ID: id, ProjectID: "prj_1", Status: domain.WorkUnitOpen, CreatedByKeyID: "key_1", CreatedAt: now, PeriodID: "2026-10-03"}
				if err := projectionPut(tx, "work_units", projectionIDKey(id), row); err != nil {
					return err
				}
				if err := projectionStoreListIndexes(tx, Event{Kind: EventWorkUnitCreated, ProjectID: "prj_1", WorkUnitID: id}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		for _, filtered := range []bool{false, true} {
			b.Run(fmt.Sprintf("history_%d/filtered_%t", history, filtered), func(b *testing.B) {
				q := WorkUnitListQuery{GovernanceListOptions: listTestOptions(10), ProjectID: "prj_1"}
				if filtered {
					q.Status = domain.WorkUnitClosed
				}
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					page, err := p.ReadWorkUnitPage(context.Background(), q)
					if err != nil || len(page.Items) > 10 || page.Scanned > 104 || page.Bytes > q.Limits.Bytes {
						b.Fatal(page, err)
					}
				}
			})
		}
	}
}

// The raw row limit is applied before JSON decoding, including a wide project.
func TestGovernanceListWideRowRejectsBeforeDecode(t *testing.T) {
	p := projectionTestStore(t)
	e := projectionTestAttribution()[0]
	e.ProjectID = "prj_" + strings.Repeat("p", 32769)
	projectionApplyEvents(t, p, []Event{e}, 0)
	q := WorkUnitListQuery{GovernanceListOptions: listTestOptions(1), ProjectID: e.ProjectID}
	q.Limits.Bytes = 1024
	page, err := p.ReadWorkUnitPage(context.Background(), q)
	if !errors.Is(err, ErrProjectionPageLimit) || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
	q.Limits.Bytes = GovernanceListMaxBytes
	page, err = p.ReadWorkUnitPage(context.Background(), q)
	if err != nil || len(page.Items) != 1 || page.Items[0].ProjectID != e.ProjectID {
		t.Fatal(page, err)
	}
	if err := p.db.View(func(tx *bbolt.Tx) error {
		k, _ := tx.Bucket([]byte("work_units_list_project")).Cursor().First()
		if !bytes.Equal(k, projectionListKey(e.ProjectID, e.WorkUnitID)) || len(k) > 96 {
			t.Fatal("wide scope escaped finite key", len(k))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
