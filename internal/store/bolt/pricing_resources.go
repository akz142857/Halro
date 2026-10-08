package bolt

import (
	"sync"

	"github.com/akz142857/Halro/internal/pricinglimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

type pricingResources struct {
	mu                  sync.Mutex
	started, configured bool
	manager             *resourcebudget.Manager
	lifetime            resourcebudget.Permit
	lifetimeOwned       bool
	ioRAM               int64
}

// ConfigurePricingResources is called exactly once on an unpublished Store.
// Acquire the fixed lifetime envelope before any pricing table/owner allocation.
// Disk remains disabled until startup reconciliation and durable owners exist.
func (s *Store) ConfigurePricingResources(manager *resourcebudget.Manager, lifetimeRAM, ioRAM int64) error {
	leave, err := s.enter()
	if err != nil {
		return err
	}
	defer leave()
	r := &s.pricingResources
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.configured || manager == nil || lifetimeRAM < pricinglimits.MinLifetimeRAMBytes || ioRAM < pricinglimits.MinIORAMBytes {
		return resourcebudget.ErrInvalid
	}
	permit, err := manager.TryAcquireRecovery(resourcebudget.Cost{RAM: lifetimeRAM})
	if err != nil {
		return err
	}
	r.manager, r.lifetime, r.lifetimeOwned, r.ioRAM, r.configured = manager, permit, true, ioRAM, true
	return nil
}

type pricingIOReservation struct {
	permit  resourcebudget.Permit
	owned   bool
	retired bool
}

func (s *Store) acquirePricingIO() (*pricingIOReservation, error) {
	r := &s.pricingResources
	r.mu.Lock()
	r.started = true
	manager, ram := r.manager, r.ioRAM
	r.mu.Unlock()
	operation := &pricingIOReservation{retired: true}
	if manager != nil {
		permit, err := manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: ram})
		if err != nil {
			return nil, err
		}
		operation.permit, operation.owned = permit, true
	}
	return operation, nil
}
func (r *pricingIOReservation) release() error {
	if r == nil || !r.owned || !r.retired {
		return nil
	}
	r.owned = false
	return r.permit.Release()
}

// Store admission has already joined all operations. Clock Close must precede
// refund; cleanup errors alone do not mean a physically closed descriptor lives.
func (s *Store) retirePricingResources() error {
	r := &s.pricingResources
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lifetimeOwned {
		return nil
	}
	if s.pricingClock != nil && !s.pricingClock.fileRetired {
		return nil
	}
	s.pricingClock, s.pricingClockErr = nil, nil
	permit := r.lifetime
	r.lifetimeOwned = false
	r.lifetime = resourcebudget.Permit{}
	r.manager = nil
	return permit.Release()
}
