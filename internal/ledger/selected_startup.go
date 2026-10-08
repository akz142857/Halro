package ledger

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

var ErrStartupAuditPending = errors.New("selected Ledger startup requires historical audit before writes or readiness")

// SelectedSealOpenOptions requires the actual selected seal publisher and its
// independently established authority. Directory is the complete selected
// directory namespace, not the unauthenticated native manifest or a caller cut.
type SelectedSealOpenOptions struct {
	Publisher        *authenticatedstate.SealCandidatePublisher
	Guard            *lock.Lock
	Authority        authenticatedstate.SealEligibility
	Directory        string
	DirectoryOptions AuthenticatedDirectoryOptions
	recoveryPhase    func(string) error
}

type NativeStartupReceipt struct {
	Saved, Head ReplayAnchor
	// Allowed is set only by the independently authorized HA constructor.
	// Head remains the authenticated complete physical source, including tails
	// which have been received but cannot yet be applied or exposed to readers.
	Allowed                ReplayAnchor
	DecodedRecords         uint64
	HistoricalAuditPending bool
}

type selectedNativeStartup struct {
	ctx               context.Context
	anchor            ReplayAnchor
	pin               *authenticatedstate.Snapshot
	state             authenticatedDirectoryState
	decoded           uint64
	retainedFile      *os.File
	retainedDirectory *segmentDirectoryIndex
	retainedSegment   *segmentReader
	retirementErr     error
	activeInfo        os.FileInfo
	checkSelection    func() error
	recoveryPhase     func(string) error
	nativePath        string
}

type selectedStartupCleanup struct {
	store           *authenticatedstate.Store
	pin             *authenticatedstate.Snapshot
	debt            resourcebudget.Permit
	err             error
	nativeFile      *os.File
	nativeDirectory *segmentDirectoryIndex
	nativeSegment   *segmentReader
}

func retireSelectedStartupFile(file *os.File, selected *selectedNativeStartup) {
	err := ownedfile.CloseFile(file)
	if err != nil && selected != nil {
		selected.retainedFile = file
		selected.retirementErr = errors.Join(selected.retirementErr, ownedfile.ErrRetirement, ErrSegmentRetirement, err)
	}
}

func retireSelectedStartupDirectory(directory *segmentDirectoryIndex, selected *selectedNativeStartup) {
	if err := directory.Close(); err != nil && selected != nil {
		selected.retainedDirectory = directory
		selected.retirementErr = errors.Join(selected.retirementErr, err)
	}
}

func (s *selectedStartupCleanup) close() error {
	// Actual owners already attempted retirement. Their stable unknown-exit
	// error never licenses discarding descriptors, keys or the whole job fee.
	return s.err
}

func retainSelectedStartupCleanup(result **Log, store *authenticatedstate.Store, pin *authenticatedstate.Snapshot, debt resourcebudget.Permit, err error) {
	if *result == nil {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := make(chan struct{})
		close(done)
		signal := make(chan struct{})
		close(signal)
		*result = &Log{status: NewStatus(), closed: true, closeContext: ctx, closeCancel: cancel, writerDone: done, closeSignal: signal}
	}
	l := *result
	l.startupCleanup = &selectedStartupCleanup{store: store, pin: pin, debt: debt, err: err}
	l.startupAuditPending = true
	l.status.RequireRecovery()
}

// OpenWithSelectedSeal initializes the native active chain by authenticating
// only facts after a selected saved cut. It still performs O(segment count)
// descriptor/existence/length checks. Completed later Rolls authenticate the
// cross-file suffix; interrupted Rolls require a full bounded native audit
// before repair. It refuses HA
// roles. The returned Log rejects appends until the finite historical audit
// grants readiness, and rejects Roll until that audit completes. Callers must
// check SelectedStartupReady for ordinary work and structural readiness for
// publication or changes to the native files.
// The first subsequent Roll recomputes the complete active SHA256 checksum.
// The explicitly migrated Standalone Runtime uses this primitive.
func OpenWithSelectedSeal(ctx context.Context, path string, status *Status, options Options, o SelectedSealOpenOptions) (result *Log, receipt NativeStartupReceipt, resultErr error) {
	if options.Replica || options.AfterDurable != nil || options.BeforeDurable != nil || options.AfterRoll != nil || options.BeforeRoll != nil || options.WaitConfirmed != nil || o.Authority.Role != authenticatedstate.SealStandalone {
		return nil, receipt, ErrProjectionSource
	}
	return openWithSelectedSeal(ctx, path, status, options, o, nil)
}

func openWithSelectedSeal(ctx context.Context, path string, status *Status, options Options, o SelectedSealOpenOptions, ha *haSelectedStartupProvenance) (result *Log, receipt NativeStartupReceipt, resultErr error) {
	d := o.DirectoryOptions
	if o.Publisher == nil || o.Guard == nil || d.Budget == nil || d.Identity == ([32]byte{}) || o.Directory == "" || len(options.ChainKey) != 32 || o.Authority.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return nil, receipt, ErrProjectionSource
	}
	if !o.Guard.CoversNativePath(path) {
		return nil, receipt, ErrProjectionSource
	}
	if err := o.Publisher.ValidateOwner(o.Guard, d.Budget); err != nil {
		return nil, receipt, err
	}
	seal, found, err := o.Publisher.Select(o.Authority)
	if err != nil || !found {
		return nil, receipt, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	var root authenticatedstate.SealRoot
	var position authenticatedstate.SealPosition
	for _, p := range seal.Positions[:seal.PositionCount] {
		if p.Component == authenticatedstate.SealComponentLedger {
			position = p
		}
	}
	for _, r := range seal.Roots[:seal.RootCount] {
		if r.Kind == authenticatedDirectorySchema.Kind && r.Component == authenticatedDirectorySchema.Component && r.IndexID == authenticatedDirectorySchema.IndexID && r.Codec == authenticatedDirectorySchema.Codec {
			root = r
		}
	}
	anchor, err := directoryAnchorFromPosition(position)
	if err != nil || root.Rows != anchor.Head.Generation {
		return nil, receipt, errors.Join(ErrProjectionSource, err)
	}
	work, err := d.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return nil, receipt, err
	}
	var store *authenticatedstate.Store
	var pin *authenticatedstate.Snapshot
	var selected *selectedNativeStartup
	defer func() {
		if selected != nil {
			resultErr = errors.Join(resultErr, selected.retirementErr)
		}
		if pin != nil {
			resultErr = errors.Join(resultErr, pin.Close())
		}
		if store != nil {
			resultErr = errors.Join(resultErr, store.Close())
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, ErrSegmentRetirement) {
			retainSelectedStartupCleanup(&result, store, pin, work, resultErr)
			if selected != nil {
				result.startupCleanup.nativeFile = selected.retainedFile
				result.startupCleanup.nativeDirectory = selected.retainedDirectory
				result.startupCleanup.nativeSegment = selected.retainedSegment
			}
			return
		}
		if err := work.Release(); err != nil {
			resultErr = errors.Join(resultErr, err)
			retainSelectedStartupCleanup(&result, store, pin, work, resultErr)
		}
	}()
	scope := authenticatedDirectoryScope(d.Identity)
	key, err := hkdf.Key(sha256.New, options.ChainKey, scope[:], "halro:ledger:authenticated-directory:hmac:v1", 32)
	if err != nil {
		return nil, receipt, err
	}
	defer clear(key)
	schema := authenticatedDirectorySchema
	store, err = authenticatedstate.OpenStore(ctx, o.Directory, authenticatedstate.StoreOptions{Scope: scope, ExpectedCut: authenticatedDirectoryCut(scope, anchor), Key: key, Budget: d.Budget, MaxDiskBytes: d.MaxDiskBytes, MaxAppendBytes: d.MaxAppendBytes, DiskReservation: d.DiskReservation, ExpectedSealRoot: &root, ExpectedSealSchema: &schema})
	if err != nil {
		return nil, receipt, err
	}
	pin, err = store.Snapshot(ctx)
	if err != nil {
		return nil, receipt, err
	}
	raw, found, err := pin.Get(ctx, []byte{0})
	var state authenticatedDirectoryState
	if err != nil || !found || json.Unmarshal(raw, &state) != nil || state.Version != 1 || state.Anchor != anchor || state.Segments != anchor.Head.Generation-1 {
		return nil, receipt, errors.Join(ErrProjectionCorrupt, err)
	}
	options.RequireExisting = true
	selected = &selectedNativeStartup{ctx: ctx, anchor: anchor, pin: pin, state: state, recoveryPhase: o.recoveryPhase, nativePath: path}
	selected.checkSelection = func() error {
		current, found, err := o.Publisher.Select(o.Authority)
		if err != nil || !found || current != seal {
			return errors.Join(ErrProjectionSource, err)
		}
		return errors.Join(o.Publisher.ValidateOwner(o.Guard, d.Budget), ctx.Err())
	}
	options.selectedStartup = selected
	result, err = OpenWithOptions(path, status, options)
	if err != nil {
		return nil, receipt, err
	}
	result.options.selectedStartup = nil // the temporary bootstrap never becomes a live borrower
	result.startupBudget = d.Budget
	result.startupSaved = selected.anchor
	result.startupProvenance = &selectedStartupProvenance{publisher: o.Publisher, guard: o.Guard, seal: seal, identity: d.Identity, ha: ha}
	current, found, err := o.Publisher.Select(o.Authority)
	if err != nil || !found || current != seal || ctx.Err() != nil {
		resultErr = errors.Join(ErrProjectionSource, err, ctx.Err(), result.Close())
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			result = nil
		}
		return result, receipt, resultErr
	}
	head, hash, _ := result.ChainHead()
	receipt = NativeStartupReceipt{Saved: anchor, Head: ReplayAnchor{Head: head, Hash: hash, Epoch: result.chainEpoch}, DecodedRecords: selected.decoded, HistoricalAuditPending: true}
	return result, receipt, nil
}

func (s *selectedNativeStartup) validateDirectory(d *segmentDirectoryIndex, generation uint64, file *os.File) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if d.parsed.pending != nil {
		return ErrProjectionSource
	}
	if err := s.validateDirectoryEntries(d, generation); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || generation == s.anchor.Head.Generation && info.Size() < s.anchor.Head.Offset {
		return errors.Join(ErrProjectionSource, err)
	}
	s.activeInfo = info
	if err := s.validateActive(file); err != nil {
		return err
	}
	return errors.Join(d.manifest.check(), s.ctx.Err())
}

func (s *selectedNativeStartup) validateDirectoryEntries(d *segmentDirectoryIndex, generation uint64) error {
	if generation < s.anchor.Head.Generation || d.parsed.count != generation-1 || d.parsed.count < s.state.Segments {
		return ErrProjectionSource
	}
	for g := uint64(1); g <= s.state.Segments; g++ {
		raw, found, err := s.pin.Get(s.ctx, authenticatedDirectoryKey(g))
		var saved Segment
		if err != nil || !found || json.Unmarshal(raw, &saved) != nil || saved.Generation != g {
			return errors.Join(ErrProjectionCorrupt, err)
		}
		actual, found, err := d.Get(g)
		if err != nil || !found {
			return errors.Join(ErrProjectionSource, err)
		}
		if saved != actual {
			if err := s.verifySelectedCompression(saved, actual); err != nil {
				return err
			}
		}
	}
	return errors.Join(d.manifest.check(), s.ctx.Err())
}

func (s *selectedNativeStartup) validateActive(file *os.File) error {
	info, err := file.Stat()
	named, namedErr := os.Lstat(file.Name())
	if err != nil || namedErr != nil || s.activeInfo == nil || !named.Mode().IsRegular() || !os.SameFile(info, named) || !os.SameFile(info, s.activeInfo) || info.Size() != s.activeInfo.Size() || !info.ModTime().Equal(s.activeInfo.ModTime()) {
		return errors.Join(ErrProjectionSource, err, namedErr)
	}
	return s.ctx.Err()
}

// SelectedStartupReady never clears accounting fault status. Runtime must
// include this gate in readiness when using OpenWithSelectedSeal.
func (l *Log) SelectedStartupReady() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.selectedStartupAppendAllowedLocked() && l.file != nil && l.closeContext.Err() == nil && l.status.Load() == AccountingHealthy
}

// AuditSelectedStartup owns one bounded complete native authentication pass.
// Close cancels and joins it through the native mutex; failure retains the
// startup gate and native source errors latch recovery-required. This API
// deliberately cannot mark an unavailable/recovery-required Log healthy.
func (l *Log) AuditSelectedStartup(ctx context.Context, budget *resourcebudget.Manager) (resultErr error) {
	if l == nil || budget == nil || l.startupBudget != budget {
		return ErrProjectionSource
	}
	l.mu.Lock()
	active := l.startupAudit
	l.mu.Unlock()
	if active != nil {
		return active.Wait(ctx)
	}
	jobCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.closeContext, cancel)
	defer func() { stop(); cancel() }()
	work, err := budget.TryAcquireContext(jobCtx, resourcebudget.IO, resourcebudget.Cost{RAM: AuthenticatedLedgerSourceRAMBytes})
	if err != nil {
		return err
	}
	defer func() { resultErr = finishAuthenticatedSourceCredit(work, resultErr) }()
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := jobCtx.Err(); err != nil {
		return err
	}
	if l.startupAudit != nil {
		a := l.startupAudit
		l.mu.Unlock()
		err := a.Wait(ctx)
		l.mu.Lock()
		return err
	}
	if !l.startupAuditPending {
		return nil
	}
	if l.file == nil || l.closeContext.Err() != nil || l.status.Load() != AccountingHealthy {
		return ErrProjectionSource
	}
	check := func() error {
		info, err := l.file.Stat()
		named, namedErr := os.Lstat(l.path)
		parent, parentErr := os.Lstat(l.directory)
		if err != nil || namedErr != nil || parentErr != nil || !named.Mode().IsRegular() || !os.SameFile(info, named) || !parent.IsDir() || !os.SameFile(parent, l.directoryInfo) {
			return errors.Join(ErrProjectionSource, err, namedErr, parentErr)
		}
		return jobCtx.Err()
	}
	if err := check(); err != nil {
		l.status.RequireRecovery()
		return err
	}
	head := Watermark{Generation: l.generation, Sequence: l.sequence, Offset: l.offset}
	actual, err := l.replayAuthenticatedLocked(jobCtx, ReplayAnchor{}, 0, nil, nil)
	if err != nil || actual != head {
		return errors.Join(ErrProjectionSource, err)
	}
	if err := check(); err != nil {
		l.status.RequireRecovery()
		return err
	}
	l.startupAuditPending = false
	l.startupVerified.Store(true)
	return nil
}
