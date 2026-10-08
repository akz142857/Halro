package bolt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func replacementRows(n int) []UsageRollupReplacementRow {
	rows := make([]UsageRollupReplacementRow, n)
	for i := range rows {
		key := totalRollupKey()
		key.Dimension = domain.RollupDimensionRequestedModel
		key.DimensionKey = fmt.Sprint("replacement_", i)
		row := rollupIncrement(int64(i+1), int64(i+10))
		row.FirstSequence = 1
		rows[i] = UsageRollupReplacementRow{key, row}
	}
	return rows
}

func TestUsageRollupReplacementRepairsCorruptDerivativeAndPublishesEmpty(t *testing.T) {
	s, err := openUsageMaintenanceForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.update(func(tx *Tx) error {
		return tx.Bucket(bucketMeta).Put(keyUsageRollupState, []byte(`invalid old derivative`))
	}); err != nil {
		t.Fatal(err)
	}
	head := ledger.Watermark{Generation: 1, Sequence: 4, Offset: 200}
	rows := replacementRows(2)
	total := rollupIncrement(3, 30)
	total.FirstSequence = 1
	rows = append(rows, UsageRollupReplacementRow{totalRollupKey(), total})
	if report, err := s.ReplaceUsageRollups(context.Background(), head, replacementFeed(rows)); err != nil || report.Rows != 3 || report.AccountingDays != 1 {
		t.Fatal(report, err)
	}
	if report, err := s.ReplaceUsageRollups(context.Background(), ledger.Watermark{}, replacementFeed(nil)); err != nil || report != (UsageRollupReplacement{}) || len(replacementVisible(t, s)) != 0 {
		t.Fatal(report, err)
	}
	if _, _, err := s.UsageCheckpoint(); !errors.Is(err, ErrNotFound) {
		t.Fatal("empty rebuild retained old checkpoint", err)
	}
}

func TestUsageRollupReplacementBusyAdmissionWaitsForActualCallback(t *testing.T) {
	s, err := openUsageMaintenanceForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	head := ledger.Watermark{Generation: 1, Sequence: 4, Offset: 200}
	done := make(chan error, 1)
	go func() {
		report, err := s.ReplaceUsageRollups(ctx, head, func(context.Context) ([]UsageRollupReplacementRow, bool, error) {
			close(entered)
			<-release
			return nil, true, nil
		})
		if report != (UsageRollupReplacement{}) {
			done <- errors.New("cancelled job published report")
			return
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not start")
	}
	cancel()
	if _, err := s.ReplaceUsageRollups(context.Background(), head, replacementFeed(nil)); err == nil {
		t.Fatal("retiring physical callback did not retain zero-queue slot")
	}
	select {
	case <-done:
		t.Fatal("cancellation detached physical callback")
	default:
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback join did not finish")
	}
	if _, err := s.ReplaceUsageRollups(context.Background(), head, replacementFeed(replacementRows(1))); err != nil {
		t.Fatal("physical callback did not release slot", err)
	}
}

func replacementFeed(rows []UsageRollupReplacementRow) func(context.Context) ([]UsageRollupReplacementRow, bool, error) {
	return func(context.Context) ([]UsageRollupReplacementRow, bool, error) {
		n := min(100, len(rows))
		part := rows[:n]
		rows = rows[n:]
		return part, len(rows) == 0, nil
	}
}

func replacementVisible(t *testing.T, s *Store) map[domain.RollupKey]domain.DailyRollup {
	t.Helper()
	result := map[domain.RollupKey]domain.DailyRollup{}
	if err := s.UsageRollupRange("", "", func(k domain.RollupKey, r domain.DailyRollup) error { result[k] = r; return nil }); err != nil {
		t.Fatal(err)
	}
	return result
}

func openUsageMaintenanceForTest(t *testing.T, path string) (*Store, error) {
	t.Helper()
	seed, err := openForTest(t, path)
	if err != nil {
		return nil, err
	}
	if err := seed.Close(); err != nil {
		return nil, err
	}
	return OpenPrimary(path)
}

func TestUsageRollupReplacementStagesAtomicallyReopensAndAlternates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	s, err := openUsageMaintenanceForTest(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	head := ledger.Watermark{Generation: 1, Sequence: 4, Offset: 200}
	if err := s.PutUsageCheckpoint(head, []byte(`{"version":15}`), nil, nil, domain.RollupVersion, map[string]domain.DailyRollup{totalRollupKey().Encode(): rollupIncrement(1, 90)}); err != nil {
		t.Fatal(err)
	}
	old := replacementVisible(t, s)
	rows := replacementRows(203)
	source := replacementFeed(rows)
	calls := 0
	report, err := s.ReplaceUsageRollups(context.Background(), head, func(ctx context.Context) ([]UsageRollupReplacementRow, bool, error) {
		calls++
		if !reflect.DeepEqual(replacementVisible(t, s), old) {
			t.Fatal("staging was visible")
		}
		return source(ctx)
	})
	if err != nil || report.Rows != 203 || report.AccountingDays != 0 || calls != 3 {
		t.Fatal(report, calls, err)
	}
	state, err := s.UsageRollupState()
	if err != nil || state.Slot != 1 || state.Watermark != head {
		t.Fatal(state, err)
	}
	want := map[domain.RollupKey]domain.DailyRollup{}
	for _, r := range rows {
		want[r.Key] = r.Row
	}
	if !reflect.DeepEqual(replacementVisible(t, s), want) {
		t.Fatal("complete root differs")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replacementVisible(t, s), want) {
		t.Fatal("reopen selected stale legacy bucket")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err = OpenExistingDerivedMaintenance(path, info)
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range []int{2, 1} {
		rows = rows[:1]
		if report, err := s.ReplaceUsageRollups(context.Background(), head, replacementFeed(rows)); err != nil || report.Rows != 1 {
			t.Fatal(report, err)
		}
		state, err := s.UsageRollupState()
		if err != nil || state.Slot != slot || len(replacementVisible(t, s)) != 1 {
			t.Fatal(state, err)
		}
	}
	// Ordinary increments follow the selected root; a reset withdraws it and
	// returns to the legacy path, leaving at most the same two fixed slots.
	r := rows[0]
	increment := r.Row
	increment.Attempts = 1
	increment.CostMicrosUSD = 2
	head.Sequence++
	head.Offset++
	if err := s.PutUsageCheckpoint(head, []byte(`{"version":15}`), nil, nil, domain.RollupVersion, map[string]domain.DailyRollup{r.Key.Encode(): increment}); err != nil {
		t.Fatal(err)
	}
	state, err = s.UsageRollupState()
	if err != nil || state.Slot != 1 {
		t.Fatal(state, err)
	}
	if replacementVisible(t, s)[r.Key].Attempts != r.Row.Attempts+1 {
		t.Fatal("increment ignored selected slot")
	}
	if err := s.ResetUsageDerivatives(); err != nil {
		t.Fatal(err)
	}
	if len(replacementVisible(t, s)) != 0 {
		t.Fatal("reset retained selected rows")
	}
}

func TestUsageRollupReplacementFailureWithdrawsPublicationAndRetries(t *testing.T) {
	for _, mode := range []string{"source", "cancel", "duplicate", "oversize", "changed-root"} {
		t.Run(mode, func(t *testing.T) {
			s, err := openUsageMaintenanceForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			head := ledger.Watermark{Generation: 1, Sequence: 4, Offset: 200}
			old := replacementVisible(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			report, err := s.ReplaceUsageRollups(ctx, head, func(context.Context) ([]UsageRollupReplacementRow, bool, error) {
				calls++
				if calls == 1 {
					return replacementRows(100), false, nil
				}
				switch mode {
				case "source":
					return nil, false, errors.New("injected source error")
				case "cancel":
					cancel()
					return replacementRows(1), true, nil
				case "duplicate":
					return replacementRows(1), true, nil
				case "oversize":
					return replacementRows(101), true, nil
				case "changed-root":
					if err := s.ResetUsageDerivatives(); err != nil {
						t.Fatal(err)
					}
					if err := s.PutUsageCheckpoint(head, []byte(`{"version":15}`), nil, nil, domain.RollupVersion, nil); err != nil {
						t.Fatal(err)
					}
					return nil, true, nil
				}
				panic("unknown fault")
			})
			if err == nil || report != (UsageRollupReplacement{}) || !reflect.DeepEqual(replacementVisible(t, s), old) {
				t.Fatal(report, err)
			}
			if report, err := s.ReplaceUsageRollups(context.Background(), head, replacementFeed(replacementRows(2))); err != nil || report.Rows != 2 || len(replacementVisible(t, s)) != 2 {
				t.Fatal("retry exposed orphan staging", report, err)
			}
		})
	}
}

func TestUsageDerivedMaintenanceRefusesPhysicalReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	s, err := openForTest(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(t.TempDir(), "other.db")
	s, err = openForTest(t, otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(otherPath, path); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenExistingDerivedMaintenance(path, info); err == nil {
		s.Close()
		t.Fatal("replacement accepted")
	}
	if s, err := OpenExistingDerivedMaintenance(path+".missing", info); err == nil {
		s.Close()
		t.Fatal("missing Metadata seeded")
	}
	if _, err := os.Stat(path + ".missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
