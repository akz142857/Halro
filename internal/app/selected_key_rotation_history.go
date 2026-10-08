package app

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/vault"
)

const selectedRotationHistorySlots = 4
const selectedRotationMetadataSlots = 8
const selectedRotationPreparationAttempts = 8

type selectedRotationRetirementFailure struct {
	runtime       *Runtime
	source        *metadatastate.Baseline
	archive       *metadatajournal.NativeArchive
	root, journal resourcebudget.Permit
	cause         error
}

func (e *selectedRotationRetirementFailure) Error() string { return e.cause.Error() }
func (e *selectedRotationRetirementFailure) Unwrap() error { return e.cause }

func retainSelectedRotationRetirementDebt(r *Runtime, source *metadatastate.Baseline, archive *metadatajournal.NativeArchive, root, journal resourcebudget.Permit, cause error) error {
	debt := &selectedRotationRetirementFailure{runtime: r, source: source, archive: archive, root: root, journal: journal, cause: cause}
	if err := r.lock.RetainCleanup(func() error { return debt }); err != nil {
		debt.cause = errors.Join(debt.cause, err)
	}
	return debt
}

type selectedRotationHistoryFailure struct {
	native *metadatajournal.Log
	work   resourcebudget.Permit
	cause  error
}

func (e *selectedRotationHistoryFailure) Error() string { return e.cause.Error() }
func (e *selectedRotationHistoryFailure) Unwrap() error { return e.cause }

func selectedRotationHistoryPaths(cfg config.Config, slot uint8) (string, string) {
	stem := filepath.Join(cfg.Storage.DataDir, fmt.Sprintf("memory-state.metadata-history-%d", slot))
	return stem + ".journal", stem + ".authority"
}
func selectedRotationHistoryPreflight(ctx context.Context, cfg config.Config, v *vault.Vault, b memoryStateBootstrap) (uint8, uint16, resourcebudget.Permit, error) {
	var opaque uint16
	m := keyScope(ctx).root.pressure.logicalBudget()
	reserve, err := m.TryAcquireRecovery(resourcebudget.Cost{Disk: selectedRotationHistorySlots*cfg.ResourceBudget.MetadataJournalDiskBytes + selectedRotationHistorySlots*selectedRotationIntentLimit})
	if err != nil {
		return 0, opaque, reserve, err
	}
	unused := uint8(selectedRotationHistorySlots)
	for slot := uint8(0); slot < selectedRotationHistorySlots; slot++ {
		journal, capsule := selectedRotationHistoryPaths(cfg, slot)
		ji, je := os.Lstat(journal)
		_, ce := os.Lstat(capsule)
		if errors.Is(je, os.ErrNotExist) && errors.Is(ce, os.ErrNotExist) {
			if unused == selectedRotationHistorySlots {
				unused = slot
			}
			continue
		}
		if je != nil || ce != nil || !ji.Mode().IsRegular() || ji.Mode().Perm()&0077 != 0 || ji.Size() > cfg.ResourceBudget.MetadataJournalDiskBytes {
			return 0, opaque, reserve, errors.Join(authenticatedstate.ErrCorrupt, je, ce)
		}
		historical, err := loadSelectedRotationHistory(cfg, capsule, v, nil, b)
		if err != nil {
			return 0, opaque, reserve, err
		}
		if err := verifySelectedRotationArchivedNative(ctx, cfg, journal, historical, b.LedgerKey, m); err != nil {
			return 0, opaque, reserve, err
		}
		opaque |= historical.OpaqueMetadataSlots
		clearRotationIntentKeys(historical)
	}
	if unused == selectedRotationHistorySlots {
		return 0, opaque, reserve, errors.New("four Metadata native history slots are full; explicit offline archival is required before another DEK rotation")
	}
	return unused, opaque, reserve, nil
}
func loadSelectedRotationHistory(cfg config.Config, path string, old, next *vault.Vault, current memoryStateBootstrap) (*selectedRotationIntent, error) {
	raw, _, err := readSelectedRotationBytes(path)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	var capsule selectedRotationCapsule
	if err := strictSelectedRotationJSON(raw, &capsule); err != nil || capsule.Version != 1 {
		return nil, errors.Join(authenticatedstate.ErrCorrupt, err)
	}
	var plain []byte
	for _, v := range []*vault.Vault{old, next} {
		if v == nil {
			continue
		}
		for _, envelope := range [][]byte{capsule.New, capsule.Old} {
			plain, err = v.DecryptCredential("system:selected-dek-rotation", "system", "halro:selected-dek-rotation:v1", envelope)
			if err == nil {
				break
			}
		}
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	var history selectedRotationIntent
	if err := strictSelectedRotationJSON(plain, &history); err != nil {
		return nil, err
	}
	if history.Version != 1 || history.SourceBootstrap.InstanceID != current.InstanceID || history.SourceBootstrap.KeyGeneration != current.KeyGeneration || history.SourceBootstrap.Authority.Instance != current.Authority.Instance || history.HistorySlot >= selectedRotationHistorySlots || history.OldFingerprint != keyFingerprint(history.OldKey) || !bytes.Equal(history.SourceBootstrap.LedgerKey, current.LedgerKey) || !bytes.Equal(history.SourceBootstrap.AuditKey, current.AuditKey) || !bytes.Equal(history.SourceBootstrap.GovernanceKey, current.GovernanceKey) || history.SourceRoot.Position.Epoch != history.SourceHead.Epoch || history.SourceRoot.Position.Hash != history.SourceHead.Hash {
		clearRotationIntentKeys(&history)
		return nil, authenticatedstate.ErrSealIneligible
	}
	return &history, nil
}
func persistSelectedRotationHistory(ctx context.Context, cfg config.Config, path string, i *selectedRotationIntent, old, next *vault.Vault) error {
	return persistSelectedRotationIntentAt(ctx, path, i, old, next)
}
func verifySelectedRotationArchivedNative(ctx context.Context, cfg config.Config, path string, i *selectedRotationIntent, ledgerKey []byte, m *resourcebudget.Manager) (resultErr error) {
	work, err := m.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: metadatastate.DeltaRAMBytes})
	if err != nil {
		return err
	}
	key, err := hkdf.Key(sha256.New, ledgerKey, i.SourceHead.Baseline.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		return errors.Join(err, work.Release())
	}
	defer clear(key)
	native, err := metadatajournal.OpenRotationSourceReadOnly(ctx, path, key, i.SourceHead, cfg.ResourceBudget.MetadataJournalDiskBytes)
	if err != nil {
		if native != nil {
			if closeErr := native.Close(); closeErr != nil {
				return &selectedRotationHistoryFailure{native: native, work: work, cause: errors.Join(ownedfile.ErrRetirement, err, closeErr)}
			}
		}
		if errors.Is(err, ownedfile.ErrRetirement) {
			return &selectedRotationHistoryFailure{native: native, work: work, cause: err}
		}
		return errors.Join(err, work.Release())
	}
	defer func() {
		if closeErr := native.Close(); closeErr != nil {
			resultErr = &selectedRotationHistoryFailure{native: native, work: work, cause: errors.Join(resultErr, closeErr)}
			return
		}
		if releaseErr := work.Release(); releaseErr != nil {
			resultErr = &selectedRotationHistoryFailure{native: native, work: work, cause: errors.Join(resultErr, releaseErr)}
		}
	}()
	head, err := native.InspectBoundContext(ctx, nil)
	if err != nil || head != i.SourceHead {
		return errors.Join(authenticatedstate.ErrCorrupt, err)
	}
	return nil
}

func rewrapSelectedRotationHistory(ctx context.Context, cfg config.Config, i *selectedRotationIntent, newKey []byte) error {
	old, err := vault.New(i.OldKey)
	if err != nil {
		return err
	}
	defer old.Close()
	next, err := vault.New(newKey)
	if err != nil {
		return err
	}
	defer next.Close()
	for slot := uint8(0); slot < selectedRotationHistorySlots; slot++ {
		if slot == i.HistorySlot {
			continue
		}
		_, path := selectedRotationHistoryPaths(cfg, slot)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		history, err := loadSelectedRotationHistory(cfg, path, old, next, i.TargetBootstrap)
		if err != nil {
			return err
		}
		err = persistSelectedRotationHistory(ctx, cfg, path, history, old, next)
		clearRotationIntentKeys(history)
		if err != nil {
			return err
		}
	}
	return nil
}

// The old owner is independently reopened from the source authorization under
// the actual verified target Runtime's Manager and original process guard. Its
// complete native bytes are archived before its superseded COW pack is retired.
func retireSelectedRotationSource(ctx context.Context, cfg config.Config, i *selectedRotationIntent, newKey []byte, r *Runtime, hook func(string) error) (resultErr error) {
	s := r.selectedState
	if s == nil {
		return authenticatedstate.ErrSealIneligible
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			var retained *selectedRotationRetirementFailure
			if !errors.As(resultErr, &retained) {
				// Early archived-reader failures also keep the original Runtime
				// guard. Their nested failure owns the actual FD and IO claim.
				resultErr = retainSelectedRotationRetirementDebt(r, nil, nil, resourcebudget.Permit{}, resourcebudget.Permit{}, resultErr)
			}
		}
	}()
	m := r.resourcePressure.logicalBudget()
	history, authorityPath := selectedRotationHistoryPaths(cfg, i.HistorySlot)
	oldVault, err := vault.New(i.OldKey)
	if err != nil {
		return err
	}
	defer oldVault.Close()
	newVault, err := vault.New(newKey)
	if err != nil {
		return err
	}
	defer newVault.Close()
	if i.OldRootRetired {
		return verifySelectedRotationArchivedNative(ctx, cfg, history, i, s.bootstrap.LedgerKey, m)
	}
	if i.SourceRootRetirementStarted {
		// A restarted process has no receipts for any individual child already
		// unlinked by the old owner. Preserve the remaining namespace opaquely;
		// the independently selected successor and authenticated native archive
		// suffice to finish activation without claiming physical retirement.
		if !i.NativeArchived {
			return authenticatedstate.ErrSealIneligible
		}
		if _, err := selectedRotationQualifiedSuccessor(ctx, s, i); err != nil {
			return err
		}
		if _, err := m.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.MetadataStoreDiskBytes + cfg.ResourceBudget.MetadataJournalDiskBytes}); err != nil {
			return err
		}
		if err := verifySelectedRotationArchivedNative(ctx, cfg, history, i, s.bootstrap.LedgerKey, m); err != nil {
			return err
		}
		i.OpaqueMetadataSlots |= uint16(1) << i.SourceBootstrap.MetadataSlot
		if err := persistSelectedRotationHistory(ctx, cfg, authorityPath, i, oldVault, newVault); err != nil {
			return err
		}
		return persistSelectedRotationIntent(ctx, cfg, i, oldVault, newVault)
	}
	rootPath, journalPath := memoryStateMetadataPaths(cfg, i.SourceBootstrap.MetadataSlot)
	if _, err := os.Lstat(history); err == nil {
		journalPath = history
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	rootDisk, err := m.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.MetadataStoreDiskBytes})
	if err != nil {
		return err
	}
	journalDisk, err := m.TryAcquireRecovery(resourcebudget.Cost{Disk: cfg.ResourceBudget.MetadataJournalDiskBytes})
	if err != nil {
		return err
	}
	eligibility := i.TargetBootstrap.Authority
	selected, found, err := s.publisher.Current()
	if err != nil || !found {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	copy(eligibility.Positions[:], selected.Positions[:])
	root := i.SourceRoot.Root
	source, _, err := metadatastate.OpenAuthorizedRotationRoot(ctx, s.bootstrap.LedgerKey, rootPath, journalPath, metadatastate.OpenOptions{BaselineOptions: selectedRotationBaselineOptions(cfg, m, i.SourceBootstrap), Evidence: i.SourceBootstrap.Evidence, SavedRoot: authenticatedstate.RootIdentity{Scope: root.State.Scope, Cut: root.State.Cut, Digest: root.State.Digest, ID: root.State.PackID, Revision: root.State.Revision, Rows: root.Rows}, SavedHead: i.SourceHead, NativeHead: i.SourceHead, PermittedHead: i.SourceHead, RootDiskReservation: rootDisk, JournalDiskReservation: journalDisk, MaxJournalBytes: cfg.ResourceBudget.MetadataJournalDiskBytes}, i.SourceSealBytes, i.SourceBootstrap.Authority, eligibility, statecut.HardLimits(), s.publisher, r.lock)
	if err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) {
			return retainSelectedRotationRetirementDebt(r, source, nil, rootDisk, journalDisk, err)
		}
		return fmt.Errorf("open independently authorized old Metadata: %w", err)
	}
	var archive *metadatajournal.NativeArchive
	defer func() {
		resultErr = errors.Join(resultErr, source.Close())
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			// Keep the actual Runtime exclusion with this same uncertain reader
			// and all fees. Runtime.Close must not release it for another writer.
			resultErr = retainSelectedRotationRetirementDebt(r, source, archive, rootDisk, journalDisk, resultErr)
		}
	}()
	if err := source.CaptureRotationRetirementLock(); err != nil {
		return err
	}
	if journalPath != history {
		archive, err = source.PrepareNativeRotationArchive(i.SourceHead, history, r.lock)
		if err != nil {
			return err
		}
	}
	if err := persistSelectedRotationHistory(ctx, cfg, authorityPath, i, oldVault, newVault); err != nil {
		return err
	}
	if err := source.Close(); err != nil {
		return err
	}
	if archive != nil {
		if err := archive.Archive(); err != nil {
			return err
		}
	}
	i.NativeArchived = true
	if err := persistSelectedRotationIntent(ctx, cfg, i, oldVault, newVault); err != nil {
		return err
	}
	if err := callRotationHook(hook, "selected_after_native_archive"); err != nil {
		return err
	}
	eligibility, err = selectedRotationQualifiedSuccessor(ctx, s, i)
	if err != nil {
		return err
	}
	i.SourceRootRetirementStarted = true
	i.OpaqueMetadataSlots |= uint16(1) << i.SourceBootstrap.MetadataSlot
	if err := persistSelectedRotationIntent(ctx, cfg, i, oldVault, newVault); err != nil {
		return err
	}
	if err := persistSelectedRotationHistory(ctx, cfg, authorityPath, i, oldVault, newVault); err != nil {
		return err
	}
	if err := callRotationHook(hook, "selected_before_old_root_retirement"); err != nil {
		return err
	}
	if err := source.RetireClosedRotationRoot(ctx, s.generation.Owners.Metadata, i.SourceRoot, s.publisher, r.lock, eligibility, rootDisk); err != nil {
		return fmt.Errorf("retire old actual COW pack: %w", err)
	}
	if err := rootDisk.Release(); err != nil {
		return err
	}
	i.OldRootRetired = true
	i.OpaqueMetadataSlots &^= uint16(1) << i.SourceBootstrap.MetadataSlot
	if err := persistSelectedRotationIntent(ctx, cfg, i, oldVault, newVault); err != nil {
		return err
	}
	if err := persistSelectedRotationHistory(ctx, cfg, authorityPath, i, oldVault, newVault); err != nil {
		return err
	}
	return callRotationHook(hook, "selected_after_old_root_retirement")
}

func selectedRotationQualifiedSuccessor(ctx context.Context, s *runtimeSelectedState, i *selectedRotationIntent) (authority authenticatedstate.SealEligibility, resultErr error) {
	// This private Runtime is offline, has joined its startup audit, and its
	// business admission has been withdrawn by the rotation owner. Acquire the
	// real native lease without prematurely closing Metadata's shutdown writer.
	owners := s.generation.Owners
	lease, err := owners.NativeLedger.BeginMaintenance(ctx)
	if err != nil {
		return authority, err
	}
	defer func() { resultErr = errors.Join(resultErr, lease.Close()) }()
	owners.Lease = lease
	authority, err = statecut.CurrentStandaloneAuthority(ctx, owners, i.TargetBootstrap.Authority)
	if err != nil {
		return authority, err
	}
	current, found, err := s.publisher.Select(authority)
	if err != nil || !found {
		return authority, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	ref, err := s.generation.Owners.Metadata.SealReference(ctx)
	if err != nil || ref.Position.Epoch != i.TargetHead.Epoch {
		return authority, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	actual := false
	for _, root := range current.Roots[:current.RootCount] {
		if root == i.SourceRoot.Root {
			return authority, authenticatedstate.ErrSealIneligible
		}
		actual = actual || root == ref.Root
	}
	if !actual {
		return authority, authenticatedstate.ErrSealIneligible
	}
	return authority, nil
}
func clearRotationIntentKeys(i *selectedRotationIntent) {
	if i == nil {
		return
	}
	clear(i.OldKey)
	clear(i.NewKey)
	for _, b := range []memoryStateBootstrap{i.SourceBootstrap, i.TargetBootstrap} {
		clear(b.LedgerKey)
		clear(b.AuditKey)
		clear(b.GovernanceKey)
	}
}
