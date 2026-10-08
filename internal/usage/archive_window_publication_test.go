package usage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func publicationWindowFixture(t *testing.T, format string, preserve bool) (*diskFailureFixture, *Exporter) {
	t.Helper()
	f := newDiskFailureFixture(t, windowRequest("first", false, false))
	e, err := NewExporterWithOptions(t.TempDir(), Options{Format: format})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.exportDisk(context.Background(), f.p, DefaultDiskExportLimits(), false, preserve); err != nil {
		t.Fatal(err)
	}
	return f, e
}

func drainVerifiedWindow(t *testing.T, f *diskFailureFixture, e *Exporter) {
	t.Helper()
	for i := 0; i < 100; i++ {
		out, err := e.PruneVerifiedWindowStep(context.Background(), f.p, windowTestCutoff, windowTestLimits)
		if err != nil {
			t.Fatal(out, err)
		}
		if out.Done {
			return
		}
	}
	t.Fatal("no finite window progress")
}

func TestArchiveWindowPublicationExtendsFinitePrefixAfterActualTrim(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		for _, preserve := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy_%t", format, preserve), func(t *testing.T) {
				f, e := publicationWindowFixture(t, format, preserve)
				ctx := context.Background()
				drainVerifiedWindow(t, f, e)
				stats, err := f.p.ReadStats(ctx)
				if err != nil || stats.Window.Attempts != 0 || stats.Window.AttemptAfter == 0 {
					t.Fatal(stats, err)
				}
				events := windowRequest("first", false, false)
				for _, id := range []string{"second", "third", "keeper"} {
					next := windowRequest(id, id == "keeper", id == "keeper")
					events = append(events, next...)
					for _, event := range next {
						appendDiskDetailEvent(t, f, event)
					}
				}
				limits := DefaultDiskExportStepLimits()
				limits.Source.Rows = 1
				var last uint64
				for i := 0; i < 100; i++ {
					cut, err := e.exportDisk(ctx, f.p, limits, true, preserve)
					if err != nil || cut.ThroughSequence <= last {
						t.Fatal(cut, err)
					}
					last = cut.ThroughSequence
					if e.windowProof == nil || e.windowProof.position.LastSequence != last {
						t.Fatal("missing publisher proof", cut)
					}
					drainVerifiedWindow(t, f, e)
					if cut.ThroughSequence == cut.Head.Sequence {
						break
					}
					if i == 99 {
						t.Fatal("publisher failed to advance")
					}
				}
				stats, err = f.p.ReadStats(ctx)
				if err != nil || stats.Window.Attempts != 2 || stats.Window.Summaries != 1 || last != f.a.Watermark().Sequence {
					t.Fatal(stats, last, err)
				}
				// Independently reconstruct the complete authenticated source, including
				// facts already deleted from the publishing projection, then compare all
				// archive rows. Continuation never replays those facts into the live owner.
				reference := newDiskFailureFixture(t, events)
				reopened, err := NewExporter(e.root)
				if err != nil {
					t.Fatal(err)
				}
				proof, err := reopened.VerifyDiskArchive(ctx, reference.p, DefaultArchiveVerifyLimits())
				if err != nil || proof.SourceRows != 8 || proof.ArchiveRows != 8 {
					t.Fatal(proof, err)
				}
			})
		}
	}
}

func TestArchiveWindowUnacknowledgedPublicationRepeatsBarriersAfterTrim(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		t.Run(fmt.Sprint(preserve), func(t *testing.T) {
			f, e := publicationWindowFixture(t, FormatNDJSON, preserve)
			drainVerifiedWindow(t, f, e)
			for _, event := range windowRequest("next", false, false) {
				appendDiskDetailEvent(t, f, event)
			}
			injected := errors.New("post-rename directory barrier unavailable")
			cut, err := e.exportDiskWithRootHook(context.Background(), f.p, DefaultDiskExportLimits(), true, preserve, func() error { return injected })
			if !errors.Is(err, injected) || cut != (DiskArchiveCut{}) || e.windowProof != nil || e.windowPending == nil {
				t.Fatal(cut, err)
			}
			position, err := e.ReadArchivePosition(context.Background())
			if err != nil || position.LastSequence != f.a.Watermark().Sequence {
				t.Fatal(position, err)
			}
			digest := windowDigest(t, f.p, diskUsageBuckets)
			if out, err := e.PruneVerifiedWindowStep(context.Background(), f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) || out != (DiskWindowProgress{}) || windowDigest(t, f.p, diskUsageBuckets) != digest {
				t.Fatal(out, err)
			}
			cut, err = e.exportDisk(context.Background(), f.p, DefaultDiskExportLimits(), true, preserve)
			if err != nil || cut.AfterSequence != position.LastSequence || e.windowPending != nil || e.windowProof == nil {
				t.Fatal(cut, err)
			}
			drainVerifiedWindow(t, f, e)
			stats, err := f.p.ReadStats(context.Background())
			if err != nil || stats.Window.Attempts != 0 || stats.Window.Summaries != 0 {
				t.Fatal(stats, err)
			}
		})
	}
}

func TestArchiveWindowPendingRootReplacementAndReopenCannotInherit(t *testing.T) {
	for _, mutation := range []string{"inode", "directory", "exporter", "owner"} {
		t.Run(mutation, func(t *testing.T) {
			f, e := publicationWindowFixture(t, FormatNDJSON, false)
			for _, event := range windowRequest("next", false, false) {
				appendDiskDetailEvent(t, f, event)
			}
			_, err := e.exportDiskWithRootHook(context.Background(), f.p, DefaultDiskExportLimits(), true, false, func() error { return errors.New("unacknowledged") })
			if err == nil || e.windowPending == nil {
				t.Fatal(err)
			}
			path := filepath.Join(e.root, "manifest.json")
			switch mutation {
			case "inode":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".replacement", raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".replacement", path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				old := e.root + ".old"
				if err := os.Rename(e.root, old); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(e.root, 0o700); err != nil {
					t.Fatal(err)
				}
				entries, err := os.ReadDir(old)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					from, to := filepath.Join(old, entry.Name()), filepath.Join(e.root, entry.Name())
					if entry.IsDir() {
						err = os.Rename(from, to)
					} else {
						err = os.Link(from, to)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			case "exporter":
				e, err = NewExporter(e.root)
				if err != nil {
					t.Fatal(err)
				}
			case "owner":
				f = newDiskFailureFixture(t, append(windowRequest("first", false, false), windowRequest("next", false, false)...))
			}
			// Even if an export operation succeeds from this visible frontier, it
			// cannot turn a replaced/unknown receipt into permission to delete rows.
			if _, err := e.ExportDiskStep(context.Background(), f.p, DefaultDiskExportStepLimits()); err != nil {
				t.Fatal(err)
			}
			digest := windowDigest(t, f.p, diskUsageBuckets)
			if out, err := e.PruneVerifiedWindowStep(context.Background(), f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) || out != (DiskWindowProgress{}) || windowDigest(t, f.p, diskUsageBuckets) != digest {
				t.Fatal(out, err)
			}
		})
	}
}

func TestArchiveWindowPublicationCancellationAndCloseJoinActualRename(t *testing.T) {
	f, e := publicationWindowFixture(t, FormatNDJSON, false)
	for _, event := range windowRequest("next", false, false) {
		appendDiskDetailEvent(t, f, event)
	}
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
	done := make(chan error, 1)
	go func() {
		cut, err := e.exportDiskWithRootHook(ctx, f.p, DefaultDiskExportLimits(), true, false, func() error { close(entered); <-release; return nil })
		if cut != (DiskArchiveCut{}) {
			done <- errors.New("retired publisher returned success")
			return
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no actual rename")
	}
	cancel()
	// Publication still owns the Exporter mutex and physical heavy slot.
	if _, err := e.PruneVerifiedWindowStep(context.Background(), f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrDiskUsageBusy) {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- f.p.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for !f.p.retired.Load() {
		if time.Now().After(deadline) {
			t.Fatal("owner did not retire")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatal("Close escaped publisher", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if e.windowProof != nil {
		t.Fatal("cancelled publication minted permission")
	}
}

func TestArchiveWindowFailedSourceCannotInventGenesisPublication(t *testing.T) {
	f := newDiskFailureFixture(t, windowRequest("first", false, false))
	e, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultDiskExportLimits()
	limits.Source.Rows = 1
	if cut, err := e.ExportDisk(context.Background(), f.p, limits); !errors.Is(err, ErrDiskUsageLimit) || cut != (DiskArchiveCut{}) {
		t.Fatal(cut, err)
	}
	if e.windowProof != nil || e.windowPending != nil {
		t.Fatal("incomplete source minted genesis")
	}
	if out, err := e.PruneVerifiedWindowStep(context.Background(), f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) || out != (DiskWindowProgress{}) {
		t.Fatal(out, err)
	}
	// Remove all attempts via the internal primitive, then make a missing root:
	// a missing catalog cannot authorize inventing the lost source prefix.
	existing, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := existing.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	drainVerifiedWindow(t, f, existing)
	if cut, err := e.ExportDiskStep(context.Background(), f.p, DefaultDiskExportStepLimits()); !errors.Is(err, ErrDiskUsageCursor) || cut != (DiskArchiveCut{}) {
		t.Fatal(cut, err)
	}
	if e.windowProof != nil || e.windowPending != nil {
		t.Fatal("trimmed source minted genesis")
	}
}

func TestArchiveWindowPublicationCleanupFailureRequiresExactRetry(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		t.Run(fmt.Sprint(preserve), func(t *testing.T) {
			f, e := publicationWindowFixture(t, FormatNDJSON, preserve)
			drainVerifiedWindow(t, f, e)
			for _, event := range windowRequest("next", false, false) {
				appendDiskDetailEvent(t, f, event)
			}
			var replacement string
			cut, err := e.exportDiskWithRootHook(context.Background(), f.p, DefaultDiskExportLimits(), true, preserve, func() error {
				entries, err := os.ReadDir(e.root)
				if err != nil {
					return err
				}
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".archive-entries-") {
						replacement = filepath.Join(e.root, entry.Name())
						// Keep the actual descriptor alive but replace its named path. Cleanup
						// must close the descriptor, preserve the replacement and fail the job.
						if err := os.Rename(replacement, replacement+".owned"); err != nil {
							return err
						}
						return os.Mkdir(replacement, 0o700)
					}
				}
				return errors.New("no actual stage descriptor")
			})
			if !errors.Is(err, ErrDiskUsageCorrupt) || cut != (DiskArchiveCut{}) || e.windowProof != nil || e.windowPending == nil {
				t.Fatal(cut, err)
			}
			if info, err := os.Stat(replacement); err != nil || !info.IsDir() {
				t.Fatal("cleanup removed replacement", info, err)
			}
			if out, err := e.PruneVerifiedWindowStep(context.Background(), f.p, windowTestCutoff, windowTestLimits); !errors.Is(err, ErrArchiveWindowProof) || out != (DiskWindowProgress{}) {
				t.Fatal(out, err)
			}
			cut, err = e.exportDisk(context.Background(), f.p, DefaultDiskExportLimits(), true, preserve)
			if err != nil || cut.AfterSequence != f.a.Watermark().Sequence || e.windowProof == nil || e.windowPending != nil {
				t.Fatal(cut, err)
			}
			drainVerifiedWindow(t, f, e)
		})
	}
}
