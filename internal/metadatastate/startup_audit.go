package metadatastate

import (
	"context"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// StartSelectedStartupAudit starts the selected native owner's finite progress
// grant. Every business admission and callback operation rechecks actual native
// grant health; setting this candidate bit never grants readiness by itself.
// Seal/trim/full-source qualifications remain blocked through reader retirement.
func (b *Baseline) StartSelectedStartupAudit(ctx context.Context, options metadatajournal.StartupAuditOptions) (*metadatajournal.StartupAudit, error) {
	return b.StartSelectedStartupAuditObserved(ctx, options, nil)
}

// StartSelectedStartupAuditObserved reports bounded authenticated scalar
// progress to maintenance callers. It does not lend source or readiness proof.
func (b *Baseline) StartSelectedStartupAuditObserved(ctx context.Context, options metadatajournal.StartupAuditOptions, observe func(context.Context, uint64, int64) error) (*metadatajournal.StartupAudit, error) {
	if b == nil || !b.mu.TryLock() {
		return nil, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.selectedStartup || b.ctx.Err() != nil || !b.linked || b.journal == nil || b.retirementErr != nil {
		return nil, authenticatedstate.ErrClosed
	}
	jobCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	a, err := b.journal.StartSelectedStartupAuditObserved(jobCtx, b.budget, options, observe)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	// Native Close joins the actual reader/monitor before these lifetime links
	// are retired by Baseline.Close. No unowned cleanup goroutine is spawned.
	b.startupStop, b.startupCancel = stop, cancel
	b.ready.Store(true)
	return a, nil
}

// SelectedStartupFacade lends only the actual selected startup candidate.
// It differs from StandaloneFacade's structural freeze qualification: business
// reads/writes may use a live finite historical grant, while Seal/HA stay denied.
func (b *Baseline) SelectedStartupFacade(ctx context.Context, budget *resourcebudget.Manager, identity [32]byte, instanceID string, keyGeneration uint64) (boltstore.FacadeBackend, error) {
	if b == nil || !b.mu.TryLock() {
		return nil, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.selectedStartup || !b.ready.Load() || b.ctx.Err() != nil || b.budget != budget || b.evidence.Identity != identity || b.evidence.InstanceID != instanceID || b.evidence.KeyGeneration != keyGeneration || !b.linked || b.journal == nil || b.retirementErr != nil {
		return nil, ErrSource
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.journal.HealthError(); err != nil {
		return nil, err
	}
	if b.position != b.journal.Head() {
		return nil, ErrSource
	}
	if err := checkSavedRows(ctx, b.store, OpenOptions{BaselineOptions: BaselineOptions{InstanceID: instanceID}, Evidence: b.evidence, SavedHead: b.position}); err != nil {
		return nil, err
	}
	b.businessOnce.Do(func() { b.businessLane = make(chan struct{}, 1) })
	return &standaloneFacade{owner: b}, nil
}
