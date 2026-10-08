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

func TestArchiveDatePolicyDerivedMetadataCannotAdvanceOriginalBaseline(t *testing.T) {
	for _, format := range []string{FormatNDJSON, FormatParquet} {
		t.Run(format, func(t *testing.T) {
			f, e, _ := dateReclaimFileFixture(t, format)
			ctx := context.Background()
			original := f.a.Watermark().Sequence
			for _, event := range windowRequest("late-baseline-probe", false, false) {
				appendDiskDetailEvent(t, f, event)
			}
			var late []AttemptEvent
			for _, row := range f.a.Snapshot().Attempts {
				if row.Sequence > original {
					late = append(late, row)
				}
			}
			entry := publishDateComparisonFault(t, e, "2026-10-03", "baseline-probe", late)
			// Even a genuine fresh proof obtained before the mutation is withdrawn.
			authorizeDateFileReclaim(t, f, e)
			path := filepath.Join(e.root, archiveDateActivationName)
			state, _, file, _, err := readDateActivationState(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			old, current, err := state.positions()
			if err != nil {
				t.Fatal(err)
			}
			fake := f.a.Watermark().Sequence
			current.Retention.PruneThroughSequence = fake
			state.OldRoot, _, err = archiveCatalogRootBytes(fake, old.CompletePrefix, old.CatalogHead, old.Retention)
			if err != nil {
				t.Fatal(err)
			}
			state.NewRoot, _, err = archiveCatalogRootBytes(fake, false, current.CatalogHead, current.Retention)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(e.root, "manifest.json"), state.NewRoot, 0600); err != nil {
				t.Fatal(err)
			}
			nativePath := filepath.Join(filepath.Dir(f.p.db.Path()), "ledger.wal")
			nativeBefore, err := os.ReadFile(nativePath)
			if err != nil {
				t.Fatal(err)
			}
			out, err := e.ReclaimRetainedDateFile(ctx, f.p, archiveDateCutoff, entry, DefaultDateFileReclaimLimits())
			if !errors.Is(err, ErrArchiveDateProof) || out != (DateFileReclamation{}) {
				t.Fatalf("original baseline %d moved to %d: %+v %v", original, fake, out, err)
			}
			verified, err := e.VerifyPublishedDateCatalogStep(ctx, f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits())
			if !errors.Is(err, ErrArchiveDateProof) || verified != (DateCatalogVerifyProgress{}) || e.dateProof != nil {
				t.Fatal("forged policy minted a fresh proof", verified, err)
			}
			step, err := e.ReclaimRetainedDateStep(ctx, f.p, archiveDateCutoff, DefaultDateReclaimStepLimits())
			if !errors.Is(err, ErrArchiveDateProof) || step != (DateReclaimProgress{}) {
				t.Fatal(step, err)
			}
			nativeAfter, err := os.ReadFile(nativePath)
			if err != nil || !bytes.Equal(nativeBefore, nativeAfter) {
				t.Fatal("native authority changed", err)
			}
			if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(entry.Path))); err != nil {
				t.Fatal("protected late archive file removed", err)
			}
		})
	}
}

func TestArchiveDatePolicyRequiresIndependentKeyAndOriginalNativePrefix(t *testing.T) {
	for _, fault := range []string{"unsigned-v1", "unsigned-v2", "wrong-key", "signature", "native-branch"} {
		t.Run(fault, func(t *testing.T) {
			f, e, entry := dateReclaimFileFixture(t, FormatNDJSON)
			ctx := context.Background()
			path := filepath.Join(e.root, archiveDateActivationName)
			state, _, file, _, err := readDateActivationState(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "unsigned-v1":
				state.Version, state.Receipt = 1, nil
			case "unsigned-v2":
				state.Receipt.MAC = ""
			case "signature":
				if state.Receipt.MAC[0] == '0' {
					state.Receipt.MAC = "1" + state.Receipt.MAC[1:]
				} else {
					state.Receipt.MAC = "0" + state.Receipt.MAC[1:]
				}
			case "wrong-key":
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				options := datePolicyTestOptions(f, FormatNDJSON)
				options.DatePolicyKey = bytes.Repeat([]byte{0x72}, 32)
				e, err = NewExporterWithOptions(e.root, options)
				if err != nil {
					t.Fatal(err)
				}
				defer e.Close()
			case "native-branch":
				// A different, validly MAC-authenticated history at the same sequence
				// cannot reuse a receipt from the original history.
				events := append(windowRequest("expired", false, false), windowRequest("retained", true, true)...)
				for i := range events {
					events[i].OccurredAt = events[i].OccurredAt.Add(time.Second)
				}
				other := newDiskFailureFixture(t, events)
				if err := f.native.Close(); err != nil {
					t.Fatal(err)
				}
				replacement, err := os.ReadFile(filepath.Join(filepath.Dir(other.p.db.Path()), "ledger.wal"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(f.p.db.Path()), "ledger.wal"), replacement, 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			out, err := e.VerifyPublishedDateCatalogStep(ctx, f.p, archiveDateCutoff, DefaultDateCatalogVerifyLimits())
			if !errors.Is(err, ErrArchiveDateProof) || out != (DateCatalogVerifyProgress{}) || e.dateProof != nil {
				t.Fatal("untrusted policy accepted", fault, out, err)
			}
			if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(entry.Path))); err != nil {
				t.Fatal(err)
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(unchanged, raw) {
				t.Fatal("refusal silently backfilled receipt", err)
			}
		})
	}
}

func TestArchiveDatePolicyAbsentAuthorityCannotPublishOrBackfillIntent(t *testing.T) {
	for _, legacyIntent := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent-authority", true: "unsigned-intent"}[legacyIntent], func(t *testing.T) {
			f, e := dateActivationFixture(t, FormatNDJSON)
			finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
			root, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(e.root, archiveDateActivationName)
			var intent []byte
			if legacyIntent {
				proof := e.dateProof
				candidate, _, err := archiveCatalogRootBytes(proof.candidate.LastSequence, false, proof.candidate.CatalogHead, proof.candidate.Retention)
				if err != nil {
					t.Fatal(err)
				}
				intent, err = json.Marshal(archiveDateActivationState{Version: 1, Phase: "intent", OldRoot: root, NewRoot: candidate})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, intent, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				e, err = NewExporterWithOptions(e.root, Options{Format: FormatNDJSON})
				if err != nil {
					t.Fatal(err)
				}
				defer e.Close()
				finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
			}
			out, err := e.ActivateRetainedDateCatalog(context.Background(), f.p, archiveDateCutoff)
			if !errors.Is(err, ErrArchiveDateProof) || out != (DateCatalogActivation{}) || e.dateProof != nil {
				t.Fatal(out, err)
			}
			after, err := os.ReadFile(filepath.Join(e.root, "manifest.json"))
			if err != nil || !bytes.Equal(after, root) {
				t.Fatal("refusal published root", err)
			}
			afterIntent, err := os.ReadFile(path)
			if legacyIntent {
				if err != nil || !bytes.Equal(afterIntent, intent) {
					t.Fatal("unsigned intent silently acquired receipt", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("missing authority wrote obligation", err)
			}
		})
	}
}

func TestArchiveDatePolicyCloseJoinsResolverBeforeErasingKey(t *testing.T) {
	f, e := dateActivationFixture(t, FormatNDJSON)
	finishDateVerification(t, e, f.p, DefaultDateCatalogVerifyLimits())
	original := e.datePolicyPrefix
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	e.datePolicyPrefix = func(ctx context.Context, head ledger.Watermark) (ledger.ProjectionPrefix, error) {
		close(entered)
		<-release
		return original(ctx, head)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.ActivateRetainedDateCatalog(context.Background(), f.p, archiveDateCutoff)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("resolver not entered")
	}
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close returned before real resolver exit", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	if err := <-done; !errors.Is(err, ErrDiskUsageClosed) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if e.datePolicyKey != ([32]byte{}) || e.datePolicyPrefix != nil || e.datePolicyReady {
		t.Fatal("policy authority retained after actual Close")
	}
}
