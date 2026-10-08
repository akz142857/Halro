package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func dateReclaimStepFixture(t *testing.T, format string, n int) (*diskFailureFixture, *Exporter, []ManifestFile) {
	t.Helper()
	var events []ledger.Event
	for i := 0; i < n; i++ {
		for _, event := range windowRequest(fmt.Sprintf("reclaim_%d", i), false, false) {
			event.OccurredAt = event.OccurredAt.AddDate(0, 0, -i)
			events = append(events, event)
		}
	}
	f := newDiskFailureFixture(t, events)
	e, err := NewExporterWithOptions(t.TempDir(), datePolicyTestOptions(f, format))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if _, err = e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err = e.MigrateArchiveCatalog(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	var all []ManifestFile
	for cursor := ""; ; {
		page, err := e.ReadArchiveFiles(context.Background(), cursor, 64)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Files...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(all) != n {
		t.Fatal("partitions", len(all), n)
	}
	finishDateCandidate(t, e, archiveDateCutoff)
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	if _, err = e.ActivateRetainedDateCatalog(context.Background(), f.p, archiveDateCutoff); err != nil {
		t.Fatal(err)
	}
	return f, e, all
}

func finishDateReclaimStep(t *testing.T, f *diskFailureFixture, e *Exporter, l DateReclaimStepLimits) DateReclaimProgress {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if e.dateProof == nil {
			authorizeDateReclaimStep(t, f, e)
		}
		out, err := e.ReclaimRetainedDateStep(context.Background(), f.p, archiveDateCutoff, l)
		if err != nil {
			t.Fatal(i, out, err)
		}
		if out.EntriesProcessed > l.Entries || out.PagesRead > l.Pages || out.FilesRemoved+out.FilesMissing > l.Files || out.RowsVerified > l.File.FileRows {
			t.Fatal("unbounded step", out)
		}
		if out.Phase == "ready" {
			return out
		}
	}
	t.Fatal("cleanup did not finish")
	return DateReclaimProgress{}
}

func authorizeDateReclaimStep(t *testing.T, f *diskFailureFixture, e *Exporter) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		out, err := e.VerifyPublishedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits())
		if err != nil {
			t.Fatal(err)
		}
		if out.Phase == "ready" {
			finishDateReferences(t, e, DefaultDateCatalogReferenceLimits())
			return
		}
	}
	t.Fatal("published comparison stalled")
}

func TestArchiveDateReclaimStepMultiplePagesAndAggregateRows(t *testing.T) {
	f, e, files := dateReclaimStepFixture(t, FormatNDJSON, 70)
	l := DefaultDateReclaimStepLimits()
	l.File.FileRows = 3
	authorizeDateReclaimStep(t, f, e)
	out, err := e.ReclaimRetainedDateStep(context.Background(), f.p, archiveDateCutoff, l)
	if err != nil || out.FilesRemoved != 1 || out.RowsVerified != 2 || out.Visited != 1 {
		t.Fatal(out, err)
	}
	l.File.FileRows = 32
	l.Files = 16
	done := finishDateReclaimStep(t, f, e, l)
	if done.Visited != 70 || done.Eligible != 70 {
		t.Fatal(done)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = NewExporterWithOptions(e.root, datePolicyTestOptions(f, FormatNDJSON))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	l.Entries, l.Pages = 16, 1
	authorizeDateReclaimStep(t, f, e)
	first, err := e.ReclaimRetainedDateStep(context.Background(), f.p, archiveDateCutoff, l)
	if err != nil || first.Phase != "revalidating" || first.ReplayEntries == 0 || first.ReplayEntries > 16 || first.PagesRead != 1 {
		t.Fatal(first, err)
	}
	again := finishDateReclaimStep(t, f, e, l)
	if again.Visited != 70 || again.LogicalBytesUnlinked != 0 || again.FilesMissing != 0 {
		t.Fatal(again)
	}
	for _, entry := range files {
		if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(entry.Path))); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func TestArchiveDateReclaimStepReplayExactlyAtCapContinuesSameOwner(t *testing.T) {
	f, e, _ := dateReclaimStepFixture(t, FormatNDJSON, 8)
	l := DefaultDateReclaimStepLimits()
	l.Entries = 4
	authorizeDateReclaimStep(t, f, e)
	first, err := e.ReclaimRetainedDateStep(context.Background(), f.p, archiveDateCutoff, l)
	if err != nil || first.Visited != 4 || first.FilesRemoved != 4 || first.Phase != "data" {
		t.Fatal(first, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = NewExporterWithOptions(e.root, datePolicyTestOptions(f, FormatNDJSON))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	authorizeDateReclaimStep(t, f, e)
	replay, err := e.ReclaimRetainedDateStep(context.Background(), f.p, archiveDateCutoff, l)
	if err != nil || replay.Phase != "revalidating" || replay.ReplayEntries != 4 || replay.EntriesProcessed != 4 || e.dateProof == nil {
		t.Fatal("exact-cap replay cannot hand off to new work", replay, err)
	}
	next, err := e.ReclaimRetainedDateStep(context.Background(), f.p, archiveDateCutoff, l)
	if err != nil || next.Phase != "ready" || next.Visited != 8 || next.FilesRemoved != 4 || e.dateProof != nil {
		t.Fatal("same fresh owner failed to extend after replay", next, err)
	}
}

func TestArchiveDateReclaimStepMissingFilesDoNotConsumeDecodeRows(t *testing.T) {
	for _, fault := range []string{"all-missing", "missing-after-row-cap"} {
		t.Run(fault, func(t *testing.T) {
			f, e, files := dateReclaimStepFixture(t, FormatNDJSON, 4)
			start, rows := 0, 1
			if fault == "missing-after-row-cap" {
				start, rows = 2, 4
			}
			for _, entry := range files[start:] {
				if err := os.Remove(filepath.Join(e.root, filepath.FromSlash(entry.Path))); err != nil {
					t.Fatal(err)
				}
			}
			l := DefaultDateReclaimStepLimits()
			l.File.FileRows = rows
			authorizeDateReclaimStep(t, f, e)
			out, err := e.ReclaimRetainedDateStep(context.Background(), f.p, archiveDateCutoff, l)
			if err != nil || out.Phase != "ready" || out.Visited != 4 || out.FilesRemoved != start || out.FilesMissing != 4-start || out.RowsVerified != 2*start || !out.ObligationPending {
				t.Fatal("missing descriptors exhausted decoder budget", out, err)
			}
		})
	}
}

func TestArchiveDateReclaimStepProtectsCurrentReferencesAndResetsOnlyProgress(t *testing.T) {
	f, e, entry := dateReclaimFileFixture(t, FormatNDJSON)
	ctx := context.Background()
	baseline := f.a.Watermark().Sequence
	for _, event := range windowRequest("late-current-ref", false, false) {
		appendDiskDetailEvent(t, f, event)
	}
	var late []AttemptEvent
	for _, row := range f.a.Snapshot().Attempts {
		if row.Sequence > baseline {
			late = append(late, row)
		}
	}
	current := publishDateComparisonFault(t, e, entry.Date, "current-ref", late)
	if err := os.Rename(filepath.Join(e.root, filepath.FromSlash(current.Path)), filepath.Join(e.root, filepath.FromSlash(entry.Path))); err != nil {
		t.Fatal(err)
	}
	current.Path = entry.Path
	root, err := openArchiveManifest(e.root)
	if err != nil {
		t.Fatal(err)
	}
	position, _, err := root.walk(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := archiveCatalogBuilder{exporter: e, ctx: ctx, head: position.CatalogHead, work: &archiveWork{remaining: 4 << 20}}
	if err := b.add(current); err != nil {
		t.Fatal(err)
	}
	if err := b.flush(); err != nil {
		t.Fatal(err)
	}
	if err := e.publishArchiveCatalogRootWithRetention(ctx, root, false, f.a.Watermark().Sequence, false, b.head, position.Retention, &archiveWork{remaining: archiveManifestValueLimit}, nil); err != nil {
		t.Fatal(err)
	}
	if err := root.file.Close(); err != nil {
		t.Fatal(err)
	}
	done := finishDateReclaimStep(t, f, e, DefaultDateReclaimStepLimits())
	if done.Visited != 2 || done.Eligible != 0 || done.FilesRemoved != 0 || done.Current.Files != 2 {
		t.Fatal(done)
	}
	if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(entry.Path))); err != nil {
		t.Fatal("referenced file removed", err)
	}
	if err := e.ResetRetainedDateReclamation(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.root, archiveDateReclaimName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if e.dateReclaimReplay != nil || e.dateProof != nil {
		t.Fatal("reset retained authority/cache")
	}
	if _, err := os.Stat(filepath.Join(e.root, archiveDateActivationName)); err != nil {
		t.Fatal("reset erased obligation", err)
	}
}

func TestArchiveDateReclaimStepFiniteBothFormatsAndReadyReplay(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e, files := dateReclaimStepFixture(t, format, 8)
			l := DefaultDateReclaimStepLimits()
			l.Entries, l.Files, l.File.FileRows = 1, 1, 2
			done := finishDateReclaimStep(t, f, e, l)
			if done.Visited != 8 || done.Eligible != 8 || !done.ObligationPending || done.Current.Files != 0 {
				t.Fatal(done)
			}
			for _, entry := range files {
				if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(entry.Path))); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			e, err = NewExporterWithOptions(e.root, datePolicyTestOptions(f, format))
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			authorizeDateFileReclaim(t, f, e)
			first, err := e.ReclaimRetainedDateStep(context.Background(), f.p, archiveDateCutoff, l)
			if err != nil || first.Phase != "revalidating" || first.ReplayEntries != 1 || first.Visited != 0 || first.Current != (ArchivePosition{}) || e.dateProof == nil || first.FilesRemoved != 0 {
				t.Fatal(first, err)
			}
			again := finishDateReclaimStep(t, f, e, l)
			if again.Visited != 8 || again.Eligible != 8 || again.FilesRemoved != 0 || again.FilesMissing != 0 || again.LogicalBytesUnlinked != 0 {
				t.Fatal(again)
			}
			if _, err := os.Stat(filepath.Join(e.root, archiveDateActivationName)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArchiveDateReclaimStepPartialReopenAndCheckpointForgery(t *testing.T) {
	for _, fault := range []string{"partial", "offset", "prefix", "ready", "recreated", "symlink", "oversize", "source"} {
		t.Run(fault, func(t *testing.T) {
			f, e, files := dateReclaimStepFixture(t, FormatNDJSON, 4)
			l := DefaultDateReclaimStepLimits()
			l.Entries, l.Files = 1, 1
			path := filepath.Join(e.root, archiveDateReclaimName)
			authorizeDateFileReclaim(t, f, e)
			first, err := e.ReclaimRetainedDateStep(context.Background(), f.p, archiveDateCutoff, l)
			if err != nil || first.Visited != 1 || first.FilesRemoved != 1 {
				t.Fatal(first, err)
			}
			s, _, file, _, err := readDateReclaimState(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if err = file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			e, err = NewExporterWithOptions(e.root, datePolicyTestOptions(f, FormatNDJSON))
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			switch fault {
			case "offset":
				s.Offset++
				s.Visited++
			case "prefix":
				s.PrefixSHA = "0000000000000000000000000000000000000000000000000000000000000000"
			case "ready":
				s.Next = ArchivePageRef{}
				s.Offset = 0
				s.Pages = s.Old.CatalogHead.Pages
				s.Visited = s.Old.Files
				s.Eligible = s.Visited
			case "recreated":
				// A claimed processed object that reappears cannot be skipped or deleted.
				if err := os.WriteFile(filepath.Join(e.root, filepath.FromSlash(files[0].Path)), []byte("foreign replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".foreign", path); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(path, make([]byte, archiveDateBuildBytes+1), 0o600); err != nil {
					t.Fatal(err)
				}
			case "source":
				s.Head.Sequence++
			}
			if fault == "offset" || fault == "prefix" || fault == "ready" || fault == "source" {
				raw, err := json.Marshal(s)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			authorizeDateFileReclaim(t, f, e)
			if fault == "partial" {
				out := finishDateReclaimStep(t, f, e, l)
				if out.Visited != 4 || out.Eligible != 4 {
					t.Fatal(out)
				}
				return
			}
			for i := 0; i < 10; i++ {
				out, err := e.ReclaimRetainedDateStep(context.Background(), f.p, archiveDateCutoff, l)
				if err != nil {
					if out != (DateReclaimProgress{}) || e.dateReclaimReplay != nil || e.dateProof != nil {
						t.Fatal(out, err)
					}
					return
				}
				if out.Phase != "revalidating" {
					t.Fatal("saved progress bypassed replay", out)
				}
			}
			t.Fatal("invalid checkpoint accepted")
		})
	}
}

func TestArchiveDateReclaimStepPublicationFailureAndActualJoin(t *testing.T) {
	for _, fault := range []string{"before-save", "after-rename", "cancel-join"} {
		t.Run(fault, func(t *testing.T) {
			f, e, _ := dateReclaimStepFixture(t, FormatNDJSON, 4)
			l := DefaultDateReclaimStepLimits()
			l.Entries, l.Files = 1, 1
			authorizeDateFileReclaim(t, f, e)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stop := errors.New("interrupted checkpoint publication")
			var before, after func() error
			if fault == "after-rename" {
				after = func() error { return stop }
			} else if fault == "before-save" {
				before = func() error { return stop }
			} else {
				entered, release := make(chan struct{}), make(chan struct{})
				before = func() error { close(entered); <-release; return nil }
				done := make(chan error, 1)
				go func() {
					out, err := e.reclaimRetainedDateStep(ctx, f.p, archiveDateCutoff, l, before, nil)
					if out != (DateReclaimProgress{}) {
						err = errors.Join(err, errors.New("retired step returned progress"))
					}
					done <- err
				}()
				<-entered
				cancel()
				closed := make(chan error, 1)
				go func() { closed <- e.Close() }()
				deadline := time.Now().Add(time.Second)
				for !e.archiveRetired.Load() && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				select {
				case err := <-closed:
					t.Fatal("Close passed physical operation", err)
				default:
				}
				close(release)
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
				if e.dateReclaimReplay != nil || e.dateProof != nil {
					t.Fatal("Close retained authority/cache")
				}
				return
			}
			out, err := e.reclaimRetainedDateStep(ctx, f.p, archiveDateCutoff, l, before, after)
			if !errors.Is(err, stop) || out != (DateReclaimProgress{}) || e.dateReclaimReplay != nil || e.dateProof != nil {
				t.Fatal(out, err)
			}
			if _, err := os.Stat(filepath.Join(e.root, archiveDateActivationName)); err != nil {
				t.Fatal("obligation erased", err)
			}
			again := finishDateReclaimStep(t, f, e, l)
			if again.Visited != 4 || !again.ObligationPending {
				t.Fatal(again)
			}
		})
	}
}
