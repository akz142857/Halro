package usage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func signArchiveTestCursor(t *testing.T, e *Exporter, state archiveFilesCursor) string {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	h := hmac.New(sha256.New, e.cursorKey[:])
	h.Write(raw)
	return base64.RawURLEncoding.EncodeToString(append(raw, h.Sum(nil)...))
}

func TestArchiveReaderActualFileLifetimeBlocksReclamationAndClose(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			_, e := dateArchiveFixture(t, format, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opened := make(chan *os.File, 1)
			release := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			readDone := make(chan error, 1)
			go func() {
				readDone <- e.verifyArchiveFileChecksumsWithHook(ctx, func(f *os.File) error {
					opened <- f
					<-release // deliberately cannot be interrupted by cancellation
					return nil
				})
			}()
			var f *os.File
			select {
			case f = <-opened:
			case <-time.After(5 * time.Second):
				t.Fatal("no actual open")
			}
			called := false
			if err := e.withArchiveReclamation(context.Background(), func(advance func() error) error { called = true; return advance() }); !errors.Is(err, ErrDiskUsageBusy) || called || e.archiveEpoch != 0 {
				t.Fatal("reclaimed while file was open", err, called)
			}
			cancel()
			if _, err := f.Stat(); err != nil {
				t.Fatal("cancellation closed the borrowed descriptor", err)
			}
			closed := make(chan error, 1)
			go func() { closed <- e.Close() }()
			select {
			case err := <-closed:
				t.Fatal("Close returned before file exit", err)
			case <-time.After(50 * time.Millisecond):
			}
			if _, err := e.ReadArchivePosition(context.Background()); !errors.Is(err, ErrDiskUsageClosed) {
				t.Fatal(err)
			}
			once.Do(func() { close(release) })
			if err := <-readDone; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDiskUsageClosed) {
				t.Fatal(err)
			}
			if err := <-closed; err != nil {
				t.Fatal(err)
			}
			if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatal("descriptor survived join", err)
			}
			if len(e.archiveReaders) != 0 {
				t.Fatal("permit survived join")
			}
		})
	}
}

func TestArchiveReaderSlotsAreFiniteAndNestedHelpersShareOne(t *testing.T) {
	_, e := dateArchiveFixture(t, FormatNDJSON, false)
	var finish []func() error
	for i := 0; i < ArchiveReaderSlots-1; i++ {
		done, err := e.beginArchiveRead(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		finish = append(finish, done)
	}
	defer func() {
		for _, done := range finish {
			if err := done(); err != nil {
				t.Error(err)
			}
		}
	}()
	if _, err := e.ReadArchiveFiles(context.Background(), "", 1); err != nil {
		t.Fatal(err)
	}
	if err := e.ValidateArchiveCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.VerifyArchiveFileChecksums(context.Background()); err != nil {
		t.Fatal(err)
	}
	last, err := e.beginArchiveRead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := e.ReadArchiveFiles(context.Background(), "", 1); !errors.Is(err, ErrDiskUsageBusy) || out.LastSequence != 0 || len(out.Files) != 0 || out.NextCursor != "" {
		t.Fatal(out, err)
	}
	if _, err := e.beginArchiveRead(context.Background()); !errors.Is(err, ErrDiskUsageBusy) {
		t.Fatal(err)
	}
	if err := last(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveReaderRAMReservationCoversLifetimeAndRefusalReleasesSlot(t *testing.T) {
	_, e := dateArchiveFixture(t, FormatNDJSON, false)
	acquired, released := 0, 0
	e.archiveRAMReserve = func(context.Context) (func() error, error) {
		acquired++
		return func() error { released++; return nil }, nil
	}
	done, err := e.beginArchiveRead(context.Background())
	if err != nil || acquired != 1 || released != 0 {
		t.Fatal("reader RAM was not reserved for the accepted operation", acquired, released, err)
	}
	if err := done(); err != nil || released != 1 {
		t.Fatal("reader RAM reservation did not follow reader lifetime", released, err)
	}
	refusal := errors.New("resource budget refused archive reader")
	e.archiveRAMReserve = func(context.Context) (func() error, error) {
		return nil, refusal
	}
	if _, err := e.beginArchiveRead(context.Background()); !errors.Is(err, refusal) {
		t.Fatal("budget refusal did not reject the archive reader", err)
	}
	e.archiveRAMReserve = nil
	done, err = e.beginArchiveRead(context.Background())
	if err != nil {
		t.Fatal("refused reservation leaked the fixed reader slot", err)
	}
	if err := done(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveReclamationEpochRejectsStaleAndEOFBeforeOpeningFiles(t *testing.T) {
	f, e := dateArchiveFixture(t, FormatNDJSON, false)
	first, err := e.ReadArchiveFiles(context.Background(), "", 1)
	if err != nil || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	old := first.NextCursor
	// A real finite append must preserve the captured frontier and immutable pages.
	for _, event := range windowRequest("more", true, true) {
		appendDiskDetailEvent(t, f, event)
	}
	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	if page, err := e.ReadArchiveFiles(context.Background(), old, 1); err != nil || page.LastSequence != first.LastSequence {
		t.Fatal("append invalidated frozen cursor", page, err)
	}
	eof := signArchiveTestCursor(t, e, archiveFilesCursor{Version: archiveFilesCursorVersion, Epoch: 0, LastSequence: first.LastSequence})
	if page, err := e.ReadArchiveFiles(context.Background(), eof, 1); err != nil || page.LastSequence != first.LastSequence || len(page.Files) != 0 {
		t.Fatal(page, err)
	}
	legacy := signArchiveTestCursor(t, e, archiveFilesCursor{Epoch: 0})
	if _, err := e.ReadArchiveFiles(context.Background(), legacy, 1); !errors.Is(err, ErrDiskUsageCursor) {
		t.Fatal("versionless cursor accepted", err)
	}
	if err := e.withArchiveReclamation(context.Background(), func(advance func() error) error { return nil }); err != nil || e.archiveEpoch != 0 {
		t.Fatal("no-op advanced epoch", err)
	}
	injected := errors.New("publication failed after invalidation")
	if err := e.withArchiveReclamation(context.Background(), func(advance func() error) error {
		if err := advance(); err != nil {
			return err
		}
		if err := advance(); err != nil {
			return err
		}
		return injected
	}); !errors.Is(err, injected) || e.archiveEpoch != 1 {
		t.Fatal(err, e.archiveEpoch)
	}
	for _, cursor := range []string{old, eof} {
		page, err := e.ReadArchiveFiles(context.Background(), cursor, 1)
		if !errors.Is(err, ErrDiskUsageCursor) || page.LastSequence != 0 || len(page.Files) != 0 || page.NextCursor != "" {
			t.Fatal("stale cursor returned data", page, err)
		}
	}
	if _, err := e.ReadArchiveFiles(context.Background(), "", 1); err != nil {
		t.Fatal(err)
	}
	e.archiveEpoch = math.MaxUint64
	mutated := false
	if err := e.withArchiveReclamation(context.Background(), func(advance func() error) error {
		if err := advance(); err != nil {
			return err
		}
		mutated = true
		return nil
	}); !errors.Is(err, ErrDiskUsageLimit) || mutated || e.archiveEpoch != math.MaxUint64 {
		t.Fatal(err, mutated)
	}
}

func TestArchiveRetirementJoinsActualPublisherAndWithdrawsProof(t *testing.T) {
	f, e := dateArchiveFixture(t, FormatNDJSON, false)
	for _, event := range windowRequest("new", true, true) {
		appendDiskDetailEvent(t, f, event)
	}
	rename := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	done := make(chan error, 1)
	go func() {
		out, err := e.exportDiskWithRootHook(context.Background(), f.p, DefaultDiskExportLimits(), true, false, func() error { close(rename); <-release; return nil })
		if out != (DiskArchiveCut{}) {
			done <- errors.New("retired publisher returned receipt")
			return
		}
		done <- err
	}()
	select {
	case <-rename:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not rename")
	}
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close returned before publisher barrier", err)
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	if err := <-done; !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if e.windowProof != nil || e.windowPending != nil || e.cursorKey != ([32]byte{}) {
		t.Fatal("retired authority remained")
	}
	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if _, err := e.PruneBefore(time.Now()); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
}

func TestArchiveLegacyPruneUsesReaderExclusionAndAdvancesOnlyOnMutation(t *testing.T) {
	_, e := dateArchiveFixture(t, FormatNDJSON, true)
	ctx := context.Background()
	path := filepath.Join(e.root, "manifest.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	finish, err := e.beginArchiveRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.PruneBefore(archiveDateCutoff)
	if !errors.Is(err, ErrDiskUsageBusy) || out != (RetentionReport{}) {
		t.Fatal(out, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) || e.archiveEpoch != 0 {
		t.Fatal("busy prune changed root", err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	out, err = e.PruneBefore(time.Date(1, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || out.FilesRemoved != 0 || e.archiveEpoch != 0 {
		t.Fatal("no-op changed epoch", out, err)
	}
	out, err = e.PruneBefore(archiveDateCutoff)
	if err != nil || out.FilesRemoved < 1 || out.RowsRemoved != 4 || e.archiveEpoch != 1 {
		t.Fatal(out, err, e.archiveEpoch)
	}
	manifest, err := e.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Files {
		if entry.Date < archiveDateCutoff.Format("2006-01-02") {
			t.Fatal("expired descriptor survived", entry)
		}
		if _, err := os.Stat(filepath.Join(e.root, entry.Path)); err != nil {
			t.Fatal("retained file deleted", err)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.LoadManifest(); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := e.Verify(nil); !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if out, err := e.Reconcile(Snapshot{}); !errors.Is(err, ErrDiskUsageClosed) || out != (ReconciliationReport{}) {
		t.Fatal(out, err)
	}
}
