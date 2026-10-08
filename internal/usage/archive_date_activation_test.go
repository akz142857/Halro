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

	"github.com/akz142857/Halro/internal/ledger"
)

// Independent native authority for fixtures; no derived root supplies key/hash.
func datePolicyTestOptions(f *diskFailureFixture, format string) Options {
	key := bytes.Repeat([]byte{0x71}, 32)
	return Options{Format: format, DatePolicyKey: key, DatePolicyPrefix: func(ctx context.Context, head ledger.Watermark) (ledger.ProjectionPrefix, error) {
		prefix, _, err := ledger.InspectProjectionCursor(ctx, filepath.Join(filepath.Dir(f.p.db.Path()), "ledger.wal"), key, head.Generation, head.Sequence)
		if err != nil || prefix.Head != head {
			return ledger.ProjectionPrefix{}, errors.Join(err, ErrArchiveDateProof)
		}
		return prefix, nil
	}}
}

func dateActivationFixture(t *testing.T, format string) (*diskFailureFixture, *Exporter) {
	t.Helper()
	f := newDiskFailureFixture(t, append(windowRequest("expired", false, false), windowRequest("retained", true, true)...))
	e, err := NewExporterWithOptions(t.TempDir(), datePolicyTestOptions(f, format))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if _, err := e.ExportDisk(context.Background(), f.p, DefaultDiskExportLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MigrateArchiveCatalog(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	finishDateCandidate(t, e, archiveDateCutoff)
	return f, e
}

func finishPublishedDateVerification(t *testing.T, e *Exporter, p *DiskProjection) DateCatalogVerifyProgress {
	t.Helper()
	limits := DefaultDateCatalogVerifyLimits()
	limits.Files, limits.Source.Rows, limits.Source.PageRows = 1, 1, 1
	for i := 0; i < 100; i++ {
		out, err := e.VerifyPublishedDateCatalogStep(context.Background(), p, archiveDateCutoff, limits)
		if err != nil {
			t.Fatal(i, out, err)
		}
		if out.Phase == "ready" {
			return out
		}
	}
	t.Fatal("published comparison stalled")
	return DateCatalogVerifyProgress{}
}

func TestArchiveDateActivationDurableObligationAndReopenComparison(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e := dateActivationFixture(t, format)
			ctx := context.Background()
			files, err := e.ReadArchiveFiles(ctx, "", 16)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
			out, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff)
			if err != nil || out.AlreadyPublished || !out.ReclamationPending || out.Current.SchemaVersion != 10 || out.Current.Files != 1 || out.Previous.Files != 2 || e.archiveEpoch != 1 || e.dateProof != nil || e.windowProof != nil || e.windowPending != nil {
				t.Fatal(out, err)
			}
			state, _, marker, _, err := readDateActivationState(ctx, filepath.Join(e.root, archiveDateActivationName))
			if err != nil {
				t.Fatal(err)
			}
			if err := marker.Close(); err != nil {
				t.Fatal(err)
			}
			if state.Phase != "published" || !bytes.Equal(state.OldRoot, before) {
				t.Fatal(state)
			}
			// Logical expiry preserves every original physical partition.
			for _, entry := range files.Files {
				if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(entry.Path))); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			e, err = NewExporterWithOptions(e.root, datePolicyTestOptions(f, format))
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if out, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); !errors.Is(err, ErrArchiveDateProof) || out != (DateCatalogActivation{}) {
				t.Fatal("saved publication was authority", out, err)
			}
			verified := finishPublishedDateVerification(t, e, f.p)
			if verified.Report.MatchedRows != 2 || verified.Report.ExpiredOmittedRows != 2 {
				t.Fatal(verified)
			}
			again, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff)
			if err != nil || !again.AlreadyPublished || !again.ReclamationPending || again.Current != out.Current || e.archiveEpoch != 0 {
				t.Fatal(again, err)
			}
		})
	}
}

func TestArchiveDateActivationInterruptedRenameRecoveryAndProofConsumption(t *testing.T) {
	f, e := dateActivationFixture(t, FormatNDJSON)
	ctx := context.Background()
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	stop := errors.New("interrupted after root rename before directory sync")
	out, err := e.activateRetainedDateCatalog(ctx, f.p, archiveDateCutoff, func() error { return stop })
	if !errors.Is(err, stop) || out != (DateCatalogActivation{}) || e.dateProof != nil {
		t.Fatal(out, err)
	}
	state, _, marker, _, err := readDateActivationState(ctx, filepath.Join(e.root, archiveDateActivationName))
	if err != nil {
		t.Fatal(err)
	}
	if err := marker.Close(); err != nil {
		t.Fatal(err)
	}
	if state.Phase != "intent" {
		t.Fatal(state)
	}
	position, err := e.ReadArchivePosition(ctx)
	if err != nil || position.SchemaVersion != 10 {
		t.Fatal(position, err)
	}
	if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); !errors.Is(err, ErrArchiveDateProof) {
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
	finishPublishedDateVerification(t, e, f.p)
	out, err = e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff)
	if err != nil || !out.AlreadyPublished || out.Current != position {
		t.Fatal(out, err)
	}
	// Completing the derived phase must not make this operation's checkpoint
	// unusable. A new full comparison is still required on every retry.
	finishPublishedDateVerification(t, e, f.p)
	if again, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); err != nil || !again.AlreadyPublished || again.Current != position {
		t.Fatal(again, err)
	}
}

func TestArchiveDateActivationRefusesChangedProofAndReader(t *testing.T) {
	for _, fault := range []string{"no-proof", "build", "verify", "root", "owner", "reader", "source", "published-checkpoint", "obligation"} {
		t.Run(fault, func(t *testing.T) {
			f, e := dateActivationFixture(t, FormatNDJSON)
			ctx := context.Background()
			if fault != "no-proof" {
				finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
			}
			if fault == "published-checkpoint" || fault == "obligation" {
				if _, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff); err != nil {
					t.Fatal(err)
				}
				finishPublishedDateVerification(t, e, f.p)
			}
			before, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "build", "verify", "published-checkpoint":
				name := archiveDateBuildName
				if fault == "verify" {
					name = archiveDateVerifyName
				}
				if fault == "published-checkpoint" {
					name = archiveDatePublishedVerifyName
				}
				path := filepath.Join(e.root, name)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			case "root":
				path := filepath.Join(e.root, "manifest.json")
				if err := os.WriteFile(path+".replacement", before, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".replacement", path); err != nil {
					t.Fatal(err)
				}
			case "owner":
				f.p = newDiskFailureFixture(t, append(windowRequest("expired", false, false), windowRequest("retained", true, true)...)).p
			case "source":
				for _, event := range windowRequest("future", true, true) {
					appendDiskDetailEvent(t, f, event)
				}
			case "reader":
				release, err := e.beginArchiveRead(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			case "obligation":
				path := filepath.Join(e.root, archiveDateActivationName)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var state archiveDateActivationState
				if err := json.Unmarshal(raw, &state); err != nil {
					t.Fatal(err)
				}
				state.Phase = "intent"
				raw, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff)
			if err == nil || out != (DateCatalogActivation{}) {
				t.Fatal(out, err)
			}
			if fault == "reader" && !errors.Is(err, ErrDiskUsageBusy) {
				t.Fatal(err)
			}
			after, readErr := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("invalid proof changed live coverage", readErr)
			}
		})
	}
}

func TestArchiveDateActivationRecoveryPreservesLateExpiredAppend(t *testing.T) {
	f, e := dateActivationFixture(t, FormatNDJSON)
	ctx := context.Background()
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	first, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff)
	if err != nil {
		t.Fatal(err)
	}
	// Old UTC dates above the prune baseline are new facts. Their files remain
	// referenced, and recovery must preserve the appended current root exactly.
	for _, event := range windowRequest("late-expired", false, false) {
		appendDiskDetailEvent(t, f, event)
	}
	if _, err := e.ExportDiskStepPreservingCatalog(ctx, f.p, DefaultDiskExportStepLimits()); err != nil {
		t.Fatal(err)
	}
	position, err := e.ReadArchivePosition(ctx)
	if err != nil || position.LastSequence <= first.Current.LastSequence || position.Files != 2 || position.Retention != first.Current.Retention {
		t.Fatal(position, err)
	}
	before, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
	if err != nil {
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
	verified := finishPublishedDateVerification(t, e, f.p)
	if verified.Report.RequiredRows != 4 || verified.Report.MatchedRows != 4 || verified.Report.ExpiredOmittedRows != 2 {
		t.Fatal(verified)
	}
	out, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff)
	if err != nil || !out.AlreadyPublished || out.Current != position || out.Previous != first.Previous {
		t.Fatal(out, err)
	}
	after, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("recovery replaced appended root", err)
	}
}

func TestArchiveDateActivationCloseJoinsActualRootPublication(t *testing.T) {
	f, e := dateActivationFixture(t, FormatNDJSON)
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() {
		out, err := e.activateRetainedDateCatalog(ctx, f.p, archiveDateCutoff, func() error { close(entered); <-release; return nil })
		if out != (DateCatalogActivation{}) {
			err = errors.Join(err, errors.New("retired activation returned authority"))
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not enter")
	}
	cancel()
	exporterClosed, projectionClosed := make(chan error, 1), make(chan error, 1)
	go func() { exporterClosed <- e.Close() }()
	go func() { projectionClosed <- f.p.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for !e.archiveRetired.Load() || !f.p.retired.Load() {
		if time.Now().After(deadline) {
			t.Fatal("owners did not withdraw")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-exporterClosed:
		t.Fatal("Exporter Close passed physical publication", err)
	default:
	}
	select {
	case err := <-projectionClosed:
		t.Fatal("projection Close passed physical publication", err)
	default:
	}
	unblock()
	if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := <-exporterClosed; err != nil {
		t.Fatal(err)
	}
	if err := <-projectionClosed; err != nil {
		t.Fatal(err)
	}
	state, _, marker, _, err := readDateActivationState(context.Background(), filepath.Join(e.root, archiveDateActivationName))
	if err != nil {
		t.Fatal(err)
	}
	if err := marker.Close(); err != nil {
		t.Fatal(err)
	}
	if state.Phase != "intent" {
		t.Fatal("late cancellation erased obligation", state)
	}
}

func TestArchiveDateActivationPublishedAncestryCannotComeFromMarker(t *testing.T) {
	f, e := dateActivationFixture(t, FormatNDJSON)
	ctx := context.Background()
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	out, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.root, archiveDateActivationName)
	state, _, marker, _, err := readDateActivationState(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := marker.Close(); err != nil {
		t.Fatal(err)
	}
	// The old all-date head exists and has valid hashes, but it is not an
	// ancestor of the published retained head. Saved intent cannot assert it is.
	state.NewRoot, _, err = archiveCatalogRootBytes(out.Previous.LastSequence, false, out.Previous.CatalogHead, out.Current.Retention)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		progress, err := e.VerifyPublishedDateCatalogStep(ctx, f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits())
		if err != nil {
			if e.dateProof != nil || progress != (DateCatalogVerifyProgress{}) {
				t.Fatal(progress, err)
			}
			return
		}
		if progress.Phase == "ready" {
			t.Fatal("marker granted ancestry", progress)
		}
	}
	t.Fatal("bad ancestry was not rejected")
}

func TestArchiveDateActivationPublishedBaselineRequiresLateExpiredFacts(t *testing.T) {
	f, e := dateActivationFixture(t, FormatNDJSON)
	ctx := context.Background()
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	out, err := e.ActivateRetainedDateCatalog(ctx, f.p, archiveDateCutoff)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range windowRequest("missing-late-expired", false, false) {
		appendDiskDetailEvent(t, f, event)
	}
	// Fault: claim a newer processed H but omit every late expired attempt.
	// Existing candidate pages still form valid ancestry and their own rows match.
	root, err := openArchiveManifest(e.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.publishArchiveCatalogRootWithRetention(ctx, root, false, f.a.Watermark().Sequence, false, out.Current.CatalogHead, out.Current.Retention, &archiveWork{remaining: archiveManifestValueLimit}, nil); err != nil {
		t.Fatal(err)
	}
	if err := root.file.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		progress, err := e.VerifyPublishedDateCatalogStep(ctx, f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits())
		if err != nil {
			if !errors.Is(err, ErrDiskUsageCorrupt) || e.dateProof != nil || progress != (DateCatalogVerifyProgress{}) {
				t.Fatal(progress, err)
			}
			return
		}
		if progress.Phase == "ready" {
			t.Fatal("expired date hid a missing new fact", progress)
		}
	}
	t.Fatal("missing late fact was not rejected")
}
