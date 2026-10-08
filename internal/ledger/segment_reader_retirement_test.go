package ledger

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// The stream reads a real native file. Retirement first closes that actual
// descriptor, then observes a real failed Close on the same descriptor owner.
// This is unknown-exit evidence, not a synthetic error returned by the scanner.
type actualSegmentCloseFailure struct {
	*os.File
	closeCalls int
}

func (f *actualSegmentCloseFailure) Close() error {
	f.closeCalls++
	if err := f.File.Close(); err != nil {
		return ownedfile.CloseError(f.File, err)
	}
	return ownedfile.CloseFile(f.File)
}

func TestSegmentGzipFailedOpenRetainsActualCloseOwner(t *testing.T) {
	dir := t.TempDir()
	name := "invalid.wal.gz"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("invalid gzip header"), 0600); err != nil {
		t.Fatal(err)
	}
	var actual *actualSegmentCloseFailure
	r, err := openSegmentWithStream(dir, Segment{Generation: 1, File: name, Compressed: true}, func(path string) (io.ReadCloser, error) {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		actual = &actualSegmentCloseFailure{File: file}
		return actual, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Seek(0, io.SeekStart); !errors.Is(err, ErrCorrupt) || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("failed gzip header lost actual Close failure", err)
	}
	if r.reader != actual || actual == nil || actual.File == nil || !r.closed {
		t.Fatal("failed gzip initialization dropped its actual stream")
	}
	sticky := r.closeErr
	for i := 0; i < 2; i++ {
		if err := r.Close(); err != sticky || !errors.Is(err, ErrSegmentRetirement) {
			t.Fatal("repeated retirement erased unknown exit", err)
		}
		if _, err := r.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) || !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("retired reader admitted IO", err)
		}
		if _, err := r.Seek(0, io.SeekStart); !errors.Is(err, os.ErrClosed) {
			t.Fatal("retired reader reopened a replacement", err)
		}
	}
	if actual.closeCalls != 1 || r.reader != actual {
		t.Fatal("sticky retirement retried or discarded actual owner", actual.closeCalls)
	}
}

func TestSelectedRolledSuffixReverifyActualCloseDebtPinsOwnerKeyAndFee(t *testing.T) {
	ctx := context.Background()
	f := newSelectedStartupFixture(t, true)
	if _, err := f.native.Append(ctx, projectionTestLifecycle(2, true)[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.native.Roll(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.native.Append(ctx, projectionTestLifecycle(3, true)[0]); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	var actual *actualSegmentCloseFailure
	f.native.replaySegmentOpen = func(directory string, segment Segment) (*segmentReader, error) {
		return openSegmentWithStream(directory, segment, func(path string) (io.ReadCloser, error) {
			file, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			actual = &actualSegmentCloseFailure{File: file}
			return actual, nil
		})
	}
	selected := SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority}
	budget := f.o.DirectoryOptions.Budget
	before := budget.Snapshot().Used.RAM
	err := f.native.WithSelectedStartupOwner(ctx, budget, selected, f.o.DirectoryOptions.Identity, func(ctx context.Context, source SelectedStartupSource) error {
		return source.VerifyAnchors(ctx, [2]ReplayAnchor{source.SavedAnchor(), source.NativeHead()})
	})
	if !errors.Is(err, ErrSegmentRetirement) || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal("actual sealed reader Close failure was accepted", err)
	}
	f.expectDebt = true
	retained := f.native.replayReaderDebt
	retainsActualStream := func() bool {
		if actual == nil || retained == nil {
			return false
		}
		if retained.reader == actual {
			return true
		}
		captured, ok := retained.reader.(capturedSegmentReadCloser)
		return ok && captured.Reader == actual && captured.Closer == actual
	}
	if !retainsActualStream() || len(f.native.chainKey) != 32 {
		t.Fatalf("reverify dropped actual reader or native key: actual=%p retained=%p key_bytes=%d", actual, retained, len(f.native.chainKey))
	}
	var workDebt *selectedStartupSourceDebt
	if !errors.As(err, &workDebt) {
		t.Fatal("whole-source IO permit owner missing", err)
	}
	if cost, err := workDebt.work.Cost(); err != nil || cost.RAM != AuthenticatedLedgerSourceRAMBytes || budget.Snapshot().Used.RAM < before+AuthenticatedLedgerSourceRAMBytes {
		t.Fatal("actual Close failure refunded decoder credit", cost, err)
	}
	// The native owner retains the complete loan debt after callers discard the
	// returned error. Its Close must still retain both the source work and key.
	if !errors.As(f.native.closeErr, &workDebt) {
		t.Fatal("native lifecycle did not retain the source permit owner")
	}
	used := budget.Snapshot().Used
	called := false
	if err := f.native.WithSelectedStartupOwner(ctx, budget, selected, f.o.DirectoryOptions.Identity, func(context.Context, SelectedStartupSource) error { called = true; return nil }); err == nil || called {
		t.Fatal("unknown sealed reader exit admitted another source", called, err)
	}
	if _, err := f.native.ReplayAuthenticated(ctx, f.native.startupSaved, nil); err == nil {
		t.Fatal("unknown sealed reader exit admitted authenticated replay")
	}
	if _, err := f.native.Replay(f.native.startupSaved.Head, nil); !errors.Is(err, ErrProjectionUnavailable) || !errors.Is(err, ErrSegmentRetirement) {
		t.Fatal("unknown sealed reader exit admitted legacy replay or lost its debt", err)
	}
	if budget.Snapshot().Used != used || actual.closeCalls != 1 {
		t.Fatal("refused source changed existing debt or reopened the reader")
	}
	for i := 0; i < 2; i++ {
		if err := f.native.Close(); !errors.Is(err, ErrSegmentRetirement) || !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("native Close erased sealed reader debt", err)
		}
		if f.native.replayReaderDebt != retained || !retainsActualStream() || len(f.native.chainKey) != 32 {
			t.Fatal("native Close discarded reader or key")
		}
		if cost, err := workDebt.work.Cost(); err != nil || cost.RAM != AuthenticatedLedgerSourceRAMBytes {
			t.Fatal("native Close retired outstanding source credit", cost, err)
		}
	}
}
