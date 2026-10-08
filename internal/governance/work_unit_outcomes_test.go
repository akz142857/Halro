package governance

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func workUnitOutcomeFixture(i, revision int, previous string) Event {
	e := projectionEvent(i)
	e.EventID = fmt.Sprintf("gov_detail_%d_%d", i, revision)
	e.WorkUnitID = "wku_detail"
	e.DefinitionID = fmt.Sprintf("odef_def%d", i)
	e.OutcomeID = fmt.Sprintf("out_def%drev%02d", i, revision)
	e.Revision = uint64(revision)
	e.SupersedesOutcomeID = previous
	e.IdempotencyKeyHash = shaLabel(e.OutcomeID)
	e.RequestFingerprint = shaLabel("fp-" + e.OutcomeID)
	return e
}

func workUnitOutcomeQuery(count int) WorkUnitOutcomeQuery {
	q := WorkUnitOutcomeQuery{ProjectID: "prj_1", WorkUnitID: "wku_detail"}
	for i := 0; i < count; i++ {
		q.Definitions = append(q.Definitions, domain.OutcomeDefinitionRef{ID: fmt.Sprintf("odef_def%d", i), Version: 1})
	}
	return q
}

func TestWorkUnitOutcomesPreserveCompleteInterleavedWideHistoryAndNativeOrder(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	p := buildTestGovernanceProjection(t, log)
	disk := projectedTestManager(t, old, p)
	previous := make([]string, domain.MaxDefinitionsPerWorkUnit)
	var want []domain.Outcome
	for revision := 1; revision <= domain.MaxOutcomeRevisions; revision++ {
		for i := range previous {
			e := workUnitOutcomeFixture(i, revision, previous[i])
			e.ReporterKeyID = strings.Repeat("x", 60<<10)
			appendCurrentBoth(t, old, p, e)
			previous[i] = e.OutcomeID
			row, _ := old.state.Outcome(e.OutcomeID)
			want = append(want, row)
		}
		// Sequence has legitimate gaps from another WorkUnit; ID ordering of
		// target facts also differs from the legacy native sequence ordering.
		e := projectionEvent(100 + revision)
		appendCurrentBoth(t, old, p, e)
	}
	q, ctx := workUnitOutcomeQuery(domain.MaxDefinitionsPerWorkUnit), context.Background()
	legacy, err := old.ReadOutcomeSnapshot(ctx, q.ProjectID, q.WorkUnitID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []*Manager{old, disk} {
		got, err := m.ReadWorkUnitOutcomes(ctx, q)
		if err != nil || len(got.Outcomes) != WorkUnitOutcomeMaxRows || !reflect.DeepEqual(got.Outcomes, want) || !reflect.DeepEqual(got, legacy) {
			t.Fatal("lost/reordered/cut/malformed detail", len(got.Outcomes), err)
		}
	}
	if disk.State() != nil {
		t.Fatal("disk detail retained full Gov State")
	}
}

func TestWorkUnitOutcomesEmptyDeclaredMissingAndCrossProject(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	p := buildTestGovernanceProjection(t, log)
	disk := projectedTestManager(t, old, p)
	q := workUnitOutcomeQuery(2)
	appendCurrentBoth(t, old, p, workUnitOutcomeFixture(0, 1, ""))
	for _, m := range []*Manager{old, disk} {
		got, err := m.ReadWorkUnitOutcomes(context.Background(), q)
		if err != nil || len(got.Outcomes) != 1 {
			t.Fatal(got, err)
		}
		cross := q
		cross.ProjectID = "other"
		got, err = m.ReadWorkUnitOutcomes(context.Background(), cross)
		if err != nil || len(got.Outcomes) != 0 || got.Sequence != log.Summary().Records {
			t.Fatal(got, err)
		}
		absent := q
		absent.WorkUnitID = "wku_absent"
		absent.Definitions = nil
		got, err = m.ReadWorkUnitOutcomes(context.Background(), absent)
		if err != nil || got.Outcomes == nil || len(got.Outcomes) != 0 {
			t.Fatal(got, err)
		}
	}
}

func TestWorkUnitOutcomesRejectInvalidDeclarationsAndLifecycleWithoutPartialSnapshot(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	p := buildTestGovernanceProjection(t, log)
	disk := projectedTestManager(t, old, p)
	e := workUnitOutcomeFixture(0, 1, "")
	appendCurrentBoth(t, old, p, e)
	for _, m := range []*Manager{old, disk} {
		for _, mode := range []string{"project", "unit", "too_many", "duplicate", "version", "undeclared", "wrong_version", "canceled"} {
			t.Run(fmt.Sprintf("%p/%s", m, mode), func(t *testing.T) {
				q, ctx := workUnitOutcomeQuery(1), context.Background()
				switch mode {
				case "project":
					q.ProjectID = ""
				case "unit":
					q.WorkUnitID = "invalid"
				case "too_many":
					q = workUnitOutcomeQuery(9)
				case "duplicate":
					q.Definitions = append(q.Definitions, q.Definitions[0])
				case "version":
					q.Definitions[0].Version = 0
				case "undeclared":
					q.Definitions = nil
				case "wrong_version":
					q.Definitions[0].Version = 2
				case "canceled":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				got, err := m.ReadWorkUnitOutcomes(ctx, q)
				if !errors.Is(err, ErrUnavailable) || got.Sequence != 0 || got.Offset != 0 || got.Outcomes != nil {
					t.Fatal(got, err)
				}
			})
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := disk.ReadWorkUnitOutcomes(context.Background(), workUnitOutcomeQuery(1)); !errors.Is(err, ErrUnavailable) || got.Outcomes != nil || got.Sequence != 0 {
		t.Fatal(got, err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := old.ReadWorkUnitOutcomes(context.Background(), workUnitOutcomeQuery(1)); !errors.Is(err, ErrUnavailable) || got.Outcomes != nil || got.Sequence != 0 {
		t.Fatal(got, err)
	}
}

func TestWorkUnitOutcomesSourceGapsDuplicatesAndWrongScopesFailCompleteRead(t *testing.T) {
	for _, backend := range []string{"state", "disk"} {
		for _, mode := range []string{"missing_middle", "missing_tail", "missing_definition", "duplicate", "scope", "index_sequence", "raw_size", "missing_bucket", "head_size"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				old, log, _, _, _ := testManager(t)
				p := buildTestGovernanceProjection(t, log)
				disk := projectedTestManager(t, old, p)
				var previous string
				for rev := 1; rev <= 3; rev++ {
					e := workUnitOutcomeFixture(0, rev, previous)
					appendCurrentBoth(t, old, p, e)
					previous = e.OutcomeID
				}
				appendCurrentBoth(t, old, p, workUnitOutcomeFixture(1, 1, ""))
				m := old
				if backend == "disk" {
					m = disk
				}
				if backend == "state" {
					old.state.mu.Lock()
					ids := old.state.byWorkUnit["wku_detail"]
					switch mode {
					case "missing_middle":
						old.state.byWorkUnit["wku_detail"] = append(append([]string(nil), ids[:1]...), ids[2:]...)
					case "missing_tail":
						old.state.byWorkUnit["wku_detail"] = []string{ids[0], ids[1], ids[3]}
					case "missing_definition":
						old.state.byWorkUnit["wku_detail"] = ids[:3]
					case "duplicate":
						old.state.byWorkUnit["wku_detail"] = append(ids, ids[0])
					case "scope":
						row := old.state.outcomes[ids[1]]
						row.WorkUnitID = "wku_wrong"
						old.state.outcomes[ids[1]] = row
					case "index_sequence":
						row := old.state.outcomes[ids[1]]
						row.GovernanceSequence = 0
						old.state.outcomes[ids[1]] = row
					case "raw_size":
						row := old.state.outcomes[ids[1]]
						row.ReporterKeyID = strings.Repeat("x", maxPayloadSize+1)
						old.state.outcomes[ids[1]] = row
					case "missing_bucket":
						delete(old.state.outcomes, ids[1])
					case "head_size":
						old.state.heads[headKey("prj_1", "wku_detail", "odef_def0", 1)] = strings.Repeat("x", 1000)
					}
					old.state.mu.Unlock()
				} else {
					if err := p.db.Update(func(tx *bbolt.Tx) error {
						index := tx.Bucket([]byte("by_work_unit"))
						key := func(id string) []byte { return append(govKey("wku_detail"), id...) }
						switch mode {
						case "missing_middle":
							return index.Delete(key("out_def0rev02"))
						case "missing_tail":
							return index.Delete(key("out_def0rev03"))
						case "missing_definition":
							return index.Delete(key("out_def1rev01"))
						case "duplicate":
							return index.Put(key("out_dup"), binary.BigEndian.AppendUint64(nil, 1))
						case "index_sequence":
							return index.Put(key("out_def0rev02"), binary.BigEndian.AppendUint64(nil, 99))
						case "raw_size":
							return tx.Bucket([]byte("outcomes")).Put([]byte("out_def0rev02"), []byte(strings.Repeat("x", governanceProjectionMaxRow+1)))
						case "missing_bucket":
							return tx.DeleteBucket([]byte("heads"))
						case "head_size":
							return tx.Bucket([]byte("heads")).Put(govHeadKey("prj_1", "wku_detail", "odef_def0", 1), []byte(strings.Repeat("x", 1000)))
						case "scope":
							r, _, err := govGet[Record](tx, "outcomes", []byte("out_def0rev02"))
							if err != nil {
								return err
							}
							r.Event.WorkUnitID = "wku_wrong"
							return govPut(tx, "outcomes", []byte("out_def0rev02"), r)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				got, err := m.ReadWorkUnitOutcomes(context.Background(), workUnitOutcomeQuery(2))
				if err == nil || !errors.Is(err, ErrUnavailable) || got.Outcomes != nil || got.Sequence != 0 || got.Offset != 0 {
					t.Fatal("shortened or damaged detail published", len(got.Outcomes), err)
				}
			})
		}
	}
}

func TestWorkUnitOutcomesRejectPhysicalRowOverflowInsteadOfTruncating(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	p := buildTestGovernanceProjection(t, log)
	disk := projectedTestManager(t, old, p)
	for i := 0; i < 161; i++ {
		e := workUnitOutcomeFixture(i/20, i%20+1, "")
		if e.Revision > 1 {
			e.SupersedesOutcomeID = fmt.Sprintf("out_def%drev%02d", i/20, i%20)
		}
		appendCurrentBoth(t, old, p, e)
	}
	for _, m := range []*Manager{old, disk} {
		got, err := m.ReadWorkUnitOutcomes(context.Background(), workUnitOutcomeQuery(8))
		if !errors.Is(err, ErrWorkUnitOutcomeLimit) || !errors.Is(err, ErrUnavailable) || got.Outcomes != nil || got.Sequence != 0 {
			t.Fatal(got, err)
		}
	}
}

func TestWorkUnitOutcomesConcurrentReportsReturnOneCompleteCut(t *testing.T) {
	for _, backend := range []string{"state", "disk"} {
		t.Run(backend, func(t *testing.T) {
			old, log, _, _, _ := testManager(t)
			first, _, err := old.Report(context.Background(), reportInput("accepted", "", "first"))
			if err != nil {
				t.Fatal(err)
			}
			p := buildTestGovernanceProjection(t, log)
			m := old
			if backend == "disk" {
				m = projectedTestManager(t, old, p)
			}
			q := WorkUnitOutcomeQuery{ProjectID: first.ProjectID, WorkUnitID: first.WorkUnitID, Definitions: []domain.OutcomeDefinitionRef{{ID: first.DefinitionID, Version: first.DefinitionVersion}}}
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				previous := first.ID
				for i := 2; i <= 20; i++ {
					v, _, err := m.Report(context.Background(), reportInput("rejected", previous, fmt.Sprintf("detail-%d", i)))
					if err != nil {
						t.Error(err)
						return
					}
					previous = v.ID
				}
			}()
			for i := 0; i < 100; i++ {
				got, err := m.ReadWorkUnitOutcomes(context.Background(), q)
				if err != nil || len(got.Outcomes) == 0 || uint64(len(got.Outcomes)) != got.Sequence {
					t.Fatal(len(got.Outcomes), got.Sequence, err)
				}
				for j, row := range got.Outcomes {
					if row.Revision != uint64(j+1) || row.GovernanceSequence != uint64(j+1) {
						t.Fatal(got)
					}
				}
			}
			wg.Wait()
		})
	}
}
