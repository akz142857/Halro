package app

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

const haMemoryUnlockLimit = 64 << 10

// This carrier supplies only a candidate Vault key. Its identity must later
// agree with the actual authenticated Member, baseline carrier and selected
// Metadata. No field is a root, term, applied or readiness permission.
type haMemoryUnlockPayload struct {
	Version                                    uint8
	ClusterID, Incarnation, NodeID, InstanceID string
	SourceAuthorityDigest                      [32]byte
	Descriptor                                 masterkey.KeySlotDescriptor
	VaultCheck                                 []byte
	readOwner                                  *haMemoryUnlockOwner
}

func (p *haMemoryUnlockPayload) Close() error {
	if p == nil || p.readOwner == nil {
		return nil
	}
	o := p.readOwner
	if o.uncertain != nil {
		return &haMemoryUnlockReadFailure{Owner: o, Cause: o.uncertain}
	}
	clear(p.VaultCheck)
	p.VaultCheck = nil
	for i := range p.Descriptor.Slots {
		clear(p.Descriptor.Slots[i].WrappedKey)
	}
	p.Descriptor = masterkey.KeySlotDescriptor{}
	if err := o.Close(); err != nil {
		return &haMemoryUnlockReadFailure{Owner: o, Cause: err}
	}
	p.readOwner = nil
	return nil
}

type haMemoryUnlockEnvelope struct {
	Payload json.RawMessage
	MAC     [32]byte
}

func haMemoryUnlockPath(cfg config.Config) string {
	return filepath.Join(haMemoryStateDirectory(cfg), "unlock.json")
}

func haMemoryUnlockMAC(master, payload []byte) ([32]byte, error) {
	var result [32]byte
	if len(master) != 32 {
		return result, authenticatedstate.ErrSealIneligible
	}
	key, err := hkdf.Key(sha256.New, master, nil, "halro:ha:memory-state:unlock:v1", 32)
	if err != nil {
		return result, err
	}
	defer clear(key)
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(payload)
	copy(result[:], h.Sum(nil))
	return result, nil
}

type haMemoryUnlockOwner struct {
	file      *os.File
	owner     *ownedfile.Owner
	fee       resourcebudget.Permit
	raw       []byte
	uncertain error
	joined    bool
}

func (o *haMemoryUnlockOwner) Close() error {
	if o == nil {
		return nil
	}
	if o.uncertain != nil {
		return o.uncertain
	}
	if o.joined {
		return nil
	}
	if o.file != nil {
		if err := ownedfile.CloseFile(o.file); err != nil {
			o.uncertain = errors.Join(ownedfile.ErrRetirement, err)
			return o.uncertain
		}
		o.file = nil
	}
	clear(o.raw)
	o.raw = nil
	if o.fee != (resourcebudget.Permit{}) {
		// Disk survives with the actual exclusively created namespace. Unknown
		// FD retirement retains both its bytes and its original memory credit.
		var err error
		if o.owner == nil {
			err = o.fee.Release()
			if err == nil {
				o.fee = resourcebudget.Permit{}
			}
		} else {
			err = o.fee.ReleasePart(resourcebudget.Cost{RAM: 4 * haMemoryUnlockLimit})
		}
		if err != nil {
			o.uncertain = errors.Join(ownedfile.ErrRetirement, err)
			return o.uncertain
		}
	}
	o.joined = true
	return nil
}

// Called solely from the fresh, never-served activation owner, after actual
// full boundary/source/Vault qualification and fresh namespace creation.
func createHAMemoryUnlockCarrier(ctx context.Context, a *HAMemoryRuntimeActivation, master []byte, manager *resourcebudget.Manager, phase func(string) error) error {
	if a == nil || a.maintenance == nil || a.unlock != nil {
		return authenticatedstate.ErrSealIneligible
	}
	m := a.maintenance
	if m.cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return nil
	}
	owner := &haMemoryUnlockOwner{}
	a.unlock = owner
	var err error
	owner.fee, err = manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4 * haMemoryUnlockLimit, Disk: haMemoryUnlockLimit})
	if err != nil {
		return err
	}
	p := haMemoryUnlockPayload{Version: 1, ClusterID: a.bootstrap.ClusterID, Incarnation: a.bootstrap.Incarnation, NodeID: a.bootstrap.NodeID, InstanceID: a.bootstrap.Evidence.InstanceID, SourceAuthorityDigest: a.bootstrap.SourceAuthorityDigest}
	p.Descriptor, err = m.metadata.KeySlotDescriptor(ctx)
	if err != nil {
		return err
	}
	p.VaultCheck, err = m.metadata.VaultKeyCheck()
	if err != nil {
		return err
	}
	defer clear(p.VaultCheck)
	if _, err := p.primarySlot(m.cfg); err != nil {
		return err
	}
	fingerprint, err := masterkey.MasterKeyFingerprint(master)
	if err != nil || fingerprint != p.Descriptor.MasterKeyFingerprint {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if err := (envelopeCandidateVerifier{envelope: p.VaultCheck}).VerifyCandidate(ctx, master); err != nil {
		return err
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	defer clear(payload)
	mac, err := haMemoryUnlockMAC(master, payload)
	if err != nil {
		return err
	}
	owner.raw, err = json.Marshal(haMemoryUnlockEnvelope{Payload: payload, MAC: mac})
	if err != nil || len(owner.raw) > haMemoryUnlockLimit {
		return errors.Join(authenticatedstate.ErrLimit, err)
	}
	owner.file, owner.owner, err = ownedfile.Create(haMemoryUnlockPath(m.cfg), os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if phase != nil {
		if err := phase("unlock-carrier-created"); err != nil {
			return err
		}
	}
	if _, err := owner.file.Write(owner.raw); err != nil {
		return err
	}
	if err := owner.file.Sync(); err != nil {
		return err
	}
	if err := owner.Close(); err != nil {
		return err
	}
	return durable.SyncDirectory(haMemoryStateDirectory(m.cfg))
}

func (p *haMemoryUnlockPayload) primarySlot(cfg config.Config) (*masterkey.KeySlot, error) {
	if p == nil || cfg.Replication == nil || p.Version != 1 || p.ClusterID != cfg.Replication.ClusterID || p.NodeID != cfg.Replication.NodeID || len(p.Incarnation) < 1 || len(p.Incarnation) > 256 || len(p.InstanceID) < 1 || len(p.InstanceID) > 256 || p.SourceAuthorityDigest == ([32]byte{}) || len(p.VaultCheck) == 0 || len(p.VaultCheck) > haMemoryUnlockLimit || !p.Descriptor.ProductionReady() {
		return nil, authenticatedstate.ErrSealIneligible
	}
	// A fixed carrier profile for key-only protected payloads. Bound every
	// decoded field before canonical encoding or invoking the configured KMS.
	bytes := len(p.VaultCheck)
	for _, slot := range p.Descriptor.Slots {
		bytes += len(slot.WrappedKey) + len(slot.ID) + len(slot.Provider) + len(slot.KeyReference) + len(slot.Algorithm) + len(slot.MasterKeyFingerprint)
		for k, v := range slot.ProviderParameters {
			bytes += len(k) + len(v)
		}
		if len(slot.WrappedKey) > 8192 || bytes > haMemoryUnlockLimit/4 {
			return nil, authenticatedstate.ErrLimit
		}
	}
	for _, slot := range p.Descriptor.Slots {
		if slot.ID != cfg.Storage.MasterKey.PrimarySlot {
			continue
		}
		if slot.Purpose != masterkey.KeySlotPrimary || slot.State != masterkey.KeySlotActive || slot.VerifiedAt == nil {
			break
		}
		if _, err := trustedAllowedKMSKey(cfg.Storage.MasterKey, slot); err != nil {
			return nil, err
		}
		binding, err := payloadBindingForSlot(slot)
		if err != nil || binding.InstanceID != p.InstanceID {
			return nil, errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		return &slot, nil
	}
	return nil, authenticatedstate.ErrSealIneligible
}

func decodeHAMemoryUnlock(raw []byte) (*haMemoryUnlockPayload, haMemoryUnlockEnvelope, error) {
	var envelope haMemoryUnlockEnvelope
	decode := func(raw []byte, value any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(value); err != nil {
			return err
		}
		var extra any
		if err := d.Decode(&extra); err != io.EOF {
			return authenticatedstate.ErrCorrupt
		}
		return nil
	}
	if len(raw) < 1 || len(raw) > haMemoryUnlockLimit {
		return nil, envelope, authenticatedstate.ErrLimit
	}
	if err := decode(raw, &envelope); err != nil {
		return nil, envelope, err
	}
	p := &haMemoryUnlockPayload{}
	if err := decode(envelope.Payload, p); err != nil {
		return nil, envelope, err
	}
	return p, envelope, nil
}

type haMemoryUnlockReadFailure struct {
	Owner *haMemoryUnlockOwner
	Cause error
}

func (e *haMemoryUnlockReadFailure) Error() string {
	return fmt.Sprintf("HA unlock reader retirement: %v", e.Cause)
}
func (e *haMemoryUnlockReadFailure) Unwrap() error { return e.Cause }

func readHAMemoryUnlockBytes(ctx context.Context, cfg config.Config, manager *resourcebudget.Manager) (result *haMemoryUnlockOwner, resultErr error) {
	path := haMemoryUnlockPath(cfg)
	if err := validateRuntimeDerivedPath(ctx, cfg, path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > haMemoryUnlockLimit {
		return nil, errors.Join(authenticatedstate.ErrCorrupt, err)
	}
	o := &haMemoryUnlockOwner{}
	o.fee, err = manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4 * haMemoryUnlockLimit})
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr == nil {
			return
		}
		if err := o.Close(); err != nil {
			result = o
			resultErr = &haMemoryUnlockReadFailure{Owner: o, Cause: errors.Join(resultErr, err)}
		}
	}()
	o.file, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	actual, err := o.file.Stat()
	if err != nil || !os.SameFile(actual, info) {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	o.raw, err = io.ReadAll(io.LimitReader(o.file, haMemoryUnlockLimit+1))
	if err != nil {
		return nil, err
	}
	// Close before decoder/provider use; a failed Close never starts unwrap.
	if err := ownedfile.CloseFile(o.file); err != nil {
		o.uncertain = errors.Join(ownedfile.ErrRetirement, err)
		return nil, o.uncertain
	}
	o.file = nil
	named, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, named) || info.Size() != named.Size() || !info.ModTime().Equal(named.ModTime()) {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	return o, nil
}

func unlockHAMemoryCandidate(ctx context.Context, cfg config.Config, manager *resourcebudget.Manager, factory kmsWrapperFactory) (key []byte, payload *haMemoryUnlockPayload, resultErr error) {
	if cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots || manager == nil || factory == nil {
		return nil, nil, authenticatedstate.ErrSealIneligible
	}
	o, err := readHAMemoryUnlockBytes(ctx, cfg, manager)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		clear(o.raw)
		if resultErr == nil && payload != nil {
			payload.readOwner = o
			return
		}
		resultErr = errors.Join(resultErr, o.Close())
		if o.uncertain != nil {
			resultErr = &haMemoryUnlockReadFailure{Owner: o, Cause: resultErr}
		}
		if resultErr != nil {
			clear(key)
			key = nil
			payload = nil
		}
	}()
	p, envelope, err := decodeHAMemoryUnlock(o.raw)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if resultErr != nil {
			clear(p.VaultCheck)
		}
	}()
	slot, err := p.primarySlot(cfg)
	if err != nil {
		return nil, nil, err
	}
	key, err = (kmsSlotUnwrapper{masterKey: cfg.Storage.MasterKey, factory: factory}).Unwrap(ctx, *slot)
	if err != nil {
		return nil, nil, err
	}
	fingerprint, err := masterkey.MasterKeyFingerprint(key)
	mac, macErr := haMemoryUnlockMAC(key, envelope.Payload)
	if err != nil || macErr != nil || fingerprint != p.Descriptor.MasterKeyFingerprint || !hmac.Equal(mac[:], envelope.MAC[:]) {
		return key, nil, errors.Join(authenticatedstate.ErrSealIneligible, err, macErr)
	}
	if err := (envelopeCandidateVerifier{envelope: p.VaultCheck}).VerifyCandidate(ctx, key); err != nil {
		return key, nil, err
	}
	return key, p, nil
}

func (p *haMemoryUnlockPayload) validateSelected(ctx context.Context, f *haSelectedRuntimeFoundation) error {
	if f == nil {
		return authenticatedstate.ErrSealIneligible
	}
	return p.validateMetadata(ctx, f.cfg, f.bootstrap, f.metadataStore)
}

func (p *haMemoryUnlockPayload) validateMetadata(ctx context.Context, cfg config.Config, bootstrap haMemoryStateBootstrap, metadata *boltstore.Store) error {
	if p == nil || metadata == nil || bootstrap.ClusterID != p.ClusterID || bootstrap.Incarnation != p.Incarnation || bootstrap.NodeID != p.NodeID || bootstrap.Evidence.InstanceID != p.InstanceID || bootstrap.SourceAuthorityDigest != p.SourceAuthorityDigest {
		return authenticatedstate.ErrSealIneligible
	}
	descriptor, err := metadata.KeySlotDescriptor(ctx)
	if err != nil {
		return err
	}
	a, err := json.Marshal(descriptor)
	if err != nil {
		return err
	}
	b, err := json.Marshal(p.Descriptor)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return authenticatedstate.ErrSealIneligible
	}
	check, err := metadata.VaultKeyCheck()
	defer clear(check)
	if err != nil || !bytes.Equal(check, p.VaultCheck) {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	_, err = p.primarySlot(cfg)
	return err
}

// Explicit offline restart already has independently authenticated source
// keys and all seven roots. It verifies the retained carrier with that actual
// key, without invoking KMS or deriving authority from an interrupted header.
func verifyHAMemoryActivationUnlock(ctx context.Context, a *HAMemoryRuntimeActivation, master []byte, manager *resourcebudget.Manager) (resultErr error) {
	if a == nil || a.maintenance == nil {
		return authenticatedstate.ErrSealIneligible
	}
	m := a.maintenance
	if m.cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
		return nil
	}
	o, err := readHAMemoryUnlockBytes(ctx, m.cfg, manager)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, o.Close())
		if o.uncertain != nil {
			resultErr = &haMemoryUnlockReadFailure{Owner: o, Cause: resultErr}
		}
	}()
	p, envelope, err := decodeHAMemoryUnlock(o.raw)
	if err != nil {
		return err
	}
	defer clear(p.VaultCheck)
	if _, err := p.primarySlot(m.cfg); err != nil {
		return err
	}
	mac, err := haMemoryUnlockMAC(master, envelope.Payload)
	fingerprint, fpErr := masterkey.MasterKeyFingerprint(master)
	if err != nil || fpErr != nil || !hmac.Equal(mac[:], envelope.MAC[:]) || fingerprint != p.Descriptor.MasterKeyFingerprint {
		return errors.Join(authenticatedstate.ErrSealIneligible, err, fpErr)
	}
	return p.validateMetadata(ctx, m.cfg, a.bootstrap, m.metadata)
}
