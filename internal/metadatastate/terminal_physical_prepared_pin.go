package metadatastate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// An original request owns a bounded number of unchanged Prepared rows. Hashes
// retain the complete authenticated row evidence without retaining large fields.
type preparedPinDebt struct {
	active       bool
	attempt, row [32]byte
}
type preparedPinPlan struct {
	window *TerminalPhysicalWindow
	next   [resourcebudget.MaxCompletionBatch]preparedPinDebt
	adds   [resourcebudget.MaxCompletionBatch]preparedPinDebt
}

func (w *TerminalPhysicalWindow) preparedPinIndex(key, raw []byte) int {
	attempt, row := sha256.Sum256(key), sha256.Sum256(raw)
	for i, p := range w.preparedPins {
		if p.active && p.attempt == attempt && p.row == row {
			return i
		}
	}
	return -1
}
func (w *TerminalPhysicalWindow) hasPreparedPins() bool {
	for _, p := range w.preparedPins {
		if p.active {
			return true
		}
	}
	return false
}

// The caller holds the real Baseline writer lock. Validation precedes native
// allocation/write, and recovery uses its independently authenticated authority.
func (b *Baseline) preparePinPlanLocked(ctx context.Context, ops []metadatajournal.Op) (result *preparedPinPlan, resultErr error) {
	if ctx.Value(pricePinRecoveryKey{}) != nil {
		return nil, nil
	}
	w, _ := ctx.Value(terminalWindowKey{}).(*TerminalPhysicalWindow)
	var plan *preparedPinPlan
	var snapshot *authenticatedstate.Snapshot
	defer func() {
		if snapshot != nil {
			resultErr = errors.Join(resultErr, snapshot.Close())
		}
	}()
	for _, op := range ops {
		if len(op.Path) != 1 || op.Path[0] != "deployment_price_pin_intents" {
			continue
		}
		if w == nil || w.owner != b || w.closed.Load() || w.remaining < 1 {
			return nil, resourcebudget.ErrPhysicalAllocation
		}
		if plan == nil {
			plan = &preparedPinPlan{window: w, next: w.preparedPins}
		}
		attempt := sha256.Sum256(op.Key)
		if ctx.Value(pricePinTerminalKey{}) != nil {
			// validatePhysicalPricePinLocked has checked the actual prior row.
			found := false
			for i, p := range plan.next {
				if p.active && p.attempt == attempt {
					plan.next[i] = preparedPinDebt{}
					found = true
					break
				}
			}
			if !found {
				return nil, ErrSource
			}
			continue
		}
		var pin domain.PricePinIntent
		if op.Kind != metadatajournal.OpPut || json.Unmarshal(op.Value, &pin) != nil || pin.Validate() != nil || pin.State != domain.PricePinPrepared || pin.AttemptID != string(op.Key) {
			return nil, ErrSource
		}
		if snapshot == nil {
			var err error
			snapshot, err = b.store.Snapshot(ctx)
			if err != nil {
				return nil, err
			}
		}
		key, err := RowKey([][]byte{[]byte(op.Path[0])}, op.Key, false)
		if err != nil {
			return nil, err
		}
		prior, found, err := snapshot.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if found && w.preparedPinIndex(op.Key, prior) < 0 {
			return nil, ErrSource
		}
		debt := preparedPinDebt{active: true, attempt: attempt, row: sha256.Sum256(op.Value)}
		free, count, found := -1, 0, false
		for i, p := range plan.next {
			if !p.active {
				if free < 0 {
					free = i
				}
				continue
			}
			count++
			if p.attempt == attempt {
				if p.row != debt.row {
					return nil, ErrSource
				}
				found = true
			}
		}
		if found {
			continue
		}
		if free < 0 || count >= w.remaining {
			return nil, resourcebudget.ErrPhysicalAllocation
		}
		plan.next[free], plan.adds[free] = debt, debt
	}
	return plan, nil
}

func (p *preparedPinPlan) finish(accepted, complete bool) {
	if p == nil || !accepted {
		return
	}
	if complete {
		p.window.preparedPins = p.next
		return
	}
	// Unknown acceptance cannot retire a prior obligation, even if the operation
	// intended to delete it. New possibly durable Prepared rows remain reachable.
	for i, add := range p.adds {
		if add.active {
			p.window.preparedPins[i] = add
		}
	}
}
