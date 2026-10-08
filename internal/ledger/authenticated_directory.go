package ledger

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const AuthenticatedDirectorySealIndexID uint16 = 1
const AuthenticatedDirectorySealCodec uint16 = 1

var authenticatedDirectorySchema = authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentLogDirectory, IndexID: AuthenticatedDirectorySealIndexID, Codec: AuthenticatedDirectorySealCodec}

type AuthenticatedDirectoryOptions struct {
	Identity                     [32]byte
	Budget                       *resourcebudget.Manager
	MaxDiskBytes, MaxAppendBytes int64
	DiskReservation              resourcebudget.Permit
}
type AuthenticatedDirectoryReference struct {
	Schema   authenticatedstate.SealRootSchema
	Root     authenticatedstate.SealRoot
	Position authenticatedstate.SealPosition // bound to the same exact Ledger cut
}
type AuthenticatedDirectory struct {
	selectedNative *Log // immutable selected constructor provenance
	mu             sync.Mutex
	budget         *resourcebudget.Manager
	scope          [32]byte
	ledgerScope    [32]byte
	closeCtx       context.Context
	cancel         context.CancelFunc
	sealActive     atomic.Bool
	sealDebt       resourcebudget.Permit
	sealErr        error
	store          *authenticatedstate.Store
	anchor         ReplayAnchor
	segments       uint64
	physical       *directoryPhysicalOwner
	closeDone      chan struct{}
	closeResult    error
}

func newAuthenticatedDirectory(store *authenticatedstate.Store, anchor ReplayAnchor, segments uint64, o AuthenticatedDirectoryOptions) *AuthenticatedDirectory {
	ctx, cancel := context.WithCancel(context.Background())
	return &AuthenticatedDirectory{store: store, anchor: anchor, segments: segments, budget: o.Budget, scope: authenticatedDirectoryScope(o.Identity), ledgerScope: authenticatedLedgerScope(o.Identity), closeCtx: ctx, cancel: cancel}
}

type authenticatedDirectoryState struct {
	Version  uint8
	Anchor   ReplayAnchor
	Segments uint64
}

func authenticatedDirectoryScope(identity [32]byte) [32]byte {
	return sha256.Sum256(append([]byte("halro:ledger:authenticated-directory:scope:v1\x00"), identity[:]...))
}
func authenticatedDirectoryCut(scope [32]byte, anchor ReplayAnchor) [32]byte {
	h := sha256.New()
	h.Write([]byte("halro:ledger:authenticated-directory:cut:v1\x00"))
	h.Write(scope[:])
	h.Write(anchor.Hash[:])
	var b [8]byte
	for _, n := range []uint64{anchor.Head.Generation, anchor.Head.Sequence, uint64(anchor.Head.Offset), uint64(anchor.Epoch)} {
		binary.BigEndian.PutUint64(b[:], n)
		h.Write(b[:])
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}
func authenticatedDirectoryPosition(anchor ReplayAnchor) authenticatedstate.SealPosition {
	return authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Epoch: uint64(anchor.Epoch), Generation: anchor.Head.Generation, Sequence: anchor.Head.Sequence, Offset: uint64(anchor.Head.Offset), Hash: anchor.Hash}
}
func authenticatedDirectoryKey(generation uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte{1}, generation)
}
func directoryAnchorFromPosition(position authenticatedstate.SealPosition) (ReplayAnchor, error) {
	if position.Component != authenticatedstate.SealComponentLedger || position.Generation == 0 || position.Offset > math.MaxInt64 || position.Epoch > frameVersionRunAttribution || position.Revision != 0 || position.MetadataBaseline != ([32]byte{}) || (position.Hash != ([32]byte{})) != authenticatedFrameEpoch(uint8(position.Epoch)) {
		return ReplayAnchor{}, ErrProjectionSource
	}
	return ReplayAnchor{Head: Watermark{Generation: position.Generation, Sequence: position.Sequence, Offset: int64(position.Offset)}, Hash: position.Hash, Epoch: uint8(position.Epoch)}, nil
}

// BuildAuthenticatedDirectory commits every sealed descriptor and the active
// physical anchor as one complete ordered namespace. It freezes this exact
// native writer under maintenance and reauthenticates all native history before
// constructing pages. It grants neither HA confirmation nor global publication.
// The first build still reads O(history); no historical slice or row map exists.
func BuildAuthenticatedDirectory(ctx context.Context, native *Log, lease *MaintenanceLease, owner *AuthenticatedLedgerProjection, output string, o AuthenticatedDirectoryOptions) (*AuthenticatedDirectory, error) {
	return buildAuthenticatedDirectory(ctx, native, lease, owner, output, o, false)
}

func buildAuthenticatedDirectory(ctx context.Context, native *Log, lease *MaintenanceLease, owner *AuthenticatedLedgerProjection, output string, o AuthenticatedDirectoryOptions, haReadOnly bool) (result *AuthenticatedDirectory, resultErr error) {
	if native == nil || lease == nil || owner == nil || o.Identity == ([32]byte{}) || o.Budget == nil || output == "" {
		return nil, ErrProjectionSource
	}
	if _, err := lease.ValidateOwner(native); err != nil {
		return nil, err
	}
	if err := owner.ValidateNativeSource(ctx, native, lease); err != nil {
		return nil, err
	}
	if err := owner.ValidateResourceOwner(ctx, o.Budget); err != nil {
		return nil, err
	}
	freeze := lease.WithHead
	if haReadOnly {
		if !lease.readOnly || !native.options.Replica {
			return nil, ErrProjectionSource
		}
		freeze = lease.ReadFrozenHead
	}
	resultErr = freeze(ctx, func(ctx context.Context, head Watermark, hash [32]byte) (jobErr error) {
		workspace, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
		if err != nil {
			return err
		}
		defer func() { jobErr = errors.Join(jobErr, workspace.Release()) }()
		native.mu.Lock()
		defer native.mu.Unlock()
		anchor := ReplayAnchor{Head: head, Hash: hash, Epoch: native.chainEpoch}
		ledgerRef, err := owner.SealReference(ctx)
		if err != nil {
			return err
		}
		if ledgerRef.Scope != authenticatedLedgerScope(o.Identity) {
			return ErrProjectionSource
		}
		if ledgerRef.Anchor != anchor || native.file == nil || native.segmentDirectory == nil || native.segmentDirectory.parsed.count != head.Generation-1 {
			return ErrProjectionSource
		}
		actual, err := native.replayAuthenticatedLocked(ctx, ReplayAnchor{}, 0, nil, nil)
		if err != nil || actual != head {
			return errors.Join(ErrProjectionSource, err)
		}
		scope := authenticatedDirectoryScope(o.Identity)
		key, err := hkdf.Key(sha256.New, native.chainKey, scope[:], "halro:ledger:authenticated-directory:hmac:v1", 32)
		if err != nil {
			return err
		}
		defer clear(key)
		state := authenticatedDirectoryState{Version: 1, Anchor: anchor, Segments: head.Generation - 1}
		var ordinal uint64
		store, err := authenticatedstate.CreateStore(ctx, output, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: authenticatedDirectoryCut(scope, anchor), Key: key, Budget: o.Budget, MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes}, func() (authenticatedstate.Row, bool, error) {
			if err := ctx.Err(); err != nil {
				return authenticatedstate.Row{}, false, err
			}
			if ordinal == 0 {
				ordinal++
				raw, err := json.Marshal(state)
				return authenticatedstate.Row{Key: []byte{0}, Value: raw}, true, err
			}
			if ordinal > state.Segments {
				return authenticatedstate.Row{}, false, nil
			}
			segment, found, err := native.lookupSegmentLocked(ordinal)
			if err != nil || !found {
				return authenticatedstate.Row{}, false, errors.Join(ErrSegmentMissing, err)
			}
			raw, err := json.Marshal(segment)
			if err != nil || len(raw) > replicationManifestValueLimit {
				return authenticatedstate.Row{}, false, errors.Join(authenticatedstate.ErrLimit, err)
			}
			row := authenticatedstate.Row{Key: authenticatedDirectoryKey(ordinal), Value: raw}
			ordinal++
			return row, true, nil
		})
		if store != nil {
			result = newAuthenticatedDirectory(store, anchor, state.Segments, o)
		}
		if err != nil {
			return err
		}
		finalRef, err := owner.SealReference(ctx)
		if err != nil || finalRef != ledgerRef {
			return errors.Join(ErrProjectionSource, err)
		}
		if err := errors.Join(native.segmentDirectory.manifest.check(), ctx.Err()); err != nil {
			return err
		}
		return nil
	})
	if resultErr != nil && result != nil {
		cleanup := result.Discard()
		resultErr = errors.Join(resultErr, cleanup)
		if cleanup == nil {
			result = nil
		}
	}
	return result, resultErr
}

// OpenAuthenticatedDirectory reopens only a complete independently selected
// state root, after proving its physical anchor in this native source. The
// older logical directory excludes newer generations even if native is ahead.
// Native Open and the source proof still scan history; this is no incremental
// startup or permission to replace the authoritative JSON manifest.
func OpenAuthenticatedDirectory(ctx context.Context, native *Log, lease *MaintenanceLease, selected AuthenticatedDirectoryReference, output string, o AuthenticatedDirectoryOptions) (*AuthenticatedDirectory, error) {
	return openAuthenticatedDirectory(ctx, native, lease, selected, output, o, false)
}

func openAuthenticatedDirectory(ctx context.Context, native *Log, lease *MaintenanceLease, selected AuthenticatedDirectoryReference, output string, o AuthenticatedDirectoryOptions, haReadOnly bool) (result *AuthenticatedDirectory, resultErr error) {
	if native == nil || lease == nil || o.Identity == ([32]byte{}) || o.Budget == nil || selected.Schema != authenticatedDirectorySchema {
		return nil, ErrProjectionSource
	}
	anchor, err := directoryAnchorFromPosition(selected.Position)
	if err != nil {
		return nil, err
	}
	if _, err := lease.ValidateOwner(native); err != nil {
		return nil, err
	}
	freeze := lease.WithHead
	if haReadOnly {
		if !lease.readOnly || !native.options.Replica {
			return nil, ErrProjectionSource
		}
		freeze = lease.ReadFrozenHead
	}
	resultErr = freeze(ctx, func(ctx context.Context, head Watermark, hash [32]byte) (jobErr error) {
		workspace, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
		if err != nil {
			return err
		}
		defer func() { jobErr = errors.Join(jobErr, workspace.Release()) }()
		native.mu.Lock()
		defer native.mu.Unlock()
		if native.file == nil || native.segmentDirectory == nil {
			return ErrProjectionSource
		}
		seen := false
		observe := func(actual ReplayAnchor) {
			if actual == anchor {
				seen = true
			}
		}
		actual, err := native.replayAuthenticatedLocked(ctx, ReplayAnchor{}, 0, func(r Record) error {
			observe(ReplayAnchor{Head: Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash, Epoch: r.Epoch})
			return ctx.Err()
		}, func(r ReplayAnchor) error { observe(r); return ctx.Err() })
		if err != nil || actual != head || native.chainHash != hash || !seen {
			return errors.Join(ErrProjectionSource, err)
		}
		scope := authenticatedDirectoryScope(o.Identity)
		key, err := hkdf.Key(sha256.New, native.chainKey, scope[:], "halro:ledger:authenticated-directory:hmac:v1", 32)
		if err != nil {
			return err
		}
		defer clear(key)
		root, schema := selected.Root, selected.Schema
		store, err := authenticatedstate.OpenStore(ctx, output, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: authenticatedDirectoryCut(scope, anchor), Key: key, Budget: o.Budget, MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes, DiskReservation: o.DiskReservation, ExpectedSealRoot: &root, ExpectedSealSchema: &schema})
		if err != nil {
			return err
		}
		result = newAuthenticatedDirectory(store, anchor, anchor.Head.Generation-1, o)
		pin, err := store.Snapshot(ctx)
		if err != nil {
			return err
		}
		raw, found, readErr := pin.Get(ctx, []byte{0})
		err = errors.Join(readErr, pin.Close())
		if err != nil || !found {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		var state authenticatedDirectoryState
		if err := json.Unmarshal(raw, &state); err != nil || state.Version != 1 || state.Anchor != anchor || state.Segments != result.segments || root.Rows != state.Segments+1 {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		return ctx.Err()
	})
	if resultErr != nil && result != nil {
		resultErr = errors.Join(resultErr, result.Close())
		result = nil
	}
	return result, resultErr
}
func (d *AuthenticatedDirectory) SealReference(ctx context.Context) (AuthenticatedDirectoryReference, error) {
	if d == nil || d.store == nil {
		return AuthenticatedDirectoryReference{}, ErrProjectionUnavailable
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sealReferenceLocked(ctx)
}
func (d *AuthenticatedDirectory) sealReferenceLocked(ctx context.Context) (AuthenticatedDirectoryReference, error) {
	if d.selectedNative != nil {
		if err := d.selectedNative.validateSelectedStartupGrant(ctx, true); err != nil {
			return AuthenticatedDirectoryReference{}, err
		}
	}
	if err := errors.Join(ctx.Err(), d.closeCtx.Err(), d.sealErr); err != nil {
		return AuthenticatedDirectoryReference{}, err
	}
	root, err := d.store.SealRootReference(ctx, authenticatedDirectorySchema)
	if err != nil {
		return AuthenticatedDirectoryReference{}, err
	}
	if root.Rows != d.segments+1 {
		return AuthenticatedDirectoryReference{}, ErrProjectionCorrupt
	}
	return AuthenticatedDirectoryReference{Schema: authenticatedDirectorySchema, Root: root, Position: authenticatedDirectoryPosition(d.anchor)}, nil
}
func (d *AuthenticatedDirectory) ReadSegment(ctx context.Context, generation uint64) (Segment, bool, error) {
	if d == nil || d.store == nil {
		return Segment{}, false, ErrProjectionUnavailable
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.selectedNative != nil {
		if err := d.selectedNative.ValidateSelectedStartupRead(ctx); err != nil {
			return Segment{}, false, err
		}
	}
	if err := errors.Join(ctx.Err(), d.closeCtx.Err(), d.sealErr); err != nil {
		return Segment{}, false, err
	}
	pin, err := d.store.Snapshot(ctx)
	if err != nil {
		return Segment{}, false, err
	}
	raw, found, readErr := pin.Get(ctx, authenticatedDirectoryKey(generation))
	err = errors.Join(readErr, pin.Close())
	if d.selectedNative != nil {
		err = errors.Join(err, d.selectedNative.ValidateSelectedStartupRead(ctx))
	}
	if err != nil {
		return Segment{}, false, err
	}
	if generation == 0 || generation > d.segments {
		if found {
			return Segment{}, false, ErrProjectionCorrupt
		}
		return Segment{}, false, nil
	}
	if !found {
		return Segment{}, false, ErrProjectionCorrupt
	}
	var segment Segment
	if err := json.Unmarshal(raw, &segment); err != nil || segment.Generation != generation {
		return Segment{}, false, errors.Join(ErrProjectionCorrupt, err)
	}
	return segment, true, ctx.Err()
}
func (d *AuthenticatedDirectory) Close() error {
	if d == nil || d.store == nil {
		return nil
	}
	d.cancel()
	d.mu.Lock()
	if d.closeDone != nil {
		done := d.closeDone
		d.mu.Unlock()
		<-done
		d.mu.Lock()
		err := d.closeResult
		d.mu.Unlock()
		return err
	}
	d.closeDone = make(chan struct{})
	var preparationErr error
	if d.physical != nil && d.physical.work != nil {
		preparationErr = resourcebudget.WithCompletionWorkspace(d.physical.work.Bind(context.Background()), d.closePhysicalCatchupLocked)
	} else {
		preparationErr = d.closePhysicalCatchupLocked(context.Background())
	}
	if preparationErr != nil && d.physical != nil {
		preparationErr = errors.Join(preparationErr, d.store.HoldPhysicalPreparationForRecovery(context.Background()))
	}
	result := errors.Join(preparationErr, d.store.Close(), d.sealErr)
	if result != nil && d.physical != nil {
		d.physical.native.mu.Lock()
		d.physical.closedQualified = false
		d.physical.native.mu.Unlock()
	}
	var work *resourcebudget.CompletableWork
	if result == nil && d.physical != nil {
		work = d.physical.work
	}
	d.mu.Unlock()
	// Cancellation has stopped new loans. Joining their pool must occur outside
	// Directory.mu so a canceled callback can take that mutex and actually exit.
	if work != nil {
		result = errors.Join(result, work.Close())
	}
	if result == nil && work != nil {
		result = work.Active.Release()
	}
	d.mu.Lock()
	if result != nil && d.physical != nil {
		d.physical.native.mu.Lock()
		d.physical.closedQualified = false
		d.physical.native.mu.Unlock()
	}
	if result == nil && d.physical != nil {
		d.physical.work = nil
	}
	d.closeResult = result
	close(d.closeDone)
	d.mu.Unlock()
	return result
}

func (d *AuthenticatedDirectory) Discard() error {
	if d == nil || d.store == nil {
		return nil
	}
	if err := d.Close(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.store.Discard()
}
func (d *AuthenticatedDirectory) DiskReservation() (resourcebudget.Permit, error) {
	if d == nil || d.store == nil {
		return resourcebudget.Permit{}, ErrProjectionUnavailable
	}
	return d.store.DiskReservation()
}
func (d *AuthenticatedDirectory) AdoptClosedSuccessor(next *AuthenticatedDirectory) error {
	if d == nil || next == nil || d.store == nil || next.store == nil {
		return ErrProjectionUnavailable
	}
	return d.store.AdoptClosedSuccessor(next.store)
}
