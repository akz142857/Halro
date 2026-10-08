package app

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

type selectedRotationKMSFactoryKey struct{}

func selectedRotationKMSFactory(ctx context.Context) kmsWrapperFactory {
	if factory, ok := ctx.Value(selectedRotationKMSFactoryKey{}).(kmsWrapperFactory); ok && factory != nil {
		return factory
	}
	return defaultKMSWrapperFactory
}

func rotateSelectedKMSMasterKey(ctx context.Context, cfg config.Config, options kmsRotationOptions, sample resourcebudget.SampleSource) (KeyRotationResult, error) {
	if !validKMSOperationID(options.operationID) {
		return KeyRotationResult{}, errors.New("selected KMS DEK rotation requires a valid operation ID")
	}
	if options.factory == nil {
		options.factory = defaultKMSWrapperFactory
	}
	if options.random == nil {
		options.random = rand.Reader
	}
	if options.now == nil {
		options.now = time.Now
	}
	return rotateSelectedKeyWithAuthority(ctx, cfg, "", options.hook, sample, &options)
}

func unlockSelectedRotationKeys(ctx context.Context, cfg config.Config, nextFile string, options *kmsRotationOptions) (active, next []byte, resultErr error) {
	if options == nil {
		current, err := fileMasterKeyStore(cfg)
		if err != nil {
			return nil, nil, err
		}
		oldPath, err := filepath.Abs(current.Path())
		if err != nil {
			return nil, nil, err
		}
		nextPath, err := filepath.Abs(nextFile)
		if err != nil || oldPath == nextPath {
			return nil, nil, errors.Join(errors.New("new key file must differ from active key"), err)
		}
		store, err := masterkey.NewFileStore(nextPath)
		if err != nil {
			return nil, nil, err
		}
		active, err = current.Unlock(ctx)
		if err != nil {
			return nil, nil, err
		}
		next, err = store.Unlock(ctx)
		if err != nil {
			clear(active)
			return nil, nil, err
		}
		return active, next, nil
	}
	legacy, err := openSelectedRotationDB(ctx, cfg, cfg.MetadataPath(), false)
	if err != nil {
		return nil, nil, err
	}
	active, err = unlockKMSMasterKey(ctx, cfg, legacy, masterkey.KeySlotPrimary, options.factory)
	keyring, ringErr := legacy.VaultKeyring()
	if ringErr != nil {
		clear(active)
		return nil, nil, errors.Join(err, ringErr, legacy.Close())
	}
	if err = errors.Join(err, legacy.Close()); err != nil {
		clear(active)
		return nil, nil, err
	}
	if _, err := os.Lstat(selectedRotationIntentPath(cfg)); err == nil {
		return active, nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		clear(active)
		return nil, nil, err
	}
	if keyring.RotationOperationID == options.operationID && len(keyring.RecoveryEnvelope) == 0 {
		return active, nil, nil
	}
	next = make([]byte, vault.MasterKeySize)
	if _, err := io.ReadFull(options.random, next); err != nil {
		clear(active)
		clear(next)
		return nil, nil, err
	}
	return active, next, nil
}

func prepareSelectedRotationKMSDescriptor(ctx context.Context, cfg config.Config, latest *boltstore.Store, old, next *vault.Vault, newKey []byte, options kmsRotationOptions) (*masterkey.KeySlotDescriptor, error) {
	previous, err := latest.KeySlotDescriptor(ctx)
	if err != nil || !previous.ProductionReady() {
		return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if err := verifyVaultKeyCheck(latest, old); err != nil {
		return nil, err
	}
	instance, err := descriptorInstanceID(previous)
	if err != nil {
		return nil, err
	}
	rotated, err := masterkey.NewRotatedKeySlotDescriptor(previous, keyFingerprint(newKey))
	if err != nil {
		return nil, err
	}
	check, err := next.EncryptCredential(vaultKeyCheckID, vaultKeyCheckProvider, vaultKeyCheckAudience, []byte(vaultKeyCheckPlaintext))
	if err != nil {
		return nil, err
	}
	defer clear(check)
	verifier := envelopeCandidateVerifier{envelope: check}
	unwrapper := kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: options.factory}
	transition := options.now().UTC()
	for _, target := range []struct {
		id      string
		purpose masterkey.KeySlotPurpose
	}{{cfg.Storage.MasterKey.PrimarySlot, masterkey.KeySlotPrimary}, {cfg.Storage.MasterKey.RecoverySlot, masterkey.KeySlotRecovery}} {
		pending, err := wrapPendingSlot(ctx, cfg.Storage.MasterKey, options.factory, instance, target.id, target.purpose, newKey)
		if err != nil {
			return nil, err
		}
		transition = transition.Add(time.Nanosecond)
		updated, _, err := rotated.AddSlot(pending, rotated.Revision, transition)
		clear(pending.WrappedKey)
		if err != nil {
			return nil, err
		}
		rotated = updated
		slot, _ := keySlotByID(rotated, target.id)
		transition = transition.Add(time.Nanosecond)
		rotated, _, err = rotated.VerifySlot(ctx, target.id, rotated.Revision, slot.Revision, unwrapper, verifier, transition)
		if err != nil {
			return nil, err
		}
	}
	if err := rotated.ValidateRotationSuccessor(previous); err != nil {
		return nil, err
	}
	return &rotated, nil
}
