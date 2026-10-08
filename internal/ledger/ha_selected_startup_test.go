package ledger

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestHASelectedNativeRefusesRelabelledStandaloneSealAndIgnoredVisitorFailure(t *testing.T) {
	f := newSelectedStartupFixture(t, true)
	ctx := context.Background()
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	budgetBefore := f.o.DirectoryOptions.Budget.Snapshot().Used
	for _, behavior := range []string{"return-error", "ignore-error", "duplicate", "omit"} {
		t.Run(behavior, func(t *testing.T) {
			authority := f.o.Authority
			authority.Role = authenticatedstate.SealReplica
			head, hash, _ := f.native.ChainHead()
			allowed := HASelectedLedgerPosition{Generation: head.Generation, Sequence: head.Sequence, Hash: hash}
			provider := func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error {
				if behavior == "omit" {
					return nil
				}
				err := visit(ctx, authority, allowed)
				if behavior == "duplicate" {
					visit(ctx, authority, allowed)
				}
				if behavior != "return-error" {
					return nil
				}
				return err
			}
			native, receipt, err := OpenWithHASelectedSeal(ctx, f.path, NewStatus(), Options{ChainKey: testChainKey, Replica: true}, HASelectedSealOpenOptions{SelectedSealOpenOptions: f.o, WithAuthority: provider})
			if native != nil {
				defer native.Close()
			}
			if err == nil || native != nil || receipt != (NativeStartupReceipt{}) {
				t.Fatal("relabelled saved Standalone bytes granted an HA native owner", receipt, err)
			}
			if after, err := os.ReadFile(f.path); err != nil || !bytes.Equal(before, after) {
				t.Fatal("refused grant changed native facts", err)
			}
			if after := f.o.DirectoryOptions.Budget.Snapshot().Used; after != budgetBefore {
				t.Fatal("refused grant lost actual IO retirement", budgetBefore, after)
			}
		})
	}
}

func TestHASelectedAuthorityLoanJoinsBoundedBorrowersBeforeActualFreezeExits(t *testing.T) {
	authority := authenticatedstate.SealEligibility{Role: authenticatedstate.SealReplica, Coverage: authenticatedstate.SealCoverageAuthenticated}
	allowed := HASelectedLedgerPosition{Generation: 1}
	actualFreezeExited := make(chan struct{})
	h := &haSelectedStartupProvenance{role: authenticatedstate.SealReplica}
	h.withAuthority = func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error {
		defer close(actualFreezeExited)
		return visit(ctx, authority, allowed)
	}
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	borrowDone := make(chan error, 8)
	outerDone := make(chan error, 1)
	outerReturned := make(chan *haSelectedStartupScope, 1)
	go func() {
		outerDone <- h.withScope(context.Background(), func(ctx context.Context, _ authenticatedstate.SealEligibility, _ HASelectedLedgerPosition) error {
			for i := 0; i < 8; i++ {
				go func() {
					borrowDone <- h.withScope(ctx, func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error {
						entered <- struct{}{}
						<-release
						return nil
					})
				}()
			}
			for i := 0; i < 8; i++ {
				<-entered
			}
			if err := h.withScope(ctx, func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error { return nil }); !errors.Is(err, ErrProjectionSource) {
				return errors.New("ninth borrower exceeded fixed scope capacity")
			}
			outerReturned <- ctx.Value(haSelectedStartupScopeKey{}).(*haSelectedStartupScope)
			return nil
		})
	}()
	var scope *haSelectedStartupScope
	select {
	case scope = <-outerReturned:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("borrowers did not enter")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		scope.mu.Lock()
		closing := scope.closing
		scope.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("scope did not begin join")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-actualFreezeExited:
		close(release)
		t.Fatal("actual Member/source freeze exited before borrower IO joined")
	default:
	}
	close(release)
	for i := 0; i < 8; i++ {
		if err := <-borrowDone; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-outerDone; err != nil {
		t.Fatal(err)
	}
	if scope.active.Load() {
		t.Fatal("joined scope remained active")
	}
	<-actualFreezeExited
}

// This exercises only the private capability lifetime, not HA role/source
// qualification (the actual two-member integration owns that evidence).
func TestHASelectedAuthorityLoanBorrowsOnlyLiveConstructorScope(t *testing.T) {
	authority := authenticatedstate.SealEligibility{Role: authenticatedstate.SealReplica, Coverage: authenticatedstate.SealCoverageAuthenticated}
	allowed := HASelectedLedgerPosition{Generation: 3, Sequence: 17, Hash: [32]byte{9}}
	providerCalls := 0
	h := &haSelectedStartupProvenance{role: authenticatedstate.SealReplica}
	h.withAuthority = func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error {
		providerCalls++
		if providerCalls > 1 {
			t.Fatal("nested borrow re-entered the non-reentrant Member/source freeze")
		}
		return visit(ctx, authority, allowed)
	}
	var escaped context.Context
	if err := h.withScope(context.Background(), func(ctx context.Context, a authenticatedstate.SealEligibility, p HASelectedLedgerPosition) error {
		escaped = ctx
		if a != authority || p != allowed {
			t.Fatal("loan changed the independently supplied cut")
		}
		if err := h.withScope(ctx, func(inner context.Context, a authenticatedstate.SealEligibility, p HASelectedLedgerPosition) error {
			if inner != ctx || a != authority || p != allowed {
				t.Fatal("nested loan changed actual scope")
			}
			return nil
		}); err != nil {
			return err
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		returnErr := h.withScope(canceled, func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error {
			t.Fatal("canceled scope visited derived owner")
			return nil
		})
		if !errors.Is(returnErr, context.Canceled) || !errors.Is(returnErr, ErrProjectionSource) {
			t.Fatal("canceled scope was reusable", returnErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.withScope(escaped, func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error {
		t.Fatal("escaped old cut visited derived owner")
		return nil
	}); !errors.Is(err, ErrProjectionSource) || providerCalls != 1 {
		t.Fatal("expired scope reacquired or granted authority", providerCalls, err)
	}
	if err := (&Log{}).WithHASelectedSealAuthority(context.Background(), func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error { return nil }); !errors.Is(err, ErrProjectionSource) {
		t.Fatal("unqualified Log granted an HA loan", err)
	}
}

func TestHALiveAndStartupAuthorityCannotUpgradeOrReenterEachOther(t *testing.T) {
	a := authenticatedstate.SealEligibility{Role: authenticatedstate.SealReplica, Coverage: authenticatedstate.SealCoverageAuthenticated}
	h := &haSelectedStartupProvenance{role: authenticatedstate.SealReplica}
	startupCalls, liveCalls := 0, 0
	h.withAuthority = func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error {
		startupCalls++
		return visit(ctx, a, HASelectedLedgerPosition{Generation: 1})
	}
	h.withLiveAuthority = func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error {
		liveCalls++
		return visit(ctx, a, HASelectedLedgerPosition{Generation: 1})
	}
	visit := func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error {
		t.Fatal("cross-scope visitor entered")
		return nil
	}
	if err := h.withLiveScope(context.Background(), func(ctx context.Context, _ authenticatedstate.SealEligibility, _ HASelectedLedgerPosition) error {
		if err := validateHASelectedStartupScope(ctx, h, a, Options{Replica: true}); !errors.Is(err, ErrProjectionSource) {
			t.Fatal("single-source live authority minted startup/common-cut permission", err)
		}
		if err := h.withScope(ctx, visit); !errors.Is(err, ErrProjectionSource) || startupCalls != 0 {
			t.Fatal("live read re-entered startup Member freeze", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.withScope(context.Background(), func(ctx context.Context, _ authenticatedstate.SealEligibility, _ HASelectedLedgerPosition) error {
		if err := h.withLiveScope(ctx, visit); !errors.Is(err, ErrProjectionSource) || liveCalls != 1 {
			t.Fatal("startup loan re-entered live Member freeze", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHASelectedAuthorityRefusesProviderDroppingActualMaintenanceContext(t *testing.T) {
	a := authenticatedstate.SealEligibility{Role: authenticatedstate.SealReplica, Coverage: authenticatedstate.SealCoverageAuthenticated}
	for _, omit := range []bool{false, true} {
		// This private test seam exercises capability preservation only. It
		// neither constructs a native owner nor supplies real HA authority.
		scope := &haSelectedMaintenanceScope{lease: &MaintenanceLease{}}
		scope.active.Store(true)
		ctx := context.WithValue(context.Background(), haSelectedMaintenanceScopeKey{}, scope)
		h := &haSelectedStartupProvenance{role: authenticatedstate.SealReplica}
		h.withAuthority = func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error) error {
			if omit {
				ctx = context.Background()
			}
			visit(ctx, a, HASelectedLedgerPosition{Generation: 1}) // ignore refusal
			return nil
		}
		called := false
		err := h.withScope(ctx, func(context.Context, authenticatedstate.SealEligibility, HASelectedLedgerPosition) error {
			called = true
			return nil
		})
		if omit && (called || !errors.Is(err, ErrProjectionSource)) || !omit && (!called || err != nil) {
			t.Fatal("provider lost actual maintenance gate capability", omit, called, err)
		}
	}
}
