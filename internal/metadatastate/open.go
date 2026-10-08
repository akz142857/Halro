package metadatastate

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// OpenOptions requires independently accepted content/native/allowed receipts,
// not values rediscovered from these files. Persistent reservations must already
// be reconciled in the SAME Manager. The caller holds exclusive maintenance
// admission across the factory and resulting owner; this is not an active Seal,
// HA role decision, key-unlock proof or default Runtime selection.
type OpenOptions struct {
	BaselineOptions
	Evidence                                    BaselineEvidence
	SavedRoot                                   authenticatedstate.RootIdentity
	SelectedSealRoot                            *authenticatedstate.SealRoot
	SavedHead, NativeHead, PermittedHead        metadatajournal.Head
	RootDiskReservation, JournalDiskReservation resourcebudget.Permit
	MaxJournalBytes                             int64
	TrimState                                   *metadatajournal.TrimStateBinding
	haReadSource                                *Baseline         // actual frozen source owner, never caller file authority
	selectedNativeRoot                          bool              // set only by the actual common publisher constructor
	haSelectedRead                              bool              // selected HA proof owner; no native/COW mutation
	rotationSource                              bool              // authenticated historical retirement owner, never business admission
	haPhysicalSource                            *haPhysicalSource // installed only by the independently selected HA factory
}

// ReopenReceipt separates complete source inspection from permitted semantic
// application. The source is still scanned in full; only delta frames mutate.
type ReopenReceipt struct {
	From, To, NativeHead        metadatajournal.Head
	Root                        authenticatedstate.RootIdentity
	SourceFrames, AppliedFrames uint64
}

// BaselineOpenFailure retains only an unusable cleanup owner. No method may
// grant role/root permission from a failed factory, but actual descriptor and
// accounting uncertainty must remain reachable after the caller gets an error.
type BaselineOpenFailure struct {
	Owner *Baseline
	Cause error
}

func (e *BaselineOpenFailure) Error() string { return e.Cause.Error() }
func (e *BaselineOpenFailure) Unwrap() error { return e.Cause }

func OpenFromRoot(ctx context.Context, ledgerKey []byte, output, journalPath string, o OpenOptions) (*Baseline, ReopenReceipt, error) {
	return openFromRoot(ctx, ledgerKey, output, journalPath, o, nil)
}

func openFromRoot(ctx context.Context, ledgerKey []byte, output, journalPath string, o OpenOptions, phase func(string)) (result *Baseline, receipt ReopenReceipt, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	if o.TrimState != nil {
		state := *o.TrimState
		o.TrimState = &state
	}
	var selectedSealRoot *authenticatedstate.SealRoot
	if o.SelectedSealRoot != nil {
		root := *o.SelectedSealRoot
		selectedSealRoot = &root
	}
	if err := validateOpenOptions(ledgerKey, output, journalPath, o); err != nil {
		return nil, receipt, err
	}
	b := &Baseline{budget: o.Budget, ledgerKeyTag: sha256.Sum256(ledgerKey), output: output, journalPath: journalPath, evidence: o.Evidence, borrowed: true, readOnly: o.rotationSource || o.haSelectedRead, position: o.SavedHead}
	b.haPhysicalSource = o.haPhysicalSource
	if o.TrimState != nil {
		b.trimState = *o.TrimState
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	succeeded := false
	defer func() {
		if !succeeded || resultErr != nil {
			cleanupErr := b.Close()
			resultErr = errors.Join(resultErr, cleanupErr)
			receipt = ReopenReceipt{}
			if cleanupErr != nil || errors.Is(resultErr, ownedfile.ErrRetirement) {
				result = b
				resultErr = &BaselineOpenFailure{Owner: b, Cause: errors.Join(ownedfile.ErrRetirement, resultErr)}
			} else {
				result = nil
			}
		}
	}()
	// Retain fixed persistent ownership before opening files. An existing file
	// never licenses an uncharged owner, nor does this factory retire originals.
	rootCredit := o.RootDiskReservation
	var err error
	if o.haReadSource == nil {
		rootCredit, err = o.RootDiskReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: o.MaxDiskBytes})
		if err != nil {
			return nil, receipt, err
		}
		defer func() { resultErr = errors.Join(resultErr, rootCredit.Release()) }()
	}
	// The actual frozen read source already owns this root ceiling until all
	// children join. BorrowSelectedReadStore takes the child's own permit loan;
	// no redundant temporary loan consumes a fifth ownership reference.
	b.journalDisk, err = o.JournalDiskReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: o.MaxJournalBytes})
	if err != nil {
		return nil, receipt, err
	}
	b.journalCharged = true
	permit, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: DeltaRAMBytes})
	if err != nil {
		return nil, receipt, err
	}
	defer func() {
		// Failure cleanup joins native/store descriptors before this job fee.
		if !succeeded || resultErr != nil {
			if cleanupErr := b.Close(); cleanupErr != nil {
				resultErr = errors.Join(resultErr, ownedfile.ErrRetirement, cleanupErr)
			}
		}
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, permit.Release())
		} else {
			b.sealDebt = permit
		}
	}()
	scope := baselineScope(o.BaselineOptions)
	journalKey, err := hkdf.Key(sha256.New, ledgerKey, scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		return nil, receipt, err
	}
	defer clear(journalKey)
	if o.haPhysicalSource != nil {
		s := o.haPhysicalSource
		b.journal, err = metadatajournal.OpenBoundPhysicalExactContext(ctx, journalPath, journalKey, o.NativeHead, s.permission, s.role == authenticatedstate.SealReplica)
	} else if o.rotationSource || o.haSelectedRead {
		b.journal, err = metadatajournal.OpenRotationSourceReadOnly(ctx, journalPath, journalKey, o.NativeHead, o.MaxJournalBytes)
	} else {
		b.journal, err = metadatajournal.OpenBoundExactContext(ctx, journalPath, journalKey, o.NativeHead, o.MaxJournalBytes)
	}
	if err != nil {
		return nil, receipt, err
	}
	// Compute real physical frame-end boundaries with one previous frame's
	// scalar facts; never collect history or infer an encoded JSON length.
	var previous metadatajournal.Record
	var havePrevious, savedSeen, permittedSeen bool
	var savedRecord metadatajournal.Record
	observe := func(end int64) {
		actual := o.NativeHead
		actual.Sequence, actual.Hash, actual.Offset = previous.Sequence, previous.Hash, end
		if actual == o.SavedHead {
			savedSeen = true
			// Only scalar facts are needed for the expected delta cut.
			savedRecord = metadatajournal.Record{Epoch: previous.Epoch, Sequence: previous.Sequence, Offset: previous.Offset, Hash: previous.Hash}
		}
		if actual == o.PermittedHead {
			permittedSeen = true
		}
	}
	_, err = b.journal.InspectBoundContext(ctx, func(record metadatajournal.Record) error {
		if havePrevious {
			observe(record.Offset)
		}
		if record.Kind == metadatajournal.KindOperations {
			if err := validateDelta(record.Ops); err != nil {
				return err
			}
		} else if record.Kind == metadatajournal.KindTrim {
			if o.TrimState == nil || record.Trim.State == nil || *record.Trim.State != *o.TrimState {
				return ErrSource
			}
		} else if record.Kind != metadatajournal.KindEpoch || record.Header.PreviousEpoch != o.Evidence.Metadata.MetadataJournalEpoch {
			return ErrSource
		}
		receipt.SourceFrames++
		previous = metadatajournal.Record{Epoch: record.Epoch, Sequence: record.Sequence, Offset: record.Offset, Hash: record.Hash}
		if record.Kind == metadatajournal.KindTrim {
			previous.Sequence = record.Trim.TrimmedThrough
			copy(previous.Hash[:], record.Trim.ChainHeadAtTrim)
		}
		havePrevious = true
		return ctx.Err()
	})
	if err != nil {
		return nil, receipt, err
	}
	if havePrevious {
		observe(o.NativeHead.Offset)
	}
	if !savedSeen || !permittedSeen || o.SavedHead.Sequence > o.NativeHead.TrimmedThrough && o.SavedRoot.Cut != deltaCut(o.NativeHead, savedRecord) {
		return nil, receipt, ErrSource
	}
	if phase != nil {
		phase("source-verified")
	}
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	rootKey, err := hkdf.Key(sha256.New, ledgerKey, scope[:], "halro:metadata:authenticated-baseline:hmac:v1", 32)
	if err != nil {
		return nil, receipt, err
	}
	defer clear(rootKey)
	var selectedSchema *authenticatedstate.SealRootSchema
	if selectedSealRoot != nil {
		schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentMetadata, IndexID: AuthenticatedMetadataSealIndexID, Codec: AuthenticatedMetadataSealCodec}
		selectedSchema = &schema
	}
	storeOptions := authenticatedstate.StoreOptions{
		PhysicalRecovery: o.haPhysicalSource != nil,
		ReadOnly:         o.rotationSource || o.haSelectedRead,
		Scope:            scope, ExpectedCut: o.SavedRoot.Cut, ExpectedRoot: &o.SavedRoot, Key: rootKey,
		Budget: o.Budget, MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes, DiskReservation: rootCredit,
		ExpectedSealRoot: selectedSealRoot, ExpectedSealSchema: selectedSchema,
	}
	if o.haReadSource != nil {
		b.store, err = o.haReadSource.store.BorrowSelectedReadStore(ctx, storeOptions)
	} else {
		b.store, err = authenticatedstate.OpenStore(ctx, output, storeOptions)
	}
	if b.store != nil && o.haReadSource != nil {
		err = errors.Join(err, o.haReadSource.store.RetainBorrowedReadCleanup(b.store, b.Close))
	}
	if err != nil {
		return nil, receipt, err
	}
	if phase != nil {
		phase("component-open")
	}
	if err := checkSavedRows(ctx, b.store, o); err != nil {
		return nil, receipt, err
	}
	if o.haPhysicalSource != nil {
		window, err := b.restorePhysicalTerminals(ctx, o.haPhysicalSource.recoverySeal)
		if err != nil {
			return nil, receipt, err
		}
		if window != nil {
			completion, err := b.acquirePhysicalCompletion()
			if err != nil {
				return nil, receipt, err
			}
			b.physicalRecovery = &TerminalPhysicalWindow{owner: b, cow: window, remaining: b.physicalDebt, work: completion.Active, completion: completion}
			b.physicalWindows[0] = b.physicalRecovery
			o.haPhysicalSource.receiveWindow = b.physicalRecovery
		}
	}
	// Source must still be complete and eligible after OpenStore's independent
	// orphan cleanup. No business exposure exists during this factory.
	b.linked = true
	b.ready.Store(true)
	// Reuse the same reducer and whole-source qualification as live replay.
	// Retain this factory's actual IO workspace through all replay and cleanup.
	if _, err := b.replayTo(ctx, o.NativeHead, o.PermittedHead.Sequence, &permit); err != nil {
		return nil, receipt, err
	}
	if b.position != o.PermittedHead {
		return nil, receipt, ErrSource
	}
	receipt.Root, err = b.store.RootIdentity(ctx)
	if err != nil {
		return nil, receipt, err
	}
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	receipt.From, receipt.To, receipt.NativeHead = o.SavedHead, b.position, o.NativeHead
	receipt.AppliedFrames = b.position.Sequence - o.SavedHead.Sequence
	if o.rotationSource {
		b.rotationSource = true
		b.ready.Store(false)
	}
	result, succeeded = b, true
	return result, receipt, nil
}

func validateOpenOptions(key []byte, output, journalPath string, o OpenOptions) error {
	if len(key) != 32 || o.Identity == ([32]byte{}) || o.InstanceID == "" || len(o.InstanceID) > 128 || o.KeyGeneration == 0 || o.Budget == nil || o.MaxDiskBytes <= 0 || o.MaxAppendBytes <= 0 || o.MaxAppendBytes > o.MaxDiskBytes || o.MaxJournalBytes < 4096 || !filepath.IsAbs(output) || !filepath.IsAbs(journalPath) || filepath.Clean(filepath.Dir(journalPath)) == filepath.Clean(output) {
		return ErrSource
	}
	e := o.Evidence
	scope := baselineScope(o.BaselineOptions)
	binding := o.NativeHead.Baseline
	if e.Version != 1 || e.Identity != o.Identity || e.InstanceID != o.InstanceID || e.KeyGeneration != o.KeyGeneration || e.Metadata.SchemaVersion != boltstore.CurrentSchemaVersion() || e.CatalogDigest != boltstore.MetadataBaselineCatalogDigest() || e.BackupID == "" || len(e.BackupID) > 128 || len(e.Metadata.MinimumLedgerReaderVersion) > 128 || e.File.Size <= 0 || len(e.File.SHA256) != 64 || e.CiphertextDigest == ([32]byte{}) || len(e.ManifestDigest) != 64 || e.File.Path != "data/metadata.db" || e.Rows == 0 || binding.Version != 1 || binding.Identity != o.Identity || binding.KeyGeneration != o.KeyGeneration || binding.Schema != e.Metadata.SchemaVersion || binding.Catalog != e.CatalogDigest || binding.Scope != scope || binding.Rows < e.Rows || binding.Epoch <= e.Metadata.MetadataJournalEpoch || binding.Epoch != o.NativeHead.Epoch {
		return ErrSource
	}
	cut, err := baselineCut(scope, e)
	if err != nil {
		return err
	}
	raw := append([]byte("halro:metadata:epoch-baseline-cut:v1\x00"), cut[:]...)
	raw = binary.BigEndian.AppendUint64(raw, binding.Epoch)
	if binding.Cut != sha256.Sum256(raw) || o.SavedRoot.Scope != scope || o.SavedRoot.ID != binding.RootID || o.SavedRoot.Revision == 0 || o.SavedRoot.Digest == ([32]byte{}) {
		return fmt.Errorf("%w: saved root does not match baseline lineage", ErrSource)
	}
	for _, h := range []metadatajournal.Head{o.SavedHead, o.NativeHead, o.PermittedHead} {
		if h.Baseline != binding || h.Epoch != binding.Epoch || h.FormatVersion != 2 || h.FileRevision != o.NativeHead.FileRevision || h.TrimmedThrough != o.NativeHead.TrimmedThrough || h.Offset <= 0 || h.Hash == ([32]byte{}) || h.Sequence < h.TrimmedThrough {
			return fmt.Errorf("%w: saved, native and permitted heads do not share one bound revision", ErrSource)
		}
	}
	if o.NativeHead.TrimmedThrough == 0 {
		if o.NativeHead.FileRevision != 1 || o.TrimState != nil {
			return ErrSource
		}
	} else {
		if o.TrimState == nil {
			return ErrSource
		}
		state := *o.TrimState
		if state.Version != 1 || state.Sequence != o.NativeHead.TrimmedThrough || state.SourceRevision == 0 || o.NativeHead.FileRevision <= 1 || state.SourceRevision != o.NativeHead.FileRevision-1 || state.SourceFrameOffset < 0 || state.SourceEndOffset <= state.SourceFrameOffset || state.SourceHash == ([32]byte{}) || state.Root.Scope != scope || state.Root.ID != binding.RootID || state.Root.Digest == ([32]byte{}) || state.Root.Revision == 0 || state.Root.Rows == 0 {
			return ErrSource
		}
		sourceHead := o.NativeHead
		sourceHead.FileRevision = state.SourceRevision
		if state.Root.Cut != deltaCut(sourceHead, metadatajournal.Record{Epoch: binding.Epoch, Sequence: state.Sequence, Offset: state.SourceFrameOffset, Hash: state.SourceHash}) {
			return ErrSource
		}
		if o.SavedHead.Sequence == state.Sequence && o.SavedRoot != authenticatedstate.RootIdentity(state.Root) {
			return ErrSource
		}
	}
	if o.SavedHead.Sequence > o.PermittedHead.Sequence || o.PermittedHead.Sequence > o.NativeHead.Sequence || o.SavedHead.Offset > o.PermittedHead.Offset || o.PermittedHead.Offset > o.NativeHead.Offset {
		return fmt.Errorf("%w: saved and permitted heads are outside native bounds", ErrSource)
	}
	if o.SavedHead.Sequence == 0 && (o.SavedRoot.Cut != binding.Cut || !o.selectedNativeRoot && (o.SavedRoot.Digest != binding.RootDigest || o.SavedRoot.Rows != binding.Rows)) {
		return fmt.Errorf("%w: zero-sequence root differs from initial baseline commitment", ErrSource)
	}
	return nil
}

func checkSavedRows(ctx context.Context, store *authenticatedstate.Store, o OpenOptions) (resultErr error) {
	pin, err := store.Snapshot(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	storedEvidence, err := ReadBaselineEvidence(ctx, pin)
	if err != nil {
		return errors.Join(ErrSource, errors.New("persisted baseline evidence is invalid"), err)
	}
	if storedEvidence != o.Evidence {
		return errors.Join(ErrSource, errors.New("persisted baseline evidence differs from the independently supplied evidence"))
	}
	read := func(name string) ([]byte, bool, error) {
		k, err := RowKey([][]byte{[]byte("meta")}, []byte(name), false)
		if err != nil {
			return nil, false, err
		}
		return pin.Get(ctx, k)
	}
	for _, entry := range []struct {
		name  string
		value uint64
	}{{"schema_version", o.Evidence.Metadata.SchemaVersion}, {"metadata_journal_epoch", o.SavedHead.Epoch}, {"applied_journal_sequence", o.SavedHead.Sequence}} {
		v, found, err := read(entry.name)
		if err != nil || !found || len(v) != 8 || binary.BigEndian.Uint64(v) != entry.value {
			return errors.Join(ErrSource, err)
		}
	}
	v, found, err := read("instance_id")
	if err != nil || !found || string(v) != o.InstanceID {
		return errors.Join(ErrSource, err)
	}
	v, _, err = read("minimum_ledger_reader_version")
	if err != nil || string(v) != o.Evidence.Metadata.MinimumLedgerReaderVersion {
		return errors.Join(ErrSource, err)
	}
	v, _, err = read("ledger_feature_epoch")
	if err != nil || len(v) > 1 || len(v) == 1 && v[0] != o.Evidence.Metadata.LedgerFeatureEpoch || len(v) == 0 && o.Evidence.Metadata.LedgerFeatureEpoch != 0 {
		return errors.Join(ErrSource, err)
	}
	return nil
}

// ReadBaselineEvidence reads the backup provenance committed inside a pinned
// authenticated Metadata root. The caller must first select and pin that root
// from an independent authority; this receipt does not select roots or grant
// runtime/role permission.
func ReadBaselineEvidence(ctx context.Context, snapshot *authenticatedstate.Snapshot) (BaselineEvidence, error) {
	if snapshot == nil {
		return BaselineEvidence{}, ErrSource
	}
	raw, found, err := snapshot.Get(ctx, baselineEvidenceKey)
	if err != nil || !found {
		return BaselineEvidence{}, errors.Join(ErrSource, err)
	}
	var evidence BaselineEvidence
	if err := json.Unmarshal(raw, &evidence); err != nil || evidence.Version != 1 {
		return BaselineEvidence{}, errors.Join(ErrSource, err)
	}
	canonical, err := json.Marshal(evidence)
	if err != nil || string(canonical) != string(raw) {
		return BaselineEvidence{}, errors.Join(ErrSource, err)
	}
	return evidence, nil
}

// DiskReservations retains the closed original publisher's persistent owners.
// It does not reconstruct charges after process exit; callers must reconcile
// physical files into the new Manager independently before OpenFromRoot.
func (b *Baseline) DiskReservations() (root, journal resourcebudget.Permit, err error) {
	if b == nil || !b.mu.TryLock() {
		return root, journal, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if b.borrowed || b.ctx.Err() == nil || b.ready.Load() || !b.linked || !b.journalCharged {
		return root, journal, ErrSource
	}
	root, err = b.store.DiskReservation()
	if err != nil {
		return root, journal, err
	}
	cost, costErr := b.journalDisk.Cost()
	if costErr != nil {
		return resourcebudget.Permit{}, resourcebudget.Permit{}, errors.Join(costErr, root.Release())
	}
	journal, err = b.journalDisk.RetainCovering(b.budget, resourcebudget.Cost{Disk: cost.Disk})
	if err != nil {
		err = errors.Join(err, root.Release())
		root = resourcebudget.Permit{}
	}
	return root, journal, err
}

// AdoptClosedSuccessor updates only the original publisher's root deletion
// receipt after an independently qualified borrower has closed. Native append
// retains the same inode; an actual replacement creation receipt follows a
// qualified trim successor. Borrowed reopen alone grants no deletion receipt.
func (b *Baseline) AdoptClosedSuccessor(next *Baseline) error {
	if b == nil || next == nil || b == next || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !next.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer next.mu.Unlock()
	if b.borrowed || !next.borrowed || !next.borrowedClosed || !next.borrowedClean || b.ready.Load() || next.ready.Load() || b.ctx.Err() == nil || next.ctx.Err() == nil || b.budget != next.budget || b.output != next.output || b.journalPath != next.journalPath || b.ledgerKeyTag != next.ledgerKeyTag || b.position.Baseline != next.position.Baseline || next.position.FileRevision < b.position.FileRevision || next.position.Sequence < b.position.Sequence {
		return ErrSource
	}
	if err := b.store.AdoptClosedSuccessor(next.store); err != nil {
		return err
	}
	if err := b.journal.AdoptClosedReplacement(next.journal); err != nil {
		return err
	}
	b.position, b.trimState = next.position, next.trimState
	return nil
}
