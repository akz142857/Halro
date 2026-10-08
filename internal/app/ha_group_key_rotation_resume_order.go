package app

import (
	"context"
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// The actual unlock can still be old while the key-only history file has been
// replaced under the new key. Find the same operation in the fixed capsules
// using that unlock first. This only orders reads; the caller must reread its
// original inode, check every configured member and qualify all actual sources.
func haGroupResumeSlotOrder(ctx context.Context, cfg config.Config, key []byte, operation string, manager *resourcebudget.Manager) (order [haGroupRotationIntentSlots]int, resultErr error) {
	for n := range order {
		order[n] = n
	}
	work, err := haGroupRotationIntentWorkspace(ctx, manager)
	if err != nil {
		return order, err
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &haGroupRotationGateFailure{Cause: resultErr, Work: work}
			return
		}
		resultErr = errors.Join(resultErr, work.Release())
	}()
	found := -1
	for slot := range haGroupRotationIntentSlots {
		if err := ctx.Err(); err != nil {
			return order, err
		}
		raw, _, err := readHAGroupRotationBytes(haGroupRotationIntentPath(cfg.Storage.DataDir, slot))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return order, err
		}
		intent, decodeErr := decodeHAGroupRotationIntent(raw, key)
		clear(raw)
		if decodeErr != nil {
			continue
		}
		matches := intent.OperationID == operation
		intent.clearKeys()
		if !matches {
			continue
		}
		if found >= 0 {
			return order, authenticatedstate.ErrSealIneligible
		}
		found = slot
	}
	if found >= 0 {
		order[0], order[found] = order[found], order[0]
	}
	return order, nil
}
