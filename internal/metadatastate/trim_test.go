package metadatastate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func metadataHeadAt(t *testing.T, l *metadatajournal.Log, sequence uint64) metadatajournal.Head {
	t.Helper()
	head := l.Head()
	result := head
	seen := false
	var previous metadatajournal.Record
	have := false
	observe := func(end int64) {
		if previous.Sequence == sequence {
			result.Sequence, result.Hash, result.Offset = sequence, previous.Hash, end
			seen = true
		}
	}
	_, err := l.InspectBoundContext(context.Background(), func(r metadatajournal.Record) error {
		if have {
			observe(r.Offset)
		}
		previous = metadatajournal.Record{Sequence: r.Sequence, Hash: r.Hash}
		if r.Kind == metadatajournal.KindTrim {
			previous.Sequence = r.Trim.TrimmedThrough
			copy(previous.Hash[:], r.Trim.ChainHeadAtTrim)
		}
		have = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if have {
		observe(head.Offset)
	}
	if !seen {
		t.Fatal("missing actual boundary", sequence)
	}
	return result
}
func metadataTrimCandidate(t *testing.T) (*Baseline, BaselineOptions, string) {
	t.Helper()
	b, o, path := linkedDeltaCandidate(t)
	for i := 1; i <= 2; i++ {
		if _, err := b.AppendFrame(context.Background(), []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte(fmt.Sprint("frame", i)), Value: []byte(fmt.Sprint(i))}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("frame3"), Value: []byte("3")}}); err != nil {
		t.Fatal(err)
	}
	return b, o, path
}
func trimmedOpenOptions(t *testing.T, b *Baseline, o BaselineOptions, receipt TrimReceipt) OpenOptions {
	t.Helper()
	root, err := b.store.RootIdentity(context.Background())
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
	t.Cleanup(func() { rc.Release(); jc.Release() })
	state := receipt.State
	return OpenOptions{BaselineOptions: o, Evidence: b.Evidence(), SavedRoot: root, SavedHead: receipt.AppliedHead, NativeHead: receipt.NativeHead, PermittedHead: receipt.AppliedHead, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: 16 << 20, TrimState: &state}
}
func TestMetadataTrimPreservesCompleteRootAndQualifiedReopenAcrossRepeatedRevision(t *testing.T) {
	b, o, path := metadataTrimCandidate(t)
	ctx := context.Background()
	beforeRows := deltaRows(t, b)
	root, err := b.store.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source := b.journal.Head()
	beforeFee := o.Budget.Snapshot().Used
	receipt, err := b.TrimAtApplied(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State.Root != metadatajournal.RootCommitment(root) || receipt.State.Sequence != 2 || receipt.NativeHead.Sequence != 3 || receipt.NativeHead.TrimmedThrough != 2 || receipt.NativeHead.FileRevision != 2 || receipt.AppliedHead != metadataHeadAt(t, b.journal, 2) {
		t.Fatal(receipt)
	}
	if after, err := b.store.RootIdentity(ctx); err != nil || after != root || !reflect.DeepEqual(deltaRows(t, b), beforeRows) || o.Budget.Snapshot().Used != beforeFee {
		t.Fatal("trim changed content/retained rewrite fees", after, err, o.Budget.Snapshot())
	}
	beforeNative, _ := os.ReadFile(path)
	noop, err := b.TrimAtApplied(ctx, receipt.NativeHead)
	afterNative, _ := os.ReadFile(path)
	if err != nil || noop.NativeHead != receipt.NativeHead || !bytes.Equal(beforeNative, afterNative) {
		t.Fatal("repeated cut rewrote", noop, err)
	}
	opts := trimmedOpenOptions(t, b, o, receipt)
	next, r, err := OpenFromRoot(ctx, metadataTestKey(), b.output, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if r.AppliedFrames != 0 || r.Root != root {
		t.Fatal(r)
	}
	assertMetadataRow(t, next, "frame3", nil, false)
	if _, err := next.ReplayTo(ctx, receipt.NativeHead, 3); err != nil {
		t.Fatal(err)
	}
	assertMetadataRow(t, next, "frame3", []byte("3"), true)
	second, err := next.TrimAtApplied(ctx, receipt.NativeHead)
	if err != nil || second.NativeHead.FileRevision != 3 || second.NativeHead.TrimmedThrough != 3 || second.AppliedHead != second.NativeHead {
		t.Fatal(second, err)
	}
	if _, err := next.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("frame4"), Value: []byte("4")}}); err != nil {
		t.Fatal(err)
	}
	finalRoot, err := next.store.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	finalHead, err := next.AppliedHead()
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.AdoptClosedSuccessor(next); err != nil {
		t.Fatal("qualified rewrite adoption", err)
	}
	state := second.State
	opts.SavedRoot, opts.SavedHead, opts.NativeHead, opts.PermittedHead, opts.TrimState = finalRoot, finalHead, finalHead, finalHead, &state
	last, _, err := OpenFromRoot(ctx, metadataTestKey(), b.output, path, opts)
	if err != nil {
		t.Fatal("post-trim delta saved root", err)
	}
	defer last.Close()
	assertMetadataRow(t, last, "frame4", []byte("4"), true)
	if err := last.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.AdoptClosedSuccessor(last); err != nil {
		t.Fatal(err)
	}
	if err := b.Discard(); err != nil {
		t.Fatal("adopted native replacement was not removable", err)
	}
}
func TestMetadataTrimReopenRejectsMissingOrChangedCutStateBeforeRepair(t *testing.T) {
	for _, shape := range []string{"missing", "root digest", "root rows", "source offset", "source hash", "source revision", "saved below trim", "wrong actual anchor"} {
		t.Run(shape, func(t *testing.T) {
			b, o, path := metadataTrimCandidate(t)
			receipt, err := b.TrimAtApplied(context.Background(), b.journal.Head())
			if err != nil {
				t.Fatal(err)
			}
			opts := trimmedOpenOptions(t, b, o, receipt)
			state := *opts.TrimState
			opts.TrimState = &state
			switch shape {
			case "missing":
				opts.TrimState = nil
			case "root digest":
				state.Root.Digest[0] ^= 1
			case "root rows":
				state.Root.Rows++
			case "source offset":
				state.SourceFrameOffset++
			case "source hash":
				state.SourceHash[0] ^= 1
			case "source revision":
				state.SourceRevision++
			case "saved below trim":
				opts.SavedHead.Sequence = 1
			case "wrong actual anchor":
				state.SourceEndOffset++
			}
			pages := filepath.Join(b.output, "pages")
			originalPages, _ := os.ReadFile(pages)
			orphan := append(bytes.Clone(originalPages), []byte("not-yet-qualified-orphan")...)
			if err := os.WriteFile(pages, orphan, 0600); err != nil {
				t.Fatal(err)
			}
			defer os.WriteFile(pages, originalPages, 0600)
			beforeNative, _ := os.ReadFile(path)
			beforeRoot, _ := os.ReadFile(filepath.Join(b.output, "active.root"))
			before := o.Budget.Snapshot().Used
			next, r, err := OpenFromRoot(context.Background(), metadataTestKey(), b.output, path, opts)
			if err == nil || next != nil || r != (ReopenReceipt{}) {
				t.Fatal("bad trim selection accepted", next, r, err)
			}
			afterNative, _ := os.ReadFile(path)
			afterRoot, _ := os.ReadFile(filepath.Join(b.output, "active.root"))
			afterPages, _ := os.ReadFile(pages)
			if !bytes.Equal(beforeNative, afterNative) || !bytes.Equal(beforeRoot, afterRoot) || !bytes.Equal(orphan, afterPages) || o.Budget.Snapshot().Used != before {
				t.Fatal("refusal repaired/refunded")
			}
		})
	}
}
func TestMetadataTrimBudgetPreflightAndCancelledPhysicalPublicationJoin(t *testing.T) {
	for _, phaseName := range []string{"preflight disk", "state-verified", "journal-published"} {
		t.Run(phaseName, func(t *testing.T) {
			b, o, path := metadataTrimCandidate(t)
			source := b.journal.Head()
			beforeRoot, _ := os.ReadFile(filepath.Join(b.output, "active.root"))
			beforeNative, _ := os.ReadFile(path)
			before := o.Budget.Snapshot().Used
			if phaseName == "preflight disk" {
				fee, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 175 << 20})
				if err != nil {
					t.Fatal(err)
				}
				defer fee.Release()
				used := o.Budget.Snapshot().Used
				result, err := b.TrimAtApplied(context.Background(), source)
				if err == nil || result != (TrimReceipt{}) || !b.ready.Load() || o.Budget.Snapshot().Used != used {
					t.Fatal(result, err, o.Budget.Snapshot())
				}
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				r, err := b.trimAtApplied(ctx, source, func(phase string) {
					if phase == phaseName {
						close(entered)
						<-release
					}
				})
				if r != (TrimReceipt{}) {
					err = errors.Join(err, fmt.Errorf("canceled trim returned receipt"))
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("phase not reached")
			}
			cancel()
			closeDone := make(chan error, 1)
			go func() { closeDone <- b.Close() }()
			live := o.Budget.Snapshot().Used
			if live.Disk != before.Disk+16<<20 || live.RAM < before.RAM+DeltaRAMBytes {
				t.Fatal("rewrite fee retired during live work", live, before)
			}
			select {
			case err := <-closeDone:
				t.Fatal("Close returned before trim job", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("job did not join")
			}
			if err := <-closeDone; err != nil {
				t.Fatal(err)
			}
			afterRoot, _ := os.ReadFile(filepath.Join(b.output, "active.root"))
			afterNative, _ := os.ReadFile(path)
			if !bytes.Equal(beforeRoot, afterRoot) || phaseName == "state-verified" && !bytes.Equal(beforeNative, afterNative) || o.Budget.Snapshot().Used.Disk != before.Disk {
				t.Fatal("root changed or physical rewrite fee leaked", o.Budget.Snapshot())
			}
		})
	}
}

func TestMetadataTrimReopenSurvivesPublisherProcessWithNewManager(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMetadataReopenPublisherProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HALRO_METADATA_REOPEN_TEST_DIR="+dir, "HALRO_METADATA_TRIM_TEST=1")
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
	if selection.Trim == nil || selection.Native.FileRevision != 2 || selection.Native.TrimmedThrough != 1 {
		t.Fatal("child did not publish real trim", selection)
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
	opts := OpenOptions{BaselineOptions: o, Evidence: selection.Evidence, SavedRoot: selection.Root, SavedHead: selection.Saved, NativeHead: selection.Native, PermittedHead: selection.Allowed, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: 16 << 20, TrimState: selection.Trim}
	b, r, err := OpenFromRoot(context.Background(), metadataTestKey(), filepath.Join(dir, "root"), filepath.Join(dir, "metadata.journal"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if r.AppliedFrames != 1 || r.From.Sequence != 1 || r.To.Sequence != 2 {
		t.Fatal(r)
	}
	assertMetadataRow(t, b, "permitted", []byte("two"), true)
	assertMetadataRow(t, b, "excluded", nil, false)
}
