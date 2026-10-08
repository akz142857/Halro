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
)

func dateBuildCatalogFixture(t *testing.T, count int) *Exporter {
	t.Helper()
	e, err := NewExporter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := archiveCatalogBuilder{exporter: e, ctx: context.Background(), work: &archiveWork{remaining: 64 << 20}}
	for i := 1; i <= count; i++ {
		entry := syntheticArchiveEntry(uint64(i))
		if i%2 == 0 {
			entry.Date = "2026-10-05"
			entry.Path = fmt.Sprintf("date=%s/usage-%020d.ndjson", entry.Date, i)
		}
		if err := b.add(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.flush(); err != nil {
		t.Fatal(err)
	}
	if err := e.publishArchiveCatalogRoot(context.Background(), nil, true, uint64(count), false, b.head, &archiveWork{remaining: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func finishDateCandidate(t *testing.T, e *Exporter, cutoff time.Time) DateCatalogBuildProgress {
	t.Helper()
	for i := 0; i < 2048; i++ {
		out, err := e.PrepareRetainedDateCatalogStep(context.Background(), cutoff, DefaultDateCatalogBuildLimits())
		if err != nil {
			t.Fatal(out, err)
		}
		if out.Phase == "ready" {
			return out
		}
	}
	t.Fatal("candidate did not reach EOF")
	return DateCatalogBuildProgress{}
}

func TestArchiveDateCandidateTenThousandFiniteStepsReopenAndNoAuthority(t *testing.T) {
	e := dateBuildCatalogFixture(t, 10000)
	rootPath := filepath.Join(e.root, "manifest.json")
	before, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultDateCatalogBuildLimits()
	limits.Pages = 1
	for want := uint64(1); want <= 3; want++ {
		out, err := e.PrepareRetainedDateCatalogStep(context.Background(), archiveDateCutoff, limits)
		if err != nil || out.ScannedPages != want || out.PagesProcessed != 1 || out.Phase != "build" {
			t.Fatal(out, err)
		}
	}
	statePath := filepath.Join(e.root, archiveDateBuildName)
	info, err := os.Stat(statePath)
	if err != nil || info.Size() > archiveDateBuildBytes {
		t.Fatal(info, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = NewExporter(e.root)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for want := uint64(1); want <= 2; want++ {
		out, err := e.PrepareRetainedDateCatalogStep(context.Background(), archiveDateCutoff, limits)
		if err != nil || out.Phase != "revalidate" || out.ReplayPages != want || out.ScannedPages != 3 || out.PagesProcessed != 1 {
			t.Fatal("reopen trusted saved progress", out, err)
		}
	}
	out := finishDateCandidate(t, e, archiveDateCutoff)
	if out.Candidate.Files != 5000 || out.ScannedPages != 157 || len(out.RootJSON) > archiveManifestValueLimit {
		t.Fatal(out)
	}
	position, err := readArchiveManifest(context.Background(), bytes.NewReader(out.RootJSON), nil)
	if err != nil || position.SchemaVersion != 10 || position.LastSequence != 10000 || position.CompletePrefix || position.Retention.RetainFromDate != "2026-10-05" || position.Retention.PruneThroughSequence != 10000 {
		t.Fatal(position, err)
	}
	files := uint64(0)
	for ref := out.Candidate; ref != (ArchivePageRef{}); {
		page, err := e.readCatalogPage(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.Files {
			if entry.Date != "2026-10-05" {
				t.Fatal("expired descriptor survived", entry)
			}
			files++
		}
		ref = page.Previous
	}
	if files != 5000 || e.windowProof != nil || e.windowPending != nil || e.archiveEpoch != 0 {
		t.Fatal("candidate acquired authority or changed live epoch", files)
	}
	after, err := os.ReadFile(rootPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("candidate published live root", err)
	}
	paths, err := filepath.Glob(filepath.Join(e.root, "**", ".archive-*.tmp"))
	if err != nil || len(paths) != 0 {
		t.Fatal(paths, err)
	}
}

func TestArchiveDateCandidateSavedPrefixTamperAndRootChangeRefuse(t *testing.T) {
	for _, tamper := range []string{"output", "next", "counter", "policy", "baseline", "oversize", "symlink"} {
		t.Run(tamper, func(t *testing.T) {
			e := dateBuildCatalogFixture(t, 256)
			limits := DefaultDateCatalogBuildLimits()
			limits.Pages = 1
			if _, err := e.PrepareRetainedDateCatalogStep(context.Background(), archiveDateCutoff, limits); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(e.root, archiveDateBuildName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var s archiveDateBuildState
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			switch tamper {
			case "output":
				s.Output = s.Baseline.CatalogHead
			case "next":
				s.Next.SHA256 = s.Baseline.CatalogHead.SHA256
			case "counter":
				s.ScannedPages = 0
			case "policy":
				s.RetainFrom = "2026-10-06"
			case "baseline":
				s.BaselineSHA = string(bytes.Repeat([]byte{'0'}, 64))
			case "oversize":
				raw = bytes.Repeat([]byte{'x'}, archiveDateBuildBytes+1)
			case "symlink":
				foreign := filepath.Join(t.TempDir(), "foreign")
				if err := os.WriteFile(foreign, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, path); err != nil {
					t.Fatal(err)
				}
			}
			if tamper != "symlink" {
				if tamper != "oversize" {
					raw, err = json.Marshal(s)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The same process must also discard its cache when saved bytes change.
			out, err := e.PrepareRetainedDateCatalogStep(context.Background(), archiveDateCutoff, limits)
			if err == nil || len(out.RootJSON) != 0 || out.Phase != "" {
				t.Fatal("tampered progress returned ready", out, err)
			}
		})
	}
	e := dateBuildCatalogFixture(t, 256)
	limits := DefaultDateCatalogBuildLimits()
	limits.Pages = 1
	if _, err := e.PrepareRetainedDateCatalogStep(context.Background(), archiveDateCutoff, limits); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(e.root, "manifest.json")
	raw, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	root["last_sequence"] = 257
	raw, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := e.PrepareRetainedDateCatalogStep(context.Background(), archiveDateCutoff, limits); !errors.Is(err, ErrArchiveDateBuildStale) || out.Phase != "" {
		t.Fatal(out, err)
	}
	beforeReset, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ResetRetainedDateCatalogBuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(e.root, archiveDateBuildName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if out, err := e.PrepareRetainedDateCatalogStep(context.Background(), archiveDateCutoff, limits); err != nil || out.ScannedPages != 1 {
		t.Fatal("explicit fresh candidate could not restart", out, err)
	}
	afterReset, err := os.ReadFile(rootPath)
	if err != nil || !bytes.Equal(beforeReset, afterReset) || e.archiveEpoch != 0 {
		t.Fatal("reset changed live root/epoch", err)
	}
}

func TestArchiveDateCandidateActualStateRenameCancellationAndCloseJoin(t *testing.T) {
	e := dateBuildCatalogFixture(t, 256)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		out, err := e.prepareRetainedDateCatalogStep(ctx, archiveDateCutoff, DefaultDateCatalogBuildLimits(), func() error { close(entered); <-release; return nil })
		if out.Phase != "" {
			err = errors.Join(err, errors.New("canceled candidate returned receipt"))
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no real checkpoint rename")
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close preceded real job exit", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if e.dateBuildReplay != nil {
		t.Fatal("closed replay retained")
	}
	reopened, err := NewExporter(e.root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	out := finishDateCandidate(t, reopened, archiveDateCutoff)
	if out.Candidate.Files != 128 || out.Phase != "ready" {
		t.Fatal(out)
	}
}

func TestArchiveDateCandidateLimitsAndLegacyRefusalPreserveRoot(t *testing.T) {
	e := dateBuildCatalogFixture(t, 0)
	out := finishDateCandidate(t, e, archiveDateCutoff)
	if out.Candidate != (ArchivePageRef{}) || len(out.RootJSON) == 0 {
		t.Fatal(out)
	}
	for _, limits := range []DateCatalogBuildLimits{{}, {Pages: 17, WorkBytes: 1 << 20, MaxDuration: time.Second}, {Pages: 1, WorkBytes: 255 << 10, MaxDuration: time.Second}} {
		if out, err := e.PrepareRetainedDateCatalogStep(context.Background(), archiveDateCutoff, limits); !errors.Is(err, ErrDiskUsageLimit) || out.Phase != "" {
			t.Fatal(out, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := e.PrepareRetainedDateCatalogStep(ctx, archiveDateCutoff, DefaultDateCatalogBuildLimits()); !errors.Is(err, context.Canceled) || out.Phase != "" {
		t.Fatal(out, err)
	}
	_, legacy := dateArchiveFixture(t, FormatNDJSON, true)
	if out, err := legacy.PrepareRetainedDateCatalogStep(context.Background(), archiveDateCutoff, DefaultDateCatalogBuildLimits()); !errors.Is(err, ErrArchivePagedCatalog) || out.Phase != "" {
		t.Fatal(out, err)
	}
	// A valid low byte cap with a larger page cap must still make durable
	// progress. Returning Limit after discarding the same prefix would stall.
	large := dateBuildCatalogFixture(t, 2560)
	limits := DateCatalogBuildLimits{Pages: 16, WorkBytes: 320 << 10, MaxDuration: time.Minute}
	var last uint64
	for i := 0; i < 128; i++ {
		out, err := large.PrepareRetainedDateCatalogStep(context.Background(), archiveDateCutoff, limits)
		if err != nil || out.PagesProcessed < 1 || out.PagesProcessed >= limits.Pages || out.ScannedPages <= last {
			t.Fatal("byte boundary lost progress", out, err)
		}
		last = out.ScannedPages
		if out.Phase == "ready" {
			if out.Candidate.Files != 1280 {
				t.Fatal(out)
			}
			return
		}
	}
	t.Fatal("low-byte candidate did not finish")
}
