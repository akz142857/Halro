package bolt

import (
	"context"
)

type pricePinPhysicalRecoveryBackend interface {
	PricePinRecoveryContext(context.Context, int) (context.Context, error)
	FinishPricePinRecovery(context.Context) error
}

func (s *Store) pricePinRecoveryContext(ctx context.Context, count int) (context.Context, error) {
	if b, ok := s.facade.(pricePinPhysicalRecoveryBackend); ok {
		return b.PricePinRecoveryContext(ctx, count)
	}
	return ctx, nil
}
func (s *Store) finishPricePinPhysicalRecovery(ctx context.Context) error {
	if b, ok := s.facade.(pricePinPhysicalRecoveryBackend); ok {
		return b.FinishPricePinRecovery(ctx)
	}
	return nil
}

func (s *Store) viewPricePinRecovery(ctx context.Context, visit func(*Tx) error) error {
	if s.facade == nil {
		return s.view(visit)
	}
	leave, err := s.enter()
	if err != nil {
		return err
	}
	defer leave()
	ctx, cancel := s.confirmationContext(ctx)
	defer cancel()
	err = s.facade.View(ctx, func(tx FacadeTransaction) error {
		if err := s.RecoveryError(); err != nil {
			return err
		}
		err := visit(&Tx{facade: tx})
		if fault := tx.Error(); fault != nil {
			return fault
		}
		return err
	})
	return s.facadeFailure(err)
}
