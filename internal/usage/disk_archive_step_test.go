package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func runTestArchiveStep(ctx context.Context, p *DiskProjection, after uint64, limits DiskArchiveLimits, visit func(context.Context, []AttemptEvent) error, complete func(context.Context, DiskArchiveCut) error) (DiskArchiveCut, error) {
	return p.archiveJob(ctx, limits, func(ctx context.Context) (DiskArchiveCut, error) {
		return p.streamArchiveStepHeld(ctx, after, limits, visit, complete)
	})
}

func TestDiskArchiveStepFiniteLimitsEventuallyCoverNativePrefix(t *testing.T) {
	for _, boundary := range []string{"pages", "rows", "input", "output", "page_input", "page_output"} {
		t.Run(boundary, func(t *testing.T) {
			f := diskExportFixture(t, 7)
			want := f.a.Snapshot().Attempts
			limits := archiveTestLimits
			first, _ := json.Marshal(want[0])
			second, _ := json.Marshal(want[1])
			switch boundary {
			case "pages":
				limits.Pages = 1
				limits.PageRows = 2
			case "rows":
				limits.Rows = 2
			case "input":
				limits.InputBytes = len(first) + len(second) + 16
			case "output":
				limits.OutputBytes = len(first) + len(second) + 3
			case "page_input":
				limits.Pages = 1
				limits.PageInputBytes = len(first) + len(second) + 16
			case "page_output":
				limits.Pages = 1
				limits.PageOutputBytes = len(first) + len(second) + 3
			}
			frozen := f.a.Watermark()
			after := uint64(0)
			var got []AttemptEvent
			for jobs := 0; after < frozen.Sequence; jobs++ {
				if jobs > len(want) {
					t.Fatal("bounded step made no progress", after)
				}
				cut, err := runTestArchiveStep(context.Background(), f.p, after, limits, func(ctx context.Context, page []AttemptEvent) error {
					if f.p.db.Stats().OpenTxN != 0 || len(f.p.readers) != 0 {
						t.Fatal("callback held source transaction")
					}
					got = append(got, page...)
					return nil
				}, nil)
				if err != nil || cut.Head != frozen || cut.AfterSequence != after || cut.ThroughSequence <= after || cut.ThroughSequence > frozen.Sequence || cut.Pages > limits.Pages || cut.Rows > limits.Rows || cut.InputBytes > limits.InputBytes || cut.OutputBytes > limits.OutputBytes {
					t.Fatal(cut, err)
				}
				after = cut.ThroughSequence
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("steps omitted or duplicated native rows", len(got), len(want))
			}
			if after <= want[len(want)-1].Sequence {
				t.Fatal("final step omitted trailing finalized fact", after)
			}
		})
	}
}

func TestDiskArchiveStepFrozenSourceExcludesCallbackAppend(t *testing.T) {
	f := diskExportFixture(t, 4)
	head := f.a.Watermark()
	want := f.a.Snapshot().Attempts
	limits := archiveTestLimits
	limits.Rows = 2
	limits.PageRows = 1
	var got []AttemptEvent
	cut, err := runTestArchiveStep(context.Background(), f.p, 0, limits, func(ctx context.Context, page []AttemptEvent) error {
		if len(got) == 0 {
			for _, event := range diskFailureRequest("step_future", "success", 1, false, true) {
				appendDiskDetailEvent(t, f, event)
			}
		}
		got = append(got, page...)
		return nil
	}, nil)
	if err != nil || cut.Head != head || cut.ThroughSequence != want[1].Sequence || !reflect.DeepEqual(got, want[:2]) || f.a.Watermark().Sequence <= head.Sequence {
		t.Fatal(cut, err)
	}
}

func TestDiskExportStepReopenResumeBothFormatsAndExactReconcile(t *testing.T) {
	for _, container := range []string{FormatParquet, FormatNDJSON} {
		t.Run(container, func(t *testing.T) {
			f := diskExportFixture(t, 11)
			root := t.TempDir()
			limits := DefaultDiskExportStepLimits()
			limits.Source.Rows = 2
			after := uint64(0)
			head := f.a.Watermark()
			jobs := 0
			for after < head.Sequence {
				e, err := NewExporterWithOptions(root, Options{Format: container})
				if err != nil {
					t.Fatal(err)
				}
				cut, err := e.ExportDiskStep(context.Background(), f.p, limits)
				if err != nil || cut.AfterSequence != after || cut.ThroughSequence <= after || cut.Head != head || cut.Rows > 2 {
					t.Fatal(cut, err)
				}
				position, err := e.ReadArchivePosition(context.Background())
				if err != nil || !position.CompletePrefix || position.LastSequence != cut.ThroughSequence {
					t.Fatal(position, err)
				}
				if after == 0 && position.LastSequence == head.Sequence {
					t.Fatal("first partial job credited full source head")
				}
				verified, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
				if err != nil || verified.ThroughSequence != cut.ThroughSequence || verified.SourceHead != head || verified.ArchiveRows != verified.SourceRows {
					t.Fatal(verified, err)
				}
				after = cut.ThroughSequence
				jobs++
				if jobs > 11 {
					t.Fatal("did not finish finite backlog")
				}
			}
			e, err := NewExporterWithOptions(root, Options{Format: container})
			if err != nil {
				t.Fatal(err)
			}
			verified, err := e.VerifyDiskArchive(context.Background(), f.p, DefaultArchiveVerifyLimits())
			if err != nil || verified.ArchiveRows != 11 || jobs != 6 {
				t.Fatal(verified, err, jobs)
			}
			before, err := os.ReadFile(filepath.Join(root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			repeat, err := e.ExportDiskStep(context.Background(), f.p, limits)
			now, readErr := os.ReadFile(filepath.Join(root, "manifest.json"))
			if err != nil || readErr != nil || repeat.Rows != 0 || repeat.ThroughSequence != head.Sequence || !reflect.DeepEqual(before, now) {
				t.Fatal(repeat, err, readErr)
			}
		})
	}
}

func TestDiskExportStepFailureNeverCreditsPartialSinkAndRetryPreservesRows(t *testing.T) {
	f := diskExportFixture(t, 7)
	e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultDiskExportStepLimits()
	limits.Source.Rows = 2
	limits.Source.PageRows = 1
	first, err := e.ExportDiskStep(context.Background(), f.p, limits)
	if err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(e.root, "manifest.json")
	before, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	// First output fits, second partition exhausts physical writes. No root may
	// include the first partition until this complete finite step succeeds.
	snapshot := f.a.Snapshot().Attempts
	payload, _ := json.Marshal(toParquetAttempt(snapshot[2]))
	limits.WriteBytes = int64(len(payload) + 1)
	cut, err := e.ExportDiskStep(context.Background(), f.p, limits)
	if !errors.Is(err, ErrDiskUsageLimit) || cut != (DiskArchiveCut{}) {
		t.Fatal(cut, err)
	}
	after, err := os.ReadFile(rootPath)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed sink moved prefix", err)
	}
	orphan := filepath.Join(e.root, "date="+snapshot[2].CompletedAt.UTC().Format("2006-01-02"), fmt.Sprintf("usage-bounded-v1-%020d-%020d.ndjson", snapshot[2].Sequence, snapshot[2].Sequence))
	info, err := os.Stat(orphan)
	if err != nil {
		t.Fatal("expected a retryable first partition", err)
	}
	limits.WriteBytes = DefaultDiskExportStepLimits().WriteBytes
	resumed, err := e.ExportDiskStep(context.Background(), f.p, limits)
	now, statErr := os.Stat(orphan)
	if err != nil || statErr != nil || resumed.AfterSequence != first.ThroughSequence || resumed.Rows != 2 || !os.SameFile(info, now) {
		t.Fatal(resumed, err, statErr)
	}
	for _, edit := range []func(*DiskExportLimits){func(l *DiskExportLimits) { l.Source.InputBytes = 1 }, func(l *DiskExportLimits) { l.Source.OutputBytes = 1 }, func(l *DiskExportLimits) { l.Source.PageInputBytes = 1 }, func(l *DiskExportLimits) { l.Source.PageOutputBytes = 1 }} {
		small := DefaultDiskExportStepLimits()
		edit(&small)
		frozen, err := os.ReadFile(rootPath)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := e.ExportDiskStep(context.Background(), f.p, small)
		current, readErr := os.ReadFile(rootPath)
		if !errors.Is(err, ErrDiskUsageLimit) || receipt != (DiskArchiveCut{}) || readErr != nil || !reflect.DeepEqual(frozen, current) {
			t.Fatal(receipt, err, readErr)
		}
	}
}

func TestDiskArchiveStepCancellationAndCloseJoinIncompletePublication(t *testing.T) {
	f := diskExportFixture(t, 3)
	limits := archiveTestLimits
	limits.Rows = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() {
		cut, err := runTestArchiveStep(ctx, f.p, 0, limits, func(context.Context, []AttemptEvent) error { return nil }, func(ctx context.Context, cut DiskArchiveCut) error {
			if cut.ThroughSequence >= cut.Head.Sequence || f.p.db.Stats().OpenTxN != 0 {
				return errors.New("publication received wrong prefix or source transaction")
			}
			close(entered)
			<-release
			return nil
		})
		if cut != (DiskArchiveCut{}) {
			err = errors.Join(err, errors.New("retired publication credited prefix"))
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("prefix publication did not begin")
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- f.p.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for !f.p.retired.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Close did not withdraw")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatal("Close passed physical publication", err)
	default:
	}
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestDiskArchiveStepGenesisAndNonAttemptOnlyTail(t *testing.T) {
	for _, events := range [][]ledger.Event{nil, {diskFailureRequest("only_accept", "success", 1, false, true)[0]}} {
		f := newDiskFailureFixture(t, events)
		cut, err := runTestArchiveStep(context.Background(), f.p, 0, archiveTestLimits, func(context.Context, []AttemptEvent) error {
			t.Fatal("non-attempt source emitted an attempt")
			return nil
		}, nil)
		if err != nil || cut.Rows != 0 || cut.ThroughSequence != f.a.Watermark().Sequence || cut.Head != f.a.Watermark() {
			t.Fatal(cut, err)
		}
	}
}
