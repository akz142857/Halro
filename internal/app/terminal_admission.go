package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func (r *replicationRuntime) reserveTerminalRequest(ctx context.Context, count int) (context.Context, func() error, error) {
	if r == nil || r.role != replication.RolePrimary || r.coordinator == nil {
		return ctx, nil, errors.New("terminal HA admission requires the actual Primary generation")
	}
	if count < 1 || count > resourcebudget.MaxCompletionBatch {
		return ctx, nil, resourcebudget.ErrInvalid
	}
	w, err := r.coordinator.ReserveTerminalWindow(ctx, count, ledger.TerminalReplicationFrameBytes)
	if err != nil {
		return ctx, nil, err
	}
	return w.Bind(ctx), w.Close, nil
}
