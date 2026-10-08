package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func definitionScanFixture(t *testing.T, groups, versions int) (*Store, domain.Project) {
	t.Helper()
	ctx := context.Background()
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	project, err := s.PutProject(ctx, domain.Project{ID: "prj_scan", Name: "scan", Enabled: true}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Real, bounded native transactions seed larger histories without making
	// setup itself use the public write path's O(history) validation each row.
	total := groups * versions
	for start := 0; start < total; start += 64 {
		end := min(total, start+64)
		if err := s.update(func(tx *Tx) error {
			for n := start; n < end; n++ {
				g, v := n/versions, n%versions+1
				row := domain.OutcomeDefinition{ID: fmt.Sprintf("odef_group%04d", g), ProjectID: project.ID, Name: fmt.Sprintf("group_%04d", g), Version: uint64(v), Revision: uint64(v), DataType: domain.OutcomeBoolean, SuccessValues: []string{"true"}, Enabled: v == versions && g%2 == 0 && g < 128, CreatedAt: time.Unix(100, 0).UTC(), CreatedBy: "admin"}
				if err := row.Validate(); err != nil {
					return err
				}
				raw, err := json.Marshal(row)
				if err != nil {
					return err
				}
				key := outcomeDefinitionKey(row.ID, row.Version)
				if err := tx.Bucket(bucketOutcomeDefinitions).Put(key, raw); err != nil {
					return err
				}
				if err := appendOutcomeDefinitionExport(tx, key, raw); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s, project
}

func TestDefinitionScanReducesLatestAcrossPhysicalPagesAndRetainsActiveLimit(t *testing.T) {
	s, p := definitionScanFixture(t, 70, 3)
	defer s.Close()
	ctx := context.Background()
	scan, err := s.scanOutcomeDefinitions(ctx, p.ID, "odef_group0034", nil)
	if err != nil || scan.active != 35 || scan.latest.Version != 3 || !scan.latest.Enabled {
		t.Fatal(scan, err)
	}
	next := scan.latest
	next.Version++
	next.Enabled = false
	stored, err := s.PutOutcomeDefinition(ctx, next, p.Revision, next.Revision, nil)
	if err != nil || stored.Revision != 4 {
		t.Fatal(stored, err)
	}
	scan, err = s.scanOutcomeDefinitions(ctx, p.ID, next.ID, nil)
	if err != nil || scan.active != 34 || scan.latest.Version != 4 || scan.latest.Enabled {
		t.Fatal(scan, err)
	}
	got, err := s.GetOutcomeDefinition(ctx, p.ID, next.ID, 0)
	if err != nil || got.Version != 4 {
		t.Fatal(got, err)
	}
	if _, err := s.GetOutcomeDefinition(ctx, "prj_other", next.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestDefinitionScanValidationReleasesReaderAndRefusesSourceChange(t *testing.T) {
	s, p := definitionScanFixture(t, 70, 2)
	defer s.Close()
	changed := false
	_, err := s.scanOutcomeDefinitions(context.Background(), p.ID, "odef_group0000", func(string) error {
		if changed {
			return nil
		}
		changed = true
		return s.migrate(func(tx *Tx) error {
			return tx.Bucket(bucketMeta).Put([]byte("scan-reader-probe"), bytes.Repeat([]byte("x"), 4<<20))
		})
	})
	if !errors.Is(err, ErrRevisionConflict) || !changed {
		t.Fatal(err)
	}
}

func TestDefinitionScanCommitFenceRejectsChangedSourceAndMergedDefinition(t *testing.T) {
	for _, mode := range []string{"later-tx", "same-tx-head"} {
		t.Run(mode, func(t *testing.T) {
			s, p := definitionScanFixture(t, 2, 2)
			defer s.Close()
			scan, err := s.scanOutcomeDefinitions(context.Background(), p.ID, "odef_group0000", nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "later-tx" {
				if err := s.migrate(func(tx *Tx) error { return tx.Bucket(bucketMeta).Put([]byte("probe"), []byte("changed")) }); err != nil {
					t.Fatal(err)
				}
			}
			err = s.update(func(tx *Tx) error {
				if mode == "same-tx-head" {
					next := scan.latest
					next.Version++
					next.Revision++
					raw, err := json.Marshal(next)
					if err != nil {
						return err
					}
					key := outcomeDefinitionKey(next.ID, next.Version)
					if err := tx.Bucket(bucketOutcomeDefinitions).Put(key, raw); err != nil {
						return err
					}
					if err := appendOutcomeDefinitionExport(tx, key, raw); err != nil {
						return err
					}
				}
				return scan.checkTx(tx)
			})
			if !errors.Is(err, ErrRevisionConflict) {
				t.Fatal(err)
			}
		})
	}
}

func TestDefinitionScanRejectsOffTargetCorruptionAndCancellation(t *testing.T) {
	s, p := definitionScanFixture(t, 70, 2)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	_, err := s.scanOutcomeDefinitions(ctx, p.ID, "odef_group0000", func(string) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.migrate(func(tx *Tx) error {
		return tx.Bucket(bucketOutcomeDefinitions).Put(outcomeDefinitionKey("odef_group0069", 2), []byte("bad json"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOutcomeDefinition(context.Background(), p.ID, "odef_group0000", 0); err == nil {
		t.Fatal("off-target corrupt row ignored")
	}
}

func TestDefinitionScanWriteRejectsInconsistentRevision(t *testing.T) {
	s, p := definitionScanFixture(t, 1, 1)
	defer s.Close()
	ctx := context.Background()
	row, err := s.GetOutcomeDefinition(ctx, p.ID, "odef_group0000", 1)
	if err != nil {
		t.Fatal(err)
	}
	row.Revision = ^uint64(0)
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(func(tx *Tx) error {
		return tx.Bucket(bucketOutcomeDefinitions).Put(outcomeDefinitionKey(row.ID, row.Version), raw)
	}); err != nil {
		t.Fatal(err)
	}
	row.Version++
	if _, err := s.PutOutcomeDefinition(ctx, row, p.Revision, row.Revision, nil); !errors.Is(err, ErrDefinitionExportCorrupt) {
		t.Fatal(err)
	}
}

func TestDefinitionScanGrowthSamples(t *testing.T) {
	if os.Getenv("HALRO_DEFINITION_SCAN_PROFILE") != "1" {
		t.Skip("explicit local capacity profile")
	}
	for _, n := range []int{100, 1000, 10000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s, p := definitionScanFixture(t, n, 1)
			defer s.Close()
			ctx := context.Background()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			started := time.Now()
			scan, err := s.scanOutcomeDefinitions(ctx, p.ID, "odef_group0000", nil)
			elapsed := time.Since(started)
			if err != nil || scan.latest.Version != 1 || scan.active != uint64(min(64, (n+1)/2)) {
				t.Fatal(scan, err)
			}
			var allocated runtime.MemStats
			runtime.ReadMemStats(&allocated)
			runtime.GC()
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(scan)
			next := scan.latest
			next.Version++
			next.Enabled = false
			writeStart := time.Now()
			stored, err := s.PutOutcomeDefinition(ctx, next, p.Revision, next.Revision, nil)
			writeElapsed := time.Since(writeStart)
			if err != nil || stored.Version != 2 || stored.Revision != 2 {
				t.Fatal(stored, err)
			}
			latest, err := s.GetOutcomeDefinition(ctx, p.ID, next.ID, 0)
			if err != nil || latest.Version != 2 {
				t.Fatal(latest, err)
			}
			sample := map[string]any{"groups": n, "history_rows": n, "source_scan_ns": elapsed.Nanoseconds(), "write_ns": writeElapsed.Nanoseconds(), "scan_total_alloc_bytes": allocated.TotalAlloc - before.TotalAlloc, "retained_heap_delta_bytes": int64(after.HeapAlloc) - int64(before.HeapAlloc)}
			raw, err := json.Marshal(sample)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("CAPACITY_SAMPLE " + string(raw))
		})
	}
}

func TestDefinitionScanCloseJoinsActualReductionCallback(t *testing.T) {
	s, p := definitionScanFixture(t, 2, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.scanOutcomeDefinitions(context.Background(), p.ID, "odef_group0000", func(string) error { close(entered); <-release; return nil })
		done <- err
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	<-s.lifetime.ctx.Done()
	select {
	case err := <-closed:
		close(release)
		t.Fatal("closed before reduction exit", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionScanConcurrentCreatesPreserveActiveLimit(t *testing.T) {
	s, p := definitionScanFixture(t, 126, 1)
	defer s.Close()
	ctx := context.Background()
	start := make(chan struct{})
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			<-start
			row := domain.OutcomeDefinition{ID: fmt.Sprintf("odef_parallel%d", i), ProjectID: p.ID, Name: fmt.Sprintf("parallel_%d", i), Version: 1, DataType: domain.OutcomeBoolean, SuccessValues: []string{"true"}, Enabled: true, CreatedAt: time.Unix(100, 0).UTC(), CreatedBy: "admin"}
			_, err := s.PutOutcomeDefinition(ctx, row, p.Revision, 0, nil)
			done <- err
		}(i)
	}
	close(start)
	success := 0
	for range 2 {
		if err := <-done; err == nil {
			success++
		} else if err.Error() != "active outcome definition limit exceeded" {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("wrong number of successful creates", success)
	}
	scan, err := s.scanOutcomeDefinitions(ctx, p.ID, "", nil)
	if err != nil || scan.active != 64 {
		t.Fatal(scan, err)
	}
}

func TestDefinitionExplicitVersionErrorsReturnNoDefinition(t *testing.T) {
	s, p := definitionScanFixture(t, 1, 1)
	defer s.Close()
	ctx := context.Background()
	got, err := s.GetOutcomeDefinition(ctx, "prj_other", "odef_group0000", 1)
	if !errors.Is(err, ErrNotFound) || got.ID != "" {
		t.Fatal("cross-project error retained definition", got, err)
	}
	if err := s.migrate(func(tx *Tx) error {
		return tx.Bucket(bucketOutcomeDefinitions).Put(outcomeDefinitionKey("odef_group0000", 1), []byte(`{"id":"odef_group0000","version":"invalid"}`))
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetOutcomeDefinition(ctx, p.ID, "odef_group0000", 1)
	if err == nil || got.ID != "" {
		t.Fatal("decode error retained partial definition", got, err)
	}
}
