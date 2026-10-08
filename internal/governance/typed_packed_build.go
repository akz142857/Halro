package governance

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/scratchstate"
	bbolt "go.etcd.io/bbolt"
)

// PackedGovernanceBuildRetirement retains the actual derivative owners and
// permits for any unresolved Close/unlink, including replaced namespace names.
type PackedGovernanceBuildRetirement struct {
	Cause                  error
	Scratch, Candidate     *GovernanceProjection
	Workspace, ScratchDisk resourcebudget.Permit
	uncertainSourceExit    bool
	fixedScratch           *scratchstate.Store
}

func (e *PackedGovernanceBuildRetirement) Error() string { return e.Cause.Error() }
func (e *PackedGovernanceBuildRetirement) Unwrap() error { return e.Cause }

// BuildPackedTypedGovernanceProjection is explicit offline preparation from a
// complete frozen authenticated native journal. It builds one exclusive bounded
// authenticated fixed-workspace reducer, then streams ordinal rows into CreateStore once. No saved scratch
// is accepted; codec, native cut and live COW mutation remain unchanged. The same
// Manager charges fixed decoder/workspace RAM and the full scratch Disk ceiling.
func BuildPackedTypedGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions) (*GovernanceProjection, TypedGovernanceReference, error) {
	return buildFixedPackedTypedGovernanceProjection(ctx, o, nil)
}

func buildPackedTypedGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions, phase func(string)) (result *GovernanceProjection, reference TypedGovernanceReference, resultErr error) {
	if o.Native == nil || o.Identity == ([32]byte{}) || len(o.RootKey) != 32 || o.Budget == nil || o.RootMaxDiskBytes <= 0 || o.RootMaxAppendBytes <= 0 || o.ProjectionPath == "" || o.ProjectionMaxDiskBytes <= 0 || o.ProjectionMaxDiskBytes > math.MaxInt64-2*governanceSealWorkspaceRAMBytes {
		return nil, reference, authenticatedstate.ErrRecovery
	}
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: o.ProjectionMaxDiskBytes + 2*governanceSealWorkspaceRAMBytes})
	if err != nil {
		return nil, reference, err
	}
	var scratch *GovernanceProjection
	var scratchDisk resourcebudget.Permit
	if o.ProjectionDiskReservation != (resourcebudget.Permit{}) {
		scratchDisk, err = o.ProjectionDiskReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: o.ProjectionMaxDiskBytes})
	} else {
		scratchDisk, err = o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: o.ProjectionMaxDiskBytes})
	}
	if err != nil {
		if releaseErr := work.Release(); releaseErr != nil {
			return nil, reference, &PackedGovernanceBuildRetirement{Cause: errors.Join(err, ownedfile.ErrRetirement, releaseErr), Workspace: work}
		}
		return nil, reference, err
	}
	p := &GovernanceProjection{cowNative: o.Native, cowBudget: o.Budget, cowViews: make(chan struct{}, 1)}
	debt := &PackedGovernanceBuildRetirement{Candidate: p, Workspace: work, ScratchDisk: scratchDisk}
	defer func() {
		debt.Scratch = scratch
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
	verified, partial, err := scanSealPrefix(ctx, io.NewSectionReader(source, 0, head.Bytes), native.key, func(r Record, start int64, length uint32) error {
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
	scratch, err = BuildGovernanceProjectionWithLimit(ctx, native.path, native.key, o.ProjectionPath, head, o.ProjectionMaxDiskBytes)
	if err != nil {
		return nil, reference, err
	}
	if err := scratch.file.VerifyName(); err != nil {
		return nil, reference, err
	}
	err = scratch.db.View(func(tx *bbolt.Tx) error {
		marker, ordinal := true, 0
		var cursor *bbolt.Cursor
		var k, v []byte
		var createErr error
		p.cow, createErr = authenticatedstate.CreateStore(ctx, o.RootDirectory, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: typedGovernanceCut(head), Key: rootKey, MaxDiskBytes: o.RootMaxDiskBytes, MaxAppendBytes: o.RootMaxAppendBytes, Budget: o.Budget}, func() (authenticatedstate.Row, bool, error) {
			if err := ctx.Err(); err != nil {
				return authenticatedstate.Row{}, false, err
			}
			if marker {
				marker = false
				return authenticatedstate.Row{Key: []byte{0}, Value: bytes.Clone(typedGovernanceMarker)}, true, nil
			}
			for ordinal < len(governanceProjectionBuckets) {
				if cursor == nil {
					bucket := tx.Bucket([]byte(governanceProjectionBuckets[ordinal]))
					if bucket == nil {
						return authenticatedstate.Row{}, false, ErrCorrupt
					}
					cursor = bucket.Cursor()
					k, v = cursor.First()
				}
				if k == nil {
					ordinal++
					cursor = nil
					continue
				}
				if v == nil || len(v) > governanceProjectionMaxRow {
					return authenticatedstate.Row{}, false, ErrCorrupt
				}
				key, err := typedGovernanceKey(governanceProjectionBuckets[ordinal], k)
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
	if err != nil {
		return nil, reference, err
	}
	if err := scratch.file.VerifyName(); err != nil {
		return nil, reference, err
	}
	if err := verify(); err != nil {
		return nil, reference, err
	}
	// A complete second post-reduction MAC pass detects same-inode old-byte
	// replacement even if the external writer restored its modification time.
	verified, partial, err = scanSealPrefix(ctx, io.NewSectionReader(source, 0, head.Bytes), native.key, nil)
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

// Retire retries derivative debt after restoring its original inode. A native
// source Close with an unknown physical result remains latched and charged.
func (e *PackedGovernanceBuildRetirement) Retire() error {
	if e == nil {
		return nil
	}
	if e.uncertainSourceExit {
		return e.Cause
	}
	if e.fixedScratch != nil {
		if err := e.fixedScratch.Discard(); err != nil {
			return errors.Join(ownedfile.ErrRetirement, err)
		}
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
	return releasePackedGovernanceFees(e)
}
func releasePackedGovernanceFees(e *PackedGovernanceBuildRetirement) error {
	for _, p := range []*resourcebudget.Permit{&e.Workspace, &e.ScratchDisk} {
		if *p != (resourcebudget.Permit{}) {
			if err := p.Release(); err != nil {
				return err
			}
			*p = resourcebudget.Permit{}
		}
	}
	return nil
}
