package metadatastate

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// SelectedOpenOptions has no caller NativeHead or root selector. The live
// publisher supplies the exact eligible common Metadata position and complete
// COW commitment. Evidence/Binding/identity/key/disk ownership still come from
// independent startup authority, not from a provenance receipt found in a file.
type SelectedOpenOptions struct {
	PhysicalTerminalReserve bool
	ReadOnly                bool // native suffix is reduced into a private disposable root
	BaselineOptions
	Evidence                                    BaselineEvidence
	Binding                                     metadatajournal.BaselineBinding
	Publisher                                   *authenticatedstate.SealCandidatePublisher
	Guard                                       *lock.Lock
	Authority                                   authenticatedstate.SealEligibility
	RootDiskReservation, JournalDiskReservation resourcebudget.Permit
	MaxJournalBytes                             int64
}

// OpenSelectedFromRoot initializes Metadata using its selected complete root
// and only authenticated native delta. All business, snapshots and Seal export
// remain blocked until AuditSelectedRootStartup succeeds. It does not authorize
// HA, mixed coverage, trim/epoch transitions or default Runtime activation.
func OpenSelectedFromRoot(ctx context.Context, ledgerKey []byte, output, journalPath string, o SelectedOpenOptions) (result *Baseline, receipt ReopenReceipt, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, receipt, err
	}
	if o.Publisher == nil || o.Guard == nil || o.Budget == nil || len(ledgerKey) != 32 || o.Authority.Role != authenticatedstate.SealStandalone || o.Authority.Coverage != authenticatedstate.SealCoverageAuthenticated {
		return nil, receipt, ErrSource
	}
	if err := o.Publisher.ValidateOwner(o.Guard, o.Budget); err != nil {
		return nil, receipt, err
	}
	seal, found, err := o.Publisher.Select(o.Authority)
	if err != nil || !found {
		return nil, receipt, errors.Join(ErrSource, err)
	}
	var root authenticatedstate.SealRoot
	for _, r := range seal.Roots[:seal.RootCount] {
		if r.Kind == authenticatedstate.SealRootState && r.Component == authenticatedstate.SealComponentMetadata && r.IndexID == AuthenticatedMetadataSealIndexID && r.Codec == AuthenticatedMetadataSealCodec {
			root = r
		}
	}
	if root.State.Scope != baselineScope(o.BaselineOptions) || root.Empty || root.Rows == 0 {
		return nil, receipt, ErrSource
	}
	scope := baselineScope(o.BaselineOptions)
	journalKey, err := hkdf.Key(sha256.New, ledgerKey, scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		return nil, receipt, err
	}
	defer clear(journalKey)
	b := &Baseline{budget: o.Budget, ledgerKeyTag: sha256.Sum256(ledgerKey), output: output, journalPath: journalPath, evidence: o.Evidence, borrowed: true, readOnly: o.ReadOnly}
	b.selectedPublisher, b.selectedGuard = o.Publisher, o.Guard
	b.ctx, b.cancel = context.WithCancel(context.Background())
	work, err := o.Budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: DeltaRAMBytes})
	if err != nil {
		b.cancel()
		return nil, receipt, err
	}
	succeeded := false
	defer func() {
		if !succeeded || resultErr != nil {
			resultErr = errors.Join(resultErr, b.Close())
			result, receipt = nil, ReopenReceipt{}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			b.retirementErr, b.transactionDebt = resultErr, work
			b.closeErr = resultErr
			result = b // cleanup-only actual owners and whole job fee stay reachable
			return
		}
		if releaseErr := work.Release(); releaseErr != nil {
			resultErr = errors.Join(resultErr, releaseErr)
			b.retirementErr, b.transactionDebt = resultErr, work
			b.closeErr = resultErr
			b.ready.Store(false)
			result = b
		}
	}()
	rootCredit, err := o.RootDiskReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if err != nil {
		return nil, receipt, err
	}
	defer func() { resultErr = errors.Join(resultErr, rootCredit.Release()) }()
	b.journalDisk, err = o.JournalDiskReservation.RetainCovering(o.Budget, resourcebudget.Cost{Disk: o.MaxJournalBytes})
	if err != nil {
		return nil, receipt, err
	}
	b.journalCharged = true
	var native metadatajournal.NativeStartupReceipt
	b.journal, native, err = metadatajournal.OpenBoundWithSelectedSeal(ctx, journalPath, journalKey, metadatajournal.SelectedSealOpenOptions{ReadOnly: o.ReadOnly, Publisher: o.Publisher, Guard: o.Guard, Authority: o.Authority, Budget: o.Budget, Binding: o.Binding, MaxBytes: o.MaxJournalBytes})
	if err != nil {
		return nil, receipt, err
	}
	b.position = native.Saved
	savedRoot := authenticatedstate.RootIdentity{Scope: root.State.Scope, Cut: root.State.Cut, Digest: root.State.Digest, ID: root.State.PackID, Revision: root.State.Revision, Rows: root.Rows}
	legacy := OpenOptions{BaselineOptions: o.BaselineOptions, Evidence: o.Evidence, SavedRoot: savedRoot, SelectedSealRoot: &root, SavedHead: native.Saved, NativeHead: native.Head, PermittedHead: native.Head, RootDiskReservation: rootCredit, JournalDiskReservation: b.journalDisk, MaxJournalBytes: o.MaxJournalBytes}
	// The actual independently eligible common publication may include local
	// derived Metadata updates or a repack at sequence zero. Its complete root
	// authorizes that commitment; the persisted provenance receipt alone does
	// not. Generic OpenFromRoot remains bound to the original zero-frame root.
	legacy.selectedNativeRoot = true
	if err := validateOpenOptions(ledgerKey, output, journalPath, legacy); err != nil {
		return nil, receipt, err
	}
	rootKey, err := hkdf.Key(sha256.New, ledgerKey, scope[:], "halro:metadata:authenticated-baseline:hmac:v1", 32)
	if err != nil {
		return nil, receipt, err
	}
	defer clear(rootKey)
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentMetadata, IndexID: AuthenticatedMetadataSealIndexID, Codec: AuthenticatedMetadataSealCodec}
	b.store, err = authenticatedstate.OpenStore(ctx, output, authenticatedstate.StoreOptions{PhysicalRecovery: o.PhysicalTerminalReserve && !o.ReadOnly, ReadOnly: o.ReadOnly, Scope: scope, ExpectedCut: savedRoot.Cut, ExpectedRoot: &savedRoot, Key: rootKey, Budget: o.Budget, MaxDiskBytes: o.MaxDiskBytes, MaxAppendBytes: o.MaxAppendBytes, DiskReservation: rootCredit, ExpectedSealRoot: &root, ExpectedSealSchema: &schema})
	if err != nil {
		return nil, receipt, err
	}
	if err := checkSavedRows(ctx, b.store, legacy); err != nil {
		return nil, receipt, err
	}
	var physicalWindow *authenticatedstate.PhysicalTerminalWindow
	if o.PhysicalTerminalReserve && !o.ReadOnly {
		physicalWindow, err = b.restorePhysicalTerminals(ctx, seal)
		if err != nil {
			return nil, receipt, err
		}
	}
	if o.ReadOnly && native.Head != native.Saved {
		if err := b.cloneSelectedReadRoot(ctx, rootKey, o.BaselineOptions); err != nil {
			return nil, receipt, err
		}
	}
	// Class/schema validation covers the whole independently authenticated suffix
	// before the first semantic mutation, retaining this factory's actual fee.
	err = b.journal.ReplaySelectedDeltaContext(ctx, o.Budget, func(ctx context.Context, record metadatajournal.Record) error { return validateDelta(record.Ops) })
	if err != nil {
		return nil, receipt, err
	}
	err = b.journal.ReplaySelectedDeltaContext(ctx, o.Budget, func(ctx context.Context, record metadatajournal.Record) error {
		if physicalWindow != nil && record.Sequence <= b.position.Sequence {
			return nil
		}
		if record.Epoch != b.position.Epoch || record.Sequence != b.position.Sequence+1 || record.FormatVersion != 2 {
			return ErrSource
		}
		if err := b.restorePhysicalReplay(ctx, physicalWindow, record, func(ctx context.Context) error {
			_, err := b.store.UpdateNativeFrame(ctx, deltaCut(native.Head, record), func(tx *authenticatedstate.Transaction) error { return reduceDelta(tx, record) }, func() error { return ctx.Err() })
			return err
		}); err != nil {
			return err
		}
		b.position.Sequence, b.position.Hash, b.position.Offset = record.Sequence, record.Hash, record.Offset
		return nil
	})
	if err != nil {
		return nil, receipt, err
	}
	if physicalWindow != nil {
		// Even zero remaining slots retain the actual IO loan/receipt lifetime
		// until the current root enters the independently published common cut.
		completion, err := b.acquirePhysicalCompletion()
		if err != nil {
			return nil, receipt, err
		}
		b.physicalRecovery = &TerminalPhysicalWindow{owner: b, cow: physicalWindow, remaining: b.physicalDebt, work: completion.Active, completion: completion}
		b.physicalWindows[0] = b.physicalRecovery
	}
	b.position.Offset = native.Head.Offset
	if b.position != native.Head {
		return nil, receipt, ErrSource
	}
	current, found, err := o.Publisher.Select(o.Authority)
	if err != nil || !found || current != seal || ctx.Err() != nil {
		return nil, receipt, errors.Join(ErrSource, err, ctx.Err())
	}
	b.linked = true
	b.selectedStartup = true
	receipt.Root, err = b.store.RootIdentity(ctx)
	if err != nil {
		return nil, receipt, err
	}
	receipt.From, receipt.To, receipt.NativeHead = native.Saved, b.position, native.Head
	receipt.SourceFrames, receipt.AppliedFrames = native.DecodedRecords, native.Head.Sequence-native.Saved.Sequence
	result, succeeded = b, true
	return result, receipt, nil
}

// AuditSelectedRootStartup is a joined maintenance job, not a background grant.
// Close revokes its linked context before waiting for the actual owner mutex.
// Until all old bytes and the saved physical boundary authenticate, Ready and
// every existing business/Seal entry remain false. Cancellation keeps pending.
func (b *Baseline) AuditSelectedRootStartup(ctx context.Context) error {
	if b == nil || !b.mu.TryLock() {
		return authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if b.ctx.Err() != nil || !b.borrowed || !b.linked || b.journal == nil || b.retirementErr != nil {
		return authenticatedstate.ErrClosed
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := b.journal.AuditSelectedStartup(ctx, b.budget); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.position != b.journal.Head() {
		return ErrSource
	}
	b.ready.Store(true)
	return nil
}
