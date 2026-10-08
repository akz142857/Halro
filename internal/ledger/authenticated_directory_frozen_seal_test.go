package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func frozenDirectoryFixture(t *testing.T) (*AuthenticatedDirectory, AuthenticatedDirectoryOptions) {
	t.Helper()
	f := newAuthenticatedReopenFixture(t)
	f.maintain(t)
	owner, _, err := OpenAuthenticatedLedgerProjection(context.Background(), f.native, f.directory, f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	})
	o := AuthenticatedDirectoryOptions{Identity: f.options.Identity, Budget: f.options.Budget, MaxDiskBytes: 16 << 20, MaxAppendBytes: 4 << 20}
	d, err := BuildAuthenticatedDirectory(context.Background(), f.native, f.lease, owner, filepath.Join(t.TempDir(), "directory"), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Discard(); err != nil {
			t.Error(err)
		}
	})
	return d, o
}

func TestAuthenticatedDirectoryFrozenSealQualifiesIdentityAndKeepsReceipt(t *testing.T) {
	d, o := frozenDirectoryFixture(t)
	before, err := d.SealReference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fees := o.Budget.Snapshot().Used
	called := false
	if err := d.WithFrozenSealReference(context.Background(), o.Budget, [32]byte{99}, func(context.Context, AuthenticatedDirectoryReference) error { called = true; return nil }); err == nil || called || o.Budget.Snapshot().Used != fees {
		t.Fatal("unqualified directory identity accepted", called, err)
	}
	rejected := errors.New("candidate rejected")
	err = d.WithFrozenSealReference(context.Background(), o.Budget, o.Identity, func(ctx context.Context, ref AuthenticatedDirectoryReference) error {
		if ref != before {
			t.Fatal("split directory receipt", ref, before)
		}
		if err := d.WithFrozenSealReference(ctx, o.Budget, o.Identity, func(context.Context, AuthenticatedDirectoryReference) error { return nil }); !errors.Is(err, authenticatedstate.ErrBusy) {
			t.Fatal("overlapping publication", err)
		}
		return rejected
	})
	if !errors.Is(err, rejected) || o.Budget.Snapshot().Used != fees {
		t.Fatal("known rejection changed fees", err, o.Budget.Snapshot())
	}
	after, err := d.SealReference(context.Background())
	if err != nil || after != before {
		t.Fatal("known rejection changed root", after, err)
	}
}

func TestAuthenticatedDirectoryFrozenSealDiscardCancelsAndJoinsCallback(t *testing.T) {
	d, o := frozenDirectoryFixture(t)
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
		done <- d.WithFrozenSealReference(context.Background(), o.Budget, o.Identity, func(ctx context.Context, _ AuthenticatedDirectoryReference) error {
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
		t.Fatal("callback not entered")
	}
	fees := o.Budget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- d.Discard() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Discard did not cancel frozen root")
	}
	select {
	case err := <-closed:
		t.Fatal("Discard skipped actual callback join", err)
	case <-time.After(25 * time.Millisecond):
	}
	if o.Budget.Snapshot().Used != fees {
		t.Fatal("active callback refunded")
	}
	if _, err := d.store.RootIdentity(context.Background()); err != nil {
		t.Fatal("pages retired before callback exit", err)
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("closed candidate accepted", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
