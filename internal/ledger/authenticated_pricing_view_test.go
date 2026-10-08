package ledger

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestAuthenticatedFreshPricingPagesMatchIndependentNativeReference(t *testing.T) {
	p, _, ref, _ := recoveryViewFixture(t)
	ctx := context.Background()
	limits := ProjectionPageLimits{Rows: 2, Bytes: 1 << 20}
	var expectedPending []PendingLease
	var expectedSettled []SettledAttempt
	var c ProjectionCursor
	for {
		page, err := ref.PendingLeasePage(ctx, c, limits)
		if err != nil {
			t.Fatal(err)
		}
		expectedPending = append(expectedPending, page.Items...)
		if page.Done {
			break
		}
		c = page.Next
	}
	c = ProjectionCursor{}
	for {
		page, err := ref.SettlementPage(ctx, c, limits)
		if err != nil {
			t.Fatal(err)
		}
		expectedSettled = append(expectedSettled, page.Items...)
		if page.Done {
			break
		}
		c = page.Next
	}
	var escaped AuthenticatedPricingView
	err := p.WithFreshPricingView(ctx, func(reader AuthenticatedPricingView) error {
		escaped = reader
		head, hash, err := reader.ReadPosition(ctx)
		expected, expectedHash, refErr := ref.ReadPosition(ctx)
		if err != nil || refErr != nil || head != expected || hash != expectedHash {
			t.Fatal(head, expected, err, refErr)
		}
		var actualPending []PendingLease
		var actualSettled []SettledAttempt
		var cursor ProjectionCursor
		for {
			page, err := reader.PendingLeasePage(ctx, cursor, limits)
			if err != nil {
				return err
			}
			actualPending = append(actualPending, page.Items...)
			if page.Done {
				break
			}
			cursor = page.Next
		}
		cursor = ProjectionCursor{}
		for {
			page, err := reader.SettlementPage(ctx, cursor, limits)
			if err != nil {
				return err
			}
			actualSettled = append(actualSettled, page.Items...)
			if page.Done {
				break
			}
			cursor = page.Next
		}
		if !reflect.DeepEqual(actualPending, expectedPending) || !reflect.DeepEqual(actualSettled, expectedSettled) {
			t.Fatal("pricing pages diverged", actualPending, expectedPending, actualSettled, expectedSettled)
		}
		for _, pending := range actualPending {
			got, found, err := reader.ReadLease(ctx, pending.Reservation.AttemptID)
			want, wantFound, wantErr := ref.ReadLease(ctx, pending.Reservation.AttemptID)
			if err != nil || wantErr != nil || found != wantFound || !reflect.DeepEqual(got, want) {
				t.Fatal(got, want, err, wantErr)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := escaped.ReadPosition(ctx); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal("escaped pricing pin survived", err)
	}
	if _, err := escaped.SettlementPage(ctx, ProjectionCursor{}, limits); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal(err)
	}
}

func TestAuthenticatedFreshPricingRequiresBothStreamsAndRejectsIgnoredCursorError(t *testing.T) {
	p, _, _, _ := recoveryViewFixture(t)
	ctx := context.Background()
	if err := p.WithFreshPricingView(ctx, func(AuthenticatedPricingView) error { return nil }); !errors.Is(err, ErrAuthenticatedRecoveryIncomplete) {
		t.Fatal(err)
	}
	err := p.WithFreshPricingView(ctx, func(v AuthenticatedPricingView) error {
		pending, err := v.PendingLeasePage(ctx, ProjectionCursor{}, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20})
		if err != nil {
			return err
		}
		if _, err := v.SettlementPage(ctx, pending.Next, ProjectionPageLimits{Rows: 1, Bytes: 1 << 20}); !errors.Is(err, ErrProjectionCursor) {
			t.Fatal(err)
		}
		if _, _, err := v.ReadPosition(ctx); !errors.Is(err, ErrProjectionCursor) {
			t.Fatal("failed pricing session resumed reading", err)
		}
		return nil
	})
	if !errors.Is(err, ErrProjectionCursor) {
		t.Fatal("ignored cross-stream cursor accepted", err)
	}
}

func TestAuthenticatedFreshPricingPanicRevokesEscapedReader(t *testing.T) {
	p, _, _, _ := recoveryViewFixture(t)
	before := p.budget.Snapshot().Used
	var escaped AuthenticatedPricingView
	func() {
		defer func() {
			if recovered := recover(); recovered != "pricing callback" {
				t.Fatal("callback panic changed", recovered)
			}
		}()
		_ = p.WithFreshPricingView(context.Background(), func(v AuthenticatedPricingView) error {
			escaped = v
			panic("pricing callback")
		})
	}()
	if _, _, err := escaped.ReadPosition(context.Background()); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal("panic left callback reader active", err)
	}
	// Abnormal visitor exit poisons the root, while the actual snapshot and
	// callback fee must still retire before the panic reaches its caller.
	if after := p.budget.Snapshot().Used; after != before {
		t.Fatal("panic retained the callback fee", before, after)
	}
	if err := p.WithFreshPricingView(context.Background(), func(AuthenticatedPricingView) error { return nil }); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatal("abnormal callback reused a poisoned root", err)
	}
}
