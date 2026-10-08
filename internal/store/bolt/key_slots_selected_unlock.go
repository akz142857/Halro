package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/masterkey"
)

// CopyKeySlotUnlockDescriptorFromSelected mirrors only DEK-preserving changes
// from an actual authenticated facade into the independently retained unlock
// database. It cannot install a supplied descriptor or rotate Vault material.
func (s *Store) CopyKeySlotUnlockDescriptorFromSelected(ctx context.Context, selected *Store) error {
	if s == nil || selected == nil || s.AuthenticatedMetadata() || !selected.AuthenticatedMetadata() {
		return errors.New("unlock descriptor mirror requires an ordinary destination and authenticated selected source")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var check, encoded []byte
	var descriptor masterkey.KeySlotDescriptor
	var ring VaultKeyring
	err := selected.view(func(tx *Tx) error {
		meta := tx.Bucket(bucketMeta)
		check = bytes.Clone(meta.Get(keyVaultCheck))
		raw := meta.Get(keyKeySlotDescriptor)
		if len(check) == 0 || len(raw) == 0 || len(raw) > 1<<20 {
			return errors.New("selected unlock material is absent or exceeds its fixed bound")
		}
		encoded = bytes.Clone(raw)
		if err := json.Unmarshal(encoded, &descriptor); err != nil {
			return err
		}
		if err := descriptor.Validate(); err != nil {
			return err
		}
		if err := json.Unmarshal(meta.Get(keyVaultKeyring), &ring); err != nil {
			return err
		}
		if err := ring.Validate(); err != nil {
			return err
		}
		if descriptor.MasterKeyFingerprint != ring.ActiveFingerprint || descriptor.ActiveGeneration != ring.ActiveKeyVersion {
			return errors.New("selected descriptor and Vault generation differ")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.updateContext(ctx, func(tx *Tx) error {
		meta := tx.Bucket(bucketMeta)
		if !bytes.Equal(meta.Get(keyVaultCheck), check) {
			return errors.New("unlock mirror cannot change the original DEK")
		}
		var currentRing VaultKeyring
		if err := json.Unmarshal(meta.Get(keyVaultKeyring), &currentRing); err != nil {
			return err
		}
		if err := currentRing.Validate(); err != nil {
			return err
		}
		if currentRing.ActiveFingerprint != ring.ActiveFingerprint || currentRing.ActiveKeyVersion != ring.ActiveKeyVersion {
			return errors.New("unlock mirror Vault generation changed")
		}
		raw := meta.Get(keyKeySlotDescriptor)
		var current masterkey.KeySlotDescriptor
		if err := json.Unmarshal(raw, &current); err != nil {
			return err
		}
		if err := current.Validate(); err != nil {
			return err
		}
		if current.MasterKeyFingerprint != descriptor.MasterKeyFingerprint || current.ActiveGeneration != descriptor.ActiveGeneration || current.Revision > descriptor.Revision {
			return errors.New("unlock mirror cannot change or roll back the original generation")
		}
		if bytes.Equal(raw, encoded) {
			return nil
		}
		if current.Revision == descriptor.Revision {
			return errors.New("equal unlock descriptor revisions have different contents")
		}
		return meta.Put(keyKeySlotDescriptor, encoded)
	})
}
