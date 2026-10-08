package governance

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func projectionEvent(i int) Event {
	now := time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)
	id := fmt.Sprintf("out_%08d", i)
	return Event{EventID: "gov_" + id, ProjectID: "prj_1", WorkUnitID: fmt.Sprintf("wku_%08d", i), DefinitionID: "odef_result", DefinitionVersion: 1,
		OutcomeID: id, Value: "accepted", ReporterKeyID: "key_1", ObservedAt: now, IngestedAt: now, Revision: 1,
		IdempotencyKeyHash: shaLabel(id), RequestFingerprint: shaLabel("fp-" + id)}
}

func buildTestGovernanceProjection(t testing.TB, log *Log) *GovernanceProjection {
	t.Helper()
	p, err := BuildGovernanceProjection(context.Background(), log.path, append([]byte(nil), log.key...), filepath.Join(t.TempDir(), "view.db"), log.Summary())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = log.Close()
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func projectedTestManager(t *testing.T, old *Manager, p *GovernanceProjection) *Manager {
	t.Helper()
	m, err := NewProjectedManager(context.Background(), old.log, p, old.accounting, old.definitions, old.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	m.now = old.now
	if m.State() != nil {
		t.Fatal("projected Manager retained full historical State")
	}
	return m
}

func TestGovernanceProjectionManagerPreservesRevisionsIdempotencyAndNoStateAlias(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	ctx := context.Background()
	firstInput := reportInput("accepted", "", "first")
	first, _, err := old.Report(ctx, firstInput)
	if err != nil {
		t.Fatal(err)
	}
	p := buildTestGovernanceProjection(t, log)
	m := projectedTestManager(t, old, p)
	for _, attempt := range []struct {
		input ReportInput
		want  error
	}{{firstInput, nil}, {reportInput("rejected", "", "first"), ErrIdempotency}, {reportInput("accepted", "out_stale", "stale"), ErrRevisionConflict}} {
		v, replay, err := m.Report(ctx, attempt.input)
		if !errors.Is(err, attempt.want) {
			t.Fatal(v, replay, err, attempt.want)
		}
		if attempt.want == nil && (!replay || v.ID != first.ID || !v.Provisional) {
			t.Fatal(v, replay)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, conflicts := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := m.Report(ctx, reportInput("rejected", first.ID, fmt.Sprintf("concurrent-%d", i)))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else if errors.Is(err, ErrRevisionConflict) {
				conflicts++
			} else {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if successes != 1 || conflicts != 7 || log.Summary().Records != 2 {
		t.Fatal(successes, conflicts, log.Summary())
	}
	for revision := 3; revision <= domain.MaxOutcomeRevisions; revision++ {
		current, found, err := p.ReadCurrent(ctx, "prj_1", "wku_unit", "odef_result", 1)
		if err != nil || !found {
			t.Fatal(current, found, err)
		}
		if _, _, err := m.Report(ctx, reportInput("accepted", current.ID, fmt.Sprintf("revision-%d", revision))); err != nil {
			t.Fatal(err)
		}
	}
	current, _, _ := p.ReadCurrent(ctx, "prj_1", "wku_unit", "odef_result", 1)
	if _, _, err := m.Report(ctx, reportInput("accepted", current.ID, "limit")); !errors.Is(err, ErrRevisionLimit) {
		t.Fatal(err)
	}
	state := NewState()
	if _, err := log.Replay(state.Apply); err != nil {
		t.Fatal(err)
	}
	for _, rec := range state.Snapshot().Records {
		got, found, err := p.ReadOutcome(ctx, rec.Outcome.ID)
		if err != nil || !found || !reflect.DeepEqual(got, rec.Outcome) {
			t.Fatal(got, rec, found, err)
		}
		want, _ := state.Idempotency(rec.Outcome.ProjectID, rec.IdempotencyKeyHash)
		gotIdem, found, err := p.readIdempotency(ctx, rec.Outcome.ProjectID, rec.IdempotencyKeyHash)
		if err != nil || !found || gotIdem != want {
			t.Fatal(gotIdem, want, found, err)
		}
	}
	head, err := p.ReadHead(ctx)
	if err != nil || head != log.Summary() {
		t.Fatal(head, err)
	}
	if _, err := m.ReadOutcomeSnapshot(ctx, "", "", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatal("disk path silently materialized a full snapshot", err)
	}
}

func TestGovernanceProjectionPagesBoundFilteredWorkAndFreezeNativeFacts(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	for start := 0; start < 450; start += 50 {
		batch := make([]Event, 0, 50)
		for i := start; i < start+50; i++ {
			e := projectionEvent(i)
			e.WorkUnitID = "wku_shared"
			e.DefinitionVersion = uint64(i + 1)
			if i < 440 {
				e.ProjectID = "prj_noise"
			}
			if i%2 == 0 {
				e.DefinitionID = "odef_other"
			}
			batch = append(batch, e)
		}
		if _, err := log.AppendBatch(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
	}
	p := buildTestGovernanceProjection(t, log)
	m := projectedTestManager(t, old, p)
	ctx := context.Background()
	for _, filter := range []OutcomePageQuery{{}, {ProjectID: "prj_1"}, {WorkUnitID: "wku_shared"}, {DefinitionID: "odef_result"}, {ProjectID: "prj_1", WorkUnitID: "wku_shared", DefinitionID: "odef_result"}, {ProjectID: "absent", WorkUnitID: "wku_shared"}} {
		q := filter
		q.Rows = 17
		q.Bytes = OutcomePageMaxBytes
		var ids []string
		emptyContinuations := 0
		for calls := 0; ; calls++ {
			if calls > 500 {
				t.Fatal("cursor failed to progress", q)
			}
			page, err := m.ReadOutcomePage(ctx, q)
			if err != nil || len(page.Items) > q.Rows || page.Bytes > q.Bytes {
				t.Fatal(page, err)
			}
			if len(page.Items) == 0 && page.NextCursor != "" {
				emptyContinuations++
			}
			for _, row := range page.Items {
				ids = append(ids, row.ID)
			}
			if page.NextCursor == "" {
				break
			}
			if page.NextCursor == q.Cursor {
				t.Fatal("nonadvancing cursor")
			}
			q.Cursor = page.NextCursor
		}
		var want []string
		if _, err := log.Replay(func(r Record) error {
			e := r.Event
			if (filter.ProjectID == "" || e.ProjectID == filter.ProjectID) && (filter.WorkUnitID == "" || e.WorkUnitID == filter.WorkUnitID) && (filter.DefinitionID == "" || e.DefinitionID == filter.DefinitionID) {
				want = append(want, e.OutcomeID)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sort.Strings(want)
		if !reflect.DeepEqual(ids, want) {
			t.Fatal(filter, ids, want)
		}
		if filter.ProjectID == "absent" && emptyContinuations != 2 {
			t.Fatal("filtered rows escaped physical page bound", emptyContinuations)
		}
	}
	q := OutcomePageQuery{ProjectID: "prj_1", Rows: 2, Bytes: OutcomePageMaxBytes}
	first, err := m.ReadOutcomePage(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{999, 1000} {
		e := projectionEvent(i)
		if i == 999 {
			e.OutcomeID = "out_00000000a"
		}
		r, err := log.Append(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	ids := []string{first.Items[0].ID, first.Items[1].ID}
	q.Cursor = first.NextCursor
	for q.Cursor != "" {
		page, err := m.ReadOutcomePage(ctx, q)
		if err != nil || page.Head != first.Head {
			t.Fatal(page, err)
		}
		for _, row := range page.Items {
			ids = append(ids, row.ID)
		}
		q.Cursor = page.NextCursor
	}
	if len(ids) != 10 {
		t.Fatal("new native facts leaked into old cut", ids)
	}
	newOwner := projectedTestManager(t, old, p)
	q.Cursor = first.NextCursor
	if page, err := newOwner.ReadOutcomePage(ctx, q); !errors.Is(err, ErrOutcomeCursor) || page.Head != (Summary{}) {
		t.Fatal(page, err)
	}
}

func TestGovernanceProjectionWidePageBytesAndTinyLimit(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	for i := 0; i < 25; i++ {
		e := projectionEvent(i)
		e.ReporterKeyID = strings.Repeat("x", 60<<10)
		if _, err := log.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	p := buildTestGovernanceProjection(t, log)
	m := projectedTestManager(t, old, p)
	q := OutcomePageQuery{Rows: 200, Bytes: OutcomePageMaxBytes}
	first, err := m.ReadOutcomePage(context.Background(), q)
	if err != nil || len(first.Items) == 0 || len(first.Items) >= 25 || first.NextCursor == "" || first.Bytes > q.Bytes {
		t.Fatal(len(first.Items), first.Bytes, err)
	}
	q.Cursor = first.NextCursor
	second, err := m.ReadOutcomePage(context.Background(), q)
	if err != nil || second.NextCursor != "" || len(first.Items)+len(second.Items) != 25 {
		t.Fatal(len(second.Items), err)
	}
	q.Cursor = ""
	q.Bytes = 1
	if page, err := m.ReadOutcomePage(context.Background(), q); !errors.Is(err, ErrOutcomePageLimit) || page.Head != (Summary{}) || len(page.Items) != 0 {
		t.Fatal(page, err)
	}
}

func TestGovernanceProjectionBuildAuthenticatesWholeSourceAndDiscardsFailures(t *testing.T) {
	for _, mode := range []string{"wrong_key", "late_corruption", "partial", "stale_head", "wrong_hash", "domain_invalid", "existing_output", "canceled", "empty"} {
		t.Run(mode, func(t *testing.T) {
			_, log, _, _, _ := testManager(t)
			defer log.Close()
			if mode != "empty" {
				for i := 0; i < 80; i++ {
					if _, err := log.Append(context.Background(), projectionEvent(i)); err != nil {
						t.Fatal(err)
					}
				}
			}
			key := append([]byte(nil), log.key...)
			expected := log.Summary()
			output := filepath.Join(t.TempDir(), "view.db")
			ctx := context.Background()
			switch mode {
			case "wrong_key":
				key[0] ^= 1
			case "late_corruption":
				raw, _ := os.ReadFile(log.path)
				raw[len(raw)-1] ^= 1
				if err := os.WriteFile(log.path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "partial":
				f, err := os.OpenFile(log.path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.Write([]byte("HGOV"))
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
				expected.Bytes += 4
			case "stale_head":
				if _, err := log.Append(ctx, projectionEvent(80)); err != nil {
					t.Fatal(err)
				}
			case "wrong_hash":
				expected.LastHash[0] ^= 1
			case "domain_invalid":
				e := projectionEvent(80)
				e.Revision = 2
				if _, err := log.Append(ctx, e); err != nil {
					t.Fatal(err)
				}
				expected = log.Summary()
			case "existing_output":
				if err := os.WriteFile(output, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			p, err := BuildGovernanceProjection(ctx, log.path, key, output, expected)
			if mode == "empty" {
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				head, err := p.ReadHead(ctx)
				if err != nil || head != (Summary{}) {
					t.Fatal(head, err)
				}
				return
			}
			if err == nil || p != nil {
				t.Fatal("bad candidate published", p, err)
			}
			if mode == "existing_output" {
				raw, _ := os.ReadFile(output)
				if string(raw) != "preserve" {
					t.Fatal("existing file touched")
				}
			} else if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed candidate retained", err)
			}
		})
	}
}

func TestGovernanceProjectionBatchRollsBackDomainRowsIndexesAndHead(t *testing.T) {
	_, log, _, _, _ := testManager(t)
	p := buildTestGovernanceProjection(t, log)
	ctx := context.Background()
	first, bad := projectionEvent(1), projectionEvent(2)
	bad.Revision = 2
	records, err := log.AppendBatch(ctx, []Event{first, bad})
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	err = p.applyRecords(ctx, records)
	p.mu.Unlock()
	if err == nil {
		t.Fatal("invalid batch committed")
	}
	head, err := p.ReadHead(ctx)
	if err != nil || head != (Summary{}) {
		t.Fatal(head, err)
	}
	if row, found, err := p.ReadOutcome(ctx, first.OutcomeID); err != nil || found || row.ID != "" {
		t.Fatal(row, found, err)
	}
	if _, found, err := p.ReadCurrent(ctx, first.ProjectID, first.WorkUnitID, first.DefinitionID, 1); err != nil || found {
		t.Fatal(found, err)
	}
	if _, found, err := p.readIdempotency(ctx, first.ProjectID, first.IdempotencyKeyHash); err != nil || found {
		t.Fatal(found, err)
	}
	if err := p.db.View(func(tx *bbolt.Tx) error {
		for _, name := range governanceProjectionBuckets {
			if name != "meta" && tx.Bucket([]byte(name)).Stats().KeyN != 0 {
				return fmt.Errorf("index retained: %s", name)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGovernanceProjectionCorruptReadsNeverCreditPageOrAppend(t *testing.T) {
	for _, mode := range []string{"bucket", "position", "row_json", "row_size", "row_id", "row_sequence", "index_sequence", "index_scope", "head_scope", "idem_scope"} {
		t.Run(mode, func(t *testing.T) {
			old, log, _, _, _ := testManager(t)
			first, _, err := old.Report(context.Background(), reportInput("accepted", "", "first"))
			if err != nil {
				t.Fatal(err)
			}
			p := buildTestGovernanceProjection(t, log)
			m := projectedTestManager(t, old, p)
			if err := p.db.Update(func(tx *bbolt.Tx) error {
				switch mode {
				case "bucket":
					return tx.DeleteBucket([]byte("heads"))
				case "position":
					return tx.Bucket([]byte("meta")).Put([]byte("position"), []byte("{}"))
				case "row_json":
					return tx.Bucket([]byte("outcomes")).Put([]byte(first.ID), []byte("{"))
				case "row_size":
					return tx.Bucket([]byte("outcomes")).Put([]byte(first.ID), []byte(strings.Repeat("x", governanceProjectionMaxRow+1)))
				case "row_id", "row_sequence":
					var r Record
					if err := json.Unmarshal(tx.Bucket([]byte("outcomes")).Get([]byte(first.ID)), &r); err != nil {
						return err
					}
					if mode == "row_id" {
						r.Event.OutcomeID = "out_wrong"
					} else {
						r.Sequence = 0
					}
					return govPut(tx, "outcomes", []byte(first.ID), r)
				case "index_sequence":
					return tx.Bucket([]byte("by_project")).Put(append(govKey("prj_1"), first.ID...), binary.BigEndian.AppendUint64(nil, 99))
				case "index_scope":
					return tx.Bucket([]byte("by_project")).Put(append(govKey("wrong"), first.ID...), binary.BigEndian.AppendUint64(nil, 1))
				case "head_scope":
					return govPut(tx, "heads", govHeadKey("prj_1", "wku_other", "odef_result", 1), first.ID)
				case "idem_scope":
					return govPut(tx, "idempotency", govKey("wrong", shaLabel("first")), idemRecord{shaLabel("accepted|"), first.ID})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if mode == "head_scope" {
				if v, found, err := p.ReadCurrent(context.Background(), "prj_1", "wku_other", "odef_result", 1); err == nil || found || v.ID != "" {
					t.Fatal(v, found, err)
				}
				return
			}
			if mode == "idem_scope" {
				if v, found, err := p.readIdempotency(context.Background(), "wrong", shaLabel("first")); err == nil || found || v != (idemRecord{}) {
					t.Fatal(v, found, err)
				}
				return
			}
			q := pageQuery()
			if mode == "index_scope" {
				q.ProjectID = "wrong"
			}
			if page, err := m.ReadOutcomePage(context.Background(), q); err == nil || len(page.Items) != 0 || page.Head != (Summary{}) || page.NextCursor != "" {
				t.Fatal(page, err)
			}
			if mode == "index_sequence" || mode == "index_scope" {
				return
			}
			before := log.Summary()
			if v, replay, err := m.Report(context.Background(), reportInput("accepted", "", "first")); err == nil || v.ID != "" || replay || log.Summary() != before {
				t.Fatal("corrupt read credited replay/appended", v, replay, err)
			}
		})
	}
}

func TestGovernanceProjectionCloseJoinsReaderAndRejectsFurtherOperations(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	p := buildTestGovernanceProjection(t, log)
	m := projectedTestManager(t, old, p)
	entered, release, readDone, closeDone := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan error, 1)
	go func() {
		readDone <- p.read(context.Background(), func(*bbolt.Tx, Summary) error { close(entered); <-release; return nil })
	}()
	<-entered
	go func() { closeDone <- p.Close() }()
	select {
	case err := <-closeDone:
		t.Fatal("Close released live transaction", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if page, err := m.ReadOutcomePage(context.Background(), pageQuery()); !errors.Is(err, ErrUnavailable) || page.Head != (Summary{}) {
		t.Fatal(page, err)
	}
	if _, _, err := m.Report(context.Background(), reportInput("accepted", "", "closed")); !errors.Is(err, ErrUnavailable) || log.Summary().Records != 0 {
		t.Fatal(err, log.Summary())
	}
	if ready, err := m.Ready(); ready || !errors.Is(err, ErrUnavailable) {
		t.Fatal(ready, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if head, err := p.ReadHead(ctx); !errors.Is(err, context.Canceled) || head != (Summary{}) {
		t.Fatal(head, err)
	}
}

type cancelGovernanceAnchor struct{ cancel context.CancelFunc }

func (s cancelGovernanceAnchor) PutGovernanceJournalAnchor(uint64, int64, [32]byte, [32]byte) error {
	s.cancel()
	return nil
}

func TestGovernanceProjectionDurableCompletionSurvivesCallerCancellation(t *testing.T) {
	for _, point := range []string{"native_fsync", "anchor"} {
		t.Run(point, func(t *testing.T) {
			old, log, _, _, _ := testManager(t)
			p := buildTestGovernanceProjection(t, log)
			m := projectedTestManager(t, old, p)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if point == "native_fsync" {
				log.afterDurable = func(DurableBatch) error { cancel(); return nil }
			}
			if point == "anchor" {
				m.checkpoint = cancelGovernanceAnchor{cancel}
			}
			input := reportInput("accepted", "", "canceled-durable")
			outcome, _, err := m.Report(ctx, input)
			if !errors.Is(err, context.Canceled) || outcome.ID != "" {
				t.Fatal(outcome, err)
			}
			head, err := p.ReadHead(context.Background())
			if err != nil || head != log.Summary() || head.Records != 1 {
				t.Fatal("durable fact left unapplied", head, err)
			}
			log.afterDurable = nil
			m.checkpoint = old.checkpoint
			outcome, replay, err := m.Report(context.Background(), input)
			if err != nil || !replay || outcome.ID == "" || log.Summary().Records != 1 {
				t.Fatal("cancellation poisoned owner or lost retry", outcome, replay, err)
			}
		})
	}
}

func TestGovernanceProjectionPostDurableApplyFailureRequiresCompleteRebuild(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	p := buildTestGovernanceProjection(t, log)
	m := projectedTestManager(t, old, p)
	log.afterDurable = func(DurableBatch) error {
		return p.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket([]byte("heads")) })
	}
	input := reportInput("accepted", "", "failed-apply")
	if v, _, err := m.Report(context.Background(), input); !errors.Is(err, ErrUnavailable) || v.ID != "" || log.Summary().Records != 1 {
		t.Fatal(v, err, log.Summary())
	}
	log.afterDurable = nil
	for i := 0; i < 2; i++ {
		if v, _, err := m.Report(context.Background(), input); !errors.Is(err, ErrUnavailable) || v.ID != "" || log.Summary().Records != 1 {
			t.Fatal(v, err)
		}
	}
	if page, err := m.ReadOutcomePage(context.Background(), pageQuery()); !errors.Is(err, ErrUnavailable) || page.Head != (Summary{}) {
		t.Fatal(page, err)
	}
	rebuilt := buildTestGovernanceProjection(t, log)
	next := projectedTestManager(t, old, rebuilt)
	if v, replay, err := next.Report(context.Background(), input); err != nil || !replay || v.ID == "" || log.Summary().Records != 1 {
		t.Fatal("full native rebuild lost accepted fact", v, replay, err)
	}
}

func TestGovernanceProjectionUnappliedNativeHeadAndLaterPageCorruptionFailClosed(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	ctx := context.Background()
	if _, err := log.Append(ctx, projectionEvent(1)); err != nil {
		t.Fatal(err)
	}
	p := buildTestGovernanceProjection(t, log)
	m := projectedTestManager(t, old, p)
	r, err := log.Append(ctx, projectionEvent(2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewProjectedManager(ctx, log, p, old.accounting, old.definitions, old.checkpoint); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unapplied native source published", err)
	}
	if page, err := m.ReadOutcomePage(ctx, pageQuery()); !errors.Is(err, ErrUnavailable) || page.Head != (Summary{}) {
		t.Fatal(page, err)
	}
	if err := p.Apply(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("outcomes")).Put([]byte(r.Event.OutcomeID), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	if page, err := m.ReadOutcomePage(ctx, pageQuery()); !errors.Is(err, ErrCorrupt) || page.Head != (Summary{}) || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatal("useful first row leaked after later corruption", page, err)
	}
}

func BenchmarkGovernanceProjectionOutcomePageHistory(b *testing.B) {
	for _, count := range []int{128, 8192} {
		for _, backend := range []string{"state_page", "disk_page"} {
			b.Run(fmt.Sprintf("%s/%d", backend, count), func(b *testing.B) {
				key := make([]byte, 32)
				key[0] = 31
				log, err := Open(filepath.Join(b.TempDir(), "governance.journal"), key)
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { log.Close() })
				state := NewState()
				for start := 0; start < count; start += 64 {
					var events []Event
					for i := start; i < start+64 && i < count; i++ {
						events = append(events, projectionEvent(i))
					}
					records, err := log.AppendBatch(context.Background(), events)
					if err != nil {
						b.Fatal(err)
					}
					for _, r := range records {
						if err := state.Apply(r); err != nil {
							b.Fatal(err)
						}
					}
				}
				p := buildTestGovernanceProjection(b, log)
				ctx, q, head := context.Background(), OutcomePageQuery{ProjectID: "prj_1", Rows: 50, Bytes: OutcomePageMaxBytes}, log.Summary()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var page OutcomePage
					var next string
					var err error
					if backend == "state_page" {
						page, next, err = state.readOutcomePage(ctx, q, "", head)
					} else {
						page, next, err = p.readOutcomePage(ctx, q, "", head)
					}
					if err != nil || len(page.Items) != 50 || page.Items[0].ID != "out_00000000" || page.Head != head || next != "out_00000049" {
						b.Fatal(page, next, err)
					}
				}
			})
		}
	}
}
