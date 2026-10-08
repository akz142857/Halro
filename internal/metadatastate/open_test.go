package metadatastate

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type metadataOpenFixture struct {
	owner        *Baseline
	opts         OpenOptions
	path         string
	rows         []authenticatedstate.Row
	selectedRoot authenticatedstate.SealRoot
}

func metadataReopenFixture(t *testing.T) *metadataOpenFixture {
	t.Helper()
	b, o, path := linkedDeltaCandidate(t)
	ctx := context.Background()
	if _, err := b.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("saved"), Value: []byte("one")}}); err != nil {
		t.Fatal(err)
	}
	saved, err := b.AppliedHead()
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.store.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selectedRoot, err := b.store.SealRootReference(ctx, authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentMetadata, IndexID: AuthenticatedMetadataSealIndexID, Codec: AuthenticatedMetadataSealCodec})
	if err != nil {
		t.Fatal(err)
	}
	rows := deltaRows(t, b)
	pin, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ReadBaselineEvidence(ctx, pin)
	if closeErr := pin.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || provenance != b.Evidence() {
		t.Fatalf("baseline provenance mismatch before reopen: stored=%#v memory=%#v err=%v", provenance, b.Evidence(), err)
	}
	if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("permitted"), Value: []byte("two")}}); err != nil {
		t.Fatal(err)
	}
	allowed := b.journal.Head()
	if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("excluded"), Value: []byte("three")}}); err != nil {
		t.Fatal(err)
	}
	head := b.journal.Head()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	rc, jc, err := b.DiskReservations()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rc.Release(); jc.Release() })
	return &metadataOpenFixture{owner: b, path: path, rows: rows, selectedRoot: selectedRoot, opts: OpenOptions{BaselineOptions: o, Evidence: b.Evidence(), SavedRoot: root, SavedHead: saved, NativeHead: head, PermittedHead: allowed, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: 16 << 20}}
}
func metadataTestKey() []byte { return bytes.Repeat([]byte{51}, 32) }
func assertMetadataRow(t *testing.T, b *Baseline, key string, value []byte, found bool) {
	t.Helper()
	pin, err := b.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	got, exists, err := pin.Get(context.Background(), mustDeltaKey(t, []string{"projects"}, []byte(key), false))
	if err != nil || exists != found || !bytes.Equal(got, value) {
		t.Fatal(key, got, exists, err)
	}
}
func TestMetadataReopenUsesQualifiedSavedRootAndOnlyAllowedDelta(t *testing.T) {
	f := metadataReopenFixture(t)
	before := f.opts.Budget.Snapshot().Used
	var phase string
	next, r, err := openFromRoot(context.Background(), metadataTestKey(), f.owner.output, f.path, f.opts, func(value string) { phase = value })
	if err != nil {
		t.Fatalf("phase=%s: %v", phase, err)
	}
	defer next.Close()
	if r.From != f.opts.SavedHead || r.To != f.opts.PermittedHead || r.NativeHead != f.opts.NativeHead || r.SourceFrames != 4 || r.AppliedFrames != 1 || r.Root.Revision != f.opts.SavedRoot.Revision+1 {
		t.Fatal(r)
	}
	assertMetadataRow(t, next, "saved", []byte("one"), true)
	assertMetadataRow(t, next, "permitted", []byte("two"), true)
	assertMetadataRow(t, next, "excluded", nil, false)
	if _, err := next.AppendFrame(context.Background(), []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("no"), Value: []byte("v")}}); err == nil {
		t.Fatal("append crossed excluded durable tail")
	}
	if err := next.Discard(); !errors.Is(err, ErrSource) {
		t.Fatal("borrower acquired deletion authority", err)
	}
	if _, err := next.ReplayTo(context.Background(), f.opts.NativeHead, 3); err != nil {
		t.Fatal(err)
	}
	assertMetadataRow(t, next, "excluded", []byte("three"), true)
	if _, err := next.AppendFrame(context.Background(), []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("new"), Value: []byte("four")}}); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.AdoptClosedSuccessor(next); err == nil {
		t.Fatal("active successor adopted")
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	if f.opts.Budget.Snapshot().Used != before {
		t.Fatal("borrowed fees leaked/refunded publisher", before, f.opts.Budget.Snapshot())
	}
	if err := f.owner.AdoptClosedSuccessor(next); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Discard(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataReopenRequiresSelectedSealRootBeforeOrphanRepair(t *testing.T) {
	f := metadataReopenFixture(t)
	pages := filepath.Join(f.owner.output, "pages")
	original, err := os.ReadFile(pages)
	if err != nil {
		t.Fatal(err)
	}
	orphan := append(bytes.Clone(original), []byte("private-orphan")...)
	if err := os.WriteFile(pages, orphan, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(pages, original, 0600)
	baseline := f.opts.Budget.Snapshot().Used

	wrong := f.selectedRoot
	wrong.Digest[0] ^= 1
	f.opts.SelectedSealRoot = &wrong
	if reopened, _, err := OpenFromRoot(context.Background(), metadataTestKey(), f.owner.output, f.path, f.opts); !errors.Is(err, authenticatedstate.ErrRecovery) || reopened != nil {
		t.Fatalf("mismatched selected root was accepted: %v", err)
	}
	got, err := os.ReadFile(pages)
	if err != nil || !bytes.Equal(got, orphan) || f.opts.Budget.Snapshot().Used != baseline {
		t.Fatalf("selected-root refusal repaired/refunded: err=%v usage=%+v", err, f.opts.Budget.Snapshot().Used)
	}

	f.opts.SelectedSealRoot = &f.selectedRoot
	reopened, receipt, err := OpenFromRoot(context.Background(), metadataTestKey(), f.owner.output, f.path, f.opts)
	if err != nil || receipt.AppliedFrames != 1 {
		t.Fatalf("matching selected root failed qualified reopen: receipt=%+v err=%v", receipt, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.AdoptClosedSuccessor(reopened); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataSealReferenceBindsAuthenticatedRootToAppliedPosition(t *testing.T) {
	b, _, _ := linkedDeltaCandidate(t)
	defer func() {
		if err := b.Discard(); err != nil {
			t.Fatal(err)
		}
	}()
	ctx := context.Background()
	if _, err := b.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("applied"), Value: []byte("one")}}); err != nil {
		t.Fatal(err)
	}
	// A durable native tail may be ahead of the applied root. The receipt must
	// name the exact reducer position and never pair that root with the tail.
	if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("unapplied"), Value: []byte("two")}}); err != nil {
		t.Fatal(err)
	}
	got, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	position, err := b.AppliedHead()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := b.store.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema.Component != authenticatedstate.SealComponentMetadata || got.Root.Component != got.Schema.Component || got.Root.IndexID != AuthenticatedMetadataSealIndexID || got.Root.Codec != AuthenticatedMetadataSealCodec || got.Root.FileID != identity.ID || got.Root.Rows != identity.Rows {
		t.Fatalf("root reference does not identify current authenticated root: %+v %+v", got.Root, identity)
	}
	if got.Position.Component != authenticatedstate.SealComponentMetadata || got.Position.Epoch != position.Epoch || got.Position.Revision != position.FileRevision || got.Position.Sequence != position.Sequence || got.Position.Offset != uint64(position.Offset) || got.Position.Hash != position.Hash || got.Position.MetadataBaseline != position.Baseline.Cut {
		t.Fatalf("position is not the applied Metadata cut: %+v %+v", got.Position, position)
	}
	if got.Position.Sequence+1 != b.journal.Head().Sequence {
		t.Fatalf("receipt crossed unapplied native tail: receipt=%d head=%d", got.Position.Sequence, b.journal.Head().Sequence)
	}
}

func TestMetadataSealReferenceRejectsRootWhoseAppliedPositionWasAltered(t *testing.T) {
	b, _, _ := linkedDeltaCandidate(t)
	defer func() {
		if err := b.Discard(); err != nil {
			t.Fatal(err)
		}
	}()
	ctx := context.Background()
	if _, err := b.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("applied"), Value: []byte("one")}}); err != nil {
		t.Fatal(err)
	}
	key, err := RowKey([][]byte{[]byte("meta")}, []byte("applied_journal_sequence"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.Update(ctx, [32]byte{1}, func(tx *authenticatedstate.Transaction) error {
		return tx.Put(key, binary.BigEndian.AppendUint64(nil, b.position.Sequence+1))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SealReference(ctx); !errors.Is(err, ErrSource) {
		t.Fatalf("altered authenticated applied position was accepted: %v", err)
	}
}

func TestMetadataReopenQualificationRefusesBeforeFilesChange(t *testing.T) {
	for _, shape := range []string{"key", "identity", "generation", "evidence", "root digest", "root revision", "saved hash", "saved offset", "allowed hash", "allowed before saved", "native hash", "trim revision", "foreign manager", "missing root fee", "missing native fee", "pressure", "canceled", "partial tail", "bad tail MAC"} {
		t.Run(shape, func(t *testing.T) {
			f := metadataReopenFixture(t)
			o := f.opts
			key := metadataTestKey()
			ctx := context.Background()
			originalNative, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			pages := filepath.Join(f.owner.output, "pages")
			originalPages, err := os.ReadFile(pages)
			if err != nil {
				t.Fatal(err)
			}
			withOrphan := append(bytes.Clone(originalPages), []byte("private-orphan")...)
			if err := os.WriteFile(pages, withOrphan, 0600); err != nil {
				t.Fatal(err)
			}
			defer os.WriteFile(pages, originalPages, 0600)
			switch shape {
			case "key":
				key[0] ^= 1
			case "identity":
				o.Identity[0] ^= 1
			case "generation":
				o.KeyGeneration++
			case "evidence":
				o.Evidence.ManifestDigest = "unqualified"
			case "root digest":
				o.SavedRoot.Digest[0] ^= 1
			case "root revision":
				o.SavedRoot.Revision++
			case "saved hash":
				o.SavedHead.Hash[0] ^= 1
			case "saved offset":
				o.SavedHead.Offset++
			case "allowed hash":
				o.PermittedHead.Hash[0] ^= 1
			case "allowed before saved":
				o.PermittedHead.Sequence = 0
			case "native hash":
				o.NativeHead.Hash[0] ^= 1
			case "trim revision":
				o.NativeHead.FileRevision++
			case "foreign manager":
				o.Budget = baselineBudget(t)
			case "missing root fee":
				o.RootDiskReservation = resourcebudget.Permit{}
			case "missing native fee":
				o.JournalDiskReservation = resourcebudget.Permit{}
			case "pressure":
				if err := o.Budget.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(err)
				}
				defer o.Budget.SetPressure(resourcebudget.Healthy)
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "partial tail":
				if err := os.WriteFile(f.path, append(bytes.Clone(originalNative), []byte("HM")...), 0600); err != nil {
					t.Fatal(err)
				}
				defer os.WriteFile(f.path, originalNative, 0600)
			case "bad tail MAC":
				bad := bytes.Clone(originalNative)
				bad[len(bad)-1] ^= 1
				if err := os.WriteFile(f.path, bad, 0600); err != nil {
					t.Fatal(err)
				}
				defer os.WriteFile(f.path, originalNative, 0600)
			}
			beforeNative, _ := os.ReadFile(f.path)
			beforeRoot, _ := os.ReadFile(filepath.Join(f.owner.output, "active.root"))
			before := f.opts.Budget.Snapshot().Used
			next, r, err := OpenFromRoot(ctx, key, f.owner.output, f.path, o)
			if err == nil || next != nil || r != (ReopenReceipt{}) {
				t.Fatal("unqualified reopen", next, r, err)
			}
			afterNative, _ := os.ReadFile(f.path)
			afterPages, _ := os.ReadFile(pages)
			afterRoot, _ := os.ReadFile(filepath.Join(f.owner.output, "active.root"))
			if !bytes.Equal(beforeNative, afterNative) || !bytes.Equal(withOrphan, afterPages) || !bytes.Equal(beforeRoot, afterRoot) || f.opts.Budget.Snapshot().Used != before {
				t.Fatal("refusal changed files/fees", f.opts.Budget.Snapshot())
			}
		})
	}
}

func TestMetadataReopenCancellationJoinsFactoryAndRetainsPersistentFees(t *testing.T) {
	for _, phaseName := range []string{"source-verified", "component-open"} {
		t.Run(phaseName, func(t *testing.T) {
			f := metadataReopenFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			before := f.opts.Budget.Snapshot().Used
			go func() {
				next, r, err := openFromRoot(ctx, metadataTestKey(), f.owner.output, f.path, f.opts, func(phase string) {
					if phase == phaseName {
						close(entered)
						<-release
					}
				})
				if next != nil || r != (ReopenReceipt{}) {
					err = errors.Join(err, fmt.Errorf("canceled factory returned candidate"))
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("phase not reached")
			}
			cancel()
			live := f.opts.Budget.Snapshot().Used
			if live.Disk != before.Disk || live.RAM < before.RAM+DeltaRAMBytes {
				t.Fatal("factory not charged", live, before)
			}
			select {
			case err := <-done:
				t.Fatal("returned before physical work joined", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("factory not joined")
			}
			if f.opts.Budget.Snapshot().Used != before {
				t.Fatal("factory charges leaked", f.opts.Budget.Snapshot(), before)
			}
		})
	}
}

type metadataProcessSelection struct {
	Evidence               BaselineEvidence
	Root                   authenticatedstate.RootIdentity
	Saved, Native, Allowed metadatajournal.Head
	Rows                   []authenticatedstate.Row
	Trim                   *metadatajournal.TrimStateBinding
}

func TestMetadataReopenPublisherProcessHelper(t *testing.T) {
	dir := os.Getenv("HALRO_METADATA_REOPEN_TEST_DIR")
	if dir == "" {
		t.Skip("subprocess-only helper")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	archive, _, _ := baselineArchive(t, true, nil, nil)
	o := baselineOptions(t)
	key := metadataTestKey()
	ctx := context.Background()
	b, err := BuildFromBackup(ctx, archive, key, privateSource(t), filepath.Join(dir, "root"), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.StartNewEpoch(ctx, key, filepath.Join(dir, "metadata.journal"), 8, 16<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("saved"), Value: []byte("one")}}); err != nil {
		t.Fatal(err)
	}
	root, err := b.store.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := b.AppliedHead()
	if err != nil {
		t.Fatal(err)
	}
	rows := deltaRows(t, b)
	if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("permitted"), Value: []byte("two")}}); err != nil {
		t.Fatal(err)
	}
	allowed := b.journal.Head()
	if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("excluded"), Value: []byte("three")}}); err != nil {
		t.Fatal(err)
	}
	native := b.journal.Head()
	var trim *metadatajournal.TrimStateBinding
	if os.Getenv("HALRO_METADATA_TRIM_TEST") == "1" {
		r, err := b.TrimAtApplied(ctx, native)
		if err != nil {
			t.Fatal(err)
		}
		trim = &r.State
		saved = r.AppliedHead
		native = r.NativeHead
		allowed = metadataHeadAt(t, b.journal, 2)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(metadataProcessSelection{Evidence: b.Evidence(), Root: root, Saved: saved, Native: native, Allowed: allowed, Rows: rows, Trim: trim})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "selection.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestMetadataReopenSurvivesPublisherProcessWithNewManager(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMetadataReopenPublisherProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HALRO_METADATA_REOPEN_TEST_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("publisher: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var selection metadataProcessSelection
	if err := json.Unmarshal(raw, &selection); err != nil {
		t.Fatal(err)
	}
	o := baselineOptions(t)
	rc, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Release()
	jc, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer jc.Release()
	opts := OpenOptions{BaselineOptions: o, Evidence: selection.Evidence, SavedRoot: selection.Root, SavedHead: selection.Saved, NativeHead: selection.Native, PermittedHead: selection.Saved, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: 16 << 20}
	b, r, err := OpenFromRoot(context.Background(), metadataTestKey(), filepath.Join(dir, "root"), filepath.Join(dir, "metadata.journal"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if r.AppliedFrames != 0 || !reflect.DeepEqual(deltaRows(t, b), selection.Rows) {
		t.Fatal("saved authenticated rows changed", r)
	}
	if _, err := b.ReplayTo(context.Background(), selection.Native, selection.Allowed.Sequence); err != nil {
		t.Fatal(err)
	}
	assertMetadataRow(t, b, "permitted", []byte("two"), true)
	assertMetadataRow(t, b, "excluded", nil, false)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if o.Budget.Snapshot().Used.Disk != o.MaxDiskBytes+16<<20 {
		t.Fatal("Close refunded persistent inventory", o.Budget.Snapshot())
	}
}

func TestMetadataReopenCanonicalEpochBaselineAndWholeTailQualification(t *testing.T) {
	for _, shape := range []string{"empty epoch", "permitted first frame", "forbidden excluded tail"} {
		t.Run(shape, func(t *testing.T) {
			b, o, path := linkedDeltaCandidate(t)
			ctx := context.Background()
			root, err := b.store.RootIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := b.AppliedHead()
			if err != nil {
				t.Fatal(err)
			}
			allowed := saved
			if shape != "empty epoch" {
				if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("first"), Value: []byte("v")}}); err != nil {
					t.Fatal(err)
				}
				allowed = b.journal.Head()
			}
			if shape == "forbidden excluded tail" {
				if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"usage_daily_rollup"}, Key: []byte("excluded"), Value: []byte("v")}}); err != nil {
					t.Fatal(err)
				}
			}
			native := b.journal.Head()
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			rc, jc, err := b.DiskReservations()
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Release()
			defer jc.Release()
			opts := OpenOptions{BaselineOptions: o, Evidence: b.Evidence(), SavedRoot: root, SavedHead: saved, NativeHead: native, PermittedHead: allowed, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: 16 << 20}
			beforeRoot, _ := os.ReadFile(filepath.Join(b.output, "active.root"))
			beforePages, _ := os.ReadFile(filepath.Join(b.output, "pages"))
			beforeNative, _ := os.ReadFile(path)
			before := o.Budget.Snapshot().Used
			next, r, err := OpenFromRoot(ctx, metadataTestKey(), b.output, path, opts)
			if shape == "forbidden excluded tail" {
				if err == nil || next != nil || r != (ReopenReceipt{}) {
					t.Fatal("forbidden signed tail accepted", next, r, err)
				}
				afterRoot, _ := os.ReadFile(filepath.Join(b.output, "active.root"))
				afterPages, _ := os.ReadFile(filepath.Join(b.output, "pages"))
				afterNative, _ := os.ReadFile(path)
				if !bytes.Equal(beforeRoot, afterRoot) || !bytes.Equal(beforePages, afterPages) || !bytes.Equal(beforeNative, afterNative) || o.Budget.Snapshot().Used != before {
					t.Fatal("excluded-tail refusal changed files/fees")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if r.From != saved || r.To != allowed || r.AppliedFrames != allowed.Sequence {
				t.Fatal(r)
			}
			if shape == "empty epoch" {
				if r.Root != root {
					t.Fatal("empty epoch changed root", r)
				}
			} else {
				assertMetadataRow(t, next, "first", []byte("v"), true)
			}
			if err := next.Close(); err != nil {
				t.Fatal(err)
			}
			if err := b.AdoptClosedSuccessor(next); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMetadataReopenRecoversDurableFrameAfterUnpublishedRoot(t *testing.T) {
	b, o, path := linkedDeltaCandidate(t)
	ctx := context.Background()
	if _, err := b.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("saved"), Value: []byte("one")}}); err != nil {
		t.Fatal(err)
	}
	saved, err := b.AppliedHead()
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.store.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeRoot, _ := os.ReadFile(filepath.Join(b.output, "active.root"))
	cancelCtx, cancel := context.WithCancel(ctx)
	r, err := b.appendFrame(cancelCtx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("durable"), Value: []byte("two")}}, func(phase string) {
		if phase == "native-durable" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(r, metadatajournal.Record{}) {
		t.Fatal(r, err)
	}
	actual := b.journal.Head()
	if actual.Sequence != 2 {
		t.Fatal("actual durable head lost", actual)
	}
	afterRoot, _ := os.ReadFile(filepath.Join(b.output, "active.root"))
	if !bytes.Equal(beforeRoot, afterRoot) {
		t.Fatal("unconfirmed root published")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	rc, jc, err := b.DiskReservations()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Release()
	defer jc.Release()
	opts := OpenOptions{BaselineOptions: o, Evidence: b.Evidence(), SavedRoot: root, SavedHead: saved, NativeHead: actual, PermittedHead: actual, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: 16 << 20}
	next, receipt, err := OpenFromRoot(ctx, metadataTestKey(), b.output, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if receipt.AppliedFrames != 1 || receipt.Root.Revision != root.Revision+1 {
		t.Fatal(receipt)
	}
	assertMetadataRow(t, next, "durable", []byte("two"), true)
	if _, err := next.ReplayTo(ctx, actual, actual.Sequence); err != nil {
		t.Fatal(err)
	}
	unchanged, err := next.store.RootIdentity(ctx)
	if err != nil || unchanged != receipt.Root {
		t.Fatal("retry applied durable frame twice", unchanged, err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.AdoptClosedSuccessor(next); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataReopenAuthenticatedButWrongBookkeepingNeverBecomesQueryable(t *testing.T) {
	b, o, path := linkedDeltaCandidate(t)
	ctx := context.Background()
	// A content MAC alone cannot credit malformed semantic bookkeeping.
	if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("p"), Value: []byte("v")}}); err != nil {
		t.Fatal(err)
	}
	var record metadatajournal.Record
	if _, err := b.journal.InspectBoundContext(ctx, func(r metadatajournal.Record) error {
		if r.Kind == metadatajournal.KindOperations {
			record = r
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	head := b.journal.Head()
	if _, err := b.store.Update(ctx, deltaCut(head, record), func(tx *authenticatedstate.Transaction) error {
		return tx.Put(mustDeltaKey(t, []string{"meta"}, []byte("applied_journal_sequence"), false), []byte("malformed"))
	}); err != nil {
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
	defer rc.Release()
	defer jc.Release()
	opts := OpenOptions{BaselineOptions: o, Evidence: b.Evidence(), SavedRoot: root, SavedHead: head, NativeHead: head, PermittedHead: head, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: 16 << 20}
	next, r, err := OpenFromRoot(ctx, metadataTestKey(), b.output, path, opts)
	if !errors.Is(err, ErrSource) || next != nil || r != (ReopenReceipt{}) {
		t.Fatal("bad bookkeeping became queryable", next, r, err)
	}
}

func TestMetadataReopenFailedSuccessorCannotTransferCleanupReceipt(t *testing.T) {
	f := metadataReopenFixture(t)
	next, _, err := OpenFromRoot(context.Background(), metadataTestKey(), f.owner.output, f.path, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	original, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.Clone(original)
	bad[len(bad)-1] ^= 1
	if err := os.WriteFile(f.path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(f.path, original, 0600)
	if _, err := next.ReplayTo(context.Background(), f.opts.NativeHead, f.opts.NativeHead.Sequence); err == nil {
		t.Fatal("bad source accepted")
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.AdoptClosedSuccessor(next); !errors.Is(err, ErrSource) {
		t.Fatal("failed successor transferred deletion receipt", err)
	}
	// Test fixture cleanup may explicitly adopt the still-valid root bytes;
	// the production Baseline API above must not credit this failed job.
	if err := f.owner.store.AdoptClosedSuccessor(next.store); err != nil {
		t.Fatal(err)
	}
}
