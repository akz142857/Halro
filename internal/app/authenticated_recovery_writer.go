package app

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/ledger"
)

type authenticatedRecoveryProjection struct {
	*ledger.AuthenticatedLedgerProjection
	native *ledger.Log
}

func (p authenticatedRecoveryProjection) Apply(ctx context.Context, record ledger.Record) error {
	_, err := p.AdvanceOneFromNative(ctx, p.native, ledger.ProjectionPrefix{
		Head: ledger.Watermark{Generation: record.Generation, Offset: record.Offset, Sequence: record.Sequence}, Hash: record.Hash,
	})
	return err
}

func (p authenticatedRecoveryProjection) withAppendWorkspace(ctx context.Context, visit func(context.Context) error) error {
	return p.WithNativeApplyWorkspace(ctx, visit)
}

// This explicit maintenance adapter shares the existing role/term/ordering
// target and native lease gates. It does not select an authenticated Runtime
// backend or supply cross-component publication/saved-root recovery authority.
func (r *Runtime) newAuthenticatedRecoveryWriter(ctx context.Context, lease *ledger.MaintenanceLease, projection *ledger.AuthenticatedLedgerProjection) (*projectedRecoveryWriter, error) {
	if r == nil || projection == nil {
		return nil, errors.New("authenticated recovery requires native/projection owners")
	}
	return r.newRecoveryProjectionWriter(ctx, lease, authenticatedRecoveryProjection{projection, r.ledger})
}
