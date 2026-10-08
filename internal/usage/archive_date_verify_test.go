package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

func finishDateVerification(t *testing.T, e *Exporter, p *DiskProjection, limits DateCatalogVerifyLimits) DateCatalogVerifyProgress {
	t.Helper()
	for i := 0; i < 10000; i++ {
		out, err := e.VerifyRetainedDateCatalogStep(context.Background(), p, archiveDateCutoff, limits)
		if err != nil {
			t.Fatal(i, out, err)
		}
		if out.FilesProcessed > limits.Files || out.SourceRowsProcessed > limits.Source.Rows {
			t.Fatal("unbounded step", out)
		}
		if out.Phase == "ready" {
			return out
		}
	}
	t.Fatal("candidate comparison did not finish")
	return DateCatalogVerifyProgress{}
}

func publishDateComparisonFault(t *testing.T, e *Exporter, date, label string, attempts []AttemptEvent) ManifestFile {
	t.Helper()
	temporary, err := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
	if err != nil {
		t.Fatal(err)
	}
	defer temporary.Close()
	entry, err := temporary.publishDiskPartition(context.Background(), date, attempts, 16<<20, &archiveWork{remaining: 16 << 20}, &archiveWork{remaining: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(temporary.root, filepath.FromSlash(entry.Path)))
	if err != nil {
		t.Fatal(err)
	}
	entry.Path = "date=" + date + "/fixture-" + label + ".ndjson"
	path := filepath.Join(e.root, filepath.FromSlash(entry.Path))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestArchiveDateComparisonFiniteFilesNativePagesAndRestartReplay(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			var events []ledger.Event
			for i := 0; i < 16; i++ {
				events = append(events, windowRequest(fmt.Sprintf("req_%d", i), i%2 == 0, i%2 == 0)...)
			}
			f := newDiskFailureFixture(t, events)
			e, err := NewExporterWithOptions(t.TempDir(), Options{Format: format})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			// Real bounded publishers produce numerous physical partitions, while
			// native records remain authenticated by the selected Ledger owner.
			export := DefaultDiskExportStepLimits()
			export.Source.PageRows = 2
			export.Source.Pages = 1
			for i := 0; i < 100; i++ {
				cut, err := e.ExportDiskStep(context.Background(), f.p, export)
				if err != nil {
					t.Fatal(err)
				}
				if cut.ThroughSequence == f.a.Watermark().Sequence {
					break
				}
			}
			finishDateCandidate(t, e, archiveDateCutoff)
			rootPath := filepath.Join(e.root, "manifest.json")
			before, err := os.ReadFile(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			limits := DefaultDateCatalogVerifyLimits()
			limits.Files = 1
			limits.FileRows = 2
			limits.Source.Rows = 1
			limits.Source.PageRows = 1
			for i := 0; i < 3; i++ {
				out, err := e.VerifyRetainedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, limits)
				if err != nil || out.FilesProcessed != 1 || out.Phase != "archive" {
					t.Fatal(i, out, err)
				}
			}
			directory := e.dateVerification.directory
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("scratch survived actual Close", err)
			}
			e, err = NewExporterWithOptions(e.root, Options{Format: format})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			for i := 1; i <= 2; i++ {
				out, err := e.VerifyRetainedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, limits)
				if err != nil || out.Phase != "revalidate" || out.ReplayUnits != uint64(i) || out.Report != (ArchiveDateVerification{}) {
					t.Fatal("trusted saved comparison prefix", out, err)
				}
			}
			out := finishDateVerification(t, e, f.p, limits)
			if out.Report.Files != 8 || out.Report.SourceRows != 32 || out.Report.ArchiveRows != 16 || out.Report.RequiredRows != 16 || out.Report.MatchedRows != 16 || out.Report.ExpiredOmittedRows != 16 || e.windowProof != nil || e.windowPending != nil || e.archiveEpoch != 0 {
				t.Fatal(out)
			}
			if e.dateVerification != nil {
				t.Fatal("Ready retained scratch")
			}
			after, err := os.ReadFile(rootPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("comparison activated root", err)
			}
			state, err := os.ReadFile(filepath.Join(e.root, archiveDateVerifyName))
			if err != nil || len(state) > archiveDateBuildBytes {
				t.Fatal(len(state), err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			e, err = NewExporter(e.root)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			first, err := e.VerifyRetainedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, limits)
			if err != nil || first.Phase != "revalidate" {
				t.Fatal("persisted Ready acquired authority on reopen", first, err)
			}
			again := finishDateVerification(t, e, f.p, limits)
			if again.Report != out.Report {
				t.Fatal(again, out)
			}
		})
	}
}

func TestArchiveDateComparisonRejectsMissingExtraAndChangedRows(t *testing.T) {
	for _, fault := range []string{"missing", "extra", "content", "partition-date", "duplicate-path", "duplicate-event", "duplicate-sequence"} {
		t.Run(fault, func(t *testing.T) {
			f, e := dateArchiveFixture(t, FormatNDJSON, false)
			defer e.Close()
			if fault == "missing" {
				rewriteDateArchive(t, e, func(entry ManifestFile) bool { return entry.Date < "2026-10-05" }, nil)
			}
			if fault == "content" || fault == "partition-date" || fault == "extra" {
				rewriteDateArchive(t, e, nil, func(entry *ManifestFile) {
					if entry.Date != "2026-10-05" {
						return
					}
					attempts := []AttemptEvent{f.a.Snapshot().Attempts[2], f.a.Snapshot().Attempts[3]}
					if fault == "content" {
						attempts[0].ProviderRequestID = "forged"
					}
					if fault == "extra" {
						attempts[0].EventID = "forged-extra"
					}
					date := entry.Date
					value := publishDateComparisonFault(t, e, date, fault, attempts)
					if fault == "partition-date" {
						raw, err := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(value.Path)))
						if err != nil {
							t.Fatal(err)
						}
						value.Date = "2026-10-06"
						value.Path = "date=2026-10-06/fixture-date-mismatch.ndjson"
						path := filepath.Join(e.root, filepath.FromSlash(value.Path))
						if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, raw, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					*entry = value
				})
			}
			if len(fault) >= 9 && fault[:9] == "duplicate" { // Fixture-only descriptor corruption with valid page hashes.
				position, err := e.ReadArchivePosition(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				page, err := e.readCatalogPage(context.Background(), position.CatalogHead)
				if err != nil {
					t.Fatal(err)
				}
				builder := archiveCatalogBuilder{exporter: e, ctx: context.Background(), work: &archiveWork{remaining: 16 << 20}}
				var retained ManifestFile
				for _, entry := range page.Files {
					if err := builder.add(entry); err != nil {
						t.Fatal(err)
					}
					if entry.Date == "2026-10-05" {
						retained = entry
					}
				}
				if fault != "duplicate-path" {
					attempts := []AttemptEvent{f.a.Snapshot().Attempts[2], f.a.Snapshot().Attempts[3]}
					if fault == "duplicate-sequence" {
						for i := range attempts {
							attempts[i].EventID += "-other"
						}
					}
					entry := publishDateComparisonFault(t, e, retained.Date, fault, attempts)
					retained = entry
				}
				if err := builder.add(retained); err != nil {
					t.Fatal(err)
				}
				if err := builder.flush(); err != nil {
					t.Fatal(err)
				}
				old, err := openArchiveManifest(e.root)
				if err != nil {
					t.Fatal(err)
				}
				defer old.file.Close()
				if err := e.publishArchiveCatalogRoot(context.Background(), old, false, position.LastSequence, true, builder.head, &archiveWork{remaining: 16 << 20}); err != nil {
					t.Fatal(err)
				}
			}
			finishDateCandidate(t, e, archiveDateCutoff)
			out, err := e.VerifyRetainedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits())
			if !errors.Is(err, ErrDiskUsageCorrupt) || out != (DateCatalogVerifyProgress{}) || e.dateVerification != nil || e.windowProof != nil {
				t.Fatal("bad candidate certified", out, err)
			}
		})
	}
}

func TestArchiveDateComparisonSavedStateTamperSourceFenceAndReset(t *testing.T) {
	for _, fault := range []string{"counters", "cursor", "units", "ready", "oversize", "symlink", "source", "index-mac", "index-identity", "scratch-identity"} {
		t.Run(fault, func(t *testing.T) {
			f, e := dateArchiveFixture(t, FormatNDJSON, false)
			defer e.Close()
			finishDateCandidate(t, e, archiveDateCutoff)
			limits := DefaultDateCatalogVerifyLimits()
			limits.Files = 1
			limits.Source.Rows = 1
			if _, err := e.VerifyRetainedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, limits); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(e.root, archiveDateVerifyName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var state archiveDateVerifyState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "counters":
				state.Report.ArchiveRows++
			case "cursor":
				state.After++
			case "units":
				state.Units++
			case "ready":
				state.Phase = "ready"
				state.After = state.Report.ThroughSequence
			case "oversize":
				raw = make([]byte, archiveDateBuildBytes+1)
			case "symlink":
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".saved", path); err != nil {
					t.Fatal(err)
				}
			case "source":
				for _, event := range windowRequest("new", true, true) {
					appendDiskDetailEvent(t, f, event)
				}
			case "index-mac":
				x := e.dateVerification.index
				if _, err := x.file.WriteAt([]byte{0xff}, int64(x.roots[0])+64); err != nil {
					t.Fatal(err)
				}
			case "index-identity":
				x := e.dateVerification.index
				if err := os.Rename(x.file.Name(), x.file.Name()+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(x.file.Name(), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "scratch-identity":
				directory := e.dateVerification.directory
				if err := os.Rename(directory, directory+".displaced"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "foreign-owner"), []byte("must survive"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(directory); _ = os.RemoveAll(directory + ".displaced") })
			}
			if fault == "counters" || fault == "cursor" || fault == "units" || fault == "ready" {
				raw, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
			}
			if fault == "counters" || fault == "cursor" || fault == "units" || fault == "ready" || fault == "oversize" {
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := e.VerifyRetainedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, limits)
			// A changed canonical checkpoint first triggers bounded reconstruction;
			// any apparently legal altered cursor/counters must eventually refuse.
			for i := 0; err == nil && out.Phase == "revalidate" && i < 20; i++ {
				out, err = e.VerifyRetainedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, limits)
			}
			if err == nil || out != (DateCatalogVerifyProgress{}) || e.dateVerification != nil {
				t.Fatal(fault, out, err)
			}
			if fault == "scratch-identity" {
				paths, globErr := filepath.Glob(filepath.Join(e.root, ".archive-date-index-*", "foreign-owner"))
				if globErr != nil || len(paths) != 1 {
					t.Fatal("foreign scratch removed", paths, globErr)
				}
			}
			before, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			if fault == "symlink" {
				if err := e.ResetRetainedDateCatalogVerification(context.Background()); !errors.Is(err, ErrDiskUsageCorrupt) {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.ResetRetainedDateCatalogVerification(context.Background()); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil || !bytes.Equal(before, after) || e.archiveEpoch != 0 {
				t.Fatal("reset changed authority", err)
			}
			if _, err := os.Stat(filepath.Join(e.root, archiveDateBuildName)); err != nil {
				t.Fatal("reset removed candidate build", err)
			}
		})
	}
}

func TestArchiveDateComparisonActualScratchCallbackJoinsBothOwners(t *testing.T) {
	f, e := dateArchiveFixture(t, FormatNDJSON, false)
	finishDateCandidate(t, e, archiveDateCutoff)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan *os.File, 1)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	done := make(chan error, 1)
	go func() {
		out, err := e.verifyRetainedDateCatalogStep(ctx, f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits(), func() error { opened <- e.dateVerification.index.file; <-release; return nil })
		if out != (DateCatalogVerifyProgress{}) {
			done <- fmt.Errorf("canceled operation returned report: %+v", out)
			return
		}
		done <- err
	}()
	var file *os.File
	select {
	case file = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("scratch callback did not open")
	}
	if out, err := e.VerifyRetainedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits()); !errors.Is(err, ErrDiskUsageBusy) || out != (DateCatalogVerifyProgress{}) {
		t.Fatal(out, err)
	}
	called := false
	if err := e.withArchiveReclamation(context.Background(), func(advance func() error) error { called = true; return advance() }); !errors.Is(err, ErrDiskUsageBusy) || called || e.archiveEpoch != 0 {
		t.Fatal("reclaimed during comparison", err, called)
	}
	cancel()
	pDone := make(chan error, 1)
	eDone := make(chan error, 1)
	go func() { pDone <- f.p.Close() }()
	go func() { eDone <- e.Close() }()
	select {
	case err := <-pDone:
		t.Fatal("projection Close skipped physical callback", err)
	case <-time.After(40 * time.Millisecond):
	}
	select {
	case err := <-eDone:
		t.Fatal("exporter Close skipped physical callback", err)
	case <-time.After(40 * time.Millisecond):
	}
	if _, err := file.Stat(); err != nil {
		t.Fatal("logical cancellation closed physical file", err)
	}
	once.Do(func() { close(release) })
	if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := <-pDone; err != nil {
		t.Fatal(err)
	}
	if err := <-eDone; err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("scratch descriptor survived join", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, archiveDateVerifyName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled operation published checkpoint", err)
	}
}

func TestArchiveDateComparisonLimitsEmptyScopeAndLowWorkProgress(t *testing.T) {
	f, e := dateArchiveFixture(t, FormatNDJSON, false)
	defer e.Close()
	finishDateCandidate(t, e, archiveDateCutoff)
	for _, fault := range []string{"files", "rows", "read-reserve", "index-io", "index-space", "duration", "source"} {
		t.Run(fault, func(t *testing.T) {
			limits := DefaultDateCatalogVerifyLimits()
			switch fault {
			case "files":
				limits.Files = 0
			case "rows":
				limits.FileRows = 0
			case "read-reserve":
				limits.ReadBytes = 2 << 20
				limits.FileReadBytes = limits.ReadBytes - archiveCatalogPageBytes
			case "index-io":
				limits.IndexIOBytes = 0
			case "index-space":
				limits.IndexBytes = 0
			case "duration":
				limits.MaxDuration = 0
			case "source":
				limits.Source.Pages = 0
			}
			if out, err := e.VerifyRetainedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, limits); !errors.Is(err, ErrDiskUsageLimit) || out != (DateCatalogVerifyProgress{}) {
				t.Fatal(out, err)
			}
		})
	}
	limits := DefaultDateCatalogVerifyLimits()
	limits.FileRows = 1
	if out, err := e.VerifyRetainedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, limits); !errors.Is(err, ErrDiskUsageLimit) || out != (DateCatalogVerifyProgress{}) {
		t.Fatal("oversized atomic partition certified", out, err)
	}
	limits = DefaultDateCatalogVerifyLimits()
	limits.IndexBytes = 16 * archiveVerifyNodeBytes
	out := finishDateVerification(t, e, f.p, limits)
	if out.Report.MatchedRows != 2 {
		t.Fatal(out)
	}
	if err := e.ResetRetainedDateCatalogVerification(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetRetainedDateCatalogBuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		built, err := e.PrepareRetainedDateCatalogStep(context.Background(), cutoff, DefaultDateCatalogBuildLimits())
		if err != nil {
			t.Fatal(err)
		}
		if built.Phase == "ready" {
			break
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := e.VerifyRetainedDateCatalogStep(ctx, f.p, cutoff, DefaultDateCatalogVerifyLimits()); !errors.Is(err, context.Canceled) || out != (DateCatalogVerifyProgress{}) {
		t.Fatal(out, err)
	}
	out, err := e.VerifyRetainedDateCatalogStep(context.Background(), f.p, cutoff, DefaultDateCatalogVerifyLimits())
	if err != nil || out.Phase != "ready" || out.Report.SourceRows != 6 || out.Report.ExpiredOmittedRows != 6 || out.Report.ArchiveRows != 0 {
		t.Fatal(out, err)
	}
	// Source IO is limited per step even when all rows are expired omissions.
	var events []ledger.Event
	for i := 0; i < 16; i++ {
		events = append(events, windowRequest(fmt.Sprintf("lowwork_%d", i), i%2 == 0, i%2 == 0)...)
	}
	f2 := newDiskFailureFixture(t, events)
	e2, createErr := NewExporterWithOptions(t.TempDir(), Options{Format: FormatNDJSON})
	if createErr != nil {
		t.Fatal(createErr)
	}
	export := DefaultDiskExportStepLimits()
	export.Source.PageRows = 2
	for i := 0; i < 100; i++ {
		cut, err := e2.ExportDiskStep(context.Background(), f2.p, export)
		if err != nil {
			t.Fatal(err)
		}
		if cut.ThroughSequence == f2.a.Watermark().Sequence {
			break
		}
	}
	defer e2.Close()
	finishDateCandidate(t, e2, archiveDateCutoff)
	limits = DefaultDateCatalogVerifyLimits()
	limits.Files = 16
	limits.IndexIOBytes = 1 << 20
	limits.Source.Rows = 128
	limits.Source.PageRows = 128
	first, err := e2.VerifyRetainedDateCatalogStep(context.Background(), f2.p, archiveDateCutoff, limits)
	if err != nil || first.Phase != "source" || first.SourceRowsProcessed == 0 || first.SourceRowsProcessed > 25 {
		t.Fatal(first, err)
	}
	finished := finishDateVerification(t, e2, f2.p, limits)
	if finished.Report.SourceRows != 32 || finished.Report.MatchedRows != 16 {
		t.Fatal(finished)
	}
	// Changing the caller policy cannot inherit the completed checkpoint.
	if out, err := e2.VerifyRetainedDateCatalogStep(context.Background(), f2.p, cutoff, limits); !errors.Is(err, ErrArchiveDateBuildStale) || out != (DateCatalogVerifyProgress{}) {
		t.Fatal(out, err)
	}
}

func TestArchiveDateComparisonActualCloseAndCheckpointOwnershipFailures(t *testing.T) {
	for _, fault := range []string{"index-close", "checkpoint-owner", "root-owner"} {
		t.Run(fault, func(t *testing.T) {
			f, e := dateArchiveFixture(t, FormatNDJSON, false)
			defer e.Close()
			finishDateCandidate(t, e, archiveDateCutoff)
			path := filepath.Join(e.root, archiveDateVerifyName)
			out, err := e.verifyRetainedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits(), func() error {
				switch fault {
				case "index-close":
					return e.dateVerification.index.file.Close()
				case "checkpoint-owner":
					if err := os.WriteFile(path+".foreign", []byte("must survive"), 0o600); err != nil {
						return err
					}
					return os.Symlink(path+".foreign", path)
				case "root-owner":
					if err := os.Rename(e.root, e.root+".displaced"); err != nil {
						return err
					}
					if err := os.Mkdir(e.root, 0o700); err != nil {
						return err
					}
					t.Cleanup(func() { _ = os.RemoveAll(e.root + ".displaced") })
					return os.WriteFile(filepath.Join(e.root, "foreign-owner"), []byte("must survive"), 0o600)
				}
				return nil
			})
			if err == nil || out != (DateCatalogVerifyProgress{}) || e.dateVerification != nil {
				t.Fatal("physical failure published scope", out, err)
			}
			if fault == "index-close" {
				if !errors.Is(err, os.ErrClosed) {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("Close failure committed checkpoint", err)
				}
			}
			if fault == "checkpoint-owner" {
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != "must survive" {
					t.Fatal("foreign checkpoint changed", err)
				}
			}
			if fault == "root-owner" {
				entries, err := os.ReadDir(e.root)
				if err != nil || len(entries) != 1 || entries[0].Name() != "foreign-owner" {
					t.Fatal("mutated replacement root", entries, err)
				}
			}
		})
	}
}
