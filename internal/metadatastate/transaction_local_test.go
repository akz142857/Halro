package metadatastate

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/metadatajournal"
)

func TestMetadataFacadeLocalRowsChangeContentWithoutAdvancingNativeCut(t *testing.T) {
	b, store, _ := metadataFacadeFixture(t)
	ctx := context.Background()
	// The baseline shape fixture uses opaque bytes for local rows; initialize
	// this particular DAO counter to its real absent/zero representation.
	if _, err := b.Update(ctx, func(tx *Transaction) error {
		return tx.Delete([]string{"meta"}, []byte("shutdown_truncated_attempts_total"))
	}); err != nil {
		t.Fatal(err)
	}
	head, err := b.AppliedHead()
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.store.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total, err := store.AddShutdownTruncatedAttempts(3); err != nil || total != 3 {
		t.Fatal("actual operational-counter DAO", total, err)
	}
	if after, err := b.AppliedHead(); err != nil || after != head || b.journal.Head() != head {
		t.Fatal("local row became native fact", after, head, err)
	}
	if after, err := b.store.RootIdentity(ctx); err != nil || after.Cut != root.Cut || after.Revision != root.Revision+1 || after.Digest == root.Digest {
		t.Fatal("local content was not durably committed at same cut", after, root, err)
	}
	if total, err := store.ShutdownTruncatedAttempts(); err != nil || total != 3 {
		t.Fatal(total, err)
	}
	// A journal caller cannot use the business-only local path to smuggle C/E
	// rows into a replicated frame. A refusal must preserve the healthy owner.
	if _, err := b.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"meta"}, Key: []byte("shutdown_truncated_attempts_total"), Value: []byte("x")}}); !errors.Is(err, ErrSource) {
		t.Fatal(err)
	}
	if after, err := b.AppliedHead(); err != nil || after != head || store.RecoveryError() != nil {
		t.Fatal(after, err)
	}
}

func TestMetadataFacadeMixedLocalAndAuthoritativeRowsCommitOneFilteredFrame(t *testing.T) {
	b, _, _ := metadataFacadeFixture(t)
	ctx := context.Background()
	head, err := b.AppliedHead()
	if err != nil {
		t.Fatal(err)
	}
	authoritative := []byte(`{"id":"prj_mixed","name":"Mixed transaction","enabled":true}`)
	local := []byte(`{"version":1,"sequence":1}`)
	record, err := b.Update(ctx, func(tx *Transaction) error {
		if err := tx.Put([]string{"projects"}, []byte("prj_mixed"), authoritative); err != nil {
			return err
		}
		return tx.Put([]string{"meta"}, []byte("governance_journal_anchor"), local)
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Sequence != head.Sequence+1 || len(record.Ops) != 1 || record.Ops[0].Path[0] != "projects" {
		t.Fatal("local row escaped into native format2 frame", record)
	}
	if err := b.View(ctx, func(tx *Transaction) error {
		for _, row := range []struct {
			path, key string
			value     []byte
		}{{"projects", "prj_mixed", authoritative}, {"meta", "governance_journal_anchor", local}} {
			got, found, err := tx.Get([]string{row.path}, []byte(row.key))
			if err != nil || !found || !bytes.Equal(got, row.value) {
				t.Fatal(row.key, got, found, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := b.AppliedHead()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Update(ctx, func(tx *Transaction) error {
		if err := tx.Put([]string{"meta"}, []byte("governance_journal_anchor"), []byte("changed")); err != nil {
			return err
		}
		return tx.Put([]string{"meta"}, []byte("unknown-runtime-field"), []byte("x"))
	}); err == nil {
		t.Fatal("unclassified mixed transaction accepted")
	}
	if after, err := b.AppliedHead(); err != nil || after != before {
		t.Fatal("refused mixed transaction changed native head", after, before, err)
	}
	if err := b.View(ctx, func(tx *Transaction) error {
		got, found, err := tx.Get([]string{"meta"}, []byte("governance_journal_anchor"))
		if err != nil || !found || !bytes.Equal(got, local) {
			t.Fatal("refused mixed transaction changed local content", got, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
