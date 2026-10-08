package app

import (
	"context"
	"errors"
	"sync"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

var (
	errGovernanceExportBusy    = errors.New("governance export is already running")
	errGovernanceExportRetired = errors.New("governance export admission is retired")
)

// At most one export is accepted, with no waiter queue. This is an operation
// count/lifetime bound, not the shared physical RAM/disk fee contract of I-03.
// Withdrawal is permanent; cancellation limits only this drain caller's wait.
type governanceExportLifecycle struct {
	mu      sync.Mutex
	retired bool
	done    chan struct{}
}

func (g *governanceExportLifecycle) enter(ctx context.Context) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if g.retired {
		return nil, errGovernanceExportRetired
	}
	if g.done != nil {
		return nil, errGovernanceExportBusy
	}
	done := make(chan struct{})
	g.done = done
	var once sync.Once
	return func() { once.Do(func() { g.mu.Lock(); defer g.mu.Unlock(); close(done); g.done = nil }) }, nil
}

func (g *governanceExportLifecycle) withdraw() <-chan struct{} {
	g.mu.Lock()
	g.retired = true
	done := g.done
	g.mu.Unlock()
	return done
}

func (g *governanceExportLifecycle) drain(ctx context.Context) error {
	done := g.withdraw()
	if done == nil {
		return ctx.Err()
	}
	select {
	case <-done:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Captured inside the selected Gov/Ledger cuts. The Metadata view callback
// includes file publication; no ListOutcomeDefinitions or retained closed view.
func (r *Runtime) governanceDefinitionOwner(ctx context.Context, visit func(governance.ExportDefinitionSource) error) error {
	return r.store.WithOutcomeDefinitionExport(ctx, func(view *boltstore.OutcomeDefinitionExportView) error {
		return visit(governance.ExportDefinitionSource{
			Read: func(ctx context.Context, yield func(domain.OutcomeDefinition) error) error {
				for {
					page, err := view.ReadPage(ctx, boltstore.OutcomeDefinitionExportMaxRows, boltstore.OutcomeDefinitionExportMaxBytes)
					if err != nil {
						return err
					}
					for _, row := range page.Items {
						if err := yield(row); err != nil {
							return err
						}
					}
					if page.Done {
						return nil
					}
				}
			},
			Complete: view.Complete,
		})
	})
}
