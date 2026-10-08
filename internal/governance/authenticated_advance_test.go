package governance

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func restoredAdvanceFixture(t *testing.T, suffix int) (*GovernanceProjection, *Log, *resourcebudget.Manager, []Record) {
	return restoredAdvanceFixtureWithDebt(t, suffix, false)
}

// Deliberate actual retirement faults keep the original claims and inodes.
func restoredAdvanceFixtureWithDebt(t *testing.T, suffix int, debt bool) (*GovernanceProjection, *Log, *resourcebudget.Manager, []Record) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	native, err := Open(filepath.Join(dir, "native.wal"), bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if debt {
			return
		}
		if err := native.Close(); err != nil {
			t.Error(err)
		}
	})
	first, err := native.Append(ctx, projectionEvent(1))
	if err != nil {
		t.Fatal(err)
	}
	budget := governanceSealBudget(t)
	credit, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if debt {
			return
		}
		if err := credit.Release(); err != nil {
			t.Error(err)
		}
	})
	root, err := BuildAuthenticatedGovernanceProjection(ctx, AuthenticatedGovernanceOptions{Native: native, Identity: [32]byte{1}, RootKey: bytes.Repeat([]byte{0x72}, 32), ProjectionPath: filepath.Join(dir, "scratch.db"), RootDirectory: filepath.Join(dir, "root"), ProjectionMaxDiskBytes: 16 << 20, RootMaxDiskBytes: 32 << 20, RootMaxAppendBytes: 4 << 20, ProjectionDiskReservation: credit, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Discard(); err != nil {
			t.Error(err)
		}
	})
	rows := []Record{first}
	for n := 0; n < suffix; n++ {
		record, err := native.Append(ctx, projectionEvent(n+2))
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, record)
	}
	p, err := root.RestoreProjection(ctx, filepath.Join(dir, "live.db"), native, 16<<20, credit, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if debt {
			return
		}
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
	})
	return p, native, budget, rows
}

func advanceRecordCut(r Record) Summary {
	return Summary{Records: r.Sequence, Bytes: r.Offset, LastHash: r.Hash}
}

func TestRestoredGovernanceAdvanceAuthenticatesExcludedSuffixAndAppliesOnlyPermittedDelta(t *testing.T) {
	p, native, budget, rows := restoredAdvanceFixture(t, 2)
	ctx := context.Background()
	for _, index := range []int{1, 2} {
		receipt, err := p.AdvanceAuthenticatedFromNative(ctx, native, advanceRecordCut(rows[index]), budget)
		if err != nil || receipt.AppliedRecords != 1 || receipt.To != advanceRecordCut(rows[index]) || receipt.NativeHead != native.Summary() {
			t.Fatal(receipt, err)
		}
		if _, found, err := p.ReadOutcome(ctx, rows[index].Event.OutcomeID); err != nil || !found {
			t.Fatal(found, err)
		}
		if index == 1 {
			if _, found, err := p.ReadOutcome(ctx, rows[2].Event.OutcomeID); err != nil || found {
				t.Fatal("excluded fact was applied", found, err)
			}
		}
	}
	before := budget.Snapshot().Used
	receipt, err := p.AdvanceAuthenticatedFromNative(ctx, native, native.Summary(), budget)
	if err != nil || receipt.AppliedRecords != 0 || receipt.From != receipt.To || budget.Snapshot().Used != before {
		t.Fatal("same-cut retry mutated rows or fees", receipt, err)
	}
	if _, err := p.AdvanceAuthenticatedFromNative(ctx, native, advanceRecordCut(rows[0]), budget); !errors.Is(err, ErrGovernanceSealSource) {
		t.Fatal("role shrink accepted", err)
	}
}

func TestRestoredGovernanceAdvanceRefusesUnprovedCutsAndCorruptExcludedTailBeforeApply(t *testing.T) {
	for _, kind := range []string{"bad-cut", "canceled", "pressure", "excluded-tail"} {
		t.Run(kind, func(t *testing.T) {
			p, native, budget, rows := restoredAdvanceFixture(t, 2)
			ctx := context.Background()
			target := advanceRecordCut(rows[1])
			switch kind {
			case "bad-cut":
				target.LastHash[0] ^= 1
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "pressure":
				if err := budget.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(err)
				}
			case "excluded-tail":
				file, err := os.OpenFile(native.path, os.O_RDWR, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteAt([]byte{0xff}, rows[1].Offset+frameHeaderSize)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before := budget.Snapshot().Used
			if receipt, err := p.AdvanceAuthenticatedFromNative(ctx, native, target, budget); err == nil || receipt != (AuthenticatedProjectionAdvance{}) {
				t.Fatal(receipt, err)
			}
			if budget.Snapshot().Used != before {
				t.Fatal("failed job leaked fees", budget.Snapshot())
			}
			if kind == "excluded-tail" {
				if _, err := p.ReadHead(context.Background()); !errors.Is(err, ErrUnavailable) {
					t.Fatal("corrupt source did not retire view", err)
				}
			} else {
				if head, err := p.ReadHead(context.Background()); err != nil || head != advanceRecordCut(rows[0]) {
					t.Fatal("refusal changed old cut", head, err)
				}
			}
		})
	}
}

func TestRestoredGovernanceAdvanceCanceledAfterBatchPermanentlyWithdrawsGeneration(t *testing.T) {
	p, native, budget, _ := restoredAdvanceFixture(t, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := budget.Snapshot().Used
	_, err := p.advanceAuthenticatedFromNative(ctx, native, native.Summary(), budget, func(phase string) {
		if phase == "batch-applied" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := p.ReadHead(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("partially applied view remained public", err)
	}
	if budget.Snapshot().Used != before {
		t.Fatal("partial job leaked fees", budget.Snapshot())
	}
}

func TestRestoredGovernanceAdvanceCloseCancelsAndJoinsActualSourceWork(t *testing.T) {
	p, native, budget, _ := restoredAdvanceFixture(t, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	advanced := make(chan error, 1)
	go func() {
		_, err := p.advanceAuthenticatedFromNative(context.Background(), native, advanceRecordCut(Record{Sequence: native.sequence, Offset: native.offset, Hash: native.lastHash}), budget, func(phase string) {
			if phase == "source-verified" {
				close(entered)
				<-release
			}
		})
		advanced <- err
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close did not join actual job", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-advanced; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if p.advanceDone != nil || p.advanceCancel != nil {
		t.Fatal("joined job retained owner state")
	}
}

func TestRestoredGovernanceAdvanceActualReaderCloseDebtRetainsGenerationAndJobFees(t *testing.T) {
	p, native, budget, rows := restoredAdvanceFixtureWithDebt(t, 1, true)
	native.closeReaderFile = func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		return file.Close()
	}
	before := budget.Snapshot().Used
	receipt, err := p.AdvanceAuthenticatedFromNative(context.Background(), native, advanceRecordCut(rows[1]), budget)
	if !errors.Is(err, ownedfile.ErrRetirement) || receipt != (AuthenticatedProjectionAdvance{}) {
		t.Fatal(receipt, err)
	}
	charged := budget.Snapshot().Used
	if charged.RAM <= before.RAM {
		t.Fatal("actual unknown reader refunded full job", before, charged)
	}
	if _, _, err := p.ReadOutcome(context.Background(), rows[1].Event.OutcomeID); !errors.Is(err, ErrUnavailable) {
		t.Fatal("retirement debt served candidate", err)
	}
	candidate := p.db.Path()
	first := p.Discard()
	if !errors.Is(first, ownedfile.ErrRetirement) || p.Discard() != first {
		t.Fatal("generation discarded actual reader debt", first)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatal("unknown candidate inode deleted", err)
	}
	if err := native.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	if after := budget.Snapshot().Used; after != charged {
		t.Fatal("unknown source or generation refunded fees", charged, after)
	}
}
