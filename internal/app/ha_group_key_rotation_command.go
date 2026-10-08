package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

type HAGroupKeyRotationResult struct {
	OperationID string `json:"operation_id"`
	Members     int    `json:"members"`
	KeyRotationResult
}
type haGroupCommandOwner struct {
	guards   [3]*lock.Lock
	pressure [3]*resourcePressureRuntime
	keys     [3][]byte
	rotation *haGroupFileRotation
	fresh    [3]*haSelectedRuntimeFoundation
	debt     error
}
type haGroupCommandFailure struct {
	Owner *haGroupCommandOwner
	Cause error
}

func (e *haGroupCommandFailure) Error() string { return e.Cause.Error() }
func (e *haGroupCommandFailure) Unwrap() error { return e.Cause }
func (o *haGroupCommandOwner) Close() error {
	for _, f := range o.fresh {
		if f != nil {
			o.debt = errors.Join(o.debt, f.Close())
		}
	}
	if o.rotation != nil {
		o.debt = errors.Join(o.debt, o.rotation.Close())
	}
	// A failed actual owner remains attached to its original stopped namespace.
	if o.debt != nil {
		return o.debt
	}
	for _, g := range o.guards {
		if g != nil {
			o.debt = errors.Join(o.debt, g.Close())
		}
	}
	if o.debt == nil {
		for _, key := range o.keys {
			clear(key)
		}
	}
	return o.debt
}

// RotateHAGroupMasterKey is an offline whole-group transaction. The complete
// configured members must be stopped; their original TLS replication is used
// solely to confirm the journaled key-envelope changes. No ordinary HTTP or
// provider producer runs. Supplying the same operation resumes its fixed intent.
// Historical recovery capsules and native bridges are retained; this changes
// the active unlock and authenticated state, not cryptographic revocation of a
// compromised old DEK that can still decrypt its historical recovery material.
func RotateHAGroupMasterKey(ctx context.Context, cfgs []config.Config, newKeyFile, operation string) (HAGroupKeyRotationResult, error) {
	return rotateHAGroupMasterKey(ctx, cfgs, newKeyFile, operation, nil)
}
func rotateHAGroupMasterKey(ctx context.Context, cfgs []config.Config, newKeyFile, operation string, phase func(string) error) (HAGroupKeyRotationResult, error) {
	return rotateHAGroupMasterKeyWithPressureFactory(ctx, cfgs, newKeyFile, operation, phase, newResourcePressure)
}
func rotateHAGroupMasterKeyWithPressureFactory(ctx context.Context, cfgs []config.Config, newKeyFile, operation string, phase func(string) error, pressureFactory func(config.Config) (*resourcePressureRuntime, error)) (result HAGroupKeyRotationResult, resultErr error) {
	return rotateHAGroupKeyWithOptions(ctx, cfgs, newKeyFile, operation, phase, pressureFactory, nil)
}

// RotateHAGroupKMSMasterKey rewraps one shared DEK through the configured trusted
// primary and recovery slots, then confirms the same native Metadata transaction
// on every configured member before switching key-only unlock carriers.
func RotateHAGroupKMSMasterKey(ctx context.Context, cfgs []config.Config, operation string) (HAGroupKeyRotationResult, error) {
	options := kmsRotationOptions{operationID: operation, factory: defaultKMSWrapperFactory, random: rand.Reader, now: time.Now}
	return rotateHAGroupKeyWithOptions(ctx, cfgs, "", operation, nil, newResourcePressure, &options)
}

func rotateHAGroupKeyWithOptions(ctx context.Context, cfgs []config.Config, newKeyFile, operation string, phase func(string) error, pressureFactory func(config.Config) (*resourcePressureRuntime, error), kms *kmsRotationOptions) (result HAGroupKeyRotationResult, resultErr error) {
	if len(cfgs) < 2 || len(cfgs) > 3 || !validKMSOperationID(operation) {
		return result, authenticatedstate.ErrSealIneligible
	}
	o := &haGroupCommandOwner{}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) || errors.Is(resultErr, vault.ErrMasterKeyRetirement) {
			o.debt = errors.Join(o.debt, resultErr)
			if o.rotation != nil {
				o.rotation.debt = errors.Join(o.rotation.debt, resultErr)
			}
		}
		cleanup := o.Close()
		if cleanup != nil || errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &haGroupCommandFailure{Owner: o, Cause: errors.Join(resultErr, cleanup)}
		}
		if resultErr != nil {
			result = HAGroupKeyRotationResult{}
		}
	}()
	for n, cfg := range cfgs {
		if cfg.Replication == nil || cfg.ResourceBudget == nil || cfg.ResourcePressure == nil || (kms == nil && cfg.Storage.MasterKey.Mode != config.MasterKeyModeFile || kms != nil && cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots) || cfg.ResourceBudget.PhysicalTerminalReserve || !filepath.IsAbs(cfg.Storage.DataDir) || !filepath.IsAbs(haGroupKeyDestination(cfg)) {
			return result, authenticatedstate.ErrSealIneligible
		}
		for prior := 0; prior < n; prior++ {
			if cfg.Storage.DataDir == cfgs[prior].Storage.DataDir || haGroupKeyDestination(cfg) == haGroupKeyDestination(cfgs[prior]) {
				return result, authenticatedstate.ErrSealIneligible
			}
		}
		var err error
		o.guards[n], err = lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			return result, err
		}
		if err := rejectHAGroupPhysicalReceipts(cfg, o.guards[n]); err != nil {
			return result, err
		}
		o.pressure[n], err = pressureFactory(cfg)
		if err != nil {
			return result, err
		}
		if kms == nil {
			o.keys[n], _, err = vault.LoadMasterKeyWithIdentity(ctx, cfg.Storage.MasterKey.File)
			if err != nil {
				return result, err
			}
		}
	}
	var next []byte
	if kms == nil {
		var err error
		next, _, err = vault.LoadMasterKeyWithIdentity(ctx, newKeyFile)
		if err != nil {
			return result, err
		}
	}
	defer func() { clear(next) }()
	var sampled func(int, context.Context) error
	sampled = func(n int, ctx context.Context) (visitErr error) {
		if n < len(cfgs) {
			return o.pressure[n].monitor.WithSampling(ctx, func(ctx context.Context) error { return sampled(n+1, ctx) })
		}
		defer func() {
			if errors.Is(visitErr, ownedfile.ErrRetirement) || errors.Is(visitErr, vault.ErrMasterKeyRetirement) {
				o.debt = errors.Join(o.debt, visitErr)
				if o.rotation != nil {
					o.rotation.debt = errors.Join(o.rotation.debt, visitErr)
				}
			}
			visitErr = errors.Join(visitErr, o.Close())
		}()
		if kms != nil {
			for member, cfg := range cfgs {
				key, payload, err := unlockHAMemoryCandidate(ctx, cfg, o.pressure[member].logicalBudget(), kms.factory)
				if err != nil {
					return fmt.Errorf("member %s KMS source unlock: %w", cfg.Replication.NodeID, err)
				}
				o.keys[member] = key
				if err := payload.Close(); err != nil {
					return err
				}
			}
		}
		var err error
		r, p, err := loadHAGroupRotationResume(ctx, cfgs, o, next, operation)
		o.rotation = r
		if err != nil {
			return err
		}
		if kms != nil {
			if r != nil {
				next = bytes.Clone(r.intent.NewKey)
				r.kms = kms
			} else {
				next = make([]byte, 32)
				if _, err := io.ReadFull(kms.random, next); err != nil {
					return err
				}
			}
		}
		openCtx := ctx
		if p != nil {
			openCtx = context.WithValue(ctx, haGroupResumeKey{}, p)
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		var foundations []*haSelectedRuntimeFoundation
		for member, cfg := range cfgs {
			f, err := openHASelectedRuntimeFoundation(openCtx, cfg, o.keys[member], o.guards[member], o.pressure[member].logicalBudget(), logger)
			if f != nil {
				o.fresh[member] = f
			}
			if err != nil {
				return fmt.Errorf("group member %s source reopen: %w", cfg.Replication.NodeID, err)
			}
			if err := f.prepareSelectedRoots(openCtx); err != nil {
				return err
			}
			foundations = append(foundations, f)
		}
		if r == nil {
			for n := 1; n < len(cfgs); n++ {
				if !bytes.Equal(o.keys[0], o.keys[n]) {
					return authenticatedstate.ErrSealIneligible
				}
			}
			r, err = prepareHAGroupRotationWithKMS(ctx, foundations, o.keys[0], next, operation, kms, phase)
			o.rotation = r
			if err != nil {
				return fmt.Errorf("group intent and target preflight: %w", err)
			}
		} else {
			for n, f := range foundations {
				r.members[n] = f
				f.metadataStore, err = f.reopened.OpenHAMetadataStore(openCtx)
				if err != nil {
					return err
				}
			}
		}
		if p != nil {
			if err := r.preflightPublication(ctx); err != nil {
				return err
			}
			if err := r.completeMissingIntents(openCtx, p); err != nil {
				return err
			}
			for n := range cfgs {
				if err := r.inventoryRetainedIntent(n, o.pressure[n].logicalBudget()); err != nil {
					return err
				}
			}
		}
		for n := range o.fresh {
			o.fresh[n] = nil
		}
		r.phase = phase
		if err := r.callPhase(ctx, "intents_durable"); err != nil {
			return err
		}
		if err := r.execute(openCtx, logger); err != nil {
			return fmt.Errorf("group native confirmation and key publication: %w", err)
		}
		var verified []*haSelectedRuntimeFoundation
		for n, cfg := range cfgs {
			proofCtx := context.WithValue(ctx, haGroupKeyVerificationKey{}, &haGroupKeyVerification{rotation: r, member: n})
			fresh, err := openHASelectedRuntimeFoundation(proofCtx, cfg, next, o.guards[n], o.pressure[n].logicalBudget(), logger)
			if fresh != nil {
				o.fresh[n] = fresh
			}
			if err != nil {
				return err
			}
			if err := fresh.prepareSelectedRoots(proofCtx); err != nil {
				return err
			}
			fresh.metadataStore, err = fresh.reopened.OpenHAMetadataStore(proofCtx)
			if err != nil {
				return err
			}
			if kms != nil {
				actual, payload, err := unlockHAMemoryCandidate(proofCtx, cfg, o.pressure[n].logicalBudget(), kms.factory)
				if err != nil {
					return err
				}
				same := bytes.Equal(actual, next)
				clear(actual)
				if err = payload.validateSelected(proofCtx, fresh); err != nil {
					return errors.Join(err, payload.Close())
				}
				if err = payload.Close(); err != nil {
					return err
				}
				if !same {
					return authenticatedstate.ErrSealIneligible
				}
			}
			verified = append(verified, fresh)
		}
		if err := r.completeVerified(ctx, verified); err != nil {
			return err
		}
		result = HAGroupKeyRotationResult{OperationID: operation, Members: len(cfgs), KeyRotationResult: KeyRotationResult{OldFingerprint: r.intent.OldFingerprint, NewFingerprint: r.intent.NewFingerprint, OldKeyVersion: r.intent.Members[0].VaultVersion - 1, NewKeyVersion: r.intent.Members[0].VaultVersion, RecoveredPending: p != nil}}
		return r.callPhase(ctx, "group_completed")
	}
	resultErr = sampled(0, ctx)
	return
}

func loadHAGroupRotationResume(ctx context.Context, cfgs []config.Config, o *haGroupCommandOwner, next []byte, operation string) (r *haGroupFileRotation, p *haGroupResumeKeys, resultErr error) {
	order, err := haGroupResumeSlotOrder(ctx, cfgs[0], o.keys[0], operation, o.pressure[0].logicalBudget())
	if err != nil {
		return nil, nil, err
	}
	for _, slot := range order {
		path := haGroupRotationIntentPath(cfgs[0].Storage.DataDir, slot)
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		work, err := haGroupRotationIntentWorkspace(ctx, o.pressure[0].logicalBudget())
		if err != nil {
			return nil, nil, err
		}
		r = &haGroupFileRotation{work: work, slot: slot}
		raw, info, err := readHAGroupRotationBytes(path)
		if err != nil {
			return r, nil, err
		}
		var keys [haGroupRotationIntentSlots]haGroupHistoricalKey
		i, err := decodeHAGroupRotationIntent(raw, o.keys[0])
		historical := false
		if err != nil {
			keys, err = historicalHAGroupKeys(ctx, cfgs[0], o.keys[0])
			if err == nil {
				i, historical, err = decodeHAGroupWithHistory(raw, o.keys[0], keys)
			}
		}
		if err != nil {
			clear(raw)
			clearHAGroupHistoricalKeys(&keys)
			return r, nil, err
		}
		if i.OperationID != operation {
			doneErr := validateHAGroupCompletion(cfgs[0], o.keys[0], raw, i, historical, keys, slot)
			clear(raw)
			clearHAGroupHistoricalKeys(&keys)
			i.clearKeys()
			if doneErr != nil {
				return r, nil, errors.Join(ErrHAGroupKeyRotationPending, doneErr)
			}
			if err := r.Close(); err != nil {
				return r, nil, err
			}
			r = nil
			continue
		}
		clearHAGroupHistoricalKeys(&keys)
		if historical {
			clear(raw)
			i.clearKeys()
			return r, nil, ErrHAGroupKeyRotationPending
		}
		r.intent = i
		r.intentDigest = sha256.Sum256(raw)
		clear(raw)
		if i.OperationID != operation || next != nil && !bytes.Equal(i.NewKey, next) || int(i.Count) != len(cfgs) {
			return r, nil, ErrHAGroupKeyRotationPending
		}
		p = &haGroupResumeKeys{intent: i, digest: r.intentDigest, slot: slot, guards: o.guards}
		p.names[0] = info
		r.intentNames[0] = info
		for n, cfg := range cfgs {
			e := i.Members[n]
			if e.DataDir != cfg.Storage.DataDir || e.KeyFile != haGroupKeyDestination(cfg) || (i.Mode != "" && i.Mode != cfg.Storage.MasterKey.Mode) || e.Source.Member.NodeID != cfg.Replication.NodeID || !bytes.Equal(o.keys[n], i.OldKey) && !bytes.Equal(o.keys[n], i.NewKey) {
				return r, p, authenticatedstate.ErrSealIneligible
			}
			if n != 0 {
				raw, info, err := readHAGroupRotationBytes(haGroupRotationIntentPath(e.DataDir, slot))
				if errors.Is(err, os.ErrNotExist) {
					if !bytes.Equal(o.keys[n], i.OldKey) {
						return r, p, authenticatedstate.ErrSealIneligible
					}
					p.missing[n] = true
					continue
				}
				if err != nil {
					return r, p, err
				}
				same := sha256.Sum256(raw) == r.intentDigest
				clear(raw)
				if !same {
					return r, p, authenticatedstate.ErrSealIneligible
				}
				p.names[n] = info
				r.intentNames[n] = info
			}
		}
		return r, p, nil
	}
	return nil, nil, nil
}
