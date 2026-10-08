package governance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func pageAppend(t testing.TB, m *Manager, id, project, unit, definition string) {
	t.Helper()
	now := time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)
	e := Event{EventID: "event-" + id, ProjectID: project, WorkUnitID: unit, DefinitionID: definition, DefinitionVersion: 1,
		OutcomeID: id, Value: "accepted", ReporterKeyID: "key_1", ObservedAt: now, IngestedAt: now, Revision: 1,
		IdempotencyKeyHash: shaLabel(id), RequestFingerprint: shaLabel("fp-" + id)}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.log.Append(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.state.Apply(r); err != nil {
		t.Fatal(err)
	}
}

func pageQuery() OutcomePageQuery {
	return OutcomePageQuery{ProjectID: "prj_1", Rows: 2, Bytes: OutcomePageMaxBytes}
}

func TestOutcomePageFreezesFactsAcrossClientPagesAndFiltersBeforeLimit(t *testing.T) {
	m, log, _, _, _ := testManager(t)
	defer log.Close()
	for i, id := range []string{"out_f", "out_d", "out_b", "out_e", "out_c", "out_a"} {
		pageAppend(t, m, id, "prj_1", fmt.Sprintf("wku_%d", i), "odef_result")
	}
	pageAppend(t, m, "out_0", "other", "wku_other", "odef_result")
	q, ctx := pageQuery(), context.Background()
	first, err := m.ReadOutcomePage(ctx, q)
	if err != nil || len(first.Items) != 2 || first.Items[0].ID != "out_a" || first.Items[1].ID != "out_b" || first.NextCursor == "" || first.Head != log.Summary() {
		t.Fatal(first, err)
	}
	pageAppend(t, m, "out_aa", "prj_1", "wku_later1", "odef_result")
	pageAppend(t, m, "out_z", "prj_1", "wku_later2", "odef_result")
	seen := []string{first.Items[0].ID, first.Items[1].ID}
	q.Cursor = first.NextCursor
	for q.Cursor != "" {
		page, err := m.ReadOutcomePage(ctx, q)
		if err != nil || page.Head != first.Head || len(page.Items) > q.Rows || page.Bytes > q.Bytes {
			t.Fatal("page mixed prefixes or exceeded bound", page, err)
		}
		for _, row := range page.Items {
			seen = append(seen, row.ID)
		}
		q.Cursor = page.NextCursor
	}
	if !reflect.DeepEqual(seen, []string{"out_a", "out_b", "out_c", "out_d", "out_e", "out_f"}) {
		t.Fatal(seen)
	}
	q.Cursor, q.Rows = "", 200
	fresh, err := m.ReadOutcomePage(ctx, q)
	if err != nil || len(fresh.Items) != 8 || fresh.Head.Records != 9 {
		t.Fatal(fresh, err)
	}
	q.WorkUnitID, q.DefinitionID = "wku_0", "odef_result"
	filtered, err := m.ReadOutcomePage(ctx, q)
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].ID != "out_f" {
		t.Fatal(filtered, err)
	}
}

func TestOutcomeCursorRejectsTamperScopeOwnerAndFuturePrefix(t *testing.T) {
	m, log, _, _, _ := testManager(t)
	defer log.Close()
	for i := 0; i < 3; i++ {
		pageAppend(t, m, fmt.Sprintf("out_%d", i), "prj_1", fmt.Sprintf("wku_%d", i), "odef_result")
	}
	q, ctx := pageQuery(), context.Background()
	first, err := m.ReadOutcomePage(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"project", "unit", "definition", "tamper", "future", "future_offset", "hash", "oversized", "new_owner"} {
		t.Run(mode, func(t *testing.T) {
			query, reader := q, m
			query.Cursor = first.NextCursor
			switch mode {
			case "project":
				query.ProjectID = "other"
			case "unit":
				query.WorkUnitID = "wku_0"
			case "definition":
				query.DefinitionID = "odef_other"
			case "tamper":
				raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(query.Cursor, outcomeCursorPrefix))
				raw[len(raw)-1] ^= 1
				query.Cursor = outcomeCursorPrefix + base64.RawURLEncoding.EncodeToString(raw)
			case "future", "future_offset", "hash":
				head := first.Head
				if mode == "future" {
					head.Records++
				}
				if mode == "future_offset" {
					head.Bytes++
				}
				if mode == "hash" {
					head.LastHash[0]++
				}
				query.Cursor = encodeOutcomeCursor(m.pageKey, outcomeScope(q), head, "out_1")
			case "oversized":
				query.Cursor = strings.Repeat("a", 1000)
			case "new_owner":
				reader = NewManager(log, m.state, m.accounting, m.definitions, m.checkpoint)
			}
			page, err := reader.ReadOutcomePage(ctx, query)
			if !errors.Is(err, ErrOutcomeCursor) || len(page.Items) != 0 || page.NextCursor != "" || page.Head != (Summary{}) {
				t.Fatal(page, err)
			}
		})
	}
}

func TestOutcomePageByteContinuationLegacyCursorAndErrors(t *testing.T) {
	m, log, _, _, _ := testManager(t)
	defer log.Close()
	for i := 0; i < 5; i++ {
		pageAppend(t, m, fmt.Sprintf("out_%d", i), "prj_1", fmt.Sprintf("wku_%d", i), "odef_result")
	}
	row, _ := m.state.Outcome("out_0")
	raw, _ := json.Marshal(row)
	q := pageQuery()
	q.Rows, q.Bytes = 200, 2*len(raw)
	var ids []string
	for {
		page, err := m.ReadOutcomePage(context.Background(), q)
		if err != nil || len(page.Items) != 2 && page.NextCursor != "" || page.Bytes > q.Bytes {
			t.Fatal(page, err)
		}
		for _, row := range page.Items {
			ids = append(ids, row.ID)
		}
		q.Cursor = page.NextCursor
		if q.Cursor == "" {
			break
		}
	}
	if !reflect.DeepEqual(ids, []string{"out_0", "out_1", "out_2", "out_3", "out_4"}) {
		t.Fatal(ids)
	}
	q = pageQuery()
	q.Cursor = base64.RawURLEncoding.EncodeToString([]byte("out_0"))
	page, err := m.ReadOutcomePage(context.Background(), q)
	if err != nil || page.Items[0].ID != "out_1" || !strings.HasPrefix(page.NextCursor, outcomeCursorPrefix) {
		t.Fatal(page, err)
	}
	q = pageQuery()
	q.Bytes = 1
	if page, err := m.ReadOutcomePage(context.Background(), q); !errors.Is(err, ErrOutcomePageLimit) || len(page.Items) != 0 || page.Head != (Summary{}) {
		t.Fatal(page, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if page, err := m.ReadOutcomePage(canceled, pageQuery()); !errors.Is(err, context.Canceled) || page.Head != (Summary{}) {
		t.Fatal(page, err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if page, err := m.ReadOutcomePage(context.Background(), pageQuery()); !errors.Is(err, ErrUnavailable) || page.Head != (Summary{}) {
		t.Fatal(page, err)
	}
}

func TestOutcomePageRejectsCorruptOrUnappliedSource(t *testing.T) {
	for _, mode := range []string{"lag", "wrong_id", "bad_sequence", "invalid_row"} {
		t.Run(mode, func(t *testing.T) {
			m, log, _, _, _ := testManager(t)
			defer log.Close()
			pageAppend(t, m, "out_1", "prj_1", "wku_1", "odef_result")
			m.state.mu.Lock()
			row := m.state.outcomes["out_1"]
			switch mode {
			case "lag":
				m.state.sequence = 0
			case "wrong_id":
				row.ID = "out_other"
			case "bad_sequence":
				row.GovernanceSequence = 0
			case "invalid_row":
				row.Value = "invalid value"
			}
			m.state.outcomes["out_1"] = row
			m.state.mu.Unlock()
			page, err := m.ReadOutcomePage(context.Background(), pageQuery())
			if err == nil || len(page.Items) != 0 || page.Head != (Summary{}) {
				t.Fatal(page, err)
			}
		})
	}
}

func TestOutcomePageIncludesRevisionHistoryInIDOrder(t *testing.T) {
	m, log, _, _, _ := testManager(t)
	defer log.Close()
	first, _, err := m.Report(context.Background(), reportInput("accepted", "", "first-page"))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := m.Report(context.Background(), reportInput("rejected", first.ID, "second-page"))
	if err != nil {
		t.Fatal(err)
	}
	page, err := m.ReadOutcomePage(context.Background(), pageQuery())
	if err != nil || len(page.Items) != 2 {
		t.Fatal(page, err)
	}
	want := []string{first.ID, second.ID}
	sort.Strings(want)
	if page.Items[0].ID != want[0] || page.Items[1].ID != want[1] {
		t.Fatal(page)
	}
}

func BenchmarkOutcomePageHistory(b *testing.B) {
	for _, count := range []int{128, 8192} {
		for _, backend := range []string{"legacy_snapshot", "bounded_page"} {
			b.Run(fmt.Sprintf("%s/%d", backend, count), func(b *testing.B) {
				state := NewState()
				now := time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)
				for i := 0; i < count; i++ {
					id := fmt.Sprintf("out_%08d", i)
					e := Event{EventID: "event-" + id, ProjectID: "prj_1", WorkUnitID: fmt.Sprintf("wku_%08d", i), DefinitionID: "odef_result", DefinitionVersion: 1,
						OutcomeID: id, Value: "accepted", ReporterKeyID: "key_1", ObservedAt: now, IngestedAt: now, Revision: 1,
						IdempotencyKeyHash: shaLabel(id), RequestFingerprint: shaLabel("fp-" + id)}
					if err := state.Apply(Record{Sequence: uint64(i + 1), Event: e}); err != nil {
						b.Fatal(err)
					}
				}
				q := OutcomePageQuery{ProjectID: "prj_1", Rows: 50, Bytes: OutcomePageMaxBytes}
				ctx := context.Background()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if backend == "legacy_snapshot" {
						rows, err := state.OutcomesContext(ctx, q.ProjectID, "", "")
						if err != nil {
							b.Fatal(err)
						}
						sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
						rows = rows[:q.Rows]
						if rows[0].ID != "out_00000000" {
							b.Fatal(rows)
						}
					} else {
						page, _, err := state.readOutcomePage(ctx, q, "", Summary{Records: uint64(count)})
						if err != nil || len(page.Items) != q.Rows || page.Items[0].ID != "out_00000000" {
							b.Fatal(page, err)
						}
					}
				}
			})
		}
	}
}
