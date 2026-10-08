package metadatastate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/metadatajournal"
)

func TestMetadataSelectedReadOnlyPreservesActualSourceAndRefusesMutation(t *testing.T) {
	for _, suffix := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkpointed", true: "uncheckpointed"}[suffix], func(t *testing.T) {
			f := selectedMetadataNative(t)
			ctx := context.Background()
			if suffix {
				if _, err := f.baseline.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("tail"), Value: []byte("latest")}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.baseline.Close(); err != nil {
				t.Fatal(err)
			}
			paths := []string{f.path, filepath.Join(f.baseline.output, "active.root"), filepath.Join(f.baseline.output, "pages")}
			original := make([][]byte, len(paths))
			for i, path := range paths {
				var err error
				original[i], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			root, journal, err := f.baseline.DiskReservations()
			if err != nil {
				t.Fatal(err)
			}
			defer root.Release()
			defer journal.Release()
			o := SelectedOpenOptions{ReadOnly: true, BaselineOptions: baselineOptionsForSelectedFixture(f), Evidence: f.baseline.evidence, Binding: f.options.Binding, Publisher: f.options.Publisher, Guard: f.options.Guard, Authority: f.options.Authority, RootDiskReservation: root, JournalDiskReservation: journal, MaxJournalBytes: f.options.MaxBytes}
			b, receipt, err := OpenSelectedFromRoot(ctx, bytes.Repeat([]byte{51}, 32), f.baseline.output, f.path, o)

			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			a, err := b.StartSelectedStartupAudit(ctx, metadatajournal.StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if err := b.View(ctx, func(tx *Transaction) error {
				value, found, err := tx.Get([]string{"projects"}, []byte{0})
				if err != nil || !found || !bytes.Equal(value, []byte("accepted")) {
					t.Fatal(value, found, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if suffix {
				if receipt.AppliedFrames != 1 {
					t.Fatal(receipt)
				}
				if err := b.View(ctx, func(tx *Transaction) error {
					value, found, err := tx.Get([]string{"projects"}, []byte("tail"))
					if err != nil || !found || string(value) != "latest" {
						t.Fatal(value, found, err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := b.Update(ctx, func(*Transaction) error { t.Fatal("read-only write callback entered"); return nil }); err == nil {
				t.Fatal("read-only source admitted business mutation")
			}
			if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("write"), Value: []byte("forbidden")}}); err == nil {
				t.Fatal("read-only native admitted append")
			}
			if err := b.journal.HealthError(); err != nil {
				t.Fatal("write refusal poisoned read-only native", err)
			}
			cloneParent := b.readCloneParent
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if cloneParent != "" {
				if _, err := os.Lstat(cloneParent); !os.IsNotExist(err) {
					t.Fatal("diagnostic clone did not retire", err)
				}
			}
			for i, path := range paths {
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(actual, original[i]) {
					t.Fatal("read-only factory changed its source", path, err)
				}
			}
		})
	}
}

func baselineOptionsForSelectedFixture(f selectedMetadataNativeFixture) BaselineOptions {
	return BaselineOptions{Identity: f.baseline.evidence.Identity, InstanceID: f.baseline.evidence.InstanceID, KeyGeneration: f.baseline.evidence.KeyGeneration, Budget: f.options.Budget, MaxSourceBytes: 64 << 20, MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20}
}
