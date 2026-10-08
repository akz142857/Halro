package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

func TestLedgerInspectProjectionCursorResolvesExactRecordsAndRollStarts(t *testing.T) {
	path, report, records := prefixBuildFixture(t)
	cuts := []ProjectionPrefix{{}, {Head: Watermark{Generation: 1}}, {Head: Watermark{Generation: 2, Sequence: 30}, Hash: records[29].Hash}, {Head: report.Head, Hash: report.ChainHash}}
	for _, i := range []int{0, 29, 64, 79, 149} {
		r := records[i]
		cuts = append(cuts, ProjectionPrefix{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash})
	}
	for _, want := range cuts {
		t.Run(fmt.Sprintf("%d/%d", want.Head.Generation, want.Head.Sequence), func(t *testing.T) {
			cut, complete, err := InspectProjectionCursor(context.Background(), path, testChainKey, want.Head.Generation, want.Head.Sequence)
			if err != nil || cut != want || complete != report {
				t.Fatal(cut, want, complete, err)
			}
		})
	}
	for _, cursor := range [][2]uint64{{0, 1}, {2, 29}, {1, 31}, {5, 150}, {3, 151}} {
		if cut, report, err := InspectProjectionCursor(context.Background(), path, testChainKey, cursor[0], cursor[1]); !errors.Is(err, ErrProjectionCursor) || cut != (ProjectionPrefix{}) || report != (ChainReport{}) {
			t.Fatal(cursor, cut, report, err)
		}
	}
}

func TestLedgerBuildProjectionFromCursorFreshRoleShrinkAndExclusiveOutput(t *testing.T) {
	path, _, records := prefixBuildFixture(t)
	log := openChained(t, path, nil)
	defer log.Close()
	ctx := context.Background()
	for _, cursor := range [][2]uint64{{3, 150}, {2, 30}, {1, 1}, {0, 0}, {1, 0}} {
		output := filepath.Join(t.TempDir(), "owner.db")
		p, cut, err := log.BuildProjectionFromCursor(ctx, cursor[0], cursor[1], output)
		if err != nil {
			t.Fatal(cursor, err)
		}
		head, hash, err := p.ReadPosition(ctx)
		if err != nil || head != cut.Head || hash != cut.Hash || head.Generation != cursor[0] || head.Sequence != cursor[1] {
			t.Fatal(cursor, head, cut, err)
		}
		for _, r := range records {
			_, found, err := p.ReadEventPosition(ctx, r.Event.EventID)
			if err != nil || found != (r.Sequence <= cursor[1]) {
				t.Fatal(cursor, r.Sequence, found, err)
			}
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if p, _, err := log.BuildProjectionFromCursor(ctx, cursor[0], cursor[1], output); err == nil || p != nil {
			t.Fatal("existing derived file reused", p, err)
		}
		after, err := os.ReadFile(output)
		if err != nil || string(after) != string(before) {
			t.Fatal("existing projection replaced", err)
		}
	}
	output := filepath.Join(t.TempDir(), "invalid.db")
	if p, _, err := log.BuildProjectionFromCursor(ctx, 1, 31, output); !errors.Is(err, ErrProjectionCursor) || p != nil {
		t.Fatal(p, err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) || log.Status().Load() != AccountingHealthy {
		t.Fatal("wrong role cursor changed native or created output", err, log.Status().Load())
	}
}

func TestLedgerBuildProjectionFromCursorWithLimitMatchesEveryAuthorizedPrefix(t *testing.T) {
	path, _, records := prefixBuildFixture(t)
	log := openChained(t, path, nil)
	defer log.Close()
	ctx := context.Background()
	for _, cursor := range [][2]uint64{{3, 150}, {2, 30}, {1, 1}, {0, 0}, {1, 0}} {
		t.Run(fmt.Sprintf("%d/%d", cursor[0], cursor[1]), func(t *testing.T) {
			want, _, err := InspectProjectionCursor(ctx, path, testChainKey, cursor[0], cursor[1])
			if err != nil {
				t.Fatal(err)
			}
			p, cut, err := log.BuildProjectionFromCursorWithLimit(ctx, cursor[0], cursor[1], filepath.Join(t.TempDir(), "limited.db"), 64<<20)
			if err != nil || cut != want {
				t.Fatal(cut, want, err)
			}
			defer p.Close()
			head, hash, err := p.ReadPosition(ctx)
			if err != nil || head != want.Head || hash != want.Hash {
				t.Fatal(head, hash, err)
			}
			state := NewState()
			for _, record := range records {
				allowed := record.Sequence <= cursor[1]
				if allowed {
					if err := state.Apply(record); err != nil {
						t.Fatal(err)
					}
				}
				position, found, err := p.ReadEventPosition(ctx, record.Event.EventID)
				if err != nil || found != allowed || found && position.Sequence != record.Sequence {
					t.Fatalf("event %d allowed=%v found=%v position=%+v err=%v", record.Sequence, allowed, found, position, err)
				}
			}
			for _, key := range []BalanceKey{{ProjectID: "prj_1", PeriodID: "2026-10-01", TimezoneVersion: 1}, {ProjectID: "prj_1", PeriodID: "2026-10-02", TimezoneVersion: 2}, {ProjectID: "prj_1", PeriodID: "2026-10-03", TimezoneVersion: 1}} {
				disk, err := p.ReadBalance(ctx, key)
				memory, memoryErr := state.ReadBalance(ctx, key)
				if err != nil || memoryErr != nil || !reflect.DeepEqual(disk, memory) {
					t.Fatal(key, disk, memory, err, memoryErr)
				}
			}
		})
	}
	for _, cursor := range [][2]uint64{{0, 1}, {2, 29}, {1, 31}, {9, 30}} {
		output := filepath.Join(t.TempDir(), "invalid.db")
		if p, _, err := log.BuildProjectionFromCursorWithLimit(ctx, cursor[0], cursor[1], output, 64<<20); !errors.Is(err, ErrProjectionCursor) || p != nil {
			t.Fatal(cursor, p, err)
		}
		if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid cursor left a candidate", cursor, err)
		}
		if log.Status().Load() != AccountingHealthy {
			t.Fatal("invalid role cursor poisoned source", cursor, log.Status().Load())
		}
	}
}

func TestLedgerBuildProjectionFromCursorDiskCeilingRollsBackAndRemovesCandidate(t *testing.T) {
	path, report, _ := prefixBuildFixture(t)
	log := openChained(t, path, nil)
	defer log.Close()

	// Measure only the fixed empty projection envelope; the test ceiling is
	// deliberately below the real candidate size, not a production estimate.
	empty, err := createLedgerProjection(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	var emptyBytes int64
	if err := empty.db.View(func(tx *bbolt.Tx) error { emptyBytes = tx.Size(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := empty.Discard(); err != nil {
		t.Fatal(err)
	}
	if emptyBytes <= 0 || report.Head.Sequence == 0 {
		t.Fatal(emptyBytes, report.Head)
	}

	output := filepath.Join(t.TempDir(), "limited.db")
	if p, _, err := log.BuildProjectionFromCursorWithLimit(context.Background(), report.Head.Generation, report.Head.Sequence, output, emptyBytes); !errors.Is(err, ErrProjectionDiskLimit) || p != nil {
		t.Fatal("oversized candidate was published", p, err)
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed candidate was not removed", err)
	}
	if log.Status().Load() != AccountingHealthy {
		t.Fatal("output capacity failure poisoned native owner", log.Status().Load())
	}

	output = filepath.Join(t.TempDir(), "bounded.db")
	const ceiling = int64(64 << 20)
	p, cut, err := log.BuildProjectionFromCursorWithLimit(context.Background(), report.Head.Generation, report.Head.Sequence, output, ceiling)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	var actual int64
	if err := p.db.View(func(tx *bbolt.Tx) error { actual = tx.Size(); return nil }); err != nil || actual > ceiling || cut.Head != report.Head {
		t.Fatal("successful limited candidate exceeded its ceiling", actual, ceiling, cut, err)
	}
}

func TestLedgerProjectionDiskCeilingIncludesPagesAllocatedBeforeCommit(t *testing.T) {
	_, _, records := prefixBuildFixture(t)
	path := filepath.Join(t.TempDir(), "empty.db")
	empty, err := createLedgerProjection(path)
	if err != nil {
		t.Fatal(err)
	}
	var ceiling int64
	if err := empty.db.View(func(tx *bbolt.Tx) error { ceiling = tx.Size(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := empty.Discard(); err != nil {
		t.Fatal(err)
	}

	ceiling += 256 << 10
	p, err := createLedgerProjectionWithLimit(filepath.Join(t.TempDir(), "limited.db"), ceiling)
	if err != nil {
		t.Fatal(err)
	}
	p.ready = true
	defer p.Discard()
	var refused bool
	var accepted uint64
	for _, record := range records {
		if err := p.Apply(context.Background(), record); err != nil {
			if !errors.Is(err, ErrProjectionDiskLimit) {
				t.Fatal(err)
			}
			refused = true
			break
		}
		accepted = record.Sequence
		var actual int64
		if err := p.db.View(func(tx *bbolt.Tx) error { actual = tx.Size(); return nil }); err != nil || actual > ceiling {
			t.Fatalf("committed transaction crossed ceiling: size=%d ceiling=%d err=%v", actual, ceiling, err)
		}
	}
	if !refused || accepted == 0 {
		t.Fatalf("expected a bounded non-empty prefix followed by refusal, accepted=%d refused=%t", accepted, refused)
	}
	head, _, err := p.ReadPosition(context.Background())
	if err != nil || head.Sequence != accepted {
		t.Fatalf("refused transaction changed projection position: head=%+v accepted=%d err=%v", head, accepted, err)
	}
}

func TestLedgerProjectionCursorChecksExcludedSuffixAndFrozenNativeIdentity(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(fmt.Sprintf("valid-source-replacement-%t", replaced), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.wal")
			log := openChained(t, path, nil)
			defer log.Close()
			ctx := context.Background()
			first, _, err := log.AppendRecordWithReceipt(ctx, validReservation("evt_old_1", "att_old_1"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(ctx, validReservation("evt_old_2", "att_old_2")); err != nil {
				t.Fatal(err)
			}
			if replaced {
				otherPath := filepath.Join(t.TempDir(), "ledger.wal")
				other := openChained(t, otherPath, nil)
				for i := 1; i <= 2; i++ {
					if _, err := other.Append(ctx, validReservation(fmt.Sprintf("evt_new_%d", i), fmt.Sprintf("att_new_%d", i))); err != nil {
						t.Fatal(err)
					}
				}
				if err := other.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(otherPath, path); err != nil {
					t.Fatal(err)
				}
			} else {
				tamperResumeFrame(t, path, first.Offset)
			}
			output := filepath.Join(t.TempDir(), "candidate.db")
			if p, _, err := log.BuildProjectionFromCursor(ctx, 1, 1, output); !errors.Is(err, ErrTampered) || p != nil {
				t.Fatal(p, err)
			}
			if log.Status().Load() != AccountingRecoveryRequired {
				t.Fatal(log.Status().Load())
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if p, _, err := log.BuildProjectionFromCursor(ctx, 1, 1, output); err == nil || p != nil {
				t.Fatal("failed native owner reopened by retry", p, err)
			}
		})
	}
}

func TestLedgerProjectionCursorWithLimitAuthenticatesExcludedSuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	defer log.Close()
	first, _, err := log.AppendRecordWithReceipt(context.Background(), validReservation("evt_cursor_prefix", "att_cursor_prefix"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := log.AppendRecordWithReceipt(context.Background(), validReservation("evt_cursor_suffix", "att_cursor_suffix")); err != nil {
		t.Fatal(err)
	}
	// The requested projection ends before this frame. The complete native
	// source must still authenticate the excluded durable suffix.
	// Record.Offset is the physical end, so the first record's end is the
	// second record's starting position.
	tamperResumeFrame(t, path, first.Offset)
	output := filepath.Join(t.TempDir(), "candidate.db")
	if p, _, err := log.BuildProjectionFromCursorWithLimit(context.Background(), first.Generation, first.Sequence, output, 64<<20); !errors.Is(err, ErrTampered) || p != nil {
		t.Fatal(p, err)
	}
	if log.Status().Load() != AccountingRecoveryRequired {
		t.Fatal(log.Status().Load())
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed candidate was not removed", err)
	}
}

func TestLedgerProjectionCursorEmptyCanceledAndClosedOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	ctx := context.Background()
	p, cut, err := log.BuildProjectionFromCursor(ctx, 1, 0, filepath.Join(t.TempDir(), "empty.db"))
	if err != nil || cut != (ProjectionPrefix{Head: Watermark{Generation: 1}}) {
		t.Fatal(cut, err)
	}
	_ = p.Close()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	output := filepath.Join(t.TempDir(), "absent.db")
	if p, _, err := log.BuildProjectionFromCursor(canceled, 1, 0, output); !errors.Is(err, context.Canceled) || p != nil {
		t.Fatal(p, err)
	}
	if _, _, err := InspectProjectionCursor(canceled, path, testChainKey, 1, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if p, _, err := log.BuildProjectionFromCursor(ctx, 1, 0, output); err == nil || p != nil {
		t.Fatal(p, err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestLedgerProjectionCursorSecondPassNativeFailureLatchesButOutputFailureDoesNot(t *testing.T) {
	for _, kind := range []string{"output-failure", "HMAC-failure", "valid-source-replacement"} {
		t.Run(kind, func(t *testing.T) {
			nativeFailure := kind != "output-failure"
			path := filepath.Join(t.TempDir(), "ledger.wal")
			log := openChained(t, path, nil)
			defer log.Close()
			ctx := context.Background()
			first, _, err := log.AppendRecordWithReceipt(ctx, validReservation("evt_second_1", "att_second_1"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(ctx, validReservation("evt_second_2", "att_second_2")); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(t.TempDir(), "candidate.db")
			if !nativeFailure {
				// Native lookup succeeds; exclusively creating this output fails.
				output = filepath.Join(t.TempDir(), "missing-parent", "candidate.db")
			}
			build := func(ctx context.Context, path string, key []byte, output string, report ChainReport, cut ProjectionPrefix) (*LedgerProjection, error) {
				if kind == "HMAC-failure" {
					tamperResumeFrame(t, path, first.Offset)
				}
				if kind == "valid-source-replacement" {
					otherPath := filepath.Join(t.TempDir(), "ledger.wal")
					other := openChained(t, otherPath, nil)
					for i := 1; i <= 2; i++ {
						if _, err := other.Append(ctx, validReservation(fmt.Sprintf("evt_changed_%d", i), fmt.Sprintf("att_changed_%d", i))); err != nil {
							t.Fatal(err)
						}
					}
					if err := other.Close(); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(otherPath, path); err != nil {
						t.Fatal(err)
					}
				}
				return BuildLedgerProjectionPrefix(ctx, path, key, output, report, cut)
			}
			p, _, err := log.buildProjectionFromCursor(ctx, 1, 1, output, build)
			if err == nil || p != nil || errors.Is(err, ErrProjectionSource) != nativeFailure {
				t.Fatal(p, err)
			}
			want := AccountingHealthy
			if nativeFailure {
				want = AccountingRecoveryRequired
			}
			if log.Status().Load() != want {
				t.Fatal(log.Status().Load(), want, err)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed candidate retained", err)
			}
		})
	}
}

func TestLedgerProjectionCursorBuildKeepsNativeCloseJoinedThroughSecondPass(t *testing.T) {
	log := openChained(t, filepath.Join(t.TempDir(), "ledger.wal"), nil)
	if _, err := log.Append(context.Background(), validReservation("evt_join", "att_join")); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = log.Close()
	}()
	build := func(ctx context.Context, path string, key []byte, output string, report ChainReport, cut ProjectionPrefix) (*LedgerProjection, error) {
		close(entered)
		<-release // models an in-progress native/file operation
		return BuildLedgerProjectionPrefix(ctx, path, key, output, report, cut)
	}
	type result struct {
		p   *LedgerProjection
		err error
	}
	built := make(chan result, 1)
	go func() {
		p, _, err := log.buildProjectionFromCursor(context.Background(), 1, 1, filepath.Join(t.TempDir(), "owner.db"), build)
		built <- result{p, err}
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- log.Close() }()
	<-log.closeSignal
	select {
	case err := <-closed:
		t.Fatal("native Close finished before frozen build exited", err)
	default:
	}
	close(release)
	r := <-built
	if r.err != nil || r.p == nil {
		t.Fatal(r.err)
	}
	if err := r.p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
