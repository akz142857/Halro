package usage

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const TypedUsageSealCodec uint16 = 2

type TypedUsageReference struct {
	Projection AuthenticatedUsageSealReference
	Ledger     ledger.AuthenticatedLedgerSealReference
}

func typedUsageSchema() authenticatedstate.SealRootSchema {
	return authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentUsage, IndexID: AuthenticatedUsageSealIndexID, Codec: TypedUsageSealCodec}
}
func typedUsageScope(identity, ledgerScope [32]byte) [32]byte {
	b := append([]byte("halro:usage:typed-rows:codec2:reducer6\x00"), identity[:]...)
	b = append(b, ledgerScope[:]...)
	for _, name := range diskUsageBuckets {
		b = binary.BigEndian.AppendUint16(b, uint16(len(name)))
		b = append(b, name...)
	}
	return sha256.Sum256(b)
}
func usageStateAnchor(s diskUsageState) ledger.ReplayAnchor {
	return ledger.ReplayAnchor{Head: s.Watermark, Hash: s.ReplayHash, Epoch: s.ReplayEpoch}
}
func typedUsageCut(a ledger.ReplayAnchor) [32]byte {
	b := []byte("halro:usage:typed-native-cut:v2\x00")
	b = binary.BigEndian.AppendUint64(b, a.Head.Generation)
	b = binary.BigEndian.AppendUint64(b, a.Head.Sequence)
	b = binary.BigEndian.AppendUint64(b, uint64(a.Head.Offset))
	b = append(b, a.Hash[:]...)
	b = append(b, a.Epoch)
	return sha256.Sum256(b)
}
func typedUsageRecordAnchor(r ledger.Record) ledger.ReplayAnchor {
	return ledger.ReplayAnchor{Head: ledger.Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}, Hash: r.Hash, Epoch: r.Epoch}
}
func newTypedUsageOwner(store *authenticatedstate.Store, native *ledger.Log, lease *ledger.MaintenanceLease, owner *ledger.AuthenticatedLedgerProjection, budget *resourcebudget.Manager, identity [32]byte) (*DiskProjection, error) {
	physicalCtx, physicalCancel := context.WithCancel(context.Background())
	p := &DiskProjection{physicalCtx: physicalCtx, physicalCancel: physicalCancel, reader: owner, readers: make(chan struct{}, 2), summaries: make(chan struct{}, 1), engine: &usageEngine{viewLane: make(chan struct{}, 1), cow: store, native: native, lease: lease, owner: owner, budget: budget, identity: identity}}
	if _, err := rand.Read(p.generation[:]); err != nil {
		return p, err
	}
	if _, err := rand.Read(p.cursorKey[:]); err != nil {
		return p, err
	}
	return p, nil
}

// BuildTypedUsageProjection executes the same reducer directly on authenticated
// rows. It never creates/imports an ordinary DB or consumes the legacy scratch
// reservation. The maintenance lease excludes native mutations, but grants no
// Runtime role/common-cut permission. Full native authentication is still O(N).
func BuildTypedUsageProjection(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease) (p *DiskProjection, reference TypedUsageReference, resultErr error) {
	if o.Native == nil || o.Owner == nil || o.Identity == ([32]byte{}) || len(o.UsageKey) != 32 || o.Budget == nil || o.RootDirectory == "" || o.RootMaxDiskBytes <= 0 || o.RootMaxAppendBytes <= 0 {
		return nil, reference, authenticatedstate.ErrRecovery
	}
	if err := o.Owner.ValidateNativeSource(ctx, o.Native, lease); err != nil {
		return nil, reference, err
	}
	if err := o.Owner.ValidateResourceOwner(ctx, o.Budget); err != nil {
		return nil, reference, err
	}
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes + ledger.AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return nil, reference, err
	}
	live, err := o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{RAM: TypedProjectionResidentRAMBytes})
	if err != nil {
		return nil, reference, errors.Join(err, work.Release())
	}
	var candidate *DiskProjection
	defer func() {
		if resultErr != nil {
			reference = TypedUsageReference{}
			if candidate != nil {
				if errors.Is(resultErr, ownedfile.ErrRetirement) {
					candidate.closeErr = errors.Join(candidate.closeErr, resultErr)
				}
				resultErr = errors.Join(resultErr, candidate.Discard())
			}
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				if candidate == nil {
					candidate = &DiskProjection{}
				}
				candidate.retired.Store(true)
				candidate.closeErr = errors.Join(candidate.closeErr, resultErr)
				// Unknown native exit retains both claims and the actual reader debt in
				// the typed error/native owner. No repeated Close can erase this debt.
				candidate.liveRAM = live
				candidate.typedWork = work
				p = candidate
			} else {
				p = nil
				if candidate == nil {
					resultErr = errors.Join(resultErr, live.Release())
				}
				resultErr = errors.Join(resultErr, work.Release())
			}
		} else {
			resultErr = errors.Join(resultErr, work.Release())
		}
	}()
	resultErr = lease.ReadFrozenHead(ctx, func(ctx context.Context, frozen ledger.Watermark, hash [32]byte) error {
		ref, err := o.Owner.SealReference(ctx)
		if err != nil {
			return err
		}
		if ref.Anchor.Head.After(frozen) {
			return authenticatedstate.ErrRecovery
		}
		// Authenticate every source byte, including the excluded suffix, before IO
		// creates the candidate. The selected owner is independently qualified above.
		if err := verifyTypedUsageNative(ctx, o.Native, ref.Anchor, frozen, hash, nil); err != nil {
			return err
		}
		scope := typedUsageScope(o.Identity, ref.Scope)
		key, err := hkdf.Key(sha256.New, o.UsageKey, scope[:], "halro:usage:typed-content:v2", 32)
		if err != nil {
			return err
		}
		defer clear(key)
		genesis := []authenticatedstate.Row{{Key: []byte{0}, Value: []byte{byte(TypedUsageSealCodec)}}}
		for _, v := range []struct {
			key   string
			value any
		}{{"rollup_rows", uint64(0)}, {"state", diskUsageState{Version: diskUsageVersion}}} {
			k, _ := typedUsageKey("meta", []byte(v.key))
			raw, err := json.Marshal(v.value)
			if err != nil {
				return err
			}
			genesis = append(genesis, authenticatedstate.Row{Key: k, Value: raw})
		}
		ordinal := 0
		store, err := authenticatedstate.CreateStore(ctx, o.RootDirectory, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: typedUsageCut(ledger.ReplayAnchor{}), Key: key, MaxDiskBytes: o.RootMaxDiskBytes, MaxAppendBytes: o.RootMaxAppendBytes, Budget: o.Budget}, func() (authenticatedstate.Row, bool, error) {
			if ordinal == len(genesis) {
				return authenticatedstate.Row{}, false, nil
			}
			row := genesis[ordinal]
			ordinal++
			return row, true, nil
		})
		if store != nil {
			var initErr error
			candidate, initErr = newTypedUsageOwner(store, o.Native, lease, o.Owner, o.Budget, o.Identity)
			err = errors.Join(err, initErr)
			candidate.liveRAM = live
		}
		if err != nil {
			return err
		}
		// Replayed frames have passed the source's MAC/chain decoder. Each physical
		// frame is one finite broad-profile update, never a history-sized collection.
		if err := verifyTypedUsageNative(ctx, o.Native, ref.Anchor, frozen, hash, func(r ledger.Record) error {
			if r.Sequence > ref.Anchor.Head.Sequence {
				return ctx.Err()
			}
			return candidate.reduceTypedVerifiedRecordLocked(ctx, r)
		}); err != nil {
			return err
		}
		if err := verifyTypedUsageNative(ctx, o.Native, ref.Anchor, frozen, hash, nil); err != nil {
			return err
		}
		if err := candidate.alignTypedVerifiedBoundary(ctx, ref.Anchor); err != nil {
			return err
		}
		actual, err := candidate.ReadReplayAnchor(ctx)
		if err != nil || actual != ref.Anchor {
			return errors.Join(ErrDiskUsageCorrupt, err)
		}
		final, err := o.Owner.SealReference(ctx)
		if err != nil || final != ref {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		root, err := store.SealRootReference(ctx, typedUsageSchema())
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		reference = TypedUsageReference{Ledger: ref, Projection: AuthenticatedUsageSealReference{Schema: typedUsageSchema(), Root: root, Scope: scope, Position: authenticatedUsagePosition(ref)}}
		p = candidate
		return nil
	})
	return p, reference, resultErr
}

func verifyTypedUsageNative(ctx context.Context, native *ledger.Log, target ledger.ReplayAnchor, frozen ledger.Watermark, hash [32]byte, visit func(ledger.Record) error) error {
	seen := target == (ledger.ReplayAnchor{}) || target.Head == (ledger.Watermark{Generation: 1}) && target.Hash == ([32]byte{}) && target.Epoch == 0
	full, err := native.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, func(r ledger.Record) error {
		if typedUsageRecordAnchor(r) == target {
			seen = true
		}
		if visit != nil {
			return visit(r)
		}
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	actual, actualHash, valid := native.ChainHead()
	if !seen && target.Head.Offset == 0 {
		proved, err := native.AuthenticateAppliedAnchor(ctx, target.Head)
		if err != nil {
			return err
		}
		seen = proved == target
	}
	if !seen || full != frozen || !valid && (frozen.Sequence != 0 || hash != ([32]byte{})) || actual != frozen || actualHash != hash {
		return authenticatedstate.ErrRecovery
	}
	return ctx.Err()
}

func (p *DiskProjection) prepareTypedRecord(ctx context.Context, r ledger.Record) (item diskUsageReplayRecord, resultErr error) {
	if err := r.Event.Validate(); err != nil {
		return item, err
	}
	if r.Generation == 0 || r.Sequence == 0 || r.Offset <= 0 || r.Hash == ([32]byte{}) {
		return item, ErrDiskUsageCorrupt
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return item, err
	}
	if len(raw) > diskUsageMaxValue {
		return item, ErrDiskUsageLimit
	}
	item = diskUsageReplayRecord{record: r, fingerprint: sha256.Sum256(raw)}
	e := &item.evidence
	e.first, e.found, err = p.reader.ReadEventPosition(ctx, r.Event.EventID)
	if err != nil {
		return item, err
	}
	if r.Event.Kind < ledger.EventWorkUnitCreated {
		e.final, e.finalFound, err = p.reader.ReadFinalization(ctx, r.Event.RequestID)
		if err != nil {
			return item, err
		}
		if e.finalFound {
			if source, ok := p.reader.(LegacyRequestReader); ok {
				e.legacy, err = source.ReadLegacyRequest(ctx, r.Event.RequestID)
				if err != nil {
					return item, err
				}
			}
		}
		if err := e.compactFinalization(r); err != nil {
			return item, err
		}
	}
	if !e.found || e.first.Sequence == 0 || e.first.Sequence > r.Sequence || e.first.Generation == 0 || e.first.Offset <= 0 || e.first.Sequence == r.Sequence && e.first != typedUsageRecordAnchor(r).Head {
		return item, ErrDiskUsageCorrupt
	}
	return item, nil
}
func (p *DiskProjection) reduceTypedVerifiedRecordLocked(ctx context.Context, r ledger.Record) error {
	return p.reduceTypedVerifiedBatchLocked(ctx, []ledger.Record{r})
}

// typedReduceBatchRecords bounds one typed reduction transaction, so a batch
// stays within the store's per-update append bound.
const typedReduceBatchRecords = 8

func (p *DiskProjection) reduceTypedVerifiedBatchLocked(ctx context.Context, records []ledger.Record) error {
	if len(records) == 0 || len(records) > typedReduceBatchRecords {
		return ErrDiskUsageLimit
	}
	var prepared [typedReduceBatchRecords]diskUsageReplayRecord
	total := 0
	for i, r := range records {
		item, err := p.prepareTypedRecord(ctx, r)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		total += len(raw)
		if total > diskReplayBatchBytes {
			return ErrDiskUsageLimit
		}
		if i > 0 && r.Sequence <= records[i-1].Sequence {
			return ErrDiskUsageCorrupt
		}
		prepared[i] = item
	}
	var old diskUsageState
	if err := p.engine.scoped(ctx).View(func(tx *usageTx) error {
		var found bool
		var err error
		old, found, err = diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if !found {
			return errors.Join(ErrDiskUsageCorrupt, err)
		}
		return err
	}); err != nil {
		return err
	}
	previous := old.Watermark.Sequence
	for _, item := range prepared[:len(records)] {
		r := item.record
		if r.Sequence < old.Watermark.Sequence {
			continue
		}
		if r.Sequence == old.Watermark.Sequence {
			if typedUsageRecordAnchor(r) != usageStateAnchor(old) || item.fingerprint != old.LastRecord {
				return ErrDiskUsageCorrupt
			}
			continue
		}
		if r.Sequence != previous+1 {
			return ErrDiskUsageCorrupt
		}
		previous = r.Sequence
	}
	if previous == old.Watermark.Sequence {
		return ctx.Err()
	}
	last := typedUsageRecordAnchor(records[len(records)-1])
	applied := previous - old.Watermark.Sequence
	receipt, err := p.updatePhysicalUsageFrame(ctx, last, func(update *authenticatedstate.Transaction) error {
		tx := &usageTx{update: update, ctx: ctx}
		state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if err != nil || !found || state != old {
			return errors.Join(ErrDiskUsageCorrupt, err)
		}
		for _, item := range prepared[:len(records)] {
			if item.record.Sequence <= old.Watermark.Sequence {
				continue
			}
			if err := applyDiskUsageReplayRecord(ctx, tx, item, &state); err != nil {
				return err
			}
		}
		return errors.Join(tx.failure, ctx.Err())
	}, func() error { return ctx.Err() })
	if err == nil {
		p.pack.reduced(p.engine.cow, receipt.AppendedBytes, applied, previous)
	}
	return err
}

// The public typed Apply re-authenticates the actual complete source, matches
// the complete physical Record and rechecks the independently selected owner.
// A forged payload with a copied event ID/position/hash is never admitted.
func (p *DiskProjection) applyTypedRecordLocked(ctx context.Context, r ledger.Record) error {
	return p.applyTypedBatchLocked(ctx, []ledger.Record{r})
}
func (p *DiskProjection) applyTypedBatchLocked(ctx context.Context, records []ledger.Record) (resultErr error) {
	e := p.engine
	if e == nil || e.cow == nil || e.native == nil || e.owner == nil || e.lease == nil || e.budget == nil {
		return ErrDiskUsageClosed
	}
	if len(records) == 0 || len(records) > 8 {
		return ErrDiskUsageLimit
	}
	work, err := e.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: usageSealWorkspaceRAMBytes + ledger.AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return err
	}
	mutated := false
	defer func() {
		if resultErr != nil && (mutated || e.native.Status().Load() == ledger.AccountingRecoveryRequired || errors.Is(resultErr, ledger.ErrCorrupt) || errors.Is(resultErr, authenticatedstate.ErrCorrupt)) {
			p.retired.Store(true)
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			p.closeErr = errors.Join(p.closeErr, resultErr)
			p.retired.Store(true)
			p.typedWork = work
		} else {
			resultErr = errors.Join(resultErr, work.Release())
		}
	}()
	var bodies [8][]byte
	total := 0
	for i, r := range records {
		if !typedUsageRecordWidth(r) || i > 0 && r.Sequence <= records[i-1].Sequence {
			return ErrDiskUsageLimit
		}
		if err := r.Event.Validate(); err != nil {
			return err
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if len(raw) > diskUsageMaxValue || len(raw) > diskReplayBatchBytes-total {
			return ErrDiskUsageLimit
		}
		total += len(raw)
		bodies[i] = raw
	}
	if err := e.owner.ValidateNativeSource(ctx, e.native, e.lease); err != nil {
		return err
	}
	applyFrozen := func(ctx context.Context, frozen ledger.Watermark, hash [32]byte, accepted bool) error {
		ref, err := e.owner.SealReference(ctx)
		if err != nil {
			return err
		}
		live := records[len(records)-1].Sequence > ref.Anchor.Head.Sequence
		if live {
			if !accepted || len(records) != 1 || records[0].Sequence != ref.Anchor.Head.Sequence+1 || typedUsageRecordAnchor(records[0]).Head != frozen || records[0].Hash != hash {
				return authenticatedstate.ErrRecovery
			}
			var from ledger.ReplayAnchor
			if err := e.scoped(ctx).View(func(tx *usageTx) error {
				state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
				if err != nil || !found {
					return errors.Join(ErrDiskUsageCorrupt, err)
				}
				from = usageStateAnchor(state)
				return nil
			}); err != nil {
				return err
			}
			if from != ref.Anchor && from != typedUsageRecordAnchor(records[0]) {
				return authenticatedstate.ErrRecovery
			}
		}
		var matched [8]bool
		if err := verifyTypedUsageNative(ctx, e.native, ref.Anchor, frozen, hash, func(actual ledger.Record) error {
			for i, r := range records {
				if actual.Sequence == r.Sequence {
					body, err := json.Marshal(actual)
					if err != nil {
						return err
					}
					if !bytes.Equal(body, bodies[i]) {
						return ErrDiskUsageCorrupt
					}
					matched[i] = true
				}
			}
			return ctx.Err()
		}); err != nil {
			return err
		}
		for _, match := range matched[:len(records)] {
			if !match {
				return ErrDiskUsageCorrupt
			}
		}
		before, err := e.owner.SealReference(ctx)
		if err != nil || before != ref {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		// A gap/position error is a deterministic input rejection, discovered before
		// any candidate page is appended by the shared reducer.
		identity, err := e.cow.RootIdentity(ctx)
		if err != nil {
			return err
		}
		err = p.reduceTypedVerifiedBatchLocked(ctx, records)
		after, afterErr := e.cow.RootIdentity(ctx)
		mutated = afterErr != nil || after != identity
		if err != nil {
			return err
		}
		if err := verifyTypedUsageNative(ctx, e.native, ref.Anchor, frozen, hash, nil); err != nil {
			return err
		}
		final, err := e.owner.SealReference(ctx)
		if err != nil || final != ref {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		return ctx.Err()
	}
	found, err := e.lease.ReadAcceptedApplyHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		return applyFrozen(ctx, head, hash, true)
	})
	if found {
		return err
	}
	return e.lease.ReadFrozenHead(ctx, func(ctx context.Context, head ledger.Watermark, hash [32]byte) error {
		return applyFrozen(ctx, head, hash, false)
	})
}

// OpenTypedUsageProjection reads the selected native-row root directly. Both
// references must come from independent authority; this component cannot select
// a role, term, common cut or bless an arbitrary saved ordinary Usage DB.
func OpenTypedUsageProjection(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease, selected TypedUsageReference, diskReservation resourcebudget.Permit) (p *DiskProjection, resultErr error) {
	return openTypedUsageProjection(ctx, o, lease, selected, diskReservation, nil, false)
}

// OpenSelectedTypedUsageProjection restores the actual selected Usage root and
// reduces only its authenticated suffix to the actual Ledger owner. It requires
// private native selected-startup provenance and independently selects both roots.
func OpenSelectedTypedUsageProjection(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease, selected TypedUsageReference, diskReservation resourcebudget.Permit, sourceOptions ledger.SelectedLedgerProjectionOpenOptions) (*DiskProjection, error) {
	return openTypedUsageProjection(ctx, o, lease, selected, diskReservation, &sourceOptions, false)
}

func openTypedUsageProjection(ctx context.Context, o AuthenticatedUsageOptions, lease *ledger.MaintenanceLease, selected TypedUsageReference, diskReservation resourcebudget.Permit, sourceOptions *ledger.SelectedLedgerProjectionOpenOptions, ha bool) (p *DiskProjection, resultErr error) {
	schema := typedUsageSchema()
	if o.Native == nil || o.Owner == nil || o.Budget == nil || o.Identity == ([32]byte{}) || len(o.UsageKey) != 32 || o.RootDirectory == "" || o.RootMaxDiskBytes <= 0 || o.RootMaxAppendBytes <= 0 || selected.Projection.Schema != schema || selected.Projection.Root.Component != schema.Component || selected.Projection.Root.IndexID != schema.IndexID || selected.Projection.Root.Codec != schema.Codec || selected.Ledger.Scope == ([32]byte{}) || selected.Projection.Scope != typedUsageScope(o.Identity, selected.Ledger.Scope) || selected.Projection.Position != authenticatedUsagePosition(selected.Ledger) {
		return nil, authenticatedstate.ErrRecovery
	}
	if sourceOptions == nil {
		if err := o.Owner.ValidateNativeSource(ctx, o.Native, lease); err != nil {
			return nil, err
		}
	} else if _, err := lease.ValidateOwner(o.Native); err != nil {
		return nil, err
	}
	if err := o.Owner.ValidateResourceOwner(ctx, o.Budget); err != nil {
		return nil, err
	}
	var ref ledger.AuthenticatedLedgerSealReference
	var err error
	if sourceOptions == nil {
		ref, err = o.Owner.SealReference(ctx)
		if err != nil || ref != selected.Ledger {
			return nil, errors.Join(authenticatedstate.ErrRecovery, err)
		}
	}
	workRAM := int64(usageSealWorkspaceRAMBytes)
	if sourceOptions == nil {
		workRAM += ledger.AuthenticatedLedgerSourceRAMBytes
	}
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: workRAM})
	if err != nil {
		return nil, err
	}
	live, err := o.Budget.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{RAM: TypedProjectionResidentRAMBytes})
	if err != nil {
		return nil, errors.Join(err, work.Release())
	}
	var candidate *DiskProjection
	defer func() {
		if resultErr != nil {
			if candidate != nil {
				if errors.Is(resultErr, ownedfile.ErrRetirement) {
					candidate.closeErr = errors.Join(candidate.closeErr, resultErr)
				}
				resultErr = errors.Join(resultErr, candidate.Close())
			}
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				if candidate == nil {
					candidate = &DiskProjection{}
				}
				candidate.closeErr = errors.Join(candidate.closeErr, resultErr)
				candidate.retired.Store(true)
				candidate.liveRAM = live
				candidate.typedWork = work
				p = candidate
			} else {
				p = nil
				if candidate == nil {
					resultErr = errors.Join(resultErr, live.Release())
				}
				resultErr = errors.Join(resultErr, work.Release())
			}
		} else {
			resultErr = errors.Join(resultErr, work.Release())
		}
	}()
	var source ledger.SelectedStartupSource
	var physicalNoDebtAnchor *ledger.ReplayAnchor
	if sourceOptions != nil && o.PhysicalTerminalReserve {
		if proofErr := physicalUsageNoDebt(ctx, o.Owner); proofErr == nil {
			actual, readErr := o.Owner.ReadReplayAnchor(ctx)
			if readErr != nil {
				return nil, readErr
			}
			physicalNoDebtAnchor = &actual
		} else if !errors.Is(proofErr, authenticatedstate.ErrRecovery) || errors.Is(proofErr, ownedfile.ErrRetirement) || errors.Is(proofErr, authenticatedstate.ErrCorrupt) {
			return nil, proofErr
		}
	}
	verifySource := func(ctx context.Context, frozen ledger.Watermark, hash [32]byte) error {
		if source == nil {
			return verifyTypedUsageNative(ctx, o.Native, ref.Anchor, frozen, hash, nil)
		}
		return source.VerifyAnchors(ctx, [2]ledger.ReplayAnchor{selected.Ledger.Anchor, ref.Anchor})
	}
	open := func(ctx context.Context, frozen ledger.Watermark, hash [32]byte) error {
		if source != nil {
			ref, err = o.Owner.SealReference(ctx)
			if err != nil || ref.Scope != selected.Ledger.Scope {
				return errors.Join(authenticatedstate.ErrRecovery, err)
			}
		}
		if err := verifySource(ctx, frozen, hash); err != nil {
			return err
		}
		key, err := hkdf.Key(sha256.New, o.UsageKey, selected.Projection.Scope[:], "halro:usage:typed-content:v2", 32)
		if err != nil {
			return err
		}
		defer clear(key)
		root := selected.Projection.Root
		store, err := authenticatedstate.OpenStore(ctx, o.RootDirectory, authenticatedstate.StoreOptions{Scope: selected.Projection.Scope, ExpectedCut: typedUsageCut(selected.Ledger.Anchor), Key: key, MaxDiskBytes: o.RootMaxDiskBytes, MaxAppendBytes: o.RootMaxAppendBytes, Budget: o.Budget, DiskReservation: diskReservation, ExpectedSealRoot: &root, ExpectedSealSchema: &schema, PhysicalRecovery: sourceOptions != nil && o.PhysicalTerminalReserve})
		if store != nil {
			var initErr error
			candidate, initErr = newTypedUsageOwner(store, o.Native, lease, o.Owner, o.Budget, o.Identity)
			candidate.liveRAM = live
			candidate.engine.selectedStartup = source != nil
			candidate.engine.selectedHA = ha
			if sourceOptions != nil {
				candidate.physicalPublisher = sourceOptions.Publisher
			}
			err = errors.Join(err, initErr)
		}
		if err != nil {
			return err
		}
		expectedAnchor := selected.Ledger.Anchor
		if source != nil && o.PhysicalTerminalReserve {
			expectedAnchor, _, err = candidate.restorePhysicalUsageLocked(ctx, source, sourceOptions.Publisher, physicalNoDebtAnchor)
			if err != nil {
				return err
			}
		}
		actual, err := candidate.ReadReplayAnchor(ctx)
		if err != nil || actual != expectedAnchor {
			return errors.Join(ErrDiskUsageCorrupt, err)
		}
		if source != nil {
			_, err := source.ReplayAuthenticated(ctx, selected.Ledger.Anchor, func(r ledger.Record) error {
				if r.Sequence > ref.Anchor.Head.Sequence || r.Sequence <= expectedAnchor.Head.Sequence {
					return ctx.Err()
				}
				return candidate.reduceTypedVerifiedRecordLocked(ctx, r)
			})
			if err != nil {
				return err
			}
			if err := candidate.alignTypedVerifiedBoundary(ctx, ref.Anchor); err != nil {
				return err
			}
			actual, err := candidate.ReadReplayAnchor(ctx)
			if err != nil || actual != ref.Anchor {
				return errors.Join(ErrDiskUsageCorrupt, err)
			}
		}
		if err := verifySource(ctx, frozen, hash); err != nil {
			return err
		}
		final, err := o.Owner.SealReference(ctx)
		if err != nil || final != ref {
			return errors.Join(authenticatedstate.ErrRecovery, err)
		}
		p = candidate
		return ctx.Err()
	}
	if sourceOptions == nil {
		resultErr = lease.ReadFrozenHead(ctx, open)
	} else {
		loan := o.Native.WithSelectedStartupOwner
		if ha {
			loan = o.Native.WithHASelectedStartupOwner
			if o.PhysicalTerminalReserve {
				loan = func(ctx context.Context, budget *resourcebudget.Manager, options ledger.SelectedLedgerProjectionOpenOptions, identity [32]byte, visit func(context.Context, ledger.SelectedStartupSource) error) error {
					return o.Native.WithHASelectedPhysicalStartupOwner(ctx, budget, options, identity, func(ctx context.Context, actual ledger.SelectedHAPhysicalStartupSource) error {
						return visit(ctx, actual)
					})
				}
			}
		}
		resultErr = loan(ctx, o.Budget, *sourceOptions, o.Identity, func(ctx context.Context, actual ledger.SelectedStartupSource) error {
			seal := actual.SelectedSeal()
			var ledgerRoot, usageRoot authenticatedstate.SealRoot
			for _, root := range seal.Roots[:seal.RootCount] {
				if root.Component == authenticatedstate.SealComponentLedger && root.IndexID == ledger.AuthenticatedLedgerSealIndexID && root.Codec == ledger.AuthenticatedLedgerSealCodec {
					ledgerRoot = root
				}
				if root.Component == schema.Component && root.IndexID == schema.IndexID && root.Codec == schema.Codec {
					usageRoot = root
				}
			}
			if ledgerRoot != selected.Ledger.Root || usageRoot != selected.Projection.Root || actual.SavedAnchor() != selected.Ledger.Anchor {
				return authenticatedstate.ErrRecovery
			}
			source = actual
			head := actual.NativeHead()
			return open(ctx, head.Head, head.Hash)
		})
	}
	return p, resultErr
}

func (p *DiskProjection) TypedSealReference(ctx context.Context) (result TypedUsageReference, resultErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if p == nil || p.retired.Load() {
		return result, p.unavailableError("typed seal reference")
	}
	if !p.mu.TryRLock() {
		return result, ErrDiskUsageBusy
	}
	defer p.mu.RUnlock()
	return p.typedSealReferenceLocked(ctx)
}

func (p *DiskProjection) typedSealReferenceLocked(ctx context.Context) (result TypedUsageReference, resultErr error) {
	if p.retired.Load() || p.engine == nil || p.engine.cow == nil || p.engine.owner == nil {
		return result, p.unavailableError("typed seal reference")
	}
	var anchor ledger.ReplayAnchor
	err := p.engine.scoped(ctx).View(func(tx *usageTx) error {
		state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if err != nil || !found {
			return errors.Join(ErrDiskUsageCorrupt, err)
		}
		anchor = usageStateAnchor(state)
		return nil
	})
	if err != nil {
		return result, err
	}
	ref, err := p.engine.owner.SealReference(ctx)
	if err != nil || ref.Anchor != anchor {
		return result, errors.Join(authenticatedstate.ErrRecovery, err)
	}
	root, err := p.engine.cow.SealRootReference(ctx, typedUsageSchema())
	if err != nil {
		return result, err
	}
	identity, err := p.engine.cow.RootIdentity(ctx)
	if err != nil || identity.Cut != typedUsageCut(anchor) {
		return result, errors.Join(ErrDiskUsageCorrupt, err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return TypedUsageReference{Ledger: ref, Projection: AuthenticatedUsageSealReference{Schema: typedUsageSchema(), Root: root, Scope: identity.Scope, Position: authenticatedUsagePosition(ref)}}, nil
}

// A creator may retain its captured persistent disk obligation only after all
// actual IO has exited. A reopened borrower never acquires deletion authority.
func (p *DiskProjection) TypedDiskReservation() (resourcebudget.Permit, error) {
	if p == nil {
		return resourcebudget.Permit{}, ErrDiskUsageClosed
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.engine == nil || p.engine.cow == nil || !p.typedClosed || p.closeErr != nil {
		return resourcebudget.Permit{}, ErrDiskUsageClosed
	}
	return p.engine.cow.DiskReservation()
}

// AdoptClosedTypedSuccessor transfers only a borrowed successor's actual
// closed namespace receipt to its original creator. It gives no cut, role,
// archive or deletion permission to the borrowed handle itself.
func (p *DiskProjection) AdoptClosedTypedSuccessor(next *DiskProjection) error {
	if p == nil || next == nil || p == next || !p.mu.TryLock() {
		return authenticatedstate.ErrRecovery
	}
	defer p.mu.Unlock()
	if !next.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer next.mu.Unlock()
	if p.engine == nil || next.engine == nil || p.engine.cow == nil || next.engine.cow == nil || p.engine.budget != next.engine.budget || !p.typedClosed || !next.typedClosed || p.closeErr != nil || next.closeErr != nil || p.typedWork != (resourcebudget.Permit{}) || next.typedWork != (resourcebudget.Permit{}) || p.typedSealDebt != (resourcebudget.Permit{}) || next.typedSealDebt != (resourcebudget.Permit{}) {
		return authenticatedstate.ErrRecovery
	}
	return p.engine.cow.AdoptClosedSuccessor(next.engine.cow)
}

// Inspect the caller's typed graph before JSON allocation. Native frames already
// have a decoder bound; direct Apply must not let a forged wide graph allocate
// an unbounded encoder buffer before it can compare with those actual bytes.
func typedUsageRecordWidth(r ledger.Record) bool {
	nodes, strings := 0, 0
	var visit func(reflect.Value, int) bool
	visit = func(v reflect.Value, depth int) bool {
		nodes++
		if nodes > 8192 || depth > 12 {
			return false
		}
		if v.Type() == reflect.TypeFor[time.Time]() {
			return true
		}
		switch v.Kind() {
		case reflect.String:
			strings += v.Len()
			return strings <= 1<<20
		case reflect.Pointer:
			if v.IsNil() {
				return true
			}
			return visit(v.Elem(), depth+1)
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if !visit(v.Field(i), depth+1) {
					return false
				}
			}
		case reflect.Slice, reflect.Array:
			if v.Len() > 8192-nodes {
				return false
			}
			for i := 0; i < v.Len(); i++ {
				if !visit(v.Index(i), depth+1) {
					return false
				}
			}
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		default:
			return false
		}
		return true
	}
	return visit(reflect.ValueOf(r), 0)
}

// The caller has independently selected and authenticated this physical native
// roll/genesis boundary. It changes position only, never business rows or native
// facts. A hash/epoch/sequence change requires the shared event reducer instead.
func (p *DiskProjection) alignTypedVerifiedBoundary(ctx context.Context, target ledger.ReplayAnchor) error {
	var old diskUsageState
	if err := p.engine.scoped(ctx).View(func(tx *usageTx) error {
		var found bool
		var err error
		old, found, err = diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if err != nil || !found {
			return errors.Join(ErrDiskUsageCorrupt, err)
		}
		return nil
	}); err != nil {
		return err
	}
	if usageStateAnchor(old) == target {
		return ctx.Err()
	}
	if target.Head.Offset != 0 || target.Head.Sequence != old.Watermark.Sequence || target.Hash != old.ReplayHash || target.Epoch != old.ReplayEpoch || !target.Head.After(old.Watermark) {
		return ErrDiskUsageCorrupt
	}
	_, err := p.updatePhysicalUsageFrame(ctx, target, func(update *authenticatedstate.Transaction) error {
		tx := &usageTx{update: update, ctx: ctx}
		state, found, err := diskUsageGet[diskUsageState](tx, "meta", []byte("state"))
		if err != nil || !found || state != old {
			return errors.Join(ErrDiskUsageCorrupt, err)
		}
		state.Watermark = target.Head
		return errors.Join(diskUsagePut(tx, "meta", []byte("state"), state), tx.failure, ctx.Err())
	}, func() error { return ctx.Err() })
	return err
}
