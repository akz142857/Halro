package governance

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/boundedfilecache"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/scratchstate"
	"math"
	"os"
	"syscall"
)

// This unpublished reducer uses the same governance domain mutation function
// as ordinary and live COW paths. Only its fresh disk engine is different.
func buildFixedPackedTypedGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions, phase func(string)) (result *GovernanceProjection, reference TypedGovernanceReference, resultErr error) {
	if o.Native == nil || o.Identity == ([32]byte{}) || len(o.RootKey) != 32 || o.Budget == nil || o.RootMaxDiskBytes <= 0 || o.RootMaxAppendBytes <= 0 || o.ProjectionPath == "" || o.ProjectionMaxDiskBytes <= 0 || o.ProjectionMaxDiskBytes > math.MaxInt64-2*governanceSealWorkspaceRAMBytes {
		return nil, reference, authenticatedstate.ErrRecovery
	}
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 2 * governanceSealWorkspaceRAMBytes})
	if err != nil {
		return nil, reference, err
	}
	var scratch *scratchstate.Store
	p := &GovernanceProjection{cowNative: o.Native, cowBudget: o.Budget, cowViews: make(chan struct{}, 1)}
	debt := &PackedGovernanceBuildRetirement{Candidate: p, Workspace: work, fixedScratch: scratch}
	defer func() {
		debt.fixedScratch = scratch
		debt.uncertainSourceExit = errors.Is(resultErr, ownedfile.ErrRetirement)
		if scratch != nil {
			if err := scratch.Discard(); err != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
			}
		}
		if resultErr != nil {
			if errors.Is(resultErr, ownedfile.ErrRetirement) && debt.uncertainSourceExit {
				p.retirementErr = errors.Join(p.retirementErr, resultErr)
			}
			if err := p.Discard(); err != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
			}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			debt.Cause = resultErr
			resultErr = debt
			result, reference = p, TypedGovernanceReference{}
			return
		}
		if feeErr := releasePackedGovernanceFees(debt); feeErr != nil {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, feeErr)
			if err := p.Discard(); err != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
			}
			debt.Cause = resultErr
			resultErr = debt
			result, reference = p, TypedGovernanceReference{}
		} else if resultErr != nil {
			result, reference = nil, TypedGovernanceReference{}
		}
	}()
	p.liveRAM, err = o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: governanceSealWorkspaceRAMBytes})
	if err != nil {
		return nil, reference, err
	}
	native := o.Native
	native.mu.Lock()
	defer native.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, reference, err
	}
	if native.file == nil || native.closing || native.terminalErr != nil || native.readerRetirementErr != nil {
		return nil, reference, ErrUnavailable
	}
	head := Summary{Records: native.sequence, Bytes: native.offset, LastHash: native.lastHash}
	source, err := os.OpenFile(native.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, reference, err
	}
	defer func() {
		closeFile := native.closeReaderFile
		if closeFile == nil {
			closeFile = (*os.File).Close
		}
		if err := ownedfile.CloseError(source, closeFile(source)); err != nil {
			native.readerRetirementErr = errors.Join(native.readerRetirementErr, err)
			native.closing = true
			p.ready = false
			p.retirementErr = errors.Join(p.retirementErr, err)
			resultErr = errors.Join(resultErr, err)
		}
		// Caller cancellation during the actual exit precedes publication.
		resultErr = errors.Join(resultErr, ctx.Err())
	}()
	info, err := native.file.Stat()
	if err != nil {
		return nil, reference, err
	}
	verify := func() error {
		opened, e := source.Stat()
		named, n := os.Lstat(native.path)
		if e != nil || n != nil || !opened.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(info, opened) || !os.SameFile(info, named) || opened.Size() != head.Bytes || named.Size() != head.Bytes || !opened.ModTime().Equal(info.ModTime()) || !named.ModTime().Equal(info.ModTime()) {
			return errors.Join(ErrCorrupt, e, n)
		}
		return ctx.Err()
	}
	if err := verify(); err != nil {
		return nil, reference, err
	}
	var firstHash [32]byte
	var last Record
	var frameStart int64
	var frameLength uint32
	verified, partial, err := scanSealPrefix(ctx, boundedfilecache.NewReader(source, head.Bytes), native.key, func(r Record, start int64, length uint32) error {
		if r.Sequence == 1 {
			firstHash = r.Hash
		}
		last, frameStart, frameLength = r, start, length
		return ctx.Err()
	})
	if err != nil || partial || verified != head {
		return nil, reference, errors.Join(ErrGovernanceSealSource, err)
	}
	journal, err := governanceSealReferenceFromVerified(head, native.key, firstHash, last, frameStart, frameLength)
	if err != nil {
		return nil, reference, err
	}
	if err := verify(); err != nil {
		return nil, reference, err
	}
	if phase != nil {
		phase("source-verified")
	}
	if err := ctx.Err(); err != nil {
		return nil, reference, err
	}
	scope := typedGovernanceScope(o.Identity)
	rootKey, err := hkdf.Key(sha256.New, o.RootKey, scope[:], "halro:governance:typed-content:v2", 32)
	if err != nil {
		return nil, reference, err
	}
	defer clear(rootKey)
	scratch, err = scratchstate.Create(ctx, o.ProjectionPath, scratchstate.Options{Budget: o.Budget, MaxDiskBytes: o.ProjectionMaxDiskBytes, Reservation: o.ProjectionDiskReservation})
	if err != nil {
		return nil, reference, err
	}
	if err := scratch.Update(ctx, func(tx *scratchstate.Transaction) error {
		if err := tx.Put([]byte{0}, typedGovernanceMarker); err != nil {
			return err
		}
		return govPut(&govRows{ctx: ctx, scratch: tx}, "meta", []byte("position"), Summary{})
	}); err != nil {
		return nil, reference, err
	}
	batch := make([]Record, 0, governanceProjectionBatchRows)
	defer clear(batch[:cap(batch)])
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := scratch.Update(ctx, func(tx *scratchstate.Transaction) error {
			return applyGovernanceRows(ctx, &govRows{ctx: ctx, scratch: tx}, batch)
		})
		clear(batch)
		batch = batch[:0]
		batchBytes = 0
		return err
	}
	reduced, partial, err := scanSealPrefix(ctx, boundedfilecache.NewReader(source, head.Bytes), native.key, func(r Record, _ int64, _ uint32) error {
		raw, err := json.Marshal(r)
		if err != nil || len(raw) > governanceProjectionMaxRow {
			return errors.Join(ErrCorrupt, err)
		}
		if len(batch) == governanceProjectionBatchRows || batchBytes+len(raw) > governanceProjectionBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, r)
		batchBytes += len(raw)
		return ctx.Err()
	})
	if err != nil || partial || reduced != head {
		return nil, reference, errors.Join(ErrCorrupt, err)
	}
	if err := flush(); err != nil {
		return nil, reference, err
	}
	err = scratch.View(ctx, func(tx *scratchstate.Transaction) error {
		it, err := tx.Iterator()
		if err != nil {
			return err
		}
		p.cow, err = authenticatedstate.CreateStore(ctx, o.RootDirectory, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: typedGovernanceCut(head), Key: rootKey, MaxDiskBytes: o.RootMaxDiskBytes, MaxAppendBytes: o.RootMaxAppendBytes, Budget: o.Budget}, func() (authenticatedstate.Row, bool, error) {
			row, found, err := it.Next()
			return authenticatedstate.Row{Key: row.Key, Value: row.Value}, found, err
		})
		return err
	})
	if err != nil {
		return nil, reference, err
	}

	if err := verify(); err != nil {
		return nil, reference, err
	}
	// A complete second post-reduction MAC pass detects same-inode old-byte
	// replacement even if the external writer restored its modification time.
	verified, partial, err = scanSealPrefix(ctx, boundedfilecache.NewReader(source, head.Bytes), native.key, nil)
	if err != nil || partial || verified != head {
		return nil, reference, errors.Join(ErrGovernanceSealSource, err)
	}
	if err := verify(); err != nil {
		return nil, reference, err
	}
	if err := p.viewRows(ctx, func(_ any, actual Summary) error {
		if actual != head {
			return ErrCorrupt
		}
		return nil
	}); err != nil {
		return nil, reference, err
	}
	if phase != nil {
		phase("before-publication")
	}
	if err := ctx.Err(); err != nil {
		return nil, reference, err
	}
	root, err := p.cow.SealRootReference(ctx, typedGovernanceSchema())
	if err != nil {
		return nil, reference, err
	}
	p.ready = true
	reference = TypedGovernanceReference{Journal: journal, Projection: AuthenticatedGovernanceProjectionReference{Schema: typedGovernanceSchema(), Root: root, Scope: scope, Position: journal.Position}}
	return p, reference, nil
}
