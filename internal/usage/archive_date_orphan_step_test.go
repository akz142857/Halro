package usage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func orphanStep(t *testing.T, f *diskFailureFixture, e *Exporter, limits DateOrphanReclaimLimits) DateOrphanReclaimProgress {
	t.Helper()
	if e.dateProof == nil {
		authorizeDateReclaimStep(t, f, e)
	}
	out, err := e.ReclaimRetainedDateOrphansStep(context.Background(), f.p, archiveDateCutoff, limits)
	if err != nil {
		t.Fatal(out, err)
	}
	if out.EntriesScanned > limits.Entries || out.FilesExamined > limits.Files || out.RowsInspected > limits.InspectRows || out.RowsVerified > limits.File.FileRows || !out.ObligationPending {
		t.Fatal("orphan step escaped its aggregate limits", out, limits)
	}
	return out
}

func TestArchiveDateOrphanStepFiniteBothFormatsAndFreshScan(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e, files := dateReclaimStepFixture(t, format, 8)
			copyDateOrphan(t, e, files[0], "unlisted-copy")
			policy, err := os.ReadFile(filepath.Join(e.root, archiveDateActivationName))
			if err != nil {
				t.Fatal(err)
			}
			limits := DefaultDateOrphanReclaimLimits()
			limits.Entries, limits.Files = 3, 2
			limits.File.FileRows, limits.InspectRows = 3, 6
			removed, sawPending := 0, false
			for i := 0; i < 500; i++ {
				out := orphanStep(t, f, e, limits)
				removed += out.FilesRemoved
				if e.dateOrphans != nil && e.dateOrphans.pending != "" {
					sawPending = true
				}
				if out.Phase == "scan-complete" {
					if e.dateOrphans != nil || removed != 9 {
						t.Fatal(out, removed, e.dateOrphans)
					}
					break
				}
				if i == 499 {
					t.Fatal("finite orphan scan stalled")
				}
			}
			if !sawPending {
				t.Fatal("did not exercise a pending name at an entry/row boundary")
			}
			for _, entry := range files {
				if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(entry.Path))); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			// A second fresh live scan does not trust the previous EOF as Ready.
			for i := 0; i < 100; i++ {
				out := orphanStep(t, f, e, limits)
				if out.FilesRemoved != 0 || out.LogicalBytesUnlinked != 0 {
					t.Fatal(out)
				}
				if out.Phase == "scan-complete" {
					break
				}
				if i == 99 {
					t.Fatal("fresh empty scan stalled")
				}
			}
			after, err := os.ReadFile(filepath.Join(e.root, archiveDateActivationName))
			if err != nil || !bytes.Equal(after, policy) {
				t.Fatal("scan retired or changed active policy", err)
			}
		})
	}
}

func TestArchiveDateOrphanStepReadOnlyPrefixBeyondChunkAndClose(t *testing.T) {
	f, e, _ := dateReclaimStepFixture(t, FormatNDJSON, 1)
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(filepath.Join(e.root, fmt.Sprintf(".inventory-prefix-%03d", i)), []byte("unowned metadata"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	authorizeDateReclaimStep(t, f, e)
	limits := DefaultDateOrphanReclaimLimits()
	limits.Entries = 1
	sawReadOnly := false
	var held *os.File
	for i := 0; i < 500; i++ {
		out := orphanStep(t, f, e, limits)
		if out.FilesRemoved == 0 && out.Phase != "scan-complete" {
			sawReadOnly = true
			if e.dateProof == nil {
				t.Fatal("read-only scan discarded same-owner comparison proof")
			}
		}
		if e.dateOrphans != nil {
			held = e.dateOrphans.scan
		}
		if out.FilesRemoved != 0 {
			break
		}
	}
	if !sawReadOnly || held == nil {
		t.Fatal("no live finite read-only inventory")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Stat(); !errors.Is(err, os.ErrClosed) || e.dateOrphans != nil || e.datePolicyKey != ([32]byte{}) {
		t.Fatal("Close failed to join/erase live inventory", err)
	}
}

func TestArchiveDateOrphanStepPreservesOutsidePolicyAndUnknownObjects(t *testing.T) {
	f, e, original := dateReclaimFileFixture(t, FormatNDJSON)
	baseline := f.a.Watermark().Sequence
	for _, event := range windowRequest("late-scan-orphan", false, false) {
		appendDiskDetailEvent(t, f, event)
	}
	var late []AttemptEvent
	for _, row := range f.a.Snapshot().Attempts {
		if row.Sequence > baseline {
			late = append(late, row)
		}
	}
	entry := publishDateComparisonFault(t, e, original.Date, "late-scan-orphan", late)
	unknown := filepath.Join(e.root, "date="+original.Date, "foreign.data")
	if err := os.WriteFile(unknown, []byte("unowned object"), 0600); err != nil {
		t.Fatal(err)
	}
	limits := DefaultDateOrphanReclaimLimits()
	outside, unrecognized, removed := 0, 0, 0
	for i := 0; i < 100; i++ {
		out := orphanStep(t, f, e, limits)
		outside += out.FilesOutsidePolicy
		unrecognized += out.FilesUnrecognized
		removed += out.FilesRemoved
		if out.Phase == "scan-complete" {
			break
		}
		if i == 99 {
			t.Fatal("out-of-scope object prevented bounded traversal")
		}
	}
	if outside != 1 || unrecognized != 1 || removed != 1 {
		t.Fatal(outside, unrecognized, removed)
	}
	for _, path := range []string{unknown, filepath.Join(e.root, filepath.FromSlash(entry.Path))} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("protected/unknown object removed", path, err)
		}
	}
	// A real joined physical error cannot disappear behind a policy skip.
	policy := errors.Join(ErrArchiveDateProof, errArchiveDateOrphanOutsidePolicy)
	if !onlyDateOutsidePolicy(policy) || onlyDateOutsidePolicy(ErrArchiveDateProof) || onlyDateOutsidePolicy(errors.Join(policy, os.ErrClosed)) || onlyDateOutsidePolicy(errors.Join(policy, ErrDiskUsageCorrupt)) {
		t.Fatal("outside-policy classification suppressed non-policy errors")
	}
}

func TestArchiveDateOrphanStepAggregateCapsAndPhysicalErrorsWithdraw(t *testing.T) {
	for _, fault := range []string{"inspect-rows", "file-bytes", "directory-close", "partition-replacement", "symlink", "after-unlink"} {
		t.Run(fault, func(t *testing.T) {
			f, e, files := dateReclaimStepFixture(t, FormatNDJSON, 2)
			limits := DefaultDateOrphanReclaimLimits()
			authorizeDateReclaimStep(t, f, e)
			var hook func() error
			switch fault {
			case "inspect-rows":
				limits.File.FileRows, limits.InspectRows = 1, 1
			case "file-bytes":
				limits.File.FileBytes = 1
			case "symlink":
				path := filepath.Join(e.root, filepath.FromSlash(files[0].Path))
				if err := os.Rename(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".foreign", path); err != nil {
					t.Fatal(err)
				}
			case "after-unlink":
				hook = func() error { return errors.New("actual unlink interrupted") }
			case "directory-close", "partition-replacement":
				limits.Entries = 1
				for i := 0; i < 100; i++ {
					out := orphanStep(t, f, e, limits)
					if e.dateOrphans != nil && (fault == "directory-close" || e.dateOrphans.partition != nil) {
						if fault == "directory-close" {
							if err := e.dateOrphans.scan.Close(); err != nil {
								t.Fatal(err)
							}
						} else {
							partition := filepath.Join(e.root, e.dateOrphans.partitionName)
							if err := os.Rename(partition, partition+".foreign"); err != nil {
								t.Fatal(err)
							}
							if err := os.Mkdir(partition, 0700); err != nil {
								t.Fatal(err)
							}
						}
						break
					}
					if out.Phase == "scan-complete" || i == 99 {
						t.Fatal("did not pin directory for fault")
					}
				}
			}
			out, err := e.reclaimRetainedDateOrphansStep(context.Background(), f.p, archiveDateCutoff, limits, hook)
			if err == nil || out != (DateOrphanReclaimProgress{}) || e.dateOrphans != nil || e.dateProof != nil {
				t.Fatal("physical error retained scan/result authority", out, err)
			}
			if _, err := os.Stat(filepath.Join(e.root, archiveDateActivationName)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArchiveDateOrphanStepFrozenSourceChangeAndExplicitReset(t *testing.T) {
	f, e, _ := dateReclaimStepFixture(t, FormatNDJSON, 2)
	limits := DefaultDateOrphanReclaimLimits()
	limits.Entries = 1
	orphanStep(t, f, e, limits)
	if e.dateOrphans == nil {
		t.Fatal("no bounded scan")
	}
	held := e.dateOrphans.scan
	for _, event := range windowRequest("native-appended-between-orphan-steps", true, true) {
		appendDiskDetailEvent(t, f, event)
	}
	if err := e.ResetPublishedDateCatalogVerification(context.Background()); err != nil {
		t.Fatal(err)
	}
	authorizeDateReclaimStep(t, f, e)
	out, err := e.ReclaimRetainedDateOrphansStep(context.Background(), f.p, archiveDateCutoff, limits)
	if !errors.Is(err, ErrArchiveDateBuildStale) || out != (DateOrphanReclaimProgress{}) || e.dateOrphans != nil {
		t.Fatal("silently rebased old scan", out, err)
	}
	if _, err := held.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("stale scan retained actual FD", err)
	}
	orphanStep(t, f, e, limits)
	if e.dateOrphans == nil {
		t.Fatal("no explicit replacement scan")
	}
	held = e.dateOrphans.scan
	if err := e.ResetRetainedDateOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Stat(); !errors.Is(err, os.ErrClosed) || e.dateOrphans != nil || e.dateProof != nil {
		t.Fatal("reset did not really close/withdraw scan", err)
	}
}

func TestArchiveDateOrphanStepCloseJoinsActualUnlinkAndScanFDs(t *testing.T) {
	f, e, _ := dateReclaimStepFixture(t, FormatNDJSON, 2)
	authorizeDateReclaimStep(t, f, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	done := make(chan error, 1)
	var held *os.File
	go func() {
		out, err := e.reclaimRetainedDateOrphansStep(ctx, f.p, archiveDateCutoff, DefaultDateOrphanReclaimLimits(), func() error { held = e.dateOrphans.scan; close(entered); <-release; return nil })
		if out != (DateOrphanReclaimProgress{}) {
			err = errors.Join(err, errors.New("retired scan returned success report"))
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("actual unlink not reached")
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close escaped actual callback", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, err := held.Stat(); !errors.Is(err, os.ErrClosed) || e.dateOrphans != nil || e.datePolicyKey != ([32]byte{}) {
		t.Fatal("actual scan FD/key survived retirement", err)
	}
}

func TestArchiveDateOrphanStepReopenRestartsActualInventory(t *testing.T) {
	f, e, _ := dateReclaimStepFixture(t, FormatNDJSON, 3)
	limits := DefaultDateOrphanReclaimLimits()
	limits.Files = 1
	first := orphanStep(t, f, e, limits)
	if first.FilesRemoved != 1 || e.dateOrphans == nil {
		t.Fatal(first)
	}
	held := e.dateOrphans.scan
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
	next, err := NewExporterWithOptions(e.root, datePolicyTestOptions(f, FormatNDJSON))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	if next.dateOrphans != nil {
		t.Fatal("new owner trusted a persisted directory offset")
	}
	removed := 0
	for i := 0; i < 50; i++ {
		out := orphanStep(t, f, next, limits)
		removed += out.FilesRemoved
		if out.Phase == "scan-complete" {
			break
		}
		if i == 49 {
			t.Fatal("reopened actual inventory stalled")
		}
	}
	if removed != 2 {
		t.Fatal("reopen guessed vanished entries instead of scanning actual names", removed)
	}
}

func TestArchiveDateOrphanStepTenThousandActualNamesStayFinite(t *testing.T) {
	f, e, _ := dateReclaimStepFixture(t, FormatNDJSON, 1)
	for i := 0; i < 10000; i++ {
		if err := os.WriteFile(filepath.Join(e.root, fmt.Sprintf(".inventory-large-%05d", i)), []byte("preserved"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	limits := DefaultDateOrphanReclaimLimits()
	limits.Entries = 64
	scanned, removed := 0, 0
	for i := 0; i < 500; i++ {
		out := orphanStep(t, f, e, limits)
		scanned += out.EntriesScanned
		removed += out.FilesRemoved
		if out.Phase == "scan-complete" {
			if e.dateOrphans != nil {
				t.Fatal("EOF retained scan descriptors")
			}
			break
		}
		if e.dateOrphans == nil || e.dateOrphans.root == nil || e.dateOrphans.scan == nil || len(e.dateOrphans.pending) > 4096 {
			t.Fatal("finite inventory lost its one owned root/name")
		}
		if i == 499 {
			t.Fatal("large actual directory did not advance")
		}
	}
	if scanned < 10000 || removed != 1 {
		t.Fatal(scanned, removed)
	}
}

func TestArchiveDateOrphanStepCurrentExpiredPathRemainsProtected(t *testing.T) {
	f, e, old := dateReclaimFileFixture(t, FormatNDJSON)
	baseline := f.a.Watermark().Sequence
	for _, event := range windowRequest("late-current-inventory-path", false, false) {
		appendDiskDetailEvent(t, f, event)
	}
	var late []AttemptEvent
	for _, row := range f.a.Snapshot().Attempts {
		if row.Sequence > baseline {
			late = append(late, row)
		}
	}
	current := publishDateComparisonFault(t, e, old.Date, "current-inventory", late)
	if err := os.Rename(filepath.Join(e.root, filepath.FromSlash(current.Path)), filepath.Join(e.root, filepath.FromSlash(old.Path))); err != nil {
		t.Fatal(err)
	}
	current.Path = old.Path
	ctx := context.Background()
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
	protected := 0
	for i := 0; i < 20; i++ {
		out := orphanStep(t, f, e, DefaultDateOrphanReclaimLimits())
		if out.FilesRemoved != 0 || out.RowsVerified != 0 {
			t.Fatal("current data decoded/deleted as orphan", out)
		}
		protected += out.FilesProtected
		if out.Phase == "scan-complete" {
			break
		}
	}
	if protected != 1 {
		t.Fatal("old date/current path not protected by named references", protected)
	}
	if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(old.Path))); err != nil {
		t.Fatal(err)
	}
}
