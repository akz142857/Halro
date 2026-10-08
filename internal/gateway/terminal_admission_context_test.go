package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

type testAdmissionOwnerKey struct{}

type admittedPricePinStore struct {
	*fakePricePinStore
	owner *atomic.Bool
	seen  atomic.Int32
}

func (s *admittedPricePinStore) check(ctx context.Context) error {
	if ctx.Value(testAdmissionOwnerKey{}) != s.owner || s.owner.Load() {
		return errors.New("actual request admission owner unavailable")
	}
	s.seen.Add(1)
	return ctx.Err()
}
func (s *admittedPricePinStore) PrepareDeploymentPricePin(ctx context.Context, deployment, attempt string, at time.Time, rollback, forward time.Duration) (domain.DeploymentPriceVersion, domain.PriceSnapshot, domain.PricePinIntent, error) {
	if err := s.check(ctx); err != nil {
		return domain.DeploymentPriceVersion{}, domain.PriceSnapshot{}, domain.PricePinIntent{}, err
	}
	return s.fakePricePinStore.PrepareDeploymentPricePin(ctx, deployment, attempt, at, rollback, forward)
}
func (s *admittedPricePinStore) CommitDeploymentPricePin(ctx context.Context, attempt, digest string, seq uint64, at time.Time) (domain.PricePinIntent, error) {
	if err := s.check(ctx); err != nil {
		return domain.PricePinIntent{}, err
	}
	return s.fakePricePinStore.CommitDeploymentPricePin(ctx, attempt, digest, seq, at)
}

func TestGatewayTerminalAdmissionReachesActualAttemptPricePin(t *testing.T) {
	f := newFixture(t, 10000)
	defer f.close()
	now := time.Now().UTC()
	owner := &atomic.Bool{}
	pins := &admittedPricePinStore{owner: owner, fakePricePinStore: &fakePricePinStore{price: domain.DeploymentPriceVersion{
		ID: "price_admitted", DeploymentID: "dep_admitted", Version: 1, Revision: 1,
		BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: 1000000, OutputMicrosPerMillion: 2000000,
		EffectiveFrom: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour), CreatedBy: "test",
		Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
			ReceivedAt: now.Add(-time.Hour), ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Reference: "test", AssertedWithoutArchive: true},
	}}}
	registry := provider.NewRegistry()
	if err := registry.Register(provider.Target{ID: "target_admitted", DeploymentID: "dep_admitted", PublicModel: "chat", ProviderModel: "provider-model", Adapter: f.adapter}); err != nil {
		t.Fatal(err)
	}
	calls, closes := 0, 0
	service, err := NewServiceWithOptions(f.service.auth, registry, f.accounting, ServiceOptions{Pricing: pins, TerminalAdmission: func(ctx context.Context, count int) (context.Context, func() error, error) {
		calls++
		return context.WithValue(ctx, testAdmissionOwnerKey{}, owner), func() error { closes++; owner.Store(true); return nil }, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Chat(context.Background(), f.plaintext, chatRequest()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || closes != 1 || pins.seen.Load() != 2 || pins.committed.State != domain.PricePinCommitted || f.adapter.calls != 1 {
		t.Fatal("actual attempt lost its original admission or retired it early", calls, closes, pins.seen.Load(), pins.committed.State, f.adapter.calls)
	}
}

func TestGatewayTerminalAttemptKeepsBothCancellationLifetimes(t *testing.T) {
	for _, source := range []string{"caller", "admission"} {
		t.Run(source, func(t *testing.T) {
			caller, cancelCaller := context.WithCancelCause(context.Background())
			defer cancelCaller(nil)
			admitted, cancelAdmission := context.WithCancelCause(context.WithValue(context.Background(), testAdmissionOwnerKey{}, source))
			defer cancelAdmission(nil)
			run := &requestRun{completionContext: admitted, closeTerminalAdmission: func() error { return nil }}
			ctx, done := run.attemptContext(caller)
			defer done()
			cause := errors.New(source + " retired")
			if source == "caller" {
				cancelCaller(cause)
			} else {
				cancelAdmission(cause)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("attempt outlived original canceled context")
			}
			if ctx.Value(testAdmissionOwnerKey{}) != source || !errors.Is(context.Cause(ctx), cause) {
				t.Fatal("owner copy lost current cancellation", context.Cause(ctx))
			}
		})
	}
	admitted, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	caller, cancelCaller := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelCaller()
	run := &requestRun{completionContext: admitted, closeTerminalAdmission: func() error { return nil }}
	ctx, done := run.attemptContext(caller)
	defer done()
	deadline, _ := ctx.Deadline()
	callerDeadline, _ := caller.Deadline()
	if deadline != callerDeadline {
		t.Fatal("long admission replaced shorter caller deadline")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("short caller deadline ignored")
	}
}
