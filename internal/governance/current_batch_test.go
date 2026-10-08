package governance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func currentRef(e Event) CurrentOutcomeReference {
	return CurrentOutcomeReference{ProjectID: e.ProjectID, WorkUnitID: e.WorkUnitID, DefinitionID: e.DefinitionID, DefinitionVersion: e.DefinitionVersion}
}

func appendCurrentBoth(t testing.TB, old *Manager, p *GovernanceProjection, e Event) {
	t.Helper()
	r, err := old.log.Append(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.state.Apply(r); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentOutcomeBatchFreezesRevisionAcrossIndependentWrites(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	p := buildTestGovernanceProjection(t, log)
	disk := projectedTestManager(t, old, p)
	ctx := context.Background()
	emptyState, err := old.CaptureOutcomeFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	emptyDisk, err := disk.CaptureOutcomeFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := projectionEvent(1)
	ref := currentRef(e)
	appendCurrentBoth(t, old, p, e)
	firstState, _ := old.CaptureOutcomeFence(ctx)
	firstDisk, _ := disk.CaptureOutcomeFence(ctx)
	second := e
	second.EventID = "gov_revision"
	second.OutcomeID = "out_revision"
	second.Revision = 2
	second.SupersedesOutcomeID = e.OutcomeID
	second.Value = "rejected"
	second.IdempotencyKeyHash = shaLabel("revision")
	second.RequestFingerprint = shaLabel("revision-fp")
	appendCurrentBoth(t, old, p, second)
	version := e
	version.EventID = "gov_version"
	version.OutcomeID = "out_version"
	version.DefinitionVersion = 2
	version.Value = "rejected"
	version.IdempotencyKeyHash = shaLabel("version")
	version.RequestFingerprint = shaLabel("version-fp")
	appendCurrentBoth(t, old, p, version)
	refs := []CurrentOutcomeReference{ref, ref, currentRef(version), {ProjectID: "other", WorkUnitID: ref.WorkUnitID, DefinitionID: ref.DefinitionID, DefinitionVersion: 1}, {ProjectID: ref.ProjectID, WorkUnitID: "wku_absent", DefinitionID: ref.DefinitionID, DefinitionVersion: 1}}
	for _, which := range []struct {
		m            *Manager
		empty, first OutcomeFence
	}{{old, emptyState, firstState}, {disk, emptyDisk, firstDisk}} {
		rows, err := which.m.ReadCurrentOutcomeBatch(ctx, which.empty, refs)
		if err != nil || len(rows) != len(refs) {
			t.Fatal(rows, err)
		}
		for _, row := range rows {
			if row.Found || row.Value != "" {
				t.Fatal("post-fence fact appeared", row)
			}
		}
		rows, err = which.m.ReadCurrentOutcomeBatch(ctx, which.first, refs)
		if err != nil || len(rows) != len(refs) || !rows[0].Found || rows[0].Value != "accepted" || rows[0] != rows[1] || rows[2].Found || rows[3].Found || rows[4].Found {
			t.Fatal(rows, err)
		}
		fresh, err := which.m.CaptureOutcomeFence(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rows, err = which.m.ReadCurrentOutcomeBatch(ctx, fresh, refs)
		if err != nil || rows[0].Value != "rejected" || rows[2].Value != "rejected" || rows[3].Found || rows[4].Found {
			t.Fatal(rows, err)
		}
		for i, row := range rows {
			if row.Reference != refs[i] {
				t.Fatal("batch reordered references", rows)
			}
		}
	}
}

func TestCurrentOutcomeBatchRejectsAuthorityLimitsAndCanceledOrClosedOwner(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	p := buildTestGovernanceProjection(t, log)
	m := projectedTestManager(t, old, p)
	e := projectionEvent(1)
	appendCurrentBoth(t, old, p, e)
	fence, err := m.CaptureOutcomeFence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ref := currentRef(e)
	for _, mode := range []string{"empty_fence", "tamper", "future", "offset", "new_owner", "refs", "reference", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			f, reader, refs, ctx := fence, m, []CurrentOutcomeReference{ref}, context.Background()
			want := ErrOutcomeFence
			switch mode {
			case "empty_fence":
				f = OutcomeFence{}
			case "tamper":
				f.authentication[0] ^= 1
			case "future":
				f.head.Records++
				f.authentication = outcomeFenceMAC(m.pageKey, f.head)
			case "offset":
				f.head.Bytes++
				f.authentication = outcomeFenceMAC(m.pageKey, f.head)
			case "new_owner":
				reader = projectedTestManager(t, old, p)
			case "refs":
				refs = make([]CurrentOutcomeReference, 65)
				want = ErrOutcomePageLimit
			case "reference":
				refs[0].DefinitionVersion = 0
				want = ErrOutcomePageLimit
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			if rows, err := reader.ReadCurrentOutcomeBatch(ctx, f, refs); !errors.Is(err, want) || len(rows) != 0 {
				t.Fatal(rows, err)
			}
		})
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if rows, err := m.ReadCurrentOutcomeBatch(context.Background(), fence, []CurrentOutcomeReference{ref}); !errors.Is(err, ErrUnavailable) || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if f, err := m.CaptureOutcomeFence(context.Background()); !errors.Is(err, ErrUnavailable) || f != (OutcomeFence{}) {
		t.Fatal(f, err)
	}
}

func TestCurrentOutcomeBatchRevisionGapOrScopeCorruptionDiscardsUsefulRows(t *testing.T) {
	for _, backend := range []string{"state", "disk"} {
		for _, mode := range []string{"missing_revision", "scope", "sequence", "cycle", "raw_size", "bucket"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				old, log, _, _, _ := testManager(t)
				p := buildTestGovernanceProjection(t, log)
				disk := projectedTestManager(t, old, p)
				a, b := projectionEvent(1), projectionEvent(2)
				appendCurrentBoth(t, old, p, a)
				appendCurrentBoth(t, old, p, b)
				m := old
				if backend == "disk" {
					m = disk
				}
				fence, err := m.CaptureOutcomeFence(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				newer := b
				newer.EventID = "gov_new"
				newer.OutcomeID = "out_new"
				newer.Revision = 2
				newer.SupersedesOutcomeID = b.OutcomeID
				newer.IdempotencyKeyHash = shaLabel("new")
				newer.RequestFingerprint = shaLabel("new-fp")
				appendCurrentBoth(t, old, p, newer)
				if backend == "state" {
					old.state.mu.Lock()
					row := old.state.outcomes[newer.OutcomeID]
					switch mode {
					case "missing_revision":
						delete(old.state.outcomes, b.OutcomeID)
					case "scope":
						row.ProjectID = "wrong"
					case "sequence":
						row.GovernanceSequence = 1
					case "cycle":
						row.SupersedesOutcomeID = row.ID
					case "raw_size":
						row.ReporterKeyID = strings.Repeat("x", governanceProjectionMaxRow+1)
					case "bucket":
						old.state.heads[headKey(b.ProjectID, b.WorkUnitID, b.DefinitionID, 1)] = ""
					}
					old.state.outcomes[newer.OutcomeID] = row
					old.state.mu.Unlock()
				} else {
					if err := p.db.Update(func(tx *bbolt.Tx) error {
						if mode == "bucket" {
							return tx.DeleteBucket([]byte("heads"))
						}
						if mode == "missing_revision" {
							return tx.Bucket([]byte("outcomes")).Delete([]byte(b.OutcomeID))
						}
						if mode == "raw_size" {
							return tx.Bucket([]byte("outcomes")).Put([]byte(newer.OutcomeID), []byte(strings.Repeat("x", governanceProjectionMaxRow+1)))
						}
						r, _, err := govGet[Record](tx, "outcomes", []byte(newer.OutcomeID))
						if err != nil {
							return err
						}
						switch mode {
						case "scope":
							r.Event.ProjectID = "wrong"
						case "sequence":
							r.Sequence = 1
						case "cycle":
							r.Event.SupersedesOutcomeID = r.Event.OutcomeID
						}
						return govPut(tx, "outcomes", []byte(newer.OutcomeID), r)
					}); err != nil {
						t.Fatal(err)
					}
				}
				rows, err := m.ReadCurrentOutcomeBatch(context.Background(), fence, []CurrentOutcomeReference{currentRef(a), currentRef(b)})
				if err == nil || len(rows) != 0 {
					t.Fatal("useful prefix or damaged as-of chain credited", rows, err)
				}
			})
		}
	}
}

func TestCurrentOutcomeBatchWideAsOfChainAndSameTransactionDedup(t *testing.T) {
	old, log, _, _, _ := testManager(t)
	p := buildTestGovernanceProjection(t, log)
	disk := projectedTestManager(t, old, p)
	events := make([]Event, 4)
	for i := range events {
		events[i] = projectionEvent(i)
		events[i].ReporterKeyID = strings.Repeat("x", 60<<10)
		appendCurrentBoth(t, old, p, events[i])
	}
	stateFence, _ := old.CaptureOutcomeFence(context.Background())
	diskFence, _ := disk.CaptureOutcomeFence(context.Background())
	for revision := 2; revision <= domain.MaxOutcomeRevisions; revision++ {
		for i := range events {
			previous := events[i]
			e := previous
			e.EventID = fmt.Sprintf("gov_wide_%d_%d", i, revision)
			e.OutcomeID = fmt.Sprintf("out_wide%drev%d", i, revision)
			e.SupersedesOutcomeID = previous.OutcomeID
			e.Revision = uint64(revision)
			e.Value = "rejected"
			e.IdempotencyKeyHash = shaLabel(e.EventID)
			e.RequestFingerprint = shaLabel(e.OutcomeID)
			appendCurrentBoth(t, old, p, e)
			events[i] = e
		}
	}
	refs := make([]CurrentOutcomeReference, 64)
	for i := range refs {
		refs[i] = currentRef(events[i%4])
	}
	for _, which := range []struct {
		m *Manager
		f OutcomeFence
	}{{old, stateFence}, {disk, diskFence}} {
		if rows, err := which.m.ReadCurrentOutcomeBatch(context.Background(), which.f, refs); !errors.Is(err, ErrOutcomePageLimit) || len(rows) != 0 {
			t.Fatal("wide independent chains escaped source bound", len(rows), err)
		}
		for i := range refs {
			refs[i] = currentRef(events[0])
		}
		rows, err := which.m.ReadCurrentOutcomeBatch(context.Background(), which.f, refs)
		if err != nil || len(rows) != 64 || !rows[0].Found || rows[0].Value != "accepted" {
			t.Fatal(rows, err)
		}
		for _, row := range rows {
			if row != rows[0] {
				t.Fatal("same-transaction duplicate changed", row)
			}
		}
		for i := range refs {
			refs[i] = currentRef(events[i%4])
		}
	}
}

func TestCurrentOutcomeFenceSurvivesConcurrentReports(t *testing.T) {
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
			fence, err := m.CaptureOutcomeFence(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			ref := CurrentOutcomeReference{ProjectID: first.ProjectID, WorkUnitID: first.WorkUnitID, DefinitionID: first.DefinitionID, DefinitionVersion: first.DefinitionVersion}
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				previous := first.ID
				for i := 2; i <= 20; i++ {
					v, _, err := m.Report(context.Background(), reportInput("rejected", previous, fmt.Sprintf("concurrent-%d", i)))
					if err != nil {
						t.Error(err)
						return
					}
					previous = v.ID
				}
			}()
			for i := 0; i < 100; i++ {
				rows, err := m.ReadCurrentOutcomeBatch(context.Background(), fence, []CurrentOutcomeReference{ref, ref})
				if err != nil || len(rows) != 2 || rows[0].Value != "accepted" || !reflect.DeepEqual(rows[0], rows[1]) {
					t.Fatal(rows, err)
				}
			}
			wg.Wait()
		})
	}
}

func BenchmarkCurrentOutcomeBatchHistory(b *testing.B) {
	for _, count := range []int{128, 8192} {
		for _, backend := range []string{"legacy_snapshot_map", "state_batch", "disk_batch"} {
			b.Run(fmt.Sprintf("%s/%d", backend, count), func(b *testing.B) {
				key := make([]byte, 32)
				key[0] = 51
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
				ctx, cut := context.Background(), log.Summary()
				refs := make([]CurrentOutcomeReference, 64)
				for i := range refs {
					refs[i] = currentRef(projectionEvent(i % 8))
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var result []CurrentOutcomeObservation
					var err error
					if backend == "legacy_snapshot_map" {
						rows, readErr := state.OutcomesContext(ctx, "prj_1", "", "odef_result")
						if readErr != nil {
							b.Fatal(readErr)
						}
						current := make(map[string]domain.Outcome)
						for _, row := range rows {
							if row.DefinitionVersion != 1 {
								continue
							}
							if previous, ok := current[row.WorkUnitID]; !ok || row.GovernanceSequence > previous.GovernanceSequence {
								current[row.WorkUnitID] = row
							}
						}
						result = make([]CurrentOutcomeObservation, 0, len(refs))
						for _, ref := range refs {
							row, found := current[ref.WorkUnitID]
							result = append(result, CurrentOutcomeObservation{Reference: ref, Found: found, Value: row.Value})
						}
					} else if backend == "state_batch" {
						result, err = state.readCurrentBatch(ctx, cut, refs)
					} else {
						result, err = p.readCurrentBatch(ctx, cut, refs)
					}
					if err != nil || len(result) != 64 {
						b.Fatal(result, err)
					}
					for j, row := range result {
						if row.Reference != refs[j] || !row.Found || row.Value != "accepted" {
							b.Fatal(row)
						}
					}
				}
			})
		}
	}
}
