package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func datePageFixture(t *testing.T, format string, n int) (*diskFailureFixture, *Exporter, ArchivePageRef) {
	t.Helper()
	f, e, _ := dateReclaimStepFixture(t, format, n)
	state, _, file, _, err := readDateActivationState(context.Background(), filepath.Join(e.root, archiveDateActivationName))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	old, _, err := state.positions()
	if err != nil {
		t.Fatal(err)
	}
	out := finishDateReclaimStep(t, f, e, DefaultDateReclaimStepLimits())
	if out.Phase != "ready" || e.dateDataProof == nil {
		t.Fatal("no genuine data-completion witness", out)
	}
	return f, e, old.CatalogHead
}

func reopenDatePageFixture(t *testing.T, f *diskFailureFixture, e *Exporter, format string) *Exporter {
	t.Helper()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := NewExporterWithOptions(e.root, datePolicyTestOptions(f, format))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	authorizeDateReclaimStep(t, f, next)
	return next
}

func TestArchiveDatePagesFiniteOldChainAndReopenAfterDeletedHead(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e, head := datePageFixture(t, format, 70)
			ctx := context.Background()
			first, err := e.readCatalogPage(ctx, head)
			if err != nil || first.Previous == (ArchivePageRef{}) {
				t.Fatal(first, err)
			}
			policyBefore, err := os.ReadFile(filepath.Join(e.root, archiveDateActivationName))
			if err != nil {
				t.Fatal(err)
			}
			rootBefore, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			authorizeDateReclaimStep(t, f, e)
			limits := DefaultDatePageReclaimLimits()
			limits.Pages = 1
			out, err := e.ReclaimRetainedDatePagesStep(ctx, f.p, archiveDateCutoff, limits)
			if err != nil || out.Phase != "pages" || out.PagesProcessed != 1 || out.PagesRemoved != 1 || out.Visited != 1 || !out.ObligationPending || out.LogicalBytesUnlinked != int64(head.Bytes) || e.dateDataProof != nil {
				t.Fatal(out, err)
			}
			if _, err := os.Stat(head.path(e.root)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("head not reclaimed", err)
			}
			e = reopenDatePageFixture(t, f, e, format)
			out, err = e.ReclaimRetainedDatePagesStep(ctx, f.p, archiveDateCutoff, limits)
			if err != nil || out.Phase != "ready" || out.PagesRemoved != 1 || out.PagesProcessed != 1 || out.Visited != 2 || !out.ObligationPending {
				t.Fatal(out, err)
			}
			if _, err := os.Stat(first.Previous.path(e.root)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			e = reopenDatePageFixture(t, f, e, format)
			out, err = e.ReclaimRetainedDatePagesStep(ctx, f.p, archiveDateCutoff, limits)
			if err != nil || out.Phase != "ready" || out.PagesProcessed != 0 || out.LogicalBytesUnlinked != 0 || out.Visited != 2 {
				t.Fatal(out, err)
			}
			policyAfter, err := os.ReadFile(filepath.Join(e.root, archiveDateActivationName))
			if err != nil || !bytes.Equal(policyBefore, policyAfter) {
				t.Fatal("policy/obligation changed", err)
			}
			rootAfter, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil || !bytes.Equal(rootBefore, rootAfter) {
				t.Fatal("active root changed", err)
			}
		})
	}
}

func TestArchiveDatePagesDurableIntentMissingRetryAndForgery(t *testing.T) {
	for _, fault := range []string{"intent", "unlink", "forged-next", "forged-ready", "wrong-key", "page-bytes", "symlink"} {
		t.Run(fault, func(t *testing.T) {
			f, e, head := datePageFixture(t, FormatNDJSON, 3)
			ctx := context.Background()
			authorizeDateReclaimStep(t, f, e)
			stop := errors.New("interrupted physical page transition")
			var afterIntent, afterUnlink func() error
			if fault == "unlink" {
				afterUnlink = func() error { return stop }
			} else {
				afterIntent = func() error { return stop }
			}
			out, err := e.reclaimRetainedDatePagesStep(ctx, f.p, archiveDateCutoff, DefaultDatePageReclaimLimits(), afterIntent, afterUnlink)
			if !errors.Is(err, stop) || out != (DatePageReclaimProgress{}) || e.dateProof != nil {
				t.Fatal(out, err)
			}
			path := filepath.Join(e.root, archiveDatePagesName)
			state, _, file, _, err := readDatePagesState(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if !state.Pending || state.Next != head || state.Previous != (ArchivePageRef{}) {
				t.Fatal(state)
			}
			switch fault {
			case "forged-next":
				page, err := e.readCatalogPage(ctx, head)
				if err != nil {
					t.Fatal(err)
				}
				page.Files[0].Path = "date=" + page.Files[0].Date + "/foreign-ownership.ndjson"
				payload, err := json.Marshal(page)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(payload)
				state.Next.SHA256, state.Next.Bytes = hex.EncodeToString(digest[:]), len(payload)
				if err := os.WriteFile(state.Next.path(e.root), payload, 0600); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "forged-ready":
				state.Next, state.Previous, state.Pending, state.Processed = ArchivePageRef{}, ArchivePageRef{}, false, state.Old.Pages
				raw, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-key":
				// Policy itself must reject this independently loaded key as well.
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				opts := datePolicyTestOptions(f, FormatNDJSON)
				opts.DatePolicyKey = bytes.Repeat([]byte{0x74}, 32)
				e, err = NewExporterWithOptions(e.root, opts)
				if err != nil {
					t.Fatal(err)
				}
				defer e.Close()
				if _, err := e.VerifyPublishedDateCatalogStep(ctx, f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits()); !errors.Is(err, ErrArchiveDateProof) {
					t.Fatal(err)
				}
				return
			case "page-bytes":
				raw, err := os.ReadFile(head.path(e.root))
				if err != nil {
					t.Fatal(err)
				}
				raw[0] = '['
				if err := os.WriteFile(head.path(e.root), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(head.path(e.root), head.path(e.root)+".preserved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(head.path(e.root)+".preserved", head.path(e.root)); err != nil {
					t.Fatal(err)
				}
			}
			e = reopenDatePageFixture(t, f, e, FormatNDJSON)
			out, err = e.ReclaimRetainedDatePagesStep(ctx, f.p, archiveDateCutoff, DefaultDatePageReclaimLimits())
			if fault == "intent" || fault == "unlink" {
				if err != nil || out.Phase != "ready" || out.PagesProcessed != 1 || !out.ObligationPending {
					t.Fatal(out, err)
				}
				if fault == "unlink" && (out.PagesMissing != 1 || out.PagesRemoved != 0 || out.LogicalBytesUnlinked != 0) {
					t.Fatal(out)
				}
			} else {
				if err == nil || out != (DatePageReclaimProgress{}) {
					t.Fatal("forged/replaced page accepted", out, err)
				}
				if _, err := os.Lstat(head.path(e.root)); err != nil {
					t.Fatal("refusal deleted replacement", err)
				}
				if fault == "forged-next" {
					if _, err := os.Stat(state.Next.path(e.root)); err != nil {
						t.Fatal("foreign page removed", err)
					}
				}
			}
		})
	}
}

func TestArchiveDatePagesBootstrapWitnessRejectsSameBytesRootReplacement(t *testing.T) {
	f, e, head := datePageFixture(t, FormatNDJSON, 3)
	path := filepath.Join(e.root, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".replacement", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".replacement", path); err != nil {
		t.Fatal(err)
	}
	// A changed physical root may fail at the fresh reference fence before the
	// completion witness is reached. Neither fence may mint page authority.
	verified, verifyErr := e.VerifyPublishedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits())
	if verifyErr == nil {
		for i := 0; verified.Phase != "ready" && i < 100; i++ {
			verified, verifyErr = e.VerifyPublishedDateCatalogStep(context.Background(), f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits())
			if verifyErr != nil {
				break
			}
		}
	}
	if verifyErr == nil {
		// The old index may itself refuse the changed inode. Leave that failure
		// visible; the page entry must still reject its older data witness before
		// it can publish an authenticated completion or next-hop intent.
		_, _ = e.PrepareDateCatalogReferencesStep(context.Background(), archiveDateCutoff, DefaultDateCatalogReferenceLimits())
	}
	out, err := e.ReclaimRetainedDatePagesStep(context.Background(), f.p, archiveDateCutoff, DefaultDatePageReclaimLimits())
	if !errors.Is(err, ErrArchiveDateProof) || out != (DatePageReclaimProgress{}) {
		t.Fatal("old physical scope supplied completion", out, err)
	}
	if _, err := os.Stat(head.path(e.root)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.root, archiveDatePagesName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replaced physical owner minted completion", err)
	}
}

func TestArchiveDatePagesCannotBootstrapFromSavedReadyOrFailedDataJob(t *testing.T) {
	for _, fault := range []string{"no-data", "saved-ready", "data-error"} {
		t.Run(fault, func(t *testing.T) {
			f, e, _ := dateReclaimStepFixture(t, FormatNDJSON, 3)
			ctx := context.Background()
			if fault == "saved-ready" {
				finishDateReclaimStep(t, f, e, DefaultDateReclaimStepLimits())
				e = reopenDatePageFixture(t, f, e, FormatNDJSON)
			} else if fault == "data-error" {
				authorizeDateReclaimStep(t, f, e)
				stop := errors.New("data checkpoint publication failed")
				if _, err := e.reclaimRetainedDateStep(ctx, f.p, archiveDateCutoff, DefaultDateReclaimStepLimits(), nil, func() error { return stop }); !errors.Is(err, stop) {
					t.Fatal(err)
				}
				if e.dateDataProof != nil {
					t.Fatal("failed data operation minted witness")
				}
			}
			authorizeDateReclaimStep(t, f, e)
			out, err := e.ReclaimRetainedDatePagesStep(ctx, f.p, archiveDateCutoff, DefaultDatePageReclaimLimits())
			if !errors.Is(err, ErrArchiveDateProof) || out != (DatePageReclaimProgress{}) {
				t.Fatal("saved/failed data Ready was authority", out, err)
			}
			if _, err := os.Stat(filepath.Join(e.root, archiveDatePagesName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unproved completion signed a transition", err)
			}
		})
	}
}

func TestArchiveDatePagesCloseJoinsActualUnlinkAndClearsWitness(t *testing.T) {
	f, e, head := datePageFixture(t, FormatNDJSON, 3)
	authorizeDateReclaimStep(t, f, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() {
		out, err := e.reclaimRetainedDatePagesStep(ctx, f.p, archiveDateCutoff, DefaultDatePageReclaimLimits(), nil, func() error { close(entered); <-release; return nil })
		if out != (DatePageReclaimProgress{}) {
			err = errors.Join(err, errors.New("retired job returned progress"))
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("unlink not entered")
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close skipped physical unlink join", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if e.dateDataProof != nil || e.datePolicyKey != ([32]byte{}) {
		t.Fatal("retired owner retained authority")
	}
	if _, err := os.Stat(head.path(e.root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	e = reopenDatePageFixture(t, f, e, FormatNDJSON)
	out, err := e.ReclaimRetainedDatePagesStep(context.Background(), f.p, archiveDateCutoff, DefaultDatePageReclaimLimits())
	if err != nil || out.Phase != "ready" || out.PagesMissing != 1 {
		t.Fatal(out, err)
	}
}

func TestArchiveDatePagesProtectsSharedCurrentPage(t *testing.T) {
	f := newDiskFailureFixture(t, windowRequest("retained-page", true, true))
	e, err := NewExporterWithOptions(t.TempDir(), datePolicyTestOptions(f, FormatNDJSON))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx := context.Background()
	if _, err := e.ExportDisk(ctx, f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MigrateArchiveCatalog(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	old, err := e.ReadArchivePosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	finishDateCandidate(t, e, archiveDateCutoff)
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	activated, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff)
	if err != nil || activated.Current.CatalogHead != old.CatalogHead {
		t.Fatal("fixture did not share original page", activated, err)
	}
	finishDateReclaimStep(t, f, e, DefaultDateReclaimStepLimits())
	authorizeDateReclaimStep(t, f, e)
	out, err := e.ReclaimRetainedDatePagesStep(ctx, f.p, archiveDateCutoff, DefaultDatePageReclaimLimits())
	if err != nil || out.Phase != "ready" || out.PagesProtected != 1 || out.PagesRemoved != 0 || out.LogicalBytesUnlinked != 0 || !out.ObligationPending {
		t.Fatal(out, err)
	}
	if _, err := e.readCatalogPage(ctx, old.CatalogHead); err != nil {
		t.Fatal("current page removed", err)
	}
	finishPublishedDateVerification(t, e, f.p)
}

func TestArchiveDatePagesSignedIntentSurvivesNativeAndRootAppend(t *testing.T) {
	f, e, oldHead := datePageFixture(t, FormatNDJSON, 3)
	ctx := context.Background()
	authorizeDateReclaimStep(t, f, e)
	stop := errors.New("stop with authentic next-hop intent")
	if _, err := e.reclaimRetainedDatePagesStep(ctx, f.p, archiveDateCutoff, DefaultDatePageReclaimLimits(), func() error { return stop }, nil); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	for _, event := range windowRequest("after-page-intent", true, true) {
		appendDiskDetailEvent(t, f, event)
	}
	if _, err := e.ExportDisk(ctx, f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	appended, err := e.ReadArchivePosition(ctx)
	if err != nil || appended.CatalogHead == (ArchivePageRef{}) {
		t.Fatal(appended, err)
	}
	if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetDateCatalogReferences(ctx); err != nil {
		t.Fatal(err)
	}
	e = reopenDatePageFixture(t, f, e, FormatNDJSON)
	out, err := e.ReclaimRetainedDatePagesStep(ctx, f.p, archiveDateCutoff, DefaultDatePageReclaimLimits())
	if err != nil || out.Phase != "ready" || out.PagesRemoved != 1 || out.Current != appended {
		t.Fatal(out, err)
	}
	if _, err := os.Stat(oldHead.path(e.root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := e.readCatalogPage(ctx, appended.CatalogHead); err != nil {
		t.Fatal("newly referenced page removed", err)
	}
	finishPublishedDateVerification(t, e, f.p)
}
