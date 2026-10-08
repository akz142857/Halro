package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func completedOrphanPageFixture(t *testing.T, format string) (*diskFailureFixture, *Exporter, ArchivePageRef, []byte) {
	t.Helper()
	f, e, head := datePageFixture(t, format, 2)
	raw, err := os.ReadFile(head.path(e.root))
	if err != nil {
		t.Fatal(err)
	}
	authorizeDateReclaimStep(t, f, e)
	out, err := e.ReclaimRetainedDatePagesStep(context.Background(), f.p, archiveDateCutoff, DefaultDatePageReclaimLimits())
	if err != nil || out.Phase != "ready" {
		t.Fatal(out, err)
	}
	return f, e, head, raw
}

func writeOrphanCatalogPage(t *testing.T, e *Exporter, page archiveCatalogPage) ArchivePageRef {
	t.Helper()
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	ref := ArchivePageRef{SHA256: hex.EncodeToString(digest[:]), Bytes: len(raw), Pages: page.Previous.Pages + 1, Files: page.Previous.Files + uint64(len(page.Files))}
	if err := os.WriteFile(ref.path(e.root), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return ref
}

func orphanPagesStep(t *testing.T, f *diskFailureFixture, e *Exporter, l DateOrphanPageReclaimLimits) DateOrphanPageReclaimProgress {
	t.Helper()
	if e.dateProof == nil {
		authorizeDateReclaimStep(t, f, e)
	}
	out, err := e.ReclaimRetainedDateOrphanPagesStep(context.Background(), f.p, archiveDateCutoff, l)
	if err != nil {
		t.Fatal(out, err)
	}
	if out.EntriesScanned > l.Entries || out.PagesExamined > l.Pages || out.CandidatePagesProcessed > l.CandidatePages || !out.ObligationPending {
		t.Fatal("escaped finite limits", out, l)
	}
	return out
}

func TestArchiveDateOrphanPagesFiniteRestoredAndDetachedMetadata(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e, head, raw := completedOrphanPageFixture(t, format)
			if err := os.WriteFile(head.path(e.root), raw, 0600); err != nil {
				t.Fatal(err)
			}
			page, err := decodeArchiveCatalogPage(raw)
			if err != nil {
				t.Fatal(err)
			}
			var refs []ArchivePageRef
			refs = append(refs, head)
			for i := 0; i < 8; i++ {
				page.Files[0].Path = fmt.Sprintf("date=%s/detached-%d%s", page.Files[0].Date, i, filepath.Ext(page.Files[0].Path))
				refs = append(refs, writeOrphanCatalogPage(t, e, page))
			}
			// Native rows and even predecessor files need not remain for a detached
			// metadata page. Physical data objects are outside this reclamation job.
			data := filepath.Join(e.root, filepath.FromSlash(page.Files[0].Path))
			if err := os.WriteFile(data, []byte("foreign data remains independent"), 0600); err != nil {
				t.Fatal(err)
			}
			before := map[string][]byte{}
			for _, name := range []string{"manifest.json", archiveDateActivationName, archiveDatePagesName} {
				before[name], err = os.ReadFile(filepath.Join(e.root, name))
				if err != nil {
					t.Fatal(err)
				}
			}
			unknown := filepath.Join(e.root, "catalog", "v1", "foreign.json")
			if err := os.WriteFile(unknown, []byte("unknown metadata"), 0600); err != nil {
				t.Fatal(err)
			}
			l := DefaultDateOrphanPageReclaimLimits()
			l.Entries, l.Pages = 2, 1
			removed, unrecognized := 0, 0
			for i := 0; i < 100; i++ {
				out := orphanPagesStep(t, f, e, l)
				removed += out.PagesRemoved
				unrecognized += out.EntriesUnrecognized
				if out.Phase == "scan-complete" {
					if e.dateOrphanPages != nil {
						t.Fatal("EOF retained scratch/FDs")
					}
					break
				}
				if i == 99 {
					t.Fatal("actual scan stalled")
				}
			}
			if removed != len(refs) || unrecognized != 1 {
				t.Fatal(removed, unrecognized)
			}
			for _, ref := range refs {
				if _, err := os.Stat(ref.path(e.root)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(ref, err)
				}
			}
			for _, path := range []string{unknown, data} {
				if _, err := os.Stat(path); err != nil {
					t.Fatal(path, err)
				}
			}
			for name, raw := range before {
				after, err := os.ReadFile(filepath.Join(e.root, name))
				if err != nil || !bytes.Equal(raw, after) {
					t.Fatal(name, err)
				}
			}
			// Observed EOF is not persistent authority. Restored objects are examined again.
			if err := os.WriteFile(head.path(e.root), raw, 0600); err != nil {
				t.Fatal(err)
			}
			total := 0
			for i := 0; i < 20; i++ {
				out := orphanPagesStep(t, f, e, l)
				total += out.PagesRemoved
				if out.Phase == "scan-complete" {
					break
				}
				if i == 19 {
					t.Fatal("fresh scan stalled")
				}
			}
			if total != 1 {
				t.Fatal(total)
			}
		})
	}
}

func TestArchiveDateOrphanPagesRequiresSignedCompletedOldChain(t *testing.T) {
	for _, fault := range []string{"missing", "pending", "forged-ready", "wrong-key", "foreign-binding"} {
		t.Run(fault, func(t *testing.T) {
			f, e, head := datePageFixture(t, FormatNDJSON, 3)
			ctx := context.Background()
			if fault != "missing" {
				authorizeDateReclaimStep(t, f, e)
				stop := errors.New("stop before old-chain unlink")
				_, err := e.reclaimRetainedDatePagesStep(ctx, f.p, archiveDateCutoff, DefaultDatePageReclaimLimits(), func() error { return stop }, nil)
				if !errors.Is(err, stop) {
					t.Fatal(err)
				}
				path := filepath.Join(e.root, archiveDatePagesName)
				state, _, file, _, err := readDatePagesState(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				switch fault {
				case "forged-ready":
					state.Next, state.Previous, state.Pending, state.Processed = ArchivePageRef{}, ArchivePageRef{}, false, state.Old.Pages
				case "wrong-key":
					e.datePolicyKey[0] ^= 1
				case "foreign-binding":
					state.BindingSHA = hex.EncodeToString(make([]byte, 32))
					if err := e.signDatePages(&state); err != nil {
						t.Fatal(err)
					}
				}
				if fault == "forged-ready" || fault == "foreign-binding" {
					raw, err := json.Marshal(state)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Wrong independent key already refuses published preparation; use the proof
			// minted immediately before switching keys to exercise admission itself.
			if fault != "wrong-key" {
				authorizeDateReclaimStep(t, f, e)
			} else {
				e.datePolicyKey[0] ^= 1
				authorizeDateReclaimStep(t, f, e)
				e.datePolicyKey[0] ^= 1
			}
			out, err := e.ReclaimRetainedDateOrphanPagesStep(ctx, f.p, archiveDateCutoff, DefaultDateOrphanPageReclaimLimits())
			if err == nil || out != (DateOrphanPageReclaimProgress{}) || e.dateOrphanPages != nil || e.dateProof != nil {
				t.Fatal("old obligation bypass", fault, out, err)
			}
			if _, err := os.Stat(head.path(e.root)); err != nil {
				t.Fatal("unfinished old page removed", err)
			}
		})
	}
}

func appendCurrentPageForOrphanTest(t *testing.T, f *diskFailureFixture, e *Exporter) ArchivePosition {
	t.Helper()
	for i := 0; i < 3; i++ {
		for _, event := range windowRequest(fmt.Sprintf("current-page-after-old-reclaim-%d", i), true, true) {
			event.OccurredAt = event.OccurredAt.AddDate(0, 0, i)
			appendDiskDetailEvent(t, f, event)
		}
	}
	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetPublishedDateCatalogVerification(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetDateCatalogReferences(context.Background()); err != nil {
		t.Fatal(err)
	}
	position, err := e.ReadArchivePosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return position
}

func TestArchiveDateOrphanPagesProtectsCurrentAndFullCandidateChain(t *testing.T) {
	f, e, old, raw := completedOrphanPageFixture(t, FormatNDJSON)
	current := appendCurrentPageForOrphanTest(t, f, e)
	page, err := e.readCatalogPage(context.Background(), current.CatalogHead)
	if err != nil {
		t.Fatal(err)
	}
	// An unsigned, structurally valid candidate hint against the actual current
	// baseline protects its entire output chain. It supplies no publish authority.
	head := ArchivePageRef{}
	var candidates []ArchivePageRef
	for i := 0; i < 3; i++ {
		next := page
		next.Previous = head
		next.Files = []ManifestFile{page.Files[i]}
		next.Files[0].Path = fmt.Sprintf("date=%s/candidate-%d.ndjson", next.Files[0].Date, i)
		head = writeOrphanCatalogPage(t, e, next)
		candidates = append(candidates, head)
	}
	// Three output descriptors need a baseline with at least that many files.
	root, err := openArchiveManifest(e.root)
	if err != nil {
		t.Fatal(err)
	}
	position, digest, err := root.walk(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.file.Close(); err != nil {
		t.Fatal(err)
	}
	build := archiveDateBuildState{Version: 1, Baseline: position, BaselineSHA: hex.EncodeToString(digest[:]), RetainFrom: archiveDateCutoff.AddDate(0, 0, 1).Format("2006-01-02"), Next: position.CatalogHead, Output: head}
	buildRaw, err := json.Marshal(build)
	if err != nil || !build.valid() {
		t.Fatal(build, err)
	}
	if err := os.WriteFile(filepath.Join(e.root, archiveDateBuildName), buildRaw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old.path(e.root), raw, 0600); err != nil {
		t.Fatal(err)
	}
	protected, removed := 0, 0
	for i := 0; i < 50; i++ {
		out := orphanPagesStep(t, f, e, DefaultDateOrphanPageReclaimLimits())
		protected += out.PagesProtected
		removed += out.PagesRemoved
		if out.Phase == "scan-complete" {
			break
		}
		if i == 49 {
			t.Fatal("scan stalled")
		}
	}
	if protected != 4 || removed != 1 {
		t.Fatal(protected, removed)
	}
	for _, ref := range append(candidates, current.CatalogHead) {
		if _, err := os.Stat(ref.path(e.root)); err != nil {
			t.Fatal("protected metadata gone", err)
		}
	}
	after, err := os.ReadFile(filepath.Join(e.root, archiveDateBuildName))
	if err != nil || !bytes.Equal(after, buildRaw) {
		t.Fatal("candidate checkpoint changed", err)
	}
	// A changed checkpoint invalidates an already-open scan, rather than shrinking
	// its protection to make deletion easier.
	l := DefaultDateOrphanPageReclaimLimits()
	l.Entries = 1
	orphanPagesStep(t, f, e, l)
	if e.dateOrphanPages == nil {
		t.Fatal("no live session")
	}
	held := e.dateOrphanPages.scan
	build.RetainFrom = archiveDateCutoff.AddDate(0, 0, 2).Format("2006-01-02")
	buildRaw, _ = json.Marshal(build)
	if err := os.WriteFile(filepath.Join(e.root, archiveDateBuildName), buildRaw, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := e.ReclaimRetainedDateOrphanPagesStep(context.Background(), f.p, archiveDateCutoff, l)
	if !errors.Is(err, ErrArchiveDateBuildStale) || out != (DateOrphanPageReclaimProgress{}) || e.dateOrphanPages != nil {
		t.Fatal(out, err)
	}
	if _, err := held.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("stale scan FD remained", err)
	}
}

func TestArchiveDateOrphanPagesPhysicalFaultsWithdrawAndPreserveAuthority(t *testing.T) {
	for _, fault := range []string{"page-bytes", "symlink", "inode-replace", "after-unlink", "scan-close", "index-bytes", "read-budget", "unknown-build"} {
		t.Run(fault, func(t *testing.T) {
			f, e, head, raw := completedOrphanPageFixture(t, FormatNDJSON)
			if err := os.WriteFile(head.path(e.root), raw, 0600); err != nil {
				t.Fatal(err)
			}
			policy, err := os.ReadFile(filepath.Join(e.root, archiveDateActivationName))
			if err != nil {
				t.Fatal(err)
			}
			l := DefaultDateOrphanPageReclaimLimits()
			var before func(*os.File) error
			var after func() error
			switch fault {
			case "page-bytes":
				if err := os.WriteFile(head.path(e.root), []byte("wrong content"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(head.path(e.root), head.path(e.root)+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(head.path(e.root)+".foreign", head.path(e.root)); err != nil {
					t.Fatal(err)
				}
			case "inode-replace":
				before = func(f *os.File) error {
					raw, err := os.ReadFile(f.Name())
					if err != nil {
						return err
					}
					if err := os.WriteFile(f.Name()+".replacement", raw, 0600); err != nil {
						return err
					}
					return os.Rename(f.Name()+".replacement", f.Name())
				}
			case "after-unlink":
				after = func() error { return errors.New("physical unlink interrupted") }
			case "read-budget":
				l.ReadBytes = 1
			case "unknown-build":
				if err := os.WriteFile(filepath.Join(e.root, archiveDateBuildName), []byte("foreign malformed candidate"), 0600); err != nil {
					t.Fatal(err)
				}
			case "scan-close", "index-bytes":
				l.Entries = 1
				// Preserve an ignored prefix so the actual catalog stream remains live.
				if err := os.WriteFile(filepath.Join(e.root, "catalog", "v1", ".ignored"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				first := orphanPagesStep(t, f, e, l)
				if first.Phase == "scan-complete" || e.dateOrphanPages == nil {
					t.Fatal(first)
				}
				if fault == "scan-close" {
					if err := e.dateOrphanPages.scan.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := e.dateOrphanPages.index.file.Truncate(0); err != nil {
						t.Fatal(err)
					}
				}
			}
			if e.dateProof == nil {
				authorizeDateReclaimStep(t, f, e)
			}
			out, err := e.reclaimRetainedDateOrphanPagesStep(context.Background(), f.p, archiveDateCutoff, l, before, after)
			if err == nil || out != (DateOrphanPageReclaimProgress{}) || e.dateOrphanPages != nil {
				t.Fatal("physical failure retained report/session", out, err)
			}
			afterPolicy, err := os.ReadFile(filepath.Join(e.root, archiveDateActivationName))
			if err != nil || !bytes.Equal(policy, afterPolicy) {
				t.Fatal("policy authority changed", err)
			}
		})
	}
}

func TestArchiveDateOrphanPagesCloseJoinsActualOperation(t *testing.T) {
	f, e, head, raw := completedOrphanPageFixture(t, FormatNDJSON)
	if err := os.WriteFile(head.path(e.root), raw, 0600); err != nil {
		t.Fatal(err)
	}
	authorizeDateReclaimStep(t, f, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var scan, index *os.File
	done := make(chan error, 1)
	go func() {
		out, err := e.reclaimRetainedDateOrphanPagesStep(ctx, f.p, archiveDateCutoff, DefaultDateOrphanPageReclaimLimits(), nil, func() error {
			scan = e.dateOrphanPages.scan
			index = e.dateOrphanPages.index.file
			close(entered)
			<-release
			return nil
		})
		if out != (DateOrphanPageReclaimProgress{}) {
			err = errors.Join(err, errors.New("retired report escaped"))
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("unlink not reached")
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close escaped callback", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	for _, fd := range []*os.File{scan, index} {
		if _, err := fd.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatal("real FD escaped", err)
		}
	}
	if e.dateOrphanPages != nil || e.datePolicyKey != ([32]byte{}) {
		t.Fatal("session/key remained")
	}
}

func TestArchiveDateOrphanPagesActualFutureBuildFiniteProtection(t *testing.T) {
	f, e, old, raw := completedOrphanPageFixture(t, FormatNDJSON)
	ctx := context.Background()
	for i := 0; i < 70; i++ {
		for _, event := range windowRequest(fmt.Sprintf("actual-candidate-native-%d", i), true, true) {
			event.OccurredAt = event.OccurredAt.AddDate(0, 0, i)
			appendDiskDetailEvent(t, f, event)
		}
	}
	if _, err := e.ExportDisk(ctx, f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetDateCatalogReferences(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetRetainedDateCatalogBuild(ctx); err != nil {
		t.Fatal(err)
	}
	// Build the real future candidate using the production finite transformer.
	future := archiveDateCutoff.AddDate(0, 0, 1)
	var output ArchivePageRef
	for i := 0; i < 100; i++ {
		out, err := e.PrepareRetainedDateCatalogStep(ctx, future, DefaultDateCatalogBuildLimits())
		if err != nil {
			t.Fatal(out, err)
		}
		if out.Phase == "ready" {
			output = out.Candidate
			break
		}
		if i == 99 {
			t.Fatal("future build stalled")
		}
	}
	if output.Pages < 2 {
		t.Fatal("fixture lacks actual multi-page candidate", output)
	}
	refs := []ArchivePageRef{}
	for ref := output; ref != (ArchivePageRef{}); {
		refs = append(refs, ref)
		page, err := e.readCatalogPage(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		ref = page.Previous
	}
	if err := os.WriteFile(old.path(e.root), raw, 0600); err != nil {
		t.Fatal(err)
	}
	l := DefaultDateOrphanPageReclaimLimits()
	l.CandidatePages, l.Entries = 1, 1
	first := orphanPagesStep(t, f, e, l)
	if first.Phase != "protect-candidate" || first.CandidatePagesProcessed != 1 || first.EntriesScanned != 0 || first.PagesRemoved != 0 || e.dateProof == nil || e.dateOrphanPages.scan != nil {
		t.Fatal("enumerated before full candidate protection", first)
	}
	heldIndex := e.dateOrphanPages.index.file
	if err := e.ResetRetainedDateOrphanPages(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := heldIndex.Stat(); !errors.Is(err, os.ErrClosed) || e.dateOrphanPages != nil || e.dateProof != nil {
		t.Fatal("reset retained actual index", err)
	}
	removed, protected := 0, 0
	for i := 0; i < 100; i++ {
		out := orphanPagesStep(t, f, e, l)
		removed += out.PagesRemoved
		protected += out.PagesProtected
		if out.Phase == "scan-complete" {
			break
		}
		if i == 99 {
			t.Fatal("finite multi-page protection stalled")
		}
	}
	if removed != 1 || protected < len(refs) {
		t.Fatal(removed, protected, refs)
	}
	for _, ref := range refs {
		if _, err := e.readCatalogPage(ctx, ref); err != nil {
			t.Fatal("future candidate predecessor removed", ref, err)
		}
	}
}

func TestArchiveDateOrphanPagesReopenAndActualDirectoryIdentity(t *testing.T) {
	for _, fault := range []string{"reopen", "catalog-replacement", "catalog-symlink", "source-change", "missing-build"} {
		t.Run(fault, func(t *testing.T) {
			f, e, head, raw := completedOrphanPageFixture(t, FormatNDJSON)
			if err := os.WriteFile(head.path(e.root), raw, 0600); err != nil {
				t.Fatal(err)
			}
			page, err := decodeArchiveCatalogPage(raw)
			if err != nil {
				t.Fatal(err)
			}
			page.Files[0].Path = "date=" + page.Files[0].Date + "/another-detached.ndjson"
			another := writeOrphanCatalogPage(t, e, page)
			l := DefaultDateOrphanPageReclaimLimits()
			l.Pages = 1
			if fault == "missing-build" {
				if err := e.ResetRetainedDateCatalogBuild(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			first := orphanPagesStep(t, f, e, l)
			if first.PagesRemoved != 1 || first.Phase == "scan-complete" || e.dateOrphanPages == nil {
				t.Fatal(first)
			}
			heldScan, heldIndex := e.dateOrphanPages.scan, e.dateOrphanPages.index.file
			switch fault {
			case "reopen", "missing-build":
				e = reopenDatePageFixture(t, f, e, FormatNDJSON)
				if e.dateOrphanPages != nil {
					t.Fatal("persisted inventory trusted")
				}
				total := 0
				for i := 0; i < 10; i++ {
					out := orphanPagesStep(t, f, e, l)
					total += out.PagesRemoved
					if out.Phase == "scan-complete" {
						break
					}
					if i == 9 {
						t.Fatal("reopen stalled")
					}
				}
				if total != 1 {
					t.Fatal(total)
				}
				for _, ref := range []ArchivePageRef{head, another} {
					if _, err := os.Stat(ref.path(e.root)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
				}
			case "catalog-replacement", "catalog-symlink":
				path := filepath.Join(e.root, "catalog")
				if err := os.Rename(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
				if fault == "catalog-symlink" {
					if err := os.Symlink(path+".foreign", path); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.MkdirAll(filepath.Join(path, "v1"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				out, err := e.ReclaimRetainedDateOrphanPagesStep(context.Background(), f.p, archiveDateCutoff, l)
				if err == nil || out != (DateOrphanPageReclaimProgress{}) || e.dateOrphanPages != nil {
					t.Fatal(out, err)
				}
			case "source-change":
				for _, event := range windowRequest("native-changed-orphan-pages", true, true) {
					appendDiskDetailEvent(t, f, event)
				}
				if err := e.ResetPublishedDateCatalogVerification(context.Background()); err != nil {
					t.Fatal(err)
				}
				authorizeDateReclaimStep(t, f, e)
				out, err := e.ReclaimRetainedDateOrphanPagesStep(context.Background(), f.p, archiveDateCutoff, l)
				if !errors.Is(err, ErrArchiveDateBuildStale) || out != (DateOrphanPageReclaimProgress{}) || e.dateOrphanPages != nil {
					t.Fatal(out, err)
				}
			}
			for _, fd := range []*os.File{heldScan, heldIndex} {
				if _, err := fd.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatal("stale actual FD remained", err)
				}
			}
		})
	}
}
