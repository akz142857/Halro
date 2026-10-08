package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

const haGroupRotationIntentLimit = 256 << 10
const haGroupRotationIntentSlots = 4
const haGroupRotationIntentDomain = "halro:ha:group:key-rotation:intent:v1"

type haGroupRotationMemberIntent struct {
	DataDir, KeyFile string
	Source           statecut.HAKeyRotationSource
	Bootstrap        haMemoryStateBootstrap
	VaultVersion     uint64
	SourceUnlock     []byte
	SourceHistory    []byte
	History          [haGroupRotationIntentSlots]haGroupHistoricalKey
}

type haGroupRotationIntent struct {
	Mode             string
	TargetDescriptor *masterkey.KeySlotDescriptor
	TargetVaultCheck []byte
	Version          uint8
	Count            uint8
	OperationID      string
	OldFingerprint   string
	NewFingerprint   string
	OldKey, NewKey   []byte
	ClusterKey       [32]byte
	Members          [3]haGroupRotationMemberIntent
}

func (i *haGroupRotationIntent) clearKeys() {
	clear(i.OldKey)
	clear(i.NewKey)
	clear(i.ClusterKey[:])
	clear(i.TargetVaultCheck)
	for n := range i.Members {
		clear(i.Members[n].SourceUnlock)
		clear(i.Members[n].SourceHistory)
		clearHAGroupHistoricalKeys(&i.Members[n].History)
	}
	for n := range i.Members {
		i.Members[n].Bootstrap.clearKeys()
	}
}

func haGroupRotationIntentPath(dataDir string, slot int) string {
	return filepath.Join(dataDir, fmt.Sprintf("ha-group-rotation-%d.intent", slot+1))
}

// captureHAGroupRotationIntent accepts actual stopped foundations, not caller
// root structures. Every member independently authenticates all sources and
// seven roots before the fixed complete configured group is checked. Pending
// Ledger or Prepared pin obligations require original-key recovery first.
func captureHAGroupRotationIntent(ctx context.Context, foundations []*haSelectedRuntimeFoundation, old, next []byte, operation string) (intent *haGroupRotationIntent, resultErr error) {
	if len(foundations) < 2 || len(foundations) > 3 || len(old) != 32 || len(next) != 32 || bytes.Equal(old, next) || !validKMSOperationID(operation) {
		return nil, errors.New("HA group key rotation requires two or three complete members, distinct DEKs and an operation ID")
	}
	i := &haGroupRotationIntent{Version: 1, Count: uint8(len(foundations)), OperationID: operation, OldFingerprint: keyFingerprint(old), NewFingerprint: keyFingerprint(next), OldKey: bytes.Clone(old), NewKey: bytes.Clone(next)}
	defer func() {
		if resultErr != nil {
			i.clearKeys()
		}
	}()
	for n, f := range foundations {
		if f == nil || f.ctx.Err() != nil || f.reopened == nil || f.replication == nil || f.replication.manager != nil || f.business != nil || f.promotionRecovery != nil || f.peerPromotionRecovery != nil || f.cfg.Storage.MasterKey.Mode != config.MasterKeyModeFile && f.cfg.Storage.MasterKey.Mode != config.MasterKeyModeKeySlots {
			return nil, authenticatedstate.ErrSealIneligible
		}
		actual, key, err := readMemberState(f.cfg, old)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			i.ClusterKey = key
		} else if key != i.ClusterKey {
			clear(key[:])
			return nil, errors.New("HA group members do not share the original cluster authentication key")
		}
		clear(key[:])
		source, err := statecut.CaptureHAKeyRotationSource(ctx, f.reopened.Owners)
		if err != nil {
			return nil, fmt.Errorf("capture group member %s actual seven roots: %w", actual.NodeID, err)
		}
		if !reflect.DeepEqual(source.Member, actual) {
			return nil, errors.New("HA group Member changed during source capture")
		}
		if err := f.reopened.Owners.Components.Ledger.WithRecoveryView(ctx, func(v ledger.AuthenticatedRecoveryView) error {
			p := source.Observed.Positions[0]
			if v.Head().Head.Generation != p.Generation || v.Head().Head.Sequence != p.Sequence || v.Head().Head.Offset != int64(p.Offset) || v.Head().Hash != p.Hash {
				return ledger.ErrProjectionSource
			}
			limits := ledger.ProjectionPageLimits{Rows: 1, Bytes: 4096}
			pending, err := v.PendingLeasePage(ctx, "", limits)
			if err != nil || !pending.Done || len(pending.Items) != 0 {
				return errors.Join(errors.New("group rotation requires all accepted leases to finish under the original key"), err)
			}
			requests, err := v.UnfinalizedRequestPage(ctx, "", limits)
			if err != nil || !requests.Done || len(requests.Items) != 0 {
				return errors.Join(errors.New("group rotation requires all accepted requests to finish under the original key"), err)
			}
			return v.Complete(ctx)
		}); err != nil {
			return nil, fmt.Errorf("capture group member %s actual Ledger obligations: %w", actual.NodeID, err)
		}
		dataDir, err := filepath.Abs(f.cfg.Storage.DataDir)
		if err != nil {
			return nil, err
		}
		keyFile, err := filepath.Abs(haGroupKeyDestination(f.cfg))
		if err != nil {
			return nil, err
		}
		bootstrap := f.bootstrap
		bootstrap.LedgerKey = bytes.Clone(bootstrap.LedgerKey)
		bootstrap.AuditKey = bytes.Clone(bootstrap.AuditKey)
		bootstrap.GovernanceKey = bytes.Clone(bootstrap.GovernanceKey)
		bootstrap.LegacyMetadataKey = bytes.Clone(bootstrap.LegacyMetadataKey)
		var ring boltstore.VaultKeyring
		if err := f.reopened.Owners.Components.Metadata.View(ctx, func(tx *metadatastate.Transaction) error {
			raw, found, err := tx.Get([]string{"meta"}, []byte("vault_keyring"))
			if err != nil {
				return err
			}
			if !found {
				return authenticatedstate.ErrCorrupt
			}
			if err := json.Unmarshal(raw, &ring); err != nil {
				return err
			}
			if ring.Validate() != nil || ring.ActiveFingerprint != i.OldFingerprint || ring.ActiveKeyVersion == ^uint64(0) || len(ring.RecoveryEnvelope) != 0 {
				return authenticatedstate.ErrSealIneligible
			}
			return nil
		}); err != nil {
			bootstrap.clearKeys()
			return nil, err
		}
		if n == 0 {
			i.Mode = f.cfg.Storage.MasterKey.Mode
		} else if i.Mode != f.cfg.Storage.MasterKey.Mode {
			bootstrap.clearKeys()
			return nil, authenticatedstate.ErrSealIneligible
		}
		i.Members[n] = haGroupRotationMemberIntent{DataDir: dataDir, KeyFile: keyFile, Source: source, Bootstrap: bootstrap, VaultVersion: ring.ActiveKeyVersion + 1}
		if i.Mode == config.MasterKeyModeKeySlots {
			raw, _, err := readHAGroupRotationBytes(keyFile)
			if err != nil {
				return nil, err
			}
			if err := qualifyHAGroupSourceUnlock(ctx, f, old, raw); err != nil {
				clear(raw)
				return nil, fmt.Errorf("member %s source unlock qualification: %w", actual.NodeID, err)
			}
			i.Members[n].SourceUnlock = raw
		}
	}
	if err := i.validateMembers(); err != nil {
		return nil, err
	}
	return i, nil
}

func (i *haGroupRotationIntent) validateMembers() error {
	if i == nil || i.Version != 1 || i.Count < 2 || i.Count > 3 || !validKMSOperationID(i.OperationID) || len(i.OldKey) != 32 || len(i.NewKey) != 32 || i.OldFingerprint != keyFingerprint(i.OldKey) || i.NewFingerprint != keyFingerprint(i.NewKey) || i.OldFingerprint == i.NewFingerprint || i.ClusterKey == ([32]byte{}) {
		return authenticatedstate.ErrSealIneligible
	}
	if i.Mode != "" && i.Mode != config.MasterKeyModeFile && i.Mode != config.MasterKeyModeKeySlots {
		return authenticatedstate.ErrSealIneligible
	}
	if i.Mode != config.MasterKeyModeKeySlots && (i.TargetDescriptor != nil || len(i.TargetVaultCheck) != 0) {
		return authenticatedstate.ErrSealIneligible
	}
	if i.TargetDescriptor != nil && (!i.TargetDescriptor.ProductionReady() || i.TargetDescriptor.MasterKeyFingerprint != i.NewFingerprint || len(i.TargetVaultCheck) == 0 || len(i.TargetVaultCheck) > haMemoryUnlockLimit) {
		return authenticatedstate.ErrSealIneligible
	}
	if err := validateHAGroupKMSIntent(i); err != nil {
		return err
	}
	if err := validateHAGroupIntentHistory(i); err != nil {
		return err
	}
	primary := 0
	first := i.Members[0].Source
	for n := 0; n < int(i.Count); n++ {
		entry, m := i.Members[n], i.Members[n].Source.Member
		if err := validateHAGroupRotationCapturedEntry(entry); err != nil {
			return fmt.Errorf("validate group member %s captured identity: %w", m.NodeID, err)
		}
		if !filepath.IsAbs(entry.DataDir) || !filepath.IsAbs(entry.KeyFile) || len(entry.DataDir) > 4096 || len(entry.KeyFile) > 4096 || m.Validate() != nil || len(m.Peers) != int(i.Count)-1 || m.ClusterID != first.Member.ClusterID || m.Incarnation != first.Member.Incarnation || m.Term != first.Member.Term || m.Term != m.PromisedTerm || m.DurableIndex != first.Member.DurableIndex || m.ConfirmedIndex != m.DurableIndex || m.AppliedIndex != m.DurableIndex || m.Projection.Index != m.AppliedIndex || m.OrderingHeadMAC != first.Member.OrderingHeadMAC || entry.Source.BaselineDigest != first.BaselineDigest || entry.Source.Cursors != first.Cursors || entry.Source.Heads != first.Heads {
			return errors.New("HA group key rotation requires one complete confirmed and applied same-term group")
		}
		if m.Role == replication.RolePrimary {
			primary++
		} else if m.Role != replication.RoleReplica {
			return replication.ErrMemberAuthority
		}
		for previous := 0; previous < n; previous++ {
			if entry.DataDir == i.Members[previous].DataDir || entry.KeyFile == i.Members[previous].KeyFile || m.NodeID == i.Members[previous].Source.Member.NodeID {
				return errors.New("HA group member or physical key/data namespace is duplicated")
			}
		}
		for _, peer := range m.Peers {
			found := false
			for other := 0; other < int(i.Count); other++ {
				if other != n && i.Members[other].Source.Member.NodeID == peer.Name {
					found = true
				}
			}
			if !found {
				return errors.New("HA group rotation omitted a configured peer")
			}
		}
	}
	if primary != 1 {
		return replication.ErrMemberAuthority
	}
	return nil
}

func encodeHAGroupRotationIntent(i *haGroupRotationIntent) ([]byte, error) {
	if i != nil && i.Mode == config.MasterKeyModeKeySlots && (i.TargetDescriptor == nil || len(i.TargetVaultCheck) == 0) {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if err := i.validateMembers(); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	if len(plain) > haGroupRotationIntentLimit/2-512 {
		return nil, authenticatedstate.ErrLimit
	}
	var capsule selectedRotationCapsule
	defer func() { clear(capsule.Old); clear(capsule.New) }()
	capsule.Version = 1
	for n, key := range [][]byte{i.OldKey, i.NewKey} {
		v, err := vault.New(key)
		if err != nil {
			return nil, err
		}
		sealed, err := v.EncryptCredential("system:ha-group-key-rotation", "system", haGroupRotationIntentDomain, plain)
		v.Close()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			capsule.Old = sealed
		} else {
			capsule.New = sealed
		}
	}
	raw, err := json.Marshal(capsule)
	if len(raw) > haGroupRotationIntentLimit {
		return nil, authenticatedstate.ErrLimit
	}
	return raw, err
}

func decodeHAGroupRotationIntent(raw, key []byte) (*haGroupRotationIntent, error) {
	if len(raw) == 0 || len(raw) > haGroupRotationIntentLimit || len(key) != 32 {
		return nil, authenticatedstate.ErrLimit
	}
	var capsule selectedRotationCapsule
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&capsule); err != nil || capsule.Version != 1 {
		return nil, errors.Join(authenticatedstate.ErrCorrupt, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, authenticatedstate.ErrCorrupt
	}
	v, err := vault.New(key)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	for _, sealed := range [][]byte{capsule.Old, capsule.New} {
		plain, err := v.DecryptCredential("system:ha-group-key-rotation", "system", haGroupRotationIntentDomain, sealed)
		if err != nil {
			continue
		}
		result := &haGroupRotationIntent{}
		decoder := json.NewDecoder(bytes.NewReader(plain))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(result)
		if err == nil {
			if trailing := decoder.Decode(new(any)); trailing != io.EOF {
				err = authenticatedstate.ErrCorrupt
			}
		}
		clear(plain)
		if err == nil {
			err = result.validateMembers()
		}
		if err == nil && keyFingerprint(key) != result.OldFingerprint && keyFingerprint(key) != result.NewFingerprint {
			err = authenticatedstate.ErrSealIneligible
		}
		if err != nil {
			result.clearKeys()
			return nil, err
		}
		return result, nil
	}
	return nil, errors.New("HA group rotation intent does not authenticate under the supplied DEK")
}

// These capsule helpers allocate only the declared fixed command workspace.
// The caller must keep the original job alive through actual file IO/Close.
func haGroupRotationIntentWorkspace(ctx context.Context, manager *resourcebudget.Manager) (resourcebudget.Permit, error) {
	return manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4 * haGroupRotationIntentLimit})
}

func haGroupRotationIntentDigest(raw []byte) [32]byte { return sha256.Sum256(raw) }
