package archive

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

var testKey = bytes.Repeat([]byte{0x42}, 32)

func archivedItem(project string, index int) ledger.ArchivedWorkUnit {
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	wu, run := fmt.Sprintf("wku_%02d", index), fmt.Sprintf("run_%02d", index)
	return ledger.ArchivedWorkUnit{
		WorkUnit: domain.WorkUnit{ID: wu, ProjectID: project, Status: domain.WorkUnitClosed, CreatedByKeyID: "key_1", CreatedAt: at, ClosedAt: &at, PeriodID: fmt.Sprintf("2026-09-%02d", index), PeriodTimezoneVersion: 1},
		Cost:     ledger.WorkUnitCost{KnownMicrosUSD: int64(index)},
		Runs:     []domain.Run{{ID: run, ProjectID: project, WorkUnitID: wu, Status: domain.RunClosed, CreatedAt: at, ExpiresAt: at.Add(time.Hour)}},
		Idempotency: []ledger.GovernanceIdempotency{{
			ProjectID: project, Operation: "work_unit.create", KeyHash: fmt.Sprintf("sha256:%064x", index), RequestFingerprint: "sha256:f", WorkUnitID: wu,
		}},
		Sequence: uint64(index),
	}
}

func openTest(t *testing.T, path string, key []byte) *Store {
	t.Helper()
	store, _, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestArchiveRoundTripsAndPagesInIDOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	store := openTest(t, path, testKey)
	var items []ledger.ArchivedWorkUnit
	for index := 1; index <= 6; index++ {
		project := "prj_a"
		if index%3 == 0 {
			project = "prj_b"
		}
		items = append(items, archivedItem(project, index))
	}
	if err := store.WriteArchived(items, 40); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteArchived(nil, 30); err != nil {
		t.Fatal(err)
	}
	if through, err := store.ArchivedThrough(); err != nil || through != 40 {
		t.Fatalf("through=%d err=%v (it must not move backwards)", through, err)
	}
	item, found, err := store.ArchivedWorkUnit("wku_02")
	if err != nil || !found || item.Cost.KnownMicrosUSD != 2 || len(item.Runs) != 1 {
		t.Fatalf("work unit %#v found=%t err=%v", item, found, err)
	}
	if run, found, err := store.ArchivedRun("run_03"); err != nil || !found || run.ProjectID != "prj_b" {
		t.Fatalf("run %#v found=%t err=%v", run, found, err)
	}
	if record, found, err := store.ArchivedIdempotency("prj_a", "work_unit.create", fmt.Sprintf("sha256:%064x", 4)); err != nil || !found || record.WorkUnitID != "wku_04" {
		t.Fatalf("idempotency %#v found=%t err=%v", record, found, err)
	}
	if _, found, err := store.ArchivedWorkUnit("wku_99"); err != nil || found {
		t.Fatalf("absent found=%t err=%v", found, err)
	}
	var got []string
	err = store.VisitArchivedWorkUnits(ledger.ArchiveQuery{ProjectID: "prj_a", After: "wku_01"}, func(item ledger.ArchivedWorkUnit) (bool, error) {
		got = append(got, item.WorkUnit.ID)
		return len(got) < 2, nil
	})
	if err != nil || strings.Join(got, ",") != "wku_02,wku_04" {
		t.Fatalf("page %v err=%v", got, err)
	}
	got = nil
	err = store.VisitArchivedWorkUnits(ledger.ArchiveQuery{CohortStart: "2026-09-03", CohortEnd: "2026-09-05"}, func(item ledger.ArchivedWorkUnit) (bool, error) {
		got = append(got, item.WorkUnit.ID)
		return true, nil
	})
	if err != nil || strings.Join(got, ",") != "wku_03,wku_04,wku_05" {
		t.Fatalf("cohort %v err=%v", got, err)
	}
	got = nil
	err = store.VisitArchivedRuns("prj_b", "", func(run domain.Run) (bool, error) {
		got = append(got, run.ID)
		return true, nil
	})
	if err != nil || strings.Join(got, ",") != "run_03,run_06" {
		t.Fatalf("runs %v err=%v", got, err)
	}
	store.Close()

	// Reopened under its own key it keeps everything.
	store = openTest(t, path, testKey)
	if through, err := store.ArchivedThrough(); err != nil || through != 40 {
		t.Fatalf("reopened through=%d err=%v", through, err)
	}
	store.Close()

	// Under another key it is emptied, positions included, so a snapshot
	// that relied on it is refused.
	other, reset, err := Open(path, bytes.Repeat([]byte{0x43}, 32))
	if err != nil || !reset {
		t.Fatalf("reset=%t err=%v", reset, err)
	}
	defer other.Close()
	if through, err := other.ArchivedThrough(); err != nil || through != 0 {
		t.Fatalf("foreign through=%d err=%v", through, err)
	}
	if _, found, err := other.ArchivedWorkUnit("wku_02"); err != nil || found {
		t.Fatalf("foreign found=%t err=%v", found, err)
	}
}

// An edited row fails its read, and an index entry whose row is missing or
// belongs elsewhere fails the walk; neither is skipped.
func TestArchiveRefusesTamperedRowsAndIndexEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	store := openTest(t, path, testKey)
	if err := store.WriteArchived([]ledger.ArchivedWorkUnit{archivedItem("prj_a", 1)}, 10); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		raw := bytes.Clone(tx.Bucket(bucketWorkUnits).Get([]byte("wku_01")))
		raw[5] ^= 1
		if err := tx.Bucket(bucketWorkUnits).Put([]byte("wku_01"), raw); err != nil {
			return err
		}
		return tx.Bucket(bucketRunIndex).Put(joinKey("prj_a", "run_77"), nil)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ArchivedWorkUnit("wku_01"); !errors.Is(err, ErrTampered) {
		t.Fatalf("edited row err=%v", err)
	}
	if err := store.VisitArchivedWorkUnits(ledger.ArchiveQuery{ProjectID: "prj_a"}, func(ledger.ArchivedWorkUnit) (bool, error) { return true, nil }); !errors.Is(err, ErrTampered) {
		t.Fatalf("walk over edited row err=%v", err)
	}
	if err := store.VisitArchivedRuns("prj_a", "", func(domain.Run) (bool, error) { return true, nil }); !errors.Is(err, ErrTampered) {
		t.Fatalf("dangling index entry err=%v", err)
	}
	store.Close()
}
