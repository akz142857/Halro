package governance

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type SelectedTypedGovernanceOpenOptions struct {
	Publisher       *authenticatedstate.SealCandidatePublisher
	Guard           *lock.Lock
	Authority       authenticatedstate.SealEligibility
	Selected        TypedGovernanceReference
	DiskReservation resourcebudget.Permit
	Permitted       Summary
}
type TypedGovernanceReopen struct {
	From, To, NativeHead           Summary
	SourceVerified, AppliedRecords uint64
	Reference                      TypedGovernanceReference
}
type selectedGovernanceProvenance struct {
	ha        *haSelectedStartupProvenance
	publisher *authenticatedstate.SealCandidatePublisher
	guard     *lock.Lock
	seal      authenticatedstate.CandidateSeal
	identity  [32]byte
}

func (l *Log) validateSelectedGovernanceOwner(ctx context.Context, o AuthenticatedGovernanceOptions, s SelectedTypedGovernanceOpenOptions, ha bool) (authenticatedstate.CandidateSeal, error) {
	p := l.startupProvenance
	if p == nil || p.publisher != s.Publisher || p.guard != s.Guard || p.identity != o.Identity || o.Budget != l.cacheBudget || s.Publisher == nil || s.Guard == nil || !s.Guard.CoversNativePath(l.path) || !l.selectedProjectionMode(ctx, s.Authority, ha) || s.Authority.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return authenticatedstate.CandidateSeal{}, ErrStartupCache
	}
	if err := s.Publisher.ValidateOwner(s.Guard, o.Budget); err != nil {
		return authenticatedstate.CandidateSeal{}, err
	}
	seal, found, err := s.Publisher.Select(s.Authority)
	if err != nil || !found || seal != p.seal || ctx.Err() != nil {
		return authenticatedstate.CandidateSeal{}, errors.Join(ErrStartupCache, err, ctx.Err())
	}
	return seal, nil
}

// OpenSelectedTypedGovernanceProjection binds the actual selected native
// constructor, independent publisher and exact selected native/typed roots.
// It validates/reduces only the fixed suffix, preserving complete query data in
// the authenticated COW root. Business reads dynamically require the finite
// native audit grant; publication requires complete historical retirement.
func OpenSelectedTypedGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions, s SelectedTypedGovernanceOpenOptions) (result *GovernanceProjection, receipt TypedGovernanceReopen, resultErr error) {
	return openSelectedTypedGovernanceProjection(ctx, o, s, false)
}

func openSelectedTypedGovernanceProjection(ctx context.Context, o AuthenticatedGovernanceOptions, s SelectedTypedGovernanceOpenOptions, ha bool) (result *GovernanceProjection, receipt TypedGovernanceReopen, resultErr error) {
	if o.Native == nil || o.Budget == nil || o.Identity == ([32]byte{}) || len(o.RootKey) != 32 {
		return nil, receipt, ErrStartupCache
	}
	native := o.Native
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: governanceSealWorkspaceRAMBytes})
	if err != nil {
		return nil, receipt, err
	}
	var p *GovernanceProjection
	var source *os.File
	closeSource := (*os.File).Close
	var key []byte
	registered := false
	defer func() {
		if registered {
			defer native.readerWG.Done()
			defer func() { native.mu.Lock(); native.freezeCancel = nil; native.mu.Unlock() }()
		}
		if source != nil {
			resultErr = errors.Join(resultErr, native.retireReader(source, closeSource))
		}
		if resultErr != nil && p != nil {
			resultErr = errors.Join(resultErr, p.Close())
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				p.retirementErr = resultErr
				result = p
			}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			receipt = TypedGovernanceReopen{}
			resultErr = &selectedGovernanceSourceDebt{error: resultErr, work: work, source: source, projection: p, key: key}
			return
		}
		clear(key)
		resultErr = errors.Join(resultErr, work.Release())
		if resultErr != nil {
			receipt = TypedGovernanceReopen{}
			result = nil
		}
	}()
	seal, err := native.validateSelectedGovernanceOwner(ctx, o, s, ha)
	if err != nil {
		return nil, receipt, err
	}
	schema := typedGovernanceSchema()
	saved := native.startupSaved
	position := authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentGovernance, Sequence: saved.Records, Offset: uint64(saved.Bytes), Hash: saved.LastHash}
	var root, nativeRoot authenticatedstate.SealRoot
	for _, r := range seal.Roots[:seal.RootCount] {
		if r.Kind == schema.Kind && r.Component == schema.Component && r.IndexID == schema.IndexID && r.Codec == schema.Codec {
			root = r
		}
		if r.Kind == authenticatedstate.SealRootNative && r.Component == schema.Component && r.IndexID == AuthenticatedGovernanceSealIndexID && r.Codec == AuthenticatedGovernanceSealCodec {
			nativeRoot = r
		}
	}
	ref := s.Selected.Projection
	if ref.Schema != schema || ref.Scope != typedGovernanceScope(o.Identity) || ref.Root != root || ref.Position != position || s.Selected.Journal.Position != position || s.Selected.Journal.Root != nativeRoot || s.Selected.Journal.Schema != (authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootNative, Component: authenticatedstate.SealComponentGovernance, IndexID: AuthenticatedGovernanceSealIndexID, Codec: AuthenticatedGovernanceSealCodec}) || root.IndexID != schema.IndexID {
		return nil, receipt, ErrStartupCache
	}
	opCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	native.mu.Lock()
	if native.closing || native.file == nil || native.terminalErr != nil || native.freezeCancel != nil || !native.selectedProjectionMode(ctx, s.Authority, ha) {
		native.mu.Unlock()
		return nil, receipt, ErrStartupCache
	}
	head := Summary{Records: native.sequence, Bytes: native.offset, LastHash: native.lastHash}
	if s.Permitted.Records < saved.Records || s.Permitted.Bytes < saved.Bytes || s.Permitted.Records > head.Records || s.Permitted.Bytes > head.Bytes {
		native.mu.Unlock()
		return nil, receipt, ErrStartupCache
	}
	info, err := native.file.Stat()
	if err != nil {
		native.mu.Unlock()
		return nil, receipt, err
	}
	source, err = os.OpenFile(native.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		native.mu.Unlock()
		return nil, receipt, err
	}
	key = append([]byte(nil), native.key...)
	if native.closeReaderFile != nil {
		closeSource = native.closeReaderFile
	}
	native.freezeCancel = cancel
	native.readerWG.Add(1)
	native.mu.Unlock()
	registered = true
	check := func() error {
		if err := opCtx.Err(); err != nil {
			return err
		}
		actual, err := source.Stat()
		named, nameErr := os.Lstat(native.path)
		if err != nil || nameErr != nil || !named.Mode().IsRegular() || !os.SameFile(info, actual) || !os.SameFile(actual, named) || actual.Size() != head.Bytes || !actual.ModTime().Equal(info.ModTime()) {
			return errors.Join(ErrStartupCache, err, nameErr)
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, receipt, err
	}
	var targetLast Record
	var targetStart int64
	var targetLength uint32
	var firstHash [32]byte
	seen := s.Permitted == saved
	previous := saved
	verifyDelta := func(visit func(Record) error) (Summary, error) {
		actual, partial, err := scanFrom(startupContextSeeker{opCtx, io.NewSectionReader(source, saved.Bytes, head.Bytes-saved.Bytes)}, key, saved, func(r Record) error { return visit(r) })
		if err != nil || partial || actual != head {
			return Summary{}, errors.Join(ErrCorrupt, err)
		}
		return actual, check()
	}
	_, err = verifyDelta(func(r Record) error {
		receipt.SourceVerified++
		if r.Sequence == 1 {
			firstHash = r.Hash
		}
		current := Summary{Records: r.Sequence, Bytes: r.Offset, LastHash: r.Hash}
		if current == s.Permitted {
			seen = true
			targetLast = r
			targetStart = previous.Bytes
			targetLength = uint32(r.Offset - previous.Bytes)
		}
		previous = current
		return nil
	})
	if err != nil {
		native.mu.Lock()
		native.terminalErr = errors.Join(native.terminalErr, err)
		native.startupFault.Store(true)
		native.mu.Unlock()
		return nil, receipt, err
	}
	if !seen {
		return nil, receipt, ErrStartupCache
	}
	rootKey, err := hkdf.Key(sha256.New, o.RootKey, ref.Scope[:], "halro:governance:typed-content:v2", 32)
	if err != nil {
		return nil, receipt, err
	}
	defer clear(rootKey)
	p = &GovernanceProjection{cowNative: native, cowBudget: o.Budget, cowViews: make(chan struct{}, 1)}
	p.liveRAM, err = o.Budget.TryAcquireContext(opCtx, resourcebudget.IO, resourcebudget.Cost{RAM: TypedProjectionResidentRAMBytes})
	if err != nil {
		return nil, receipt, err
	}
	physicalRecovery := ha && native.physical != nil
	p.cow, err = authenticatedstate.OpenStore(opCtx, o.RootDirectory, authenticatedstate.StoreOptions{Scope: ref.Scope, ExpectedCut: typedGovernanceCut(saved), Key: rootKey, MaxDiskBytes: o.RootMaxDiskBytes, MaxAppendBytes: o.RootMaxAppendBytes, Budget: o.Budget, DiskReservation: s.DiskReservation, ExpectedSealRoot: &root, ExpectedSealSchema: &schema, PhysicalRecovery: physicalRecovery})
	if err != nil {
		return nil, receipt, err
	}
	projected := saved
	if physicalRecovery {
		projected, err = p.restoreHAPhysicalProjection(opCtx, seal, saved, head, s.Permitted)
		if err != nil {
			return nil, receipt, err
		}
	}
	if err := p.viewRows(opCtx, func(_ any, actual Summary) error {
		if actual != projected {
			return ErrCorrupt
		}
		return nil
	}); err != nil {
		return nil, receipt, err
	}
	batch := make([]Record, 0, typedGovernanceBatchRows)
	defer clear(batch[:cap(batch)])
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := p.withPhysicalProjectionWork(opCtx, func(ctx context.Context) error { return p.applyRecords(ctx, batch) }); err != nil {
			return err
		}
		receipt.AppliedRecords += uint64(len(batch))
		clear(batch)
		batch = batch[:0]
		batchBytes = 0
		return opCtx.Err()
	}
	_, err = verifyDelta(func(r Record) error {
		if r.Sequence > s.Permitted.Records || r.Sequence <= projected.Records {
			return nil
		}
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
		return nil
	})
	if err != nil {
		return nil, receipt, err
	}
	if err := flush(); err != nil {
		return nil, receipt, err
	}
	journal := s.Selected.Journal
	if s.Permitted != saved {
		journal.Position = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentGovernance, Sequence: s.Permitted.Records, Offset: uint64(s.Permitted.Bytes), Hash: s.Permitted.LastHash}
		journal.Root.Empty = false
		journal.Root.Rows = s.Permitted.Records
		journal.Root.Offset = uint64(targetStart)
		journal.Root.Length = targetLength
		journal.Root.Digest = targetLast.Hash
		if saved.Records == 0 {
			lineage := hmac.New(sha256.New, key)
			lineage.Write([]byte("halro:governance:chain-lineage:v1\x00"))
			lineage.Write(firstHash[:])
			copy(journal.Root.FileID[:], lineage.Sum(nil))
		}
	}
	newRoot, err := p.cow.SealRootReference(opCtx, schema)
	if err != nil {
		return nil, receipt, err
	}
	current, err := native.validateSelectedGovernanceOwner(opCtx, o, s, ha)
	if err != nil || current != seal {
		return nil, receipt, errors.Join(ErrStartupCache, err)
	}
	if err := check(); err != nil {
		return nil, receipt, err
	}
	if ha {
		p.haReadCtx, p.haReadCancel = context.WithCancel(context.Background())
	}
	p.ready, p.selectedStartup = true, true
	receipt.From, receipt.To, receipt.NativeHead = saved, s.Permitted, head
	receipt.Reference = TypedGovernanceReference{Journal: journal, Projection: AuthenticatedGovernanceProjectionReference{Schema: schema, Root: newRoot, Scope: ref.Scope, Position: journal.Position}}
	return p, receipt, nil
}

type selectedGovernanceSourceDebt struct {
	error
	work       resourcebudget.Permit
	source     *os.File
	projection *GovernanceProjection
	key        []byte
}

func (e *selectedGovernanceSourceDebt) Unwrap() error { return e.error }

func (l *Log) ValidateSelectedStartupRead(ctx context.Context) error {
	return l.validateSelectedGrant(ctx, false)
}
func (l *Log) validateSelectedGrant(ctx context.Context, structural bool) error {
	if l == nil || ctx.Err() != nil {
		return errors.Join(ErrUnavailable, ctx.Err())
	}
	if l.startupProvenance == nil {
		return nil
	}
	if l.startupFault.Load() || l.startupClosed.Load() {
		return ErrUnavailable
	}
	if l.startupVerified.Load() {
		return nil
	}
	a := l.startupAuditPtr.Load()
	if a == nil {
		return ErrStartupAuditPending
	}
	progress := a.Snapshot()
	if progress.Err != nil || !progress.Ready || structural && !progress.Complete {
		return errors.Join(ErrStartupAuditPending, progress.Err)
	}
	return nil
}
