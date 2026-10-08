package metadatastate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

func TestMetadataFrozenSealQualifiesBackupIdentityAndKeepsRootNativeReceipt(t *testing.T) {
	b, o, key, path := epochCandidate(t)
	ctx := context.Background()
	head, err := b.StartNewEpoch(ctx, key, path, 8, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fees := o.Budget.Snapshot().Used
	called := false
	if err := b.WithFrozenSealReference(ctx, o.Budget, o.Identity, "wrong-instance", o.KeyGeneration, func(context.Context, AuthenticatedMetadataSealReference, metadatajournal.Head) error {
		called = true
		return nil
	}); err == nil || called || fees != o.Budget.Snapshot().Used {
		t.Fatal("foreign instance accepted", called, err)
	}
	rejected := errors.New("common candidate rejected")
	err = b.WithFrozenSealReference(ctx, o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, func(ctx context.Context, r AuthenticatedMetadataSealReference, h metadatajournal.Head) error {
		if r != ref || h != head {
			t.Fatal("split root/native", r, h)
		}
		if _, err := b.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("refused"), Value: []byte("v")}}); !errors.Is(err, authenticatedstate.ErrBusy) {
			t.Fatal("reducer entered frozen root", err)
		}
		if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("refused"), Value: []byte("v")}}); !errors.Is(err, metadatajournal.ErrNativeFrozen) {
			t.Fatal("source write entered", err)
		}
		return rejected
	})
	if !errors.Is(err, rejected) || fees != o.Budget.Snapshot().Used {
		t.Fatal("known candidate rejection changed fees", err, o.Budget.Snapshot())
	}
	after, err := b.SealReference(ctx)
	if err != nil || after != ref {
		t.Fatal("candidate rejection changed root", after, err)
	}
}
func TestMetadataFrozenSealCloseCancelsAndJoinsCallback(t *testing.T) {
	b, o, key, path := epochCandidate(t)
	if _, err := b.StartNewEpoch(context.Background(), key, path, 8, 1<<20); err != nil {
		t.Fatal(err)
	}
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- b.WithFrozenSealReference(context.Background(), o.Budget, o.Identity, o.InstanceID, o.KeyGeneration, func(ctx context.Context, _ AuthenticatedMetadataSealReference, _ metadatajournal.Head) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("job did not enter")
	}
	fees := o.Budget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel")
	}
	select {
	case err := <-closed:
		t.Fatal("Close skipped join", err)
	case <-time.After(25 * time.Millisecond):
	}
	if o.Budget.Snapshot().Used != fees {
		t.Fatal("live callback refunded")
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("closed candidate accepted", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
