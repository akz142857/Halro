package metadatastate

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestMetadataZeroSequenceDerivedRootNeedsActualCommonSelection(t *testing.T) {
	ctx := context.Background()
	f := selectedMetadataNativeCount(t, 0)
	b := f.baseline
	if _, err := b.Update(ctx, func(tx *Transaction) error {
		return tx.Put([]string{"meta"}, []byte("shutdown_truncated_attempts_total"), []byte("2"))
	}); err != nil {
		t.Fatal(err)
	}
	ref, err := b.SealReference(ctx)
	if err != nil || b.journal.Head().Sequence != 0 || ref.Root.State.Digest == f.head.Baseline.RootDigest {
		t.Fatal("fixture did not produce an actual zero-frame derived root", err)
	}
	seal, found, err := f.options.Publisher.Select(f.options.Authority)
	if err != nil || !found {
		t.Fatal(err)
	}
	seal.Revision++
	seal.Roots[0] = ref.Root
	if _, err := f.options.Publisher.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	root, err := b.store.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	rc, jc, err := b.DiskReservations()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(rc.Release(), jc.Release()); err != nil {
			t.Error(err)
		}
	}()
	o := OpenOptions{BaselineOptions: BaselineOptions{Identity: b.evidence.Identity, InstanceID: b.evidence.InstanceID, KeyGeneration: b.evidence.KeyGeneration, Budget: f.options.Budget, MaxSourceBytes: 64 << 20, MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20}, Evidence: b.evidence, SavedRoot: root, SelectedSealRoot: &ref.Root, SavedHead: f.head, NativeHead: f.head, PermittedHead: f.head, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: f.options.MaxBytes}
	if candidate, _, err := OpenFromRoot(ctx, bytes.Repeat([]byte{51}, 32), b.output, f.path, o); candidate != nil || !errors.Is(err, ErrSource) {
		t.Fatal("caller root plus receipt acquired zero-frame selection authority", candidate, err)
	}
	selected := SelectedOpenOptions{BaselineOptions: o.BaselineOptions, Evidence: o.Evidence, Binding: f.options.Binding, Publisher: f.options.Publisher, Guard: f.options.Guard, Authority: f.options.Authority, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: f.options.MaxBytes}
	reopened, receipt, err := OpenSelectedFromRoot(ctx, bytes.Repeat([]byte{51}, 32), b.output, f.path, selected)
	if err != nil || receipt.SourceFrames != 0 || receipt.AppliedFrames != 0 {
		t.Fatal("actual common root did not authorize derived state", receipt, err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := reopened.AuditSelectedRootStartup(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reopened.View(ctx, func(tx *Transaction) error {
		value, found, err := tx.Get([]string{"meta"}, []byte("shutdown_truncated_attempts_total"))
		if err != nil || !found || string(value) != "2" {
			return errors.Join(authenticatedstate.ErrCorrupt, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
