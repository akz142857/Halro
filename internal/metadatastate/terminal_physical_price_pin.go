package metadatastate

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Only the unchanged prepared intent becoming committed consumes prepaid COW
// work. This check reads the actual authenticated root before native acceptance.
func (b *Baseline) validatePhysicalPricePinLocked(ctx context.Context, ops []metadatajournal.Op) (resultErr error) {
	recovery := ctx.Value(pricePinRecoveryKey{}) != nil
	if len(ops) == 0 || len(ops) > resourcebudget.MaxCompletionBatch || !recovery && len(ops) != 1 {
		return ErrSource
	}
	pin, err := b.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	seen := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		if len(op.Path) != 1 || op.Path[0] != "deployment_price_pin_intents" || op.Kind != metadatajournal.OpPut && op.Kind != metadatajournal.OpDelete {
			return ErrSource
		}
		if _, ok := seen[string(op.Key)]; ok {
			return ErrSource
		}
		seen[string(op.Key)] = struct{}{}
		key, err := RowKey([][]byte{[]byte(op.Path[0])}, op.Key, false)
		if err != nil {
			return err
		}
		raw, found, err := pin.Get(ctx, key)
		if err != nil {
			return err
		}
		if !found {
			return ErrSource
		}
		var before, after domain.PricePinIntent
		if json.Unmarshal(raw, &before) != nil || before.Validate() != nil || before.State != domain.PricePinPrepared || before.AttemptID != string(op.Key) {
			return ErrSource
		}
		if b.physicalEnabled && !recovery {
			w, _ := ctx.Value(terminalWindowKey{}).(*TerminalPhysicalWindow)
			if w == nil || w.owner != b || w.closed.Load() || w.preparedPinIndex(op.Key, raw) < 0 {
				return ErrSource
			}
		}
		if op.Kind == metadatajournal.OpDelete {
			continue
		}
		if json.Unmarshal(op.Value, &after) != nil || after.Validate() != nil || after.State != domain.PricePinCommitted || after.LedgerSequence == 0 || after.CommittedAt == nil {
			return ErrSource
		}
		after.State = before.State
		after.LedgerSequence = before.LedgerSequence
		after.CommittedAt = before.CommittedAt
		if !reflect.DeepEqual(before, after) {
			return ErrSource
		}
	}
	return nil
}
