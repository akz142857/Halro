package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/vault"
)

const selectedRotationIntentLimit = 256 << 10

var ErrSelectedKeyRotationPending = errors.New("selected DEK rotation is pending; resume the same explicit offline rotation")

// This authorization is created from the independently unlocked source and its
// complete startup audit before a candidate root exists. Candidate headers,
// receipts and journals never choose an instance, key or allowed root.
type selectedRotationIntent struct {
	Version                          uint8
	OldKey                           []byte
	NewKey                           []byte
	OperationID                      string
	TargetDescriptor                 *masterkey.KeySlotDescriptor
	OldFingerprint, NewFingerprint   string
	SourceBootstrap, TargetBootstrap memoryStateBootstrap
	SourceSeal, TargetSeal           authenticatedstate.CandidateSeal
	SourceSealBytes                  []byte
	TargetHead                       metadatajournal.Head
	TargetRoot                       metadatastate.AuthenticatedMetadataSealReference
	UnlockArtifact                   string
	UnlockDigest                     [32]byte
	SourceHead                       metadatajournal.Head
	SourceRoot                       metadatastate.AuthenticatedMetadataSealReference
	HistorySlot                      uint8
	NativeArchived, OldRootRetired   bool
	SourceRootRetirementStarted      bool
	PreparationAttempt               uint8
	OpaqueMetadataSlots              uint16
	Credentials                      int
	OldVersion, NewVersion           uint64
}
type selectedRotationCapsule struct {
	Version  uint8
	Old, New []byte
}
type selectedRotationVerification struct{ digest [32]byte }

func selectedRotationIntentPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "memory-state.rotation")
}

func readSelectedRotationBytes(path string) (raw []byte, info os.FileInfo, resultErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > selectedRotationIntentLimit {
		return nil, nil, authenticatedstate.ErrCorrupt
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if closeErr := ownedfile.CloseFile(f); closeErr != nil {
			resultErr = &selectedExportFileRetirement{cause: errors.Join(resultErr, closeErr), file: f}
		}
	}()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	raw, err = io.ReadAll(io.LimitReader(f, selectedRotationIntentLimit+1))
	if err != nil || len(raw) > selectedRotationIntentLimit {
		clear(raw)
		return nil, nil, errors.Join(authenticatedstate.ErrLimit, err)
	}
	return raw, actual, nil
}

func retireSelectedRotationIntent(ctx context.Context, cfg config.Config, capability *selectedRotationVerification, captured os.FileInfo) (resultErr error) {
	guard, ok := keyScope(ctx).lock.(interface{ CoversDataDirectory(string) bool })
	if !ok || !guard.CoversDataDirectory(cfg.Storage.DataDir) || captured == nil {
		return authenticatedstate.ErrSealIneligible
	}
	f, err := os.OpenFile(cfg.Storage.DataDir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := ownedfile.CloseFile(f); closeErr != nil {
			resultErr = &selectedExportFileRetirement{cause: errors.Join(resultErr, closeErr), file: f}
		}
	}()
	parent, err := f.Stat()
	if err != nil {
		return err
	}
	raw, actual, err := readSelectedRotationBytes(selectedRotationIntentPath(cfg))
	defer clear(raw)
	if err != nil || !os.SameFile(captured, actual) || sha256.Sum256(raw) != capability.digest {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	named, err := os.Lstat(cfg.Storage.DataDir)
	if err != nil || !os.SameFile(parent, named) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(selectedRotationIntentPath(cfg)); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return errors.Join(ownedfile.ErrRetirement, err)
	}
	return nil
}

func checkSelectedRotationPending(cfg config.Config, capability *selectedRotationVerification) error {
	path := selectedRotationIntentPath(cfg)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if capability != nil {
			return authenticatedstate.ErrSealIneligible
		}
		return nil
	} else if err != nil {
		return err
	}
	if capability == nil {
		return ErrSelectedKeyRotationPending
	}
	raw, _, err := readSelectedRotationBytes(path)
	defer clear(raw)
	if err != nil || sha256.Sum256(raw) != capability.digest {
		return errors.Join(ErrSelectedKeyRotationPending, err)
	}
	return nil
}

func writeSelectedRotationFile(ctx context.Context, path string, raw []byte) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	old, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, owner, err := ownedfile.CreateTemporary(filepath.Dir(path), ".selected-rotation-*")
	if err != nil {
		return err
	}
	closed, published := false, false
	defer func() {
		if !closed {
			closed = true
			if closeErr := ownedfile.CloseFile(f); closeErr != nil {
				resultErr = &selectedExportFileRetirement{cause: errors.Join(resultErr, closeErr), file: f, owner: owner}
				return
			}
		}
		if resultErr != nil && !published && !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, owner.Retire(), durable.SyncDirectory(filepath.Dir(path)))
		}
	}()
	if n, err := f.Write(raw); err != nil || n != len(raw) {
		return errors.Join(err, io.ErrShortWrite)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	closed = true
	if closeErr := ownedfile.CloseFile(f); closeErr != nil {
		return &selectedExportFileRetirement{cause: closeErr, file: f, owner: owner}
	}
	if old == nil {
		linked, err := owner.LinkCaptured(path)
		if err != nil {
			return err
		}
		published = true
		if err := owner.Retire(); err != nil {
			return &selectedExportFileRetirement{cause: errors.Join(ownedfile.ErrRetirement, err), owner: linked}
		}
	} else {
		if err := owner.ReplaceCaptured(path, old); err != nil {
			return err
		}
		published = true
	}
	if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
		return &selectedExportFileRetirement{cause: errors.Join(ownedfile.ErrRetirement, err), owner: owner}
	}
	return nil
}

func persistSelectedRotationIntent(ctx context.Context, cfg config.Config, i *selectedRotationIntent, oldVault, newVault *vault.Vault) error {
	return persistSelectedRotationIntentAt(ctx, selectedRotationIntentPath(cfg), i, oldVault, newVault)
}

func persistSelectedRotationIntentAt(ctx context.Context, path string, i *selectedRotationIntent, oldVault, newVault *vault.Vault) error {
	plain, err := json.Marshal(i)
	if err != nil {
		return err
	}
	defer clear(plain)
	old, err := oldVault.EncryptCredential("system:selected-dek-rotation", "system", "halro:selected-dek-rotation:v1", plain)
	if err != nil {
		return err
	}
	defer clear(old)
	next, err := newVault.EncryptCredential("system:selected-dek-rotation", "system", "halro:selected-dek-rotation:v1", plain)
	if err != nil {
		return err
	}
	defer clear(next)
	raw, err := json.Marshal(selectedRotationCapsule{Version: 1, Old: old, New: next})
	if err != nil {
		return err
	}
	defer clear(raw)
	if len(raw) > selectedRotationIntentLimit {
		return authenticatedstate.ErrLimit
	}
	return writeSelectedRotationFile(ctx, path, raw)
}

func loadSelectedRotationIntent(cfg config.Config, activeKey, newKey []byte) (*selectedRotationIntent, error) {
	raw, _, err := readSelectedRotationBytes(selectedRotationIntentPath(cfg))
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	var c selectedRotationCapsule
	if err := strictSelectedRotationJSON(raw, &c); err != nil {
		return nil, err
	}
	if c.Version != 1 {
		return nil, authenticatedstate.ErrCorrupt
	}
	v, err := vault.New(activeKey)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	var plain []byte
	for _, envelope := range [][]byte{c.Old, c.New} {
		plain, err = v.DecryptCredential("system:selected-dek-rotation", "system", "halro:selected-dek-rotation:v1", envelope)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	var i selectedRotationIntent
	if err := strictSelectedRotationJSON(plain, &i); err != nil {
		return nil, err
	}
	if i.Version != 1 || len(i.OldKey) != 32 || i.OldFingerprint != keyFingerprint(i.OldKey) || (len(newKey) != 0 && i.NewFingerprint != keyFingerprint(newKey)) || (keyFingerprint(activeKey) != i.OldFingerprint && keyFingerprint(activeKey) != i.NewFingerprint) || i.SourceBootstrap.InstanceID == "" || i.SourceBootstrap.InstanceID != i.TargetBootstrap.InstanceID || i.SourceBootstrap.KeyGeneration != i.TargetBootstrap.KeyGeneration || i.TargetBootstrap.MetadataSlot >= selectedRotationMetadataSlots || i.OldVersion == 0 || i.NewVersion != i.OldVersion+1 {
		clearRotationIntentKeys(&i)
		return nil, authenticatedstate.ErrSealIneligible
	}
	if !validMemoryStateBootstrapVersion(i.SourceBootstrap) || !validMemoryStateBootstrapVersion(i.TargetBootstrap) || i.SourceBootstrap.Authority.Instance != i.TargetBootstrap.Authority.Instance || i.TargetBootstrap.VaultVersion != i.NewVersion || i.TargetBootstrap.VaultFingerprint != i.NewFingerprint || len(i.SourceBootstrap.LedgerKey) != 32 || !bytes.Equal(i.SourceBootstrap.LedgerKey, i.TargetBootstrap.LedgerKey) || !bytes.Equal(i.SourceBootstrap.AuditKey, i.TargetBootstrap.AuditKey) || !bytes.Equal(i.SourceBootstrap.GovernanceKey, i.TargetBootstrap.GovernanceKey) || i.PreparationAttempt == 0 || i.PreparationAttempt > selectedRotationPreparationAttempts || i.OpaqueMetadataSlots>>selectedRotationMetadataSlots != 0 {
		clearRotationIntentKeys(&i)
		return nil, authenticatedstate.ErrSealIneligible
	}
	if i.OldRootRetired && !i.SourceRootRetirementStarted || i.SourceRootRetirementStarted && (!i.NativeArchived || i.TargetSeal.RootCount == 0) {
		clearRotationIntentKeys(&i)
		return nil, authenticatedstate.ErrSealIneligible
	}
	wanted := newKey
	if len(wanted) == 0 {
		wanted = i.NewKey
	}
	old, oldErr := vault.New(i.OldKey)
	if oldErr != nil {
		clearRotationIntentKeys(&i)
		return nil, oldErr
	}
	defer old.Close()
	next, nextErr := vault.New(wanted)
	if nextErr != nil {
		clearRotationIntentKeys(&i)
		return nil, nextErr
	}
	defer next.Close()
	a, aErr := old.DecryptCredential("system:selected-dek-rotation", "system", "halro:selected-dek-rotation:v1", c.Old)
	defer clear(a)
	b, bErr := next.DecryptCredential("system:selected-dek-rotation", "system", "halro:selected-dek-rotation:v1", c.New)
	defer clear(b)
	if aErr != nil || bErr != nil || !bytes.Equal(plain, a) || !bytes.Equal(plain, b) || keyFingerprint(wanted) != i.NewFingerprint {
		clearRotationIntentKeys(&i)
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, aErr, bErr)
	}
	return &i, nil
}
func strictSelectedRotationJSON(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return authenticatedstate.ErrCorrupt
	}
	return nil
}
