package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func completeDatePolicy(t *testing.T, f *diskFailureFixture, e *Exporter) DateObligationCompletion {
	t.Helper()
	authorizeDateReclaimStep(t, f, e)
	out, err := e.CompleteRetainedDateObligation(context.Background(), f.p, archiveDateCutoff)
	if err != nil || !out.OriginalChainComplete || !out.PolicyPreserved || !out.OrphansPending {
		t.Fatal(out, err)
	}
	return out
}

func TestArchiveDateActivePolicyPreservesAuthorityWithoutOriginalMarker(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e, old, oldRaw := completedOrphanPageFixture(t, format)
			rootBefore, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			markerBefore, err := os.ReadFile(filepath.Join(e.root, archiveDateActivationName))
			if err != nil {
				t.Fatal(err)
			}
			pagesBefore, err := os.ReadFile(filepath.Join(e.root, archiveDatePagesName))
			if err != nil {
				t.Fatal(err)
			}
			first := completeDatePolicy(t, f, e)
			if first.AlreadyRecorded {
				t.Fatal(first)
			}
			for name, raw := range map[string][]byte{"manifest.json": rootBefore, archiveDateActivationName: markerBefore, archiveDatePagesName: pagesBefore} {
				actual, err := os.ReadFile(filepath.Join(e.root, name))
				if err != nil || !bytes.Equal(actual, raw) {
					t.Fatal("completion changed original facts", name, err)
				}
			}
			activeBefore, err := os.ReadFile(filepath.Join(e.root, archiveDateActivePolicyName))
			if err != nil {
				t.Fatal(err)
			}
			if len(activeBefore) > archiveDateActivePolicyBytes {
				t.Fatal("unbounded policy slot")
			}
			if err := os.Remove(filepath.Join(e.root, archiveDateActivationName)); err != nil {
				t.Fatal(err)
			}
			e = reopenDatePageFixture(t, f, e, format)
			retry, err := e.ActivateRetainedDateCatalog(context.Background(), f.p, archiveDateCutoff)
			if err != nil || !retry.AlreadyPublished || !retry.ReclamationPending {
				t.Fatal("active record did not preserve retry", retry, err)
			}
			if _, err := os.Stat(filepath.Join(e.root, archiveDateActivationName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fallback recreated old marker", err)
			}
			again := completeDatePolicy(t, f, e)
			if !again.AlreadyRecorded {
				t.Fatal(again)
			}
			after, err := os.ReadFile(filepath.Join(e.root, archiveDateActivePolicyName))
			if err != nil || !bytes.Equal(activeBefore, after) {
				t.Fatal("idempotent copy changed authority", err)
			}
			// Fresh reclamation still verifies native/current lineage using the retained
			// policy slot, including objects restored after historical chain processing.
			if err := os.WriteFile(old.path(e.root), oldRaw, 0600); err != nil {
				t.Fatal(err)
			}
			removed := 0
			for i := 0; i < 10; i++ {
				out := orphanPagesStep(t, f, e, DefaultDateOrphanPageReclaimLimits())
				removed += out.PagesRemoved
				if out.Phase == "scan-complete" {
					break
				}
				if i == 9 {
					t.Fatal("fallback inventory stalled")
				}
			}
			if removed != 1 {
				t.Fatal(removed)
			}
		})
	}
}

func TestArchiveDateActivePolicyFutureIntentCannotReplaceCurrentAuthority(t *testing.T) {
	f, e, _, _ := completedOrphanPageFixture(t, FormatNDJSON)
	completeDatePolicy(t, f, e)
	ctx := context.Background()
	current, err := e.ReadArchivePosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldRoot, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	// An authentic next-policy intent may exist before root rename. It is a future
	// decision, not the currently published retention or its original prune H.
	future := current.Retention
	future.RetainFromDate = archiveDateCutoff.AddDate(0, 0, 1).Format("2006-01-02")
	nextRoot, _, err := archiveCatalogRootBytes(current.LastSequence, false, current.CatalogHead, future)
	if err != nil {
		t.Fatal(err)
	}
	next := archiveDateActivationState{Version: 1, Phase: "intent", OldRoot: oldRoot, NewRoot: nextRoot}
	if err := e.sealDatePolicy(ctx, &next, f.a.Watermark()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.root, archiveDateActivationName)
	_, _, file, info, err := readDateActivationState(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.saveDateActivationState(ctx, path, next, file, info, false); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	futureBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ResetPublishedDateCatalogVerification(ctx); err != nil {
		t.Fatal(err)
	}
	e = reopenDatePageFixture(t, f, e, FormatNDJSON)
	result, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff)
	if err != nil || !result.AlreadyPublished || result.Current != current {
		t.Fatal("future intent supplied current policy", result, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, futureBefore) {
		t.Fatal("current retry marked future published", err)
	}
	completeDatePolicy(t, f, e)
}

func TestArchiveDateActivePolicyRequiresAuthenticOriginalCompletion(t *testing.T) {
	for _, fault := range []string{"missing", "pending", "forged-ready", "unsigned-policy"} {
		t.Run(fault, func(t *testing.T) {
			f, e, _ := datePageFixture(t, FormatNDJSON, 3)
			if fault != "missing" {
				authorizeDateReclaimStep(t, f, e)
				stop := errors.New("stop before old-page unlink")
				if _, err := e.reclaimRetainedDatePagesStep(context.Background(), f.p, archiveDateCutoff, DefaultDatePageReclaimLimits(), func() error { return stop }, nil); !errors.Is(err, stop) {
					t.Fatal(err)
				}
			}
			if fault == "forged-ready" {
				path := filepath.Join(e.root, archiveDatePagesName)
				state, _, file, _, err := readDatePagesState(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				state.Next, state.Previous, state.Pending, state.Processed = ArchivePageRef{}, ArchivePageRef{}, false, state.Old.Pages
				raw, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			authorizeDateReclaimStep(t, f, e)
			if fault == "unsigned-policy" {
				path := filepath.Join(e.root, archiveDateActivationName)
				state, _, file, _, err := readDateActivationState(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				state.Version, state.Receipt = 1, nil
				raw, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := e.CompleteRetainedDateObligation(context.Background(), f.p, archiveDateCutoff)
			if err == nil || out != (DateObligationCompletion{}) || e.dateProof != nil {
				t.Fatal("unauthenticated completion accepted", out, err)
			}
			if _, err := os.Stat(filepath.Join(e.root, archiveDateActivePolicyName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failure published active policy", err)
			}
		})
	}
}

func TestArchiveDateActivePolicyTamperingAndRollbackCannotGrantAuthority(t *testing.T) {
	for _, fault := range []string{"mac", "policy", "chain", "wrong-key", "current-root", "symlink", "future-corrupt"} {
		t.Run(fault, func(t *testing.T) {
			f, e, _, _ := completedOrphanPageFixture(t, FormatNDJSON)
			completeDatePolicy(t, f, e)
			path := filepath.Join(e.root, archiveDateActivePolicyName)
			state, _, file, _, err := readDateActivePolicyState(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(e.root, archiveDateActivationName)); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "mac":
				state.MAC = "00"
			case "policy":
				state.Policy.Receipt.MAC = "00"
			case "chain":
				state.OriginalChain.DataPrefixSHA = state.OriginalChain.BindingSHA
			case "wrong-key":
				e.datePolicyKey[0] ^= 1
			case "current-root":
				root, err := openArchiveManifest(e.root)
				if err != nil {
					t.Fatal(err)
				}
				position, _, err := root.walk(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				position.Retention.RetainFromDate = archiveDateCutoff.AddDate(0, 0, 1).Format("2006-01-02")
				if err := e.publishArchiveCatalogRootWithRetention(context.Background(), root, false, position.LastSequence, false, position.CatalogHead, position.Retention, &archiveWork{remaining: archiveManifestValueLimit}, nil); err != nil {
					t.Fatal(err)
				}
				if err := root.file.Close(); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".foreign", path); err != nil {
					t.Fatal(err)
				}
			case "future-corrupt":
				if err := os.WriteFile(filepath.Join(e.root, archiveDateActivationName), []byte("bad future slot"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "mac" || fault == "policy" || fault == "chain" {
				raw, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.ResetPublishedDateCatalogVerification(context.Background()); err != nil {
				t.Fatal(err)
			}
			cutoff := archiveDateCutoff
			if fault == "current-root" {
				cutoff = cutoff.AddDate(0, 0, 1)
			}
			out, err := e.VerifyPublishedDateCatalogStep(context.Background(), f.p, cutoff, DefaultDateCatalogVerifyLimits())
			if err == nil || out != (DateCatalogVerifyProgress{}) || e.dateProof != nil {
				t.Fatal("tampered/rolled-back slots granted current authority", fault, out, err)
			}
		})
	}
}

func TestArchiveDateActivePolicyPublishedCopyFailureAndFreshRetry(t *testing.T) {
	for _, fault := range []string{"before-save", "after-save", "signed-pages-replace", "root-replace"} {
		t.Run(fault, func(t *testing.T) {
			f, e, _, _ := completedOrphanPageFixture(t, FormatNDJSON)
			authorizeDateReclaimStep(t, f, e)
			stop := errors.New("copy interrupted")
			var before, after func() error
			switch fault {
			case "before-save":
				before = func() error { return stop }
			case "after-save":
				after = func() error { return stop }
			case "signed-pages-replace":
				before = func() error {
					path := filepath.Join(e.root, archiveDatePagesName)
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					if err := os.WriteFile(path+".replacement", raw, 0600); err != nil {
						return err
					}
					return os.Rename(path+".replacement", path)
				}
			case "root-replace":
				before = func() error {
					path := filepath.Join(e.root, "manifest.json")
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					if err := os.WriteFile(path+".replacement", raw, 0600); err != nil {
						return err
					}
					return os.Rename(path+".replacement", path)
				}
			}
			out, err := e.completeRetainedDateObligation(context.Background(), f.p, archiveDateCutoff, before, after)
			if err == nil || out != (DateObligationCompletion{}) || e.dateProof != nil {
				t.Fatal("failed copy reported completion", out, err)
			}
			_, statErr := os.Stat(filepath.Join(e.root, archiveDateActivePolicyName))
			if fault == "after-save" {
				if statErr != nil {
					t.Fatal("committed copy vanished", statErr)
				}
				again := completeDatePolicy(t, f, e)
				if !again.AlreadyRecorded {
					t.Fatal(again)
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("pre-copy failure published", statErr)
			}
		})
	}
}

func TestArchiveDateActivePolicyCloseJoinsActualCopyBeforeErasingKey(t *testing.T) {
	f, e, _, _ := completedOrphanPageFixture(t, FormatNDJSON)
	authorizeDateReclaimStep(t, f, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() {
		out, err := e.completeRetainedDateObligation(ctx, f.p, archiveDateCutoff, nil, func() error { close(entered); <-release; return nil })
		if out != (DateObligationCompletion{}) {
			err = errors.Join(err, errors.New("retired copy retained report"))
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("actual copy did not finish")
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close escaped physical callback", err)
	case <-time.After(20 * time.Millisecond):
	}
	if e.datePolicyKey == ([32]byte{}) {
		t.Fatal("key erased before actual join")
	}
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if e.datePolicyKey != ([32]byte{}) || e.datePolicyPrefix != nil {
		t.Fatal("policy key/resolver survived Close")
	}
}

func TestArchiveDateActivePolicyDerivedPhaseDoesNotRewriteCompletion(t *testing.T) {
	f, e, _, _ := completedOrphanPageFixture(t, FormatNDJSON)
	completeDatePolicy(t, f, e)
	path := filepath.Join(e.root, archiveDateActivePolicyName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(e.root, archiveDateActivationName)
	state, _, file, info, err := readDateActivationState(context.Background(), markerPath)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase = "intent"
	if err := e.saveDateActivationState(context.Background(), markerPath, state, file, info, false); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	again := completeDatePolicy(t, f, e)
	if !again.AlreadyRecorded {
		t.Fatal(again)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("derived phase changed active completion", err)
	}
}

func TestArchiveDateActivePolicyMissingMarkerCannotDiscardUnfinishedPolicy(t *testing.T) {
	f, e, _ := dateReclaimStepFixture(t, FormatNDJSON, 2)
	ctx := context.Background()
	if err := os.Remove(filepath.Join(e.root, archiveDateActivationName)); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetRetainedDateCatalogBuild(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.ResetRetainedDateCatalogVerification(ctx); err != nil {
		t.Fatal(err)
	}
	future := archiveDateCutoff.AddDate(0, 0, 1)
	for i := 0; i < 20; i++ {
		out, err := e.PrepareRetainedDateCatalogStep(ctx, future, DefaultDateCatalogBuildLimits())
		if err != nil {
			t.Fatal(out, err)
		}
		if out.Phase == "ready" {
			break
		}
		if i == 19 {
			t.Fatal("future build stalled")
		}
	}
	for i := 0; i < 20; i++ {
		out, err := e.VerifyRetainedDateCatalogStep(ctx, f.p, future, DefaultDateCatalogVerifyLimits())
		if err != nil {
			t.Fatal(out, err)
		}
		if out.Phase == "ready" {
			break
		}
		if i == 19 {
			t.Fatal("future comparison stalled")
		}
	}
	out, err := e.ActivateRetainedDateCatalog(ctx, f.p, future)
	if !errors.Is(err, ErrArchiveDateProof) || out != (DateCatalogActivation{}) {
		t.Fatal("missing marker bypassed unfinished old obligation", out, err)
	}
	position, err := e.ReadArchivePosition(ctx)
	if err != nil || position.Retention.RetainFromDate != archiveDateCutoff.Format("2006-01-02") {
		t.Fatal("failure changed current policy", position, err)
	}
	if _, err := os.Stat(filepath.Join(e.root, archiveDateActivationName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unauthorized replacement was signed", err)
	}
}
