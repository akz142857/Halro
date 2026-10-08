package metadatastate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// Component proof only: actual Member/Ordering permission is exercised by the
// app's two-node activation/recovery fixture. This checks selected COW identity,
// full native authentication and the absence of write/facade grants.
func TestHASelectedReadRootUsesActualCommonDerivedCommitment(t *testing.T) {
	t.Run("equal-cut", func(t *testing.T) { testHASelectedReadRoot(t, false) })
	t.Run("exclude-authenticated-tail", func(t *testing.T) { testHASelectedReadRoot(t, true) })
}
func testHASelectedReadRoot(t *testing.T, withTail bool) {
	ctx := context.Background()
	f := selectedMetadataNativeCount(t, 0)
	b := f.baseline
	if _, err := b.Update(ctx, func(tx *Transaction) error {
		return tx.Put([]string{"meta"}, []byte("shutdown_truncated_attempts_total"), []byte("2"))
	}); err != nil {
		t.Fatal(err)
	}
	ref, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected, found, err := f.options.Publisher.Select(f.options.Authority)
	if err != nil || !found {
		t.Fatal(err)
	}
	selected.Revision = 1
	selected.Instance = b.evidence.Identity
	selected.Role = authenticatedstate.SealPrimary
	selected.Cluster = [32]byte{2}
	selected.Member = [32]byte{3}
	selected.Term = 1
	selected.OrderingTerm = 1
	selected.OrderingIndex = 1
	selected.OrderingOffset = 256
	selected.OrderingMAC = [32]byte{4}
	selected.Roots[0] = ref.Root
	dir := t.TempDir()
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	fee, err := f.options.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	hard := authenticatedstate.SealHardLimits{MaxPageBytes: selected.MaxPageBytes, MaxValueBytes: selected.MaxValueBytes, MaxTreeHeight: selected.MaxTreeHeight, RequiredRoots: []authenticatedstate.SealRootSchema{ref.Schema}}
	pub, err := authenticatedstate.OpenSealCandidatePublisher(dir, guard, f.key, hard, f.options.Budget, fee)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(pub.Close(), pub.Retire(), fee.Release(), guard.Close()); err != nil {
			t.Error(err)
		}
	})
	if _, err = pub.Publish(ctx, selected); err != nil {
		t.Fatal(err)
	}
	authority := f.options.Authority
	authority.Instance = selected.Instance
	authority.Role = selected.Role
	authority.Cluster = selected.Cluster
	authority.Member = selected.Member
	authority.Term = selected.Term
	authority.OrderingIndex = selected.OrderingIndex
	authority.OrderingOffset = selected.OrderingOffset
	authority.OrderingMAC = selected.OrderingMAC
	nativeHead := f.head
	if withTail {
		if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("excluded-tail"), Value: []byte("pending")}}); err != nil {
			t.Fatal(err)
		}
		nativeHead = b.journal.Head()
	}
	if err = b.Close(); err != nil {
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
	o := OpenOptions{BaselineOptions: BaselineOptions{Identity: b.evidence.Identity, InstanceID: b.evidence.InstanceID, KeyGeneration: b.evidence.KeyGeneration, Budget: f.options.Budget, MaxSourceBytes: 64 << 20, MaxDiskBytes: 64 << 20, MaxAppendBytes: 8 << 20}, Evidence: b.evidence, SavedHead: f.head, NativeHead: nativeHead, PermittedHead: f.head, RootDiskReservation: rc, JournalDiskReservation: jc, MaxJournalBytes: f.options.MaxBytes}
	binding := metadatajournal.BoundReplicationAuthority{Baseline: f.head.Baseline, FileRevision: f.head.FileRevision, MaxFileBytes: f.options.MaxBytes}
	before := o.Budget.Snapshot().Used
	wrong := binding
	wrong.FileRevision++
	if owner, _, err := OpenHASelectedReadRoot(ctx, metadataTestKey(), b.output, f.path, o, wrong, pub, guard, authority); owner != nil || err == nil {
		t.Fatal("foreign independent revision permitted", owner, err)
	}
	standalone := authority
	standalone.Role = authenticatedstate.SealStandalone
	if owner, _, err := OpenHASelectedReadRoot(ctx, metadataTestKey(), b.output, f.path, o, binding, pub, guard, standalone); owner != nil || err == nil {
		t.Fatal("standalone authority treated as HA", owner, err)
	}
	if o.Budget.Snapshot().Used != before {
		t.Fatal("qualification refusal leaked fees")
	}
	owner, receipt, err := OpenHASelectedReadRoot(ctx, metadataTestKey(), b.output, f.path, o, binding, pub, guard, authority)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	actual, err := owner.SealReference(ctx)
	if err != nil || actual != ref || receipt.AppliedFrames != 0 {
		t.Fatal("actual selected commitment changed", actual, ref, receipt, err)
	}
	if withTail {
		if receipt.SourceFrames != 2 || receipt.To != f.head || receipt.NativeHead != nativeHead {
			t.Fatal("tail inspection credited pending facts", receipt)
		}
		if err := owner.View(ctx, func(tx *Transaction) error {
			_, found, err := tx.Get([]string{"projects"}, []byte("excluded-tail"))
			if err != nil || found {
				return errors.Join(ErrSource, err)
			}
			return nil
		}); err != nil {
			t.Fatal("unapplied suffix exposed", err)
		}
	}
	if err := owner.View(ctx, func(tx *Transaction) error {
		value, found, err := tx.Get([]string{"meta"}, []byte("shutdown_truncated_attempts_total"))
		if err != nil || !found || string(value) != "2" {
			return errors.Join(ErrSource, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Update(ctx, func(tx *Transaction) error {
		return tx.Put([]string{"meta"}, []byte("shutdown_truncated_attempts_total"), []byte("3"))
	}); err == nil {
		t.Fatal("read proof granted semantic write")
	}
	if _, err := owner.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("denied"), Value: []byte("v")}}); err == nil {
		t.Fatal("read proof granted native append")
	}
	if _, err := owner.StandaloneFacade(ctx, o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, ref); err == nil {
		t.Fatal("read proof granted Standalone facade")
	}
	if !withTail {
		child, _, err := OpenHASelectedReadRootFromSource(ctx, metadataTestKey(), b.output, f.path, o, binding, pub, guard, authority, owner)
		if err != nil {
			t.Fatal("actual source lock loan failed", err)
		}
		entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		viewed := make(chan error, 1)
		go func() {
			viewed <- child.View(ctx, func(tx *Transaction) error {
				close(entered)
				<-tx.ctx.Done()
				close(canceled)
				<-release
				return tx.ctx.Err()
			})
		}()
		select {
		case <-entered:
		case err := <-viewed:
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("borrowed actual View not entered")
		}
		closed := make(chan error, 1)
		go func() { closed <- owner.Close() }()
		select {
		case <-canceled:
		case <-time.After(5 * time.Second):
			t.Fatal("source Close did not revoke complete borrowed Metadata owner")
		}
		select {
		case err := <-closed:
			t.Fatal("source Close skipped child callback join", err)
		case <-time.After(25 * time.Millisecond):
		}
		close(release)
		if err := <-viewed; !errors.Is(err, context.Canceled) {
			t.Fatal("borrowed View cancellation lost", err)
		}
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := child.EpochHead(); err == nil {
			t.Fatal("native borrowed descriptor remained admitted after source Close")
		}
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if o.Budget.Snapshot().Used != before {
		t.Fatal("actual joined read owner leaked fees", o.Budget.Snapshot().Used, before)
	}
}
