package governance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"syscall"
)

type AuthenticatedProjectionAdvance struct {
	From, To, NativeHead            Summary
	VerifiedRecords, AppliedRecords uint64
}

// AdvanceAuthenticatedFromNative catches up a restored image at a caller's
// independently permitted cut. The caller retires/drains old Managers and owns
// role/term exclusion. It authenticates the whole native source before changing
// any rows, then authenticates its suffix again while applying finite batches.
// Readers and Close join the complete job; a failure after Apply starts retires
// this generation. It grants no HA permission or common-cut publication.
func (p *GovernanceProjection) AdvanceAuthenticatedFromNative(ctx context.Context, native *Log, permitted Summary, budget *resourcebudget.Manager) (AuthenticatedProjectionAdvance, error) {
	if p == nil {
		return AuthenticatedProjectionAdvance{}, authenticatedstate.ErrRecovery
	}
	var receipt AuthenticatedProjectionAdvance
	err := p.withPhysicalProjectionWork(ctx, func(ctx context.Context) error {
		return p.withPhysicalApplyAuthority(ctx, permitted, func(ctx context.Context) error {
			var err error
			receipt, err = p.advanceAuthenticatedFromNative(ctx, native, permitted, budget, nil)
			return err
		})
	})
	return receipt, err
}

func (p *GovernanceProjection) advanceAuthenticatedFromNative(ctx context.Context, native *Log, permitted Summary, budget *resourcebudget.Manager, phase func(string)) (receipt AuthenticatedProjectionAdvance, resultErr error) {
	if p == nil || native == nil || budget == nil || permitted.Bytes < 0 || p.cow != nil && (p.cowNative != native || p.cowBudget != budget) {
		return receipt, authenticatedstate.ErrRecovery
	}
	ctx, cancel := context.WithCancel(ctx)
	p.advanceMu.Lock()
	if p.retired.Load() || p.advanceDone != nil {
		p.advanceMu.Unlock()
		cancel()
		return receipt, authenticatedstate.ErrBusy
	}
	done := make(chan struct{})
	p.advanceCancel, p.advanceDone = cancel, done
	p.advanceMu.Unlock()
	defer func() {
		cancel()
		p.advanceMu.Lock()
		p.advanceCancel, p.advanceDone = nil, nil
		close(done)
		p.advanceMu.Unlock()
	}()
	defer func() {
		if resultErr != nil {
			receipt = AuthenticatedProjectionAdvance{}
		}
	}()
	if !p.mu.TryRLock() {
		return receipt, authenticatedstate.ErrBusy
	}
	credit, err := p.liveRAM.RetainCovering(budget, resourcebudget.Cost{RAM: governanceSealWorkspaceRAMBytes})
	p.mu.RUnlock()
	if err != nil {
		return receipt, err
	}
	creditHeld := true
	defer func() {
		if creditHeld && !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, credit.Release())
		}
	}()
	workspace, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: governanceSealWorkspaceRAMBytes})
	if err != nil {
		return receipt, err
	}
	workspaceHeld := true
	defer func() {
		if workspaceHeld && !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, workspace.Release())
		}
	}()
	// Native -> projection matches durable writer order. Source Close waits on
	// this actual native mutex, not on a callback's logical completion signal.
	native.mu.Lock()
	defer native.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	mutated := false
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, workspace.Release(), credit.Release())
		}
		workspaceHeld, creditHeld = false, false
		if resultErr != nil && (mutated || errors.Is(resultErr, ErrCorrupt)) {
			p.ready = false
		}
		if errors.Is(resultErr, ErrCorrupt) {
			native.terminalErr = errors.Join(native.terminalErr, resultErr)
		}
	}()
	if p.retired.Load() || !p.ready || p.db == nil && p.cow == nil || native.file == nil || native.closing || native.terminalErr != nil {
		return receipt, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	var from Summary
	if err := p.viewRows(ctx, func(_ any, head Summary) error { from = head; return nil }); err != nil {
		return receipt, err
	}
	full := Summary{Records: native.sequence, Bytes: native.offset, LastHash: native.lastHash}
	if permitted.Records < from.Records || permitted.Bytes < from.Bytes || permitted.Records > full.Records || permitted.Bytes > full.Bytes {
		return receipt, ErrGovernanceSealSource
	}
	source, err := os.OpenFile(native.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return receipt, err
	}
	defer func() {
		closeFile := native.closeReaderFile
		if closeFile == nil {
			closeFile = (*os.File).Close
		}
		if err := closeFile(source); err != nil {
			err = ownedfile.CloseError(source, err)
			native.readerRetirementErr = errors.Join(native.readerRetirementErr, err)
			native.closing = true
			p.retirementErr = errors.Join(p.retirementErr, err)
			p.ready = false
			resultErr = errors.Join(resultErr, err)
		}
		if resultErr != nil && mutated {
			p.ready = false
		}
	}()
	info, err := native.file.Stat()
	if err != nil {
		return receipt, err
	}
	verify := func() error {
		opened, e := source.Stat()
		named, n := os.Lstat(native.path)
		if e != nil || n != nil || !named.Mode().IsRegular() || !os.SameFile(info, opened) || !os.SameFile(info, named) || opened.Size() != full.Bytes || named.Size() != full.Bytes || !opened.ModTime().Equal(info.ModTime()) || !named.ModTime().Equal(info.ModTime()) {
			return errors.Join(ErrCorrupt, e, n)
		}
		return ctx.Err()
	}
	if err := verify(); err != nil {
		return receipt, err
	}
	savedSeen, permittedSeen := from == (Summary{}), permitted == (Summary{})
	verified, partial, err := scanSealPrefix(ctx, io.NewSectionReader(source, 0, full.Bytes), native.key, func(record Record, _ int64, _ uint32) error {
		head := Summary{Records: record.Sequence, Bytes: record.Offset, LastHash: record.Hash}
		if head == from {
			savedSeen = true
		}
		if head == permitted {
			permittedSeen = true
		}
		return nil
	})
	if err != nil || partial || verified != full {
		if err == nil || !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			p.ready = false
			native.terminalErr = errors.Join(ErrCorrupt, err)
		}
		return receipt, errors.Join(ErrGovernanceSealSource, err)
	}
	if !savedSeen || !permittedSeen {
		return receipt, ErrGovernanceSealSource
	}
	if err := verify(); err != nil {
		return receipt, err
	}
	if phase != nil {
		phase("source-verified")
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	receipt = AuthenticatedProjectionAdvance{From: from, To: permitted, NativeHead: full, VerifiedRecords: full.Records}
	if from == permitted {
		return receipt, nil
	}
	p.ready = false
	mutated = true
	batch := make([]Record, 0, governanceProjectionBatchRows)
	bytes := 0
	defer clear(batch[:cap(batch)])
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := p.applyRecords(ctx, batch); err != nil {
			return err
		}
		receipt.AppliedRecords += uint64(len(batch))
		clear(batch)
		batch = batch[:0]
		bytes = 0
		if phase != nil {
			phase("batch-applied")
		}
		return nil
	}
	verified, partial, err = scanFrom(governanceContextReader{ctx: ctx, source: io.NewSectionReader(source, from.Bytes, full.Bytes-from.Bytes)}, native.key, from, func(record Record) error {
		receipt.VerifiedRecords++
		if record.Sequence > permitted.Records {
			return nil
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if len(raw) > governanceProjectionBatchBytes {
			return ErrCorrupt
		}
		if len(batch) == governanceProjectionBatchRows || p.cow != nil && len(batch) == typedGovernanceBatchRows || len(raw) > governanceProjectionBatchBytes-bytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, record)
		bytes += len(raw)
		return ctx.Err()
	})
	if err != nil || partial || verified != full {
		return receipt, errors.Join(ErrGovernanceSealSource, err)
	}
	if err := flush(); err != nil {
		return receipt, err
	}
	if err := verify(); err != nil {
		return receipt, err
	}
	if err := p.viewRows(ctx, func(_ any, head Summary) error {
		if head != permitted {
			return ErrCorrupt
		}
		return ctx.Err()
	}); err != nil {
		return receipt, err
	}
	p.ready = true
	return receipt, nil
}
