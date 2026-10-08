package bolt

import "context"

type pricePinTerminalBackend interface {
	PricePinTerminalContext(context.Context) context.Context
}

// The ordinary backend keeps batching. Selected Metadata carries the actual
// request loan directly through its serialized transaction and durable append.
func (s *Store) batchPricePinContext(ctx context.Context, terminal bool, visit func(*Tx) error) error {
	if s.facade == nil {
		return s.batch(visit)
	}
	if terminal {
		if b, ok := s.facade.(pricePinTerminalBackend); ok {
			ctx = b.PricePinTerminalContext(ctx)
		}
	}
	return s.updateContext(ctx, visit)
}

// Failure cleanup keeps the original selected request's terminal capability;
// the ordinary backend retains its immediate db.Update behavior.
func (s *Store) updatePricePinTerminalContext(ctx context.Context, visit func(*Tx) error) error {
	if s.facade != nil {
		if b, ok := s.facade.(pricePinTerminalBackend); ok {
			ctx = b.PricePinTerminalContext(ctx)
		}
	}
	return s.updateContext(ctx, visit)
}
