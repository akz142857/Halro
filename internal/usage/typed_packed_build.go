package usage

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

// PackedUsageBuildRetirement retains actual construction owners and all their
// fees when a source, scratch close/removal, or candidate retirement is uncertain.
// An error is not evidence that the private scratch bytes have disappeared.
type PackedUsageBuildRetirement struct {
	Cause                                 error
	Scratch, Candidate                    *DiskProjection
	Workspace, Mapping, ScratchDisk, Live resourcebudget.Permit
	uncertainSourceExit                   bool
}

func (e *PackedUsageBuildRetirement) Error() string { return e.Cause.Error() }
func (e *PackedUsageBuildRetirement) Unwrap() error { return e.Cause }

// BuildPackedTypedUsageProjection is explicit offline preparation. It reduces
// the complete authenticated native source into one exclusive fresh scratch DB,
// then streams ordinal namespace rows into CreateStore once. ScratchPath must
// not exist; saved DB bytes are never opened/imported as authority. The returned
// owner uses exactly the same typed codec, native cut, and live mutation protocol
// as BuildTypedUsageProjection. The fixed workspace and scratch disk ceiling are charged
// to the same Manager and released only after actual scratch Discard.
func BuildPackedTypedUsageProjection(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease) (*DiskProjection, TypedUsageReference, error) {
	return buildFixedPackedTypedUsageProjection(ctx, o, lease, false)
}

func buildPackedTypedUsageProjection(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease, phase func(string)) (*DiskProjection, TypedUsageReference, error) {
	return buildPackedTypedUsageProjectionWithMode(ctx, o, lease, phase, false)
}

func buildPackedTypedUsageProjectionWithMode(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease, phase func(string), haReadOnly bool) (result *DiskProjection, reference TypedUsageReference, resultErr error) {
	if o.Native == nil || o.Owner == nil || o.Identity == ([32]byte{}) || len(o.UsageKey) != 32 || o.Budget == nil || o.RootDirectory == "" || o.RootMaxDiskBytes <= 0 || o.RootMaxAppendBytes <= 0 || o.ScratchPath == "" || o.ScratchMaxDiskBytes <= 0 || o.ScratchMaxDiskBytes > int64(^uint(0)>>1)-usageSealWorkspaceRAMBytes {
		return nil, reference, authenticatedstate.ErrRecovery
	}
	if err := o.Owner.ValidateNativeSource(ctx, o.Native, lease); err != nil {
		return nil, reference, err
	}
	if err := o.Owner.ValidateResourceOwner(ctx, o.Budget); err != nil {
		return nil, reference, err
	}
	debt := &PackedUsageBuildRetirement{}
	defer func() {
		debt.uncertainSourceExit = errors.Is(resultErr, ownedfile.ErrRetirement)
		if debt.Scratch != nil {
			if err := debt.Scratch.Discard(); err != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
			}
		}
		if resultErr != nil && debt.Candidate != nil {
			if err := debt.Candidate.Discard(); err != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
			}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			debt.Cause = resultErr
			resultErr = debt
			result, reference = debt.Candidate, TypedUsageReference{}
			return
		}
		feeErr := releasePackedUsageFees(debt, debt.Candidate == nil)
		if feeErr != nil {
			resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, feeErr)
			if debt.Candidate != nil {
				if err := debt.Candidate.Discard(); err != nil {
					resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, err)
				}
			}
			debt.Cause = resultErr
			resultErr = debt
			result, reference = debt.Candidate, TypedUsageReference{}
		} else if resultErr != nil {
			result, reference = nil, TypedUsageReference{}
		}
	}()
	var err error
	debt.Workspace, err = o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes + ledger.AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return nil, reference, err
	}
	debt.Mapping, err = o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: o.ScratchMaxDiskBytes + usageSealWorkspaceRAMBytes})
	if err != nil {
		return nil, reference, err
	}
	if o.ScratchReservation != (resourcebudget.Permit{}) {
		debt.ScratchDisk, err = o.ScratchReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: o.ScratchMaxDiskBytes})
	} else {
		debt.ScratchDisk, err = o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: o.ScratchMaxDiskBytes})
	}
	if err != nil {
		return nil, reference, err
	}
	debt.Live, err = o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes})
	if err != nil {
		return nil, reference, err
	}
	initialRef, err := o.Owner.SealReference(ctx)
	if err != nil {
		return nil, reference, err
	}
	loanSource := o.Owner.WithOfflineMaintenanceUsageSource
	if haReadOnly {
		loanSource = o.Owner.WithOfflineHAMaintenanceUsageSource
	}
	resultErr = loanSource(ctx, o.Native, lease, o.Budget, initialRef.Scope, func(ctx context.Context, source ledger.AuthenticatedUsageSource) error {
		ref := source.Reference()
		frozen := source.NativeHead().Head
		if ref.Anchor.Head.After(frozen) {
			return authenticatedstate.ErrRecovery
		}
		if err := source.VerifyAnchors(ctx, [2]ledger.ReplayAnchor{ref.Anchor, source.NativeHead()}); err != nil {
			return err
		}
		debt.Scratch, err = CreateDiskProjectionWithLimit(o.ScratchPath, packedUsageSource{source}, o.ScratchMaxDiskBytes)
		if err != nil {
			return err
		}
		if err := replayPackedUsageScratch(ctx, source, debt.Scratch, ref.Anchor); err != nil {
			return err
		}
		if err := alignPackedUsageBoundary(ctx, debt.Scratch, ref.Anchor); err != nil {
			return err
		}
		if err := source.VerifyAnchors(ctx, [2]ledger.ReplayAnchor{ref.Anchor, source.NativeHead()}); err != nil {
			return err
		}
		if err := source.Check(ctx); err != nil || source.Reference() != ref {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		if err := debt.Scratch.file.VerifyName(); err != nil {
			return err
		}
		scope := typedUsageScope(o.Identity, ref.Scope)
		key, err := hkdf.Key(sha256.New, o.UsageKey, scope[:], "halro:usage:typed-content:v2", 32)
		if err != nil {
			return err
		}
		defer clear(key)
		var store *authenticatedstate.Store
		err = debt.Scratch.db.View(func(tx *bbolt.Tx) error {
			marker := true
			ordinal := 0
			var cursor *bbolt.Cursor
			var k, v []byte
			var createErr error
			store, createErr = authenticatedstate.CreateStore(ctx, o.RootDirectory, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: typedUsageCut(ref.Anchor), Key: key, MaxDiskBytes: o.RootMaxDiskBytes, MaxAppendBytes: o.RootMaxAppendBytes, Budget: o.Budget}, func() (authenticatedstate.Row, bool, error) {
				if err := ctx.Err(); err != nil {
					return authenticatedstate.Row{}, false, err
				}
				if marker {
					marker = false
					return authenticatedstate.Row{Key: []byte{0}, Value: []byte{byte(TypedUsageSealCodec)}}, true, nil
				}
				for ordinal < len(diskUsageBuckets) {
					if cursor == nil {
						bucket := tx.Bucket([]byte(diskUsageBuckets[ordinal]))
						if bucket == nil {
							return authenticatedstate.Row{}, false, ErrDiskUsageCorrupt
						}
						cursor = bucket.Cursor()
						k, v = cursor.First()
					}
					if k == nil {
						ordinal++
						cursor = nil
						continue
					}
					if v == nil || len(v) > diskUsageMaxValue {
						return authenticatedstate.Row{}, false, ErrDiskUsageCorrupt
					}
					key, err := typedUsageKey(diskUsageBuckets[ordinal], k)
					if err != nil {
						return authenticatedstate.Row{}, false, err
					}
					row := authenticatedstate.Row{Key: key, Value: v}
					k, v = cursor.Next()
					return row, true, nil
				}
				return authenticatedstate.Row{}, false, nil
			})
			return createErr
		})
		if store != nil {
			var ownerErr error
			debt.Candidate, ownerErr = newTypedUsageOwner(store, o.Native, lease, o.Owner, o.Budget, o.Identity)
			err = errors.Join(err, ownerErr)
			debt.Candidate.liveRAM = debt.Live
			debt.Live = resourcebudget.Permit{}
		}
		if err != nil {
			return err
		}
		if err := debt.Scratch.file.VerifyName(); err != nil {
			return err
		}
		if err := source.VerifyAnchors(ctx, [2]ledger.ReplayAnchor{ref.Anchor, source.NativeHead()}); err != nil {
			return err
		}
		if err := source.Check(ctx); err != nil || source.Reference() != ref {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		actual, err := debt.Candidate.ReadReplayAnchor(ctx)
		if err != nil || actual != ref.Anchor {
			return errors.Join(ErrDiskUsageCorrupt, err)
		}
		root, err := store.SealRootReference(ctx, typedUsageSchema())
		if err != nil {
			return err
		}
		if phase != nil {
			phase("before-publication")
		}
		if err := source.Check(ctx); err != nil {
			return err
		}
		reference = TypedUsageReference{Ledger: ref, Projection: AuthenticatedUsageSealReference{Schema: typedUsageSchema(), Root: root, Scope: scope, Position: authenticatedUsagePosition(ref)}}
		result = debt.Candidate
		return ctx.Err()
	})
	return result, reference, resultErr
}

func alignPackedUsageBoundary(ctx context.Context, p *DiskProjection, target ledger.ReplayAnchor) error {
	return p.engine.scoped(ctx).Update(func(tx *usageTx) error {
		state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if err != nil || !found {
			return errors.Join(ErrDiskUsageCorrupt, err)
		}
		if usageStateAnchor(state) == target {
			return ctx.Err()
		}
		if target.Head.Offset != 0 || target.Head.Sequence != state.Watermark.Sequence || target.Hash != state.ReplayHash || target.Epoch != state.ReplayEpoch || !target.Head.After(state.Watermark) {
			return ErrDiskUsageCorrupt
		}
		state.Watermark = target.Head
		return diskUsagePut(tx, "meta", []byte("state"), state)
	})
}

// This adapter never outlives the source callback. Every dependency is read
// from its one authenticated pin; no native-length cache or map is retained.
type packedUsageSource struct {
	ledger.AuthenticatedUsageSource
}

func (s packedUsageSource) ReadPosition(ctx context.Context) (ledger.Watermark, [32]byte, error) {
	if err := s.Check(ctx); err != nil {
		return ledger.Watermark{}, [32]byte{}, err
	}
	a := s.Reference().Anchor
	return a.Head, a.Hash, nil
}

func replayPackedUsageScratch(ctx context.Context, source ledger.AuthenticatedUsageSource, scratch *DiskProjection, anchor ledger.ReplayAnchor) error {
	batch := make([]ledger.Record, 0, diskReplayBatchRows)
	bytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		scratch.mu.Lock()
		err := scratch.applyReplayBatchWithFillLocked(ctx, batch, true)
		scratch.mu.Unlock()
		if err != nil && scratch.db != nil {
			stats := scratch.db.Stats()
			var size int64 = -1
			if info, statErr := os.Stat(scratch.db.Path()); statErr == nil {
				size = info.Size()
			}
			err = fmt.Errorf("packed Usage scratch replay sequence=%d..%d rows=%d bytes=%d file_bytes=%d max_bytes=%d open_tx=%d free_pages=%d pending_pages=%d: %w", batch[0].Sequence, batch[len(batch)-1].Sequence, len(batch), bytes, size, scratch.db.MaxSize, stats.OpenTxN, stats.FreePageN, stats.PendingPageN, err)
		}
		clear(batch)
		batch, bytes = batch[:0], 0
		return err
	}
	head, err := source.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(r ledger.Record) error {
		if r.Sequence > anchor.Head.Sequence {
			return ctx.Err()
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if len(raw) > diskUsageMaxValue || len(raw) > diskReplayBatchBytes {
			return ErrDiskUsageLimit
		}
		if len(batch) == diskReplayBatchRows || len(raw) > diskReplayBatchBytes-bytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, r)
		bytes += len(raw)
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	if head != source.NativeHead().Head {
		return authenticatedstate.ErrRecovery
	}
	return flush()
}

// Retire retries known derivative namespace debt after the original inode has
// been restored. An unknown native/source IO exit cannot be cleared by deleting
// derivatives. Successfully released permits are cleared before a retry.
func (e *PackedUsageBuildRetirement) Retire() error {
	if e == nil {
		return nil
	}
	if e.uncertainSourceExit {
		return e.Cause
	}
	if e.Scratch != nil {
		if err := e.Scratch.Discard(); err != nil {
			return errors.Join(ownedfile.ErrRetirement, err)
		}
	}
	if e.Candidate != nil {
		if err := e.Candidate.Discard(); err != nil {
			return errors.Join(ownedfile.ErrRetirement, err)
		}
	}
	return releasePackedUsageFees(e, true)
}
func releasePackedUsageFees(e *PackedUsageBuildRetirement, live bool) error {
	fees := []*resourcebudget.Permit{&e.Workspace, &e.Mapping, &e.ScratchDisk}
	if live {
		fees = append(fees, &e.Live)
	}
	for _, p := range fees {
		if *p != (resourcebudget.Permit{}) {
			if err := p.Release(); err != nil {
				return err
			}
			*p = resourcebudget.Permit{}
		}
	}
	return nil
}
