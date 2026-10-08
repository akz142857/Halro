package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

const (
	coldUsageReplayBatchRows  = 512
	coldUsageReplayBatchBytes = 8 << 20
)

// replayDiskUsageBatches is only for cold construction from one frozen,
// authenticated Ledger owner. Live Collector records keep the per-record
// synchronous apply barrier. The Usage projection itself caps each atomic page
// at 512 records and 8 MiB.
func replayDiskUsageBatches(ctx context.Context, native *ledger.Log, anchor ledger.ReplayAnchor, derivative *usage.LedgerDerivative, through *ledger.Watermark) (ledger.Watermark, error) {
	batch := make([]ledger.Record, 0, coldUsageReplayBatchRows)
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := derivative.ApplyReplayBatchContext(ctx, batch); err != nil {
			return err
		}
		clear(batch)
		batch = batch[:0]
		batchBytes = 0
		return nil
	}
	head, err := native.ReplayAuthenticated(ctx, anchor, func(record ledger.Record) error {
		if through != nil {
			position := ledger.Watermark{Generation: record.Generation, Sequence: record.Sequence, Offset: record.Offset}
			if position.After(*through) {
				return nil
			}
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if len(raw) > coldUsageReplayBatchBytes {
			return fmt.Errorf("Usage replay record exceeds 8 MiB batch budget")
		}
		if batchBytes+len(raw) > coldUsageReplayBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, record)
		batchBytes += len(raw)
		if len(batch) == coldUsageReplayBatchRows {
			return flush()
		}
		return nil
	})
	if err != nil {
		return head, err
	}
	if err := flush(); err != nil {
		return head, err
	}
	return head, nil
}
