package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type preparedCleanupOwnerKey struct{}
type preparedCleanupPriceStore struct {
	fakePricePinStore
	budget     *resourcebudget.Manager
	owner      *int
	cancel     context.CancelFunc
	failure    error
	cleanupErr error
}

func (p *preparedCleanupPriceStore) PrepareDeploymentPricePin(ctx context.Context, deployment, attempt string, selected time.Time, rollback, forward time.Duration) (domain.DeploymentPriceVersion, domain.PriceSnapshot, domain.PricePinIntent, error) {
	price, snapshot, intent, err := p.fakePricePinStore.PrepareDeploymentPricePin(ctx, deployment, attempt, selected, rollback, forward)
	if err == nil {
		if p.cancel != nil {
			p.cancel()
		}
	}
	return price, snapshot, intent, err
}
func (p *preparedCleanupPriceStore) DeletePreparedDeploymentPricePin(ctx context.Context, attempt string) error {
	p.deletes.Add(1)
	if ctx.Value(preparedCleanupOwnerKey{}) != p.owner || ctx.Err() != nil || attempt != p.prepared.AttemptID {
		p.cleanupErr = errors.New("cleanup lost its accepted owner or inherited caller cancellation")
		return p.cleanupErr
	}
	if err := p.budget.SetPressure(resourcebudget.Pressured); err != nil {
		p.cleanupErr = err
		return err
	}
	before := p.budget.Snapshot()
	claim, err := p.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4096})
	if err != nil {
		p.cleanupErr = err
		return err
	}
	if err = claim.Release(); err != nil {
		p.cleanupErr = err
		return err
	}
	after := p.budget.Snapshot()
	if before.NormalClaims != after.NormalClaims || before.RecoveryClaims != after.RecoveryClaims || before.Used != after.Used {
		p.cleanupErr = errors.New("cleanup did not use its original prepaid completion workspace")
		return p.cleanupErr
	}
	return p.failure
}

func TestPreparedPriceCleanupUsesOriginalTerminalAfterActualReserveRefusal(t *testing.T) {
	for _, name := range []string{"budget-refusal", "cleanup-unknown", "caller-canceled"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 10)
			defer f.close()
			resources, _, _, _ := bindActualCompletionAccounting(t, f)
			work, err := resources.TryAcquireCompletableWork(resourcebudget.Active, resourcebudget.Cost{RAM: 64 << 10}, resourcebudget.Cost{RAM: 256 << 20, Disk: 8 << 20}, ledger.CompletionWorkspaceRequirements())
			if err != nil {
				t.Fatal(err)
			}
			defer work.Active.Release()
			defer work.Close()
			owner := new(int)
			ctx, cancel := context.WithCancel(context.WithValue(work.Bind(context.Background()), preparedCleanupOwnerKey{}, owner))
			defer cancel()
			now := time.Now().UTC()
			prices := &preparedCleanupPriceStore{budget: resources, owner: owner, fakePricePinStore: fakePricePinStore{price: domain.DeploymentPriceVersion{
				ID: "price_terminal", DeploymentID: "dep_terminal", Version: 1, Revision: 1,
				BillingMode: domain.BillingModeMetered, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
				InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 2_000_000,
				EffectiveFrom: now.Add(-time.Hour), CreatedBy: "test", CreatedAt: now.Add(-time.Hour),
				Source: domain.PriceSource{Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted, ReceivedAt: now.Add(-time.Hour), ContentSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Reference: "test", AssertedWithoutArchive: true},
			}}}
			if name == "cleanup-unknown" {
				prices.failure = errors.New("actual cleanup outcome unknown")
			}
			if name == "caller-canceled" {
				prices.cancel = cancel
			}
			registry := provider.NewRegistry()
			for _, id := range []string{"target_first", "target_later"} {
				if err = registry.Register(provider.Target{ID: id, DeploymentID: "dep_terminal", PublicModel: "chat", ProviderModel: "provider-model", Adapter: f.adapter}); err != nil {
					t.Fatal(err)
				}
			}
			service, err := NewServiceWithOptions(f.service.auth, registry, f.service.accounting, ServiceOptions{Pricing: prices})
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.Chat(ctx, f.plaintext, chatRequest())
			var response *Error
			if !errors.As(err, &response) {
				t.Fatal("refused request returned success", err)
			}
			want := "budget_exceeded"
			if name != "budget-refusal" {
				want = "accounting_unavailable"
			}
			if response.Code != want {
				t.Fatal("cleanup failure lost its terminal failure", response.Code, err)
			}
			if prices.cleanupErr != nil {
				t.Fatal("original terminal cleanup was not accessible", prices.cleanupErr)
			}
			if f.adapter.calls != 0 || prices.prepares.Load() != 1 || prices.deletes.Load() != 1 {
				t.Fatal("refused Prepared obligation reached another attempt", f.adapter.calls, prices.prepares.Load(), prices.deletes.Load())
			}
			if name == "cleanup-unknown" && !errors.Is(err, prices.failure) {
				t.Fatal("unknown cleanup error was discarded", err)
			}
		})
	}
}
