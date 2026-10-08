package usage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func compareSuccession(t *testing.T, f *diskFailureFixture, e *Exporter, cutoff time.Time, published bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		var out DateCatalogVerifyProgress
		var err error
		if published {
			out, err = e.VerifyPublishedDateCatalogStep(context.Background(), f.p, cutoff, DefaultDateCatalogVerifyLimits())
		} else {
			out, err = e.VerifyRetainedDateCatalogStep(context.Background(), f.p, cutoff, DefaultDateCatalogVerifyLimits())
		}
		if err != nil {
			t.Fatal(out, err)
		}
		if out.Phase == "ready" {
			return
		}
	}
	t.Fatal("comparison stalled")
}

func completeSuccession(t *testing.T, f *diskFailureFixture, e *Exporter, cutoff time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetDateCatalogReferences(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetRetainedDateReclamation(ctx); err != nil {
		t.Fatal(err)
	}
	compareSuccession(t, f, e, cutoff, true)
	for i := 0; ; i++ {
		out, err := e.PrepareDateCatalogReferencesStep(ctx, cutoff, DefaultDateCatalogReferenceLimits())
		if err != nil || i > 100 {
			t.Fatal(out, err)
		}
		if out.Phase == "ready" {
			break
		}
	}
	for i := 0; ; i++ {
		if e.dateProof == nil {
			compareSuccession(t, f, e, cutoff, true)
		}
		out, err := e.ReclaimRetainedDateStep(ctx, f.p, cutoff, DefaultDateReclaimStepLimits())
		if err != nil || i > 100 {
			t.Fatal(out, err)
		}
		if out.Phase == "ready" {
			break
		}
	}
	for i := 0; ; i++ {
		if e.dateProof == nil {
			compareSuccession(t, f, e, cutoff, true)
		}
		out, err := e.ReclaimRetainedDatePagesStep(ctx, f.p, cutoff, DefaultDatePageReclaimLimits())
		if err != nil || i > 100 {
			t.Fatal(out, err)
		}
		if out.Phase == "ready" {
			break
		}
	}
	if e.dateProof == nil {
		compareSuccession(t, f, e, cutoff, true)
	}
	out, err := e.CompleteRetainedDateObligation(ctx, f.p, cutoff)
	if err != nil || !out.OriginalChainComplete || !out.OrphansPending {
		t.Fatal(out, err)
	}
}

func prepareSuccession(t *testing.T, f *diskFailureFixture, e *Exporter, cutoff time.Time) {
	t.Helper()
	if err := e.ResetRetainedDateCatalogBuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetRetainedDateCatalogVerification(context.Background()); err != nil {
		t.Fatal(err)
	}
	finishDateCandidate(t, e, cutoff)
	compareSuccession(t, f, e, cutoff, false)
}

func TestArchiveDateSuccessionRepeatedPoliciesAndRestart(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e := dateActivationFixture(t, format)
			ctx := context.Background()
			compareSuccession(t, f, e, archiveDateCutoff, false)
			if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); err != nil {
				t.Fatal(err)
			}
			completeSuccession(t, f, e, archiveDateCutoff)
			for round := 1; round <= 3; round++ {
				cutoff := archiveDateCutoff.AddDate(0, 0, round*2)
				// New native facts with an expired date are still appended above
				// the prior prune H. Only this later authenticated sweep omits them.
				for _, event := range windowRequest(fmt.Sprintf("late-round-%d", round), false, false) {
					appendDiskDetailEvent(t, f, event)
				}
				if _, err := e.ExportDisk(ctx, f.p, DefaultDiskExportLimits()); err != nil {
					t.Fatal(err)
				}
				prior, err := os.ReadFile(filepath.Join(e.root, archiveDateActivePolicyName))
				if err != nil {
					t.Fatal(err)
				}
				prepareSuccession(t, f, e, cutoff)
				var out DateCatalogActivation
				if round == 2 {
					stop := errors.New("stop after successor root rename")
					out, err = e.activateRetainedDateCatalog(ctx, f.p, cutoff, func() error { return stop })
					if !errors.Is(err, stop) || out != (DateCatalogActivation{}) {
						t.Fatal("fault did not interrupt successor", out, err)
					}
					if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
						t.Fatal(err)
					}
					compareSuccession(t, f, e, cutoff, true)
				}
				out, err = e.ActivateRetainedDateCatalog(ctx, f.p, cutoff)
				if err != nil || out.AlreadyPublished != (round == 2) || out.Current.Retention.RetainFromDate != cutoff.Format("2006-01-02") || out.Current.Retention.PruneThroughSequence != f.a.Watermark().Sequence {
					t.Fatal(round, out, err)
				}
				after, err := os.ReadFile(filepath.Join(e.root, archiveDateActivePolicyName))
				if err != nil || !bytes.Equal(prior, after) {
					t.Fatal("next publication discarded previous completion", err)
				}
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				e, err = NewExporterWithOptions(e.root, datePolicyTestOptions(f, format))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = e.Close() })
				completeSuccession(t, f, e, cutoff)
				state, raw, file, _, err := readDateActivePolicyState(ctx, filepath.Join(e.root, archiveDateActivePolicyName))
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				_, current, err := state.Policy.positions()
				if err != nil || current.Retention.RetainFromDate != cutoff.Format("2006-01-02") || len(raw) > archiveDateActivePolicyBytes || state.Policy.PreviousCompletion == "" {
					t.Fatal(state, err)
				}
			}
		})
	}
}

func TestArchiveDateSuccessionPagesRequireNewDataCompletionWitness(t *testing.T) {
	f, e, _, _ := completedOrphanPageFixture(t, FormatNDJSON)
	completeDatePolicy(t, f, e)
	ctx := context.Background()
	cutoff := archiveDateCutoff.AddDate(0, 0, 2)
	before, err := os.ReadFile(filepath.Join(e.root, archiveDatePagesName))
	if err != nil {
		t.Fatal(err)
	}
	prepareSuccession(t, f, e, cutoff)
	if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, cutoff); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetDateCatalogReferences(ctx); err != nil {
		t.Fatal(err)
	}
	compareSuccession(t, f, e, cutoff, true)
	for {
		out, err := e.PrepareDateCatalogReferencesStep(ctx, cutoff, DefaultDateCatalogReferenceLimits())
		if err != nil {
			t.Fatal(err)
		}
		if out.Phase == "ready" {
			break
		}
	}
	out, err := e.ReclaimRetainedDatePagesStep(ctx, f.p, cutoff, DefaultDatePageReclaimLimits())
	if !errors.Is(err, ErrArchiveDateProof) || out != (DatePageReclaimProgress{}) {
		t.Fatal("prior completion substituted new data witness", out, err)
	}
	after, err := os.ReadFile(filepath.Join(e.root, archiveDatePagesName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("old transition discarded without witness", err)
	}
}

func TestArchiveDateSuccessionRejectsIncompleteOrCorruptPriorCompletion(t *testing.T) {
	for _, fault := range []string{"missing", "corrupt", "unfinished"} {
		t.Run(fault, func(t *testing.T) {
			f, e := dateActivationFixture(t, FormatNDJSON)
			ctx := context.Background()
			compareSuccession(t, f, e, archiveDateCutoff, false)
			if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); err != nil {
				t.Fatal(err)
			}
			if fault != "unfinished" {
				completeSuccession(t, f, e, archiveDateCutoff)
				path := filepath.Join(e.root, archiveDateActivePolicyName)
				if fault == "missing" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			root, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			marker, err := os.ReadFile(filepath.Join(e.root, archiveDateActivationName))
			if err != nil {
				t.Fatal(err)
			}
			cutoff := archiveDateCutoff.AddDate(0, 0, 2)
			prepareSuccession(t, f, e, cutoff)
			out, err := e.ActivateRetainedDateCatalog(ctx, f.p, cutoff)
			if err == nil || out != (DateCatalogActivation{}) {
				t.Fatal("invalid prior completion allowed rollover", out, err)
			}
			for name, before := range map[string][]byte{"manifest.json": root, archiveDateActivationName: marker} {
				after, readErr := os.ReadFile(filepath.Join(e.root, name))
				if readErr != nil || !bytes.Equal(before, after) {
					t.Fatal(name, readErr)
				}
			}
		})
	}
}

func TestArchiveDateSuccessionCompletionDigestIsAuthenticated(t *testing.T) {
	f, e, _, _ := completedOrphanPageFixture(t, FormatNDJSON)
	completeDatePolicy(t, f, e)
	ctx := context.Background()
	cutoff := archiveDateCutoff.AddDate(0, 0, 2)
	prepareSuccession(t, f, e, cutoff)
	if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, cutoff); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.root, archiveDateActivationName)
	state, _, file, info, err := readDateActivationState(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	state.PreviousCompletion = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := e.saveDateActivationState(ctx, path, state, file, info, false); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := e.VerifyPublishedDateCatalogStep(ctx, f.p, cutoff, DefaultDateCatalogVerifyLimits())
	if !errors.Is(err, ErrArchiveDateProof) || out != (DateCatalogVerifyProgress{}) {
		t.Fatal(out, err)
	}
}
