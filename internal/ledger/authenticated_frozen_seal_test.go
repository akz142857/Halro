package ledger

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestAuthenticatedLedgerFrozenSealQualifiesIdentityAndKeepsExactReceipt(t *testing.T) {
	p, _, _ := authenticatedAdvanceFixture(t, 0)
	ctx := context.Background()
	id := sha256.Sum256([]byte("independent-test-ledger-identity"))
	before, err := p.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fees := p.budget.Snapshot().Used
	called := false
	if err := p.WithFrozenSealReference(ctx, p.budget, [32]byte{99}, func(context.Context, AuthenticatedLedgerSealReference) error { called = true; return nil }); err == nil || called || p.budget.Snapshot().Used != fees {
		t.Fatal("unqualified identity accepted", called, err)
	}
	rejected := errors.New("common candidate rejected")
	err = p.WithFrozenSealReference(ctx, p.budget, id, func(ctx context.Context, ref AuthenticatedLedgerSealReference) error {
		if ref != before {
			t.Fatal("split root/cut", ref, before)
		}
		if err := p.WithFrozenSealReference(ctx, p.budget, id, func(context.Context, AuthenticatedLedgerSealReference) error { return nil }); !errors.Is(err, authenticatedstate.ErrBusy) {
			t.Fatal("second root job entered", err)
		}
		return rejected
	})
	if !errors.Is(err, rejected) || p.budget.Snapshot().Used != fees {
		t.Fatal("known rejection changed lifetime", err, p.budget.Snapshot())
	}
	after, err := p.SealReference(ctx)
	if err != nil || after != before {
		t.Fatal("rejection retired healthy root", after, err)
	}
}
func TestAuthenticatedLedgerFrozenSealCloseCancelsAndJoinsCallback(t *testing.T) {
	p, _, _ := authenticatedAdvanceFixture(t, 0)
	id := sha256.Sum256([]byte("independent-test-ledger-identity"))
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
		done <- p.WithFrozenSealReference(context.Background(), p.budget, id, func(ctx context.Context, _ AuthenticatedLedgerSealReference) error {
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
	fees := p.budget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel frozen root")
	}
	select {
	case err := <-closed:
		t.Fatal("Close skipped actual join", err)
	case <-time.After(25 * time.Millisecond):
	}
	if p.budget.Snapshot().Used != fees {
		t.Fatal("active root job refunded")
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("closed candidate accepted", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
