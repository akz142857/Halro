package governance

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const TypedGovernanceSealCodec uint16 = 2

type TypedGovernanceReference struct {
	Projection AuthenticatedGovernanceProjectionReference
	Journal    GovernanceSealReference
}

func typedGovernanceSchema() authenticatedstate.SealRootSchema {
	return authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentGovernance, IndexID: AuthenticatedGovernanceProjectionSealIndexID, Codec: TypedGovernanceSealCodec}
}

func typedGovernanceScope(identity [32]byte) [32]byte {
	return sha256.Sum256(append([]byte("halro:governance:typed-rows:codec2\x00"), identity[:]...))
}

func typedGovernanceCut(head Summary) [32]byte {
	b := []byte("halro:governance:typed-native-cut:v2\x00")
	b = binary.BigEndian.AppendUint64(b, head.Records)
	b = binary.BigEndian.AppendUint64(b, uint64(head.Bytes))
	b = append(b, head.LastHash[:]...)
	return sha256.Sum256(b)
}

// BuildTypedGovernanceProjection reduces complete authenticated native facts
// directly into a private COW candidate. It uses fixed source/live/update RAM,
// no bbolt scratch, full image, mmap, or historical in-memory index. Legacy
// ProjectionPath/ProjectionDiskReservation options are not consumed or removed.
// Caller supplies directory exclusion and independent identity/key/role scope.
func BuildTypedGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions) (*GovernanceProjection, TypedGovernanceReference, error) {
	return buildTypedGovernanceProjection(ctx, o, nil)
}

func buildTypedGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions, phase func(string)) (result *GovernanceProjection, reference TypedGovernanceReference, resultErr error) {
	if o.Native == nil || o.Identity == ([32]byte{}) || len(o.RootKey) != 32 || o.Budget == nil || o.RootMaxDiskBytes <= 0 || o.RootMaxAppendBytes <= 0 {
		return nil, reference, authenticatedstate.ErrRecovery
	}
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: governanceSealWorkspaceRAMBytes})
	if err != nil {
		return nil, reference, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, work.Release())
		}
		if resultErr != nil {
			reference = TypedGovernanceReference{}
		}
	}()
	p := &GovernanceProjection{cowNative: o.Native, cowBudget: o.Budget, cowViews: make(chan struct{}, 1)}
	defer func() {
		if resultErr != nil {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				p.retirementErr = errors.Join(p.retirementErr, resultErr)
			}
			if cleanup := p.Discard(); cleanup != nil {
				result = p
				resultErr = errors.Join(resultErr, cleanup)
			} else {
				result = nil
			}
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
	position, err := json.Marshal(Summary{})
	if err != nil {
		return nil, reference, err
	}
	positionKey, err := typedGovernanceKey("meta", []byte("position"))
	if err != nil {
		return nil, reference, err
	}
	ordinal := 0
	p.cow, err = authenticatedstate.CreateStore(ctx, o.RootDirectory, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: typedGovernanceCut(Summary{}), Key: rootKey, MaxDiskBytes: o.RootMaxDiskBytes, MaxAppendBytes: o.RootMaxAppendBytes, Budget: o.Budget}, func() (authenticatedstate.Row, bool, error) {
		if err := ctx.Err(); err != nil {
			return authenticatedstate.Row{}, false, err
		}
		ordinal++
		switch ordinal {
		case 1:
			return authenticatedstate.Row{Key: []byte{0}, Value: bytes.Clone(typedGovernanceMarker)}, true, nil
		case 2:
			return authenticatedstate.Row{Key: positionKey, Value: position}, true, nil
		default:
			return authenticatedstate.Row{}, false, nil
		}
	})
	if err != nil {
		return nil, reference, err
	}
	batch := make([]Record, 0, typedGovernanceBatchRows)
	defer clear(batch[:cap(batch)])
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := p.applyRecords(ctx, batch); err != nil {
			return err
		}
		clear(batch)
		batch = batch[:0]
		batchBytes = 0
		if phase != nil {
			phase("batch-applied")
		}
		return ctx.Err()
	}
	verified, partial, err = scanFrom(governanceContextReader{ctx: ctx, source: io.NewSectionReader(source, 0, head.Bytes)}, native.key, Summary{}, func(r Record) error {
		raw, err := json.Marshal(r)
		if err != nil || len(raw) > governanceProjectionBatchBytes {
			return errors.Join(ErrOutcomePageLimit, err)
		}
		if len(batch) == typedGovernanceBatchRows || len(raw) > governanceProjectionBatchBytes-batchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, r)
		batchBytes += len(raw)
		return ctx.Err()
	})
	if err != nil || partial || verified != head {
		return nil, reference, errors.Join(ErrGovernanceSealSource, err)
	}
	if err := flush(); err != nil {
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

// OpenTypedGovernanceProjection accepts only an independently selected typed
// root/native pair. It opens the authenticated pack directly, with fixed live
// workspace, and no temporary image. Native prefix qualification still scans
// history; this entry point alone is not incremental native Open or readiness.
func OpenTypedGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions, selected TypedGovernanceReference, reservation resourcebudget.Permit) (result *GovernanceProjection, resultErr error) {
	schema := typedGovernanceSchema()
	ref := selected.Projection
	if o.Native == nil || o.Identity == ([32]byte{}) || len(o.RootKey) != 32 || o.Budget == nil || ref.Schema != schema || ref.Scope != typedGovernanceScope(o.Identity) || ref.Position != selected.Journal.Position || ref.Root.Component != schema.Component || ref.Root.IndexID != schema.IndexID || ref.Root.Codec != schema.Codec || ref.Position.Offset > math.MaxInt64 {
		return nil, authenticatedstate.ErrRecovery
	}
	head := Summary{Records: ref.Position.Sequence, Bytes: int64(ref.Position.Offset), LastHash: ref.Position.Hash}
	journal, err := o.Native.SealReference(ctx, o.Budget, head)
	if err != nil || journal != selected.Journal {
		return nil, errors.Join(authenticatedstate.ErrRecovery, err)
	}
	key, err := hkdf.Key(sha256.New, o.RootKey, ref.Scope[:], "halro:governance:typed-content:v2", 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	p := &GovernanceProjection{cowNative: o.Native, cowBudget: o.Budget, cowViews: make(chan struct{}, 1)}
	defer func() {
		if resultErr != nil {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				p.retirementErr = resultErr
			}
			resultErr = errors.Join(resultErr, p.Close())
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				p.retirementErr = resultErr
				result = p
			}
		}
	}()
	p.liveRAM, err = o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: governanceSealWorkspaceRAMBytes})
	if err != nil {
		return nil, err
	}
	p.cow, err = authenticatedstate.OpenStore(ctx, o.RootDirectory, authenticatedstate.StoreOptions{Scope: ref.Scope, ExpectedCut: typedGovernanceCut(head), Key: key, MaxDiskBytes: o.RootMaxDiskBytes, MaxAppendBytes: o.RootMaxAppendBytes, Budget: o.Budget, DiskReservation: reservation, ExpectedSealRoot: &ref.Root, ExpectedSealSchema: &schema})
	if err != nil {
		return nil, err
	}
	p.ready = true
	if err := p.viewRows(ctx, func(_ any, actual Summary) error {
		if actual != head {
			return ErrCorrupt
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *GovernanceProjection) applyTypedNative(ctx context.Context, incoming Record) error {
	if p.cowNative == nil || p.cowBudget == nil {
		return authenticatedstate.ErrRecovery
	}
	native := p.cowNative
	native.mu.Lock()
	defer native.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired.Load() || !p.ready || native.file == nil || native.closing || native.terminalErr != nil {
		return ErrUnavailable
	}
	var from Summary
	if err := p.viewRows(ctx, func(_ any, head Summary) error { from = head; return nil }); err != nil {
		return err
	}
	full := Summary{Records: native.sequence, Bytes: native.offset, LastHash: native.lastHash}
	if incoming.Sequence != from.Records+1 || incoming.Offset <= from.Bytes || incoming.Offset-from.Bytes > maxPayloadSize+frameHeaderSize+frameMACSize || incoming.Offset > full.Bytes || incoming.Sequence > full.Records {
		return ErrCorrupt
	}
	if err := native.verifyHeadLocked(ctx, full); err != nil {
		return err
	}
	named, err := os.Lstat(native.path)
	if err != nil || !named.Mode().IsRegular() {
		return errors.Join(ErrCorrupt, err)
	}
	var actual Record
	count := 0
	to, partial, err := scanFrom(governanceContextReader{ctx: ctx, source: io.NewSectionReader(native.file, from.Bytes, incoming.Offset-from.Bytes)}, native.key, from, func(r Record) error {
		count++
		actual = r
		return ctx.Err()
	})
	if err != nil || partial || count != 1 || to != (Summary{Records: incoming.Sequence, Bytes: incoming.Offset, LastHash: incoming.Hash}) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		p.ready = false
		native.terminalErr = errors.Join(ErrCorrupt, err)
		native.startupFault.Store(true)
		return errors.Join(ErrCorrupt, err)
	}
	a, err := json.Marshal(actual)
	b, otherErr := json.Marshal(incoming)
	if err != nil || otherErr != nil || !bytes.Equal(a, b) {
		return errors.Join(ErrCorrupt, err, otherErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.applyRecords(ctx, []Record{actual})
}

func (p *GovernanceProjection) TypedSealReference(ctx context.Context) (TypedGovernanceReference, error) {
	if p == nil || !p.mu.TryRLock() {
		return TypedGovernanceReference{}, authenticatedstate.ErrBusy
	}
	defer p.mu.RUnlock()
	return p.typedSealReferenceLocked(ctx)
}
func (p *GovernanceProjection) typedSealReferenceLocked(ctx context.Context) (result TypedGovernanceReference, resultErr error) {
	if p.selectedStartup {
		if err := p.cowNative.validateSelectedGrant(ctx, true); err != nil {
			return result, err
		}
	}
	if p.retired.Load() || !p.ready || p.cow == nil || p.cowNative == nil || p.cowBudget == nil {
		return result, ErrUnavailable
	}
	var head Summary
	if err := p.viewRows(ctx, func(_ any, actual Summary) error { head = actual; return nil }); err != nil {
		return result, err
	}
	journal, err := p.cowNative.SealReference(ctx, p.cowBudget, head)
	if err != nil {
		return result, err
	}
	root, err := p.cow.SealRootReference(ctx, typedGovernanceSchema())
	if err != nil {
		return result, err
	}
	identity, err := p.cow.RootIdentity(ctx)
	if err != nil || identity.Cut != typedGovernanceCut(head) {
		return result, errors.Join(ErrCorrupt, err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return TypedGovernanceReference{Journal: journal, Projection: AuthenticatedGovernanceProjectionReference{Schema: typedGovernanceSchema(), Root: root, Scope: identity.Scope, Position: journal.Position}}, nil
}

// TypedDiskReservation retains the creator's real persistent fee after Close.
// It grants no native prefix or role permission and no deletion to a borrower.
func (p *GovernanceProjection) TypedDiskReservation() (resourcebudget.Permit, error) {
	if p == nil || p.cow == nil {
		return resourcebudget.Permit{}, authenticatedstate.ErrRecovery
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.cowClosed || p.closeErr != nil {
		return resourcebudget.Permit{}, authenticatedstate.ErrBusy
	}
	return p.cow.DiskReservation()
}

// AdoptClosedTypedSuccessor transfers only actual closed COW receipts to the
// original creator. Runtime role/common-cut permission is independently held
// by the caller, not inferred from this physical cleanup operation.
func (p *GovernanceProjection) AdoptClosedTypedSuccessor(next *GovernanceProjection) error {
	if p == nil || next == nil || p == next || p.cow == nil || next.cow == nil || p.cowBudget != next.cowBudget || !p.mu.TryLock() {
		return authenticatedstate.ErrRecovery
	}
	defer p.mu.Unlock()
	if !next.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer next.mu.Unlock()
	if !p.cowClosed || !next.cowClosed || p.closeErr != nil || next.closeErr != nil || p.retirementErr != nil || next.retirementErr != nil {
		return authenticatedstate.ErrRecovery
	}
	return p.cow.AdoptClosedSuccessor(next.cow)
}
