package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
	"os"
	"reflect"
)

func haGroupKeyDestination(cfg config.Config) string {
	if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		return haMemoryUnlockPath(cfg)
	}
	return cfg.Storage.MasterKey.File
}

// This is still a key-only carrier. Actual original selected Metadata and its
// independently qualified bootstrap establish identity and descriptor eligibility.
func qualifyHAGroupSourceUnlock(ctx context.Context, f *haSelectedRuntimeFoundation, key, raw []byte) error {
	p, e, err := decodeHAMemoryUnlock(raw)
	if err != nil {
		return err
	}
	defer clear(p.VaultCheck)
	if _, err = p.primarySlot(f.cfg); err != nil {
		return err
	}
	mac, err := haMemoryUnlockMAC(key, e.Payload)
	if err != nil || !hmac.Equal(mac[:], e.MAC[:]) {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	store := f.metadataStore
	if store == nil {
		store, err = f.reopened.OpenHAMetadataStore(ctx)
		if err != nil {
			return err
		}
		f.metadataStore = store
	}
	if err := p.validateMetadata(ctx, f.cfg, f.bootstrap, store); err != nil {
		return fmt.Errorf("unlock selected Metadata identity/descriptor/check: %w", err)
	}
	return nil
}
func readHAGroupOriginalKey(ctx context.Context, f *haSelectedRuntimeFoundation, old []byte, path string) ([]byte, os.FileInfo, error) {
	if f.cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return vault.LoadMasterKeyWithIdentity(ctx, path)
	}
	raw, info, err := readHAGroupRotationBytes(path)
	defer clear(raw)
	if err != nil {
		return nil, nil, err
	}
	if err = qualifyHAGroupSourceUnlock(ctx, f, old, raw); err != nil {
		return nil, nil, err
	}
	return bytes.Clone(old), info, nil
}
func (r *haGroupFileRotation) prepareKMSDescriptor(ctx context.Context) error {
	i := r.intent
	if i.Mode != config.MasterKeyModeKeySlots {
		if r.kms != nil {
			return authenticatedstate.ErrSealIneligible
		}
		return nil
	}
	if r.kms == nil {
		return authenticatedstate.ErrSealIneligible
	}
	primary := -1
	for n := 0; n < int(i.Count); n++ {
		if i.Members[n].Source.Member.Role == "primary" {
			primary = n
		}
	}
	if primary < 0 {
		return authenticatedstate.ErrSealIneligible
	}
	f := r.members[primary]
	ov, err := vault.New(i.OldKey)
	if err != nil {
		return err
	}
	defer ov.Close()
	nv, err := vault.New(i.NewKey)
	if err != nil {
		return err
	}
	defer nv.Close()
	i.TargetDescriptor, err = prepareSelectedRotationKMSDescriptor(ctx, f.cfg, f.metadataStore, ov, nv, i.NewKey, *r.kms)
	if err != nil {
		return err
	}
	i.TargetVaultCheck, err = nv.EncryptCredential(vaultKeyCheckID, vaultKeyCheckProvider, vaultKeyCheckAudience, []byte(vaultKeyCheckPlaintext))
	if err != nil {
		return err
	}
	for n := 0; n < int(i.Count); n++ {
		raw, err := encodeHAGroupTargetUnlock(r.members[n].cfg, i, n)
		if err != nil {
			return err
		}
		clear(raw)
		for _, slot := range i.TargetDescriptor.Slots {
			if _, err := trustedAllowedKMSKey(r.members[n].cfg.Storage.MasterKey, slot); err != nil {
				return err
			}
		}
	}
	return nil
}
func (r *haGroupFileRotation) bindKMSRewrite(o *boltstore.VaultRewrite, cfg config.Config) error {
	if r.intent.Mode != config.MasterKeyModeKeySlots {
		return nil
	}
	if r.kms == nil || r.intent.TargetDescriptor == nil || len(r.intent.TargetVaultCheck) == 0 {
		return authenticatedstate.ErrSealIneligible
	}
	o.KeySlotDescriptor = r.intent.TargetDescriptor
	o.VaultKeyCheck = r.intent.TargetVaultCheck
	o.Unwrapper = kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: r.kms.factory}
	o.Verifier = envelopeCandidateVerifier{envelope: r.intent.TargetVaultCheck}
	return nil
}
func encodeHAGroupTargetUnlock(cfg config.Config, i *haGroupRotationIntent, n int) ([]byte, error) {
	if i.TargetDescriptor == nil || n < 0 || n >= int(i.Count) {
		return nil, authenticatedstate.ErrSealIneligible
	}
	b := i.Members[n].Bootstrap
	p := haMemoryUnlockPayload{Version: 1, ClusterID: b.ClusterID, Incarnation: b.Incarnation, NodeID: b.NodeID, InstanceID: b.Evidence.InstanceID, SourceAuthorityDigest: b.SourceAuthorityDigest, Descriptor: *i.TargetDescriptor, VaultCheck: i.TargetVaultCheck}
	if _, err := p.primarySlot(cfg); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	mac, err := haMemoryUnlockMAC(i.NewKey, raw)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(haMemoryUnlockEnvelope{Payload: raw, MAC: mac})
	if len(encoded) > haMemoryUnlockLimit {
		return nil, authenticatedstate.ErrLimit
	}
	return encoded, err
}
func sameHAGroupTargetUnlock(cfg config.Config, i *haGroupRotationIntent, n int, raw []byte) bool {
	target, err := encodeHAGroupTargetUnlock(cfg, i, n)
	defer clear(target)
	if err != nil {
		return false
	}
	p, e, err := decodeHAMemoryUnlock(raw)
	if err != nil {
		return false
	}
	defer clear(p.VaultCheck)
	q, _, err := decodeHAMemoryUnlock(target)
	if err != nil {
		return false
	}
	defer clear(q.VaultCheck)
	mac, err := haMemoryUnlockMAC(i.NewKey, e.Payload)
	return err == nil && hmac.Equal(mac[:], e.MAC[:]) && reflect.DeepEqual(p, q)
}

// The intent can preserve key material only after the original authenticated
// unlock envelope agrees with the separately captured source identity. This
// validation grants no Member/Ordering/native/root permission on resume.
func validateHAGroupKMSIntent(i *haGroupRotationIntent) error {
	if i.Mode != config.MasterKeyModeKeySlots {
		for n := 0; n < int(i.Count); n++ {
			if len(i.Members[n].SourceUnlock) != 0 {
				return authenticatedstate.ErrSealIneligible
			}
		}
		return nil
	}
	for n := 0; n < int(i.Count); n++ {
		e := i.Members[n]
		p, envelope, err := decodeHAMemoryUnlock(e.SourceUnlock)
		if err != nil {
			return err
		}
		mac, err := haMemoryUnlockMAC(i.OldKey, envelope.Payload)
		if err != nil || !hmac.Equal(mac[:], envelope.MAC[:]) || p.Version != 1 || p.ClusterID != e.Bootstrap.ClusterID || p.NodeID != e.Bootstrap.NodeID || p.Incarnation != e.Bootstrap.Incarnation || p.InstanceID != e.Bootstrap.Evidence.InstanceID || p.SourceAuthorityDigest != e.Bootstrap.SourceAuthorityDigest || !p.Descriptor.ProductionReady() || p.Descriptor.MasterKeyFingerprint != i.OldFingerprint {
			clear(p.VaultCheck)
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		err = (envelopeCandidateVerifier{envelope: p.VaultCheck}).VerifyCandidate(context.Background(), i.OldKey)
		clear(p.VaultCheck)
		if err != nil {
			return err
		}
		if i.TargetDescriptor != nil {
			if err := i.TargetDescriptor.ValidateRotationSuccessor(p.Descriptor); err != nil {
				return err
			}
		}
	}
	if i.TargetDescriptor != nil {
		return (envelopeCandidateVerifier{envelope: i.TargetVaultCheck}).VerifyCandidate(context.Background(), i.NewKey)
	}
	return nil
}
