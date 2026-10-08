package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/vault"
)

type selectedSeedWriteFailure struct {
	File  *os.File
	Owner *ownedfile.Owner
	Raw   []byte
	Cause error
}

func (e *selectedSeedWriteFailure) Error() string { return e.Cause.Error() }
func (e *selectedSeedWriteFailure) Unwrap() error { return e.Cause }

func writeSelectedSeedBoundFile(ctx context.Context, path string, raw []byte) (resultErr error) {
	file, owner, err := ownedfile.Create(path, os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if e := ownedfile.CloseFile(file); e != nil {
			resultErr = &selectedSeedWriteFailure{file, owner, bytes.Clone(raw), errors.Join(resultErr, e, ownedfile.ErrRetirement)}
		}
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	if _, err = file.Write(raw); err != nil {
		return err
	}
	return errors.Join(file.Sync(), owner.VerifyFile(file))
}

func bindSelectedSeedBootstrap(ctx context.Context, stage, final config.Config, m SeedManifest, b *haMemoryStateBootstrap, authority *replication.MetadataBaselineAuthority, member replication.MemberState, master []byte, v *vault.Vault, archiveSuffix string) error {
	if b == nil || authority == nil || member.Role != replication.RoleReplica || member.NodeID != m.TargetNode || m.Selected == nil {
		return authenticatedstate.ErrSealIneligible
	}
	if final.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
		raw, err := readBoundedRegularFile(ctx, filepath.Join(haMemoryStateDirectory(stage)+archiveSuffix, "unlock.json"), haMemoryUnlockLimit)
		if err != nil {
			return err
		}
		p, envelope, err := decodeHAMemoryUnlock(raw)
		clear(raw)
		if err != nil {
			return err
		}
		defer clear(p.VaultCheck)
		mac, err := haMemoryUnlockMAC(master, envelope.Payload)
		if err != nil || mac != envelope.MAC || p.NodeID != m.SourceNode || p.ClusterID != member.ClusterID || p.Incarnation != member.Incarnation || p.InstanceID != b.Evidence.InstanceID || p.SourceAuthorityDigest != b.SourceAuthorityDigest {
			return errors.Join(authenticatedstate.ErrSealIneligible, err)
		}
		p.NodeID = member.NodeID
		if _, err = p.primarySlot(final); err != nil {
			return err
		}
		payload, err := json.Marshal(p)
		if err != nil {
			return err
		}
		defer clear(payload)
		mac, err = haMemoryUnlockMAC(master, payload)
		if err != nil {
			return err
		}
		raw, err = json.Marshal(haMemoryUnlockEnvelope{Payload: payload, MAC: mac})
		if err != nil || len(raw) > haMemoryUnlockLimit {
			return errors.Join(authenticatedstate.ErrLimit, err)
		}
		err = writeSelectedSeedBoundFile(ctx, haMemoryUnlockPath(stage), raw)
		clear(raw)
		if err != nil {
			return err
		}
	}
	b.NodeID = member.NodeID
	b.LedgerPath = final.LedgerPath()
	b.AuditPath = final.AuditPath()
	b.GovernancePath = final.GovernancePath()
	b.OrderingPath = final.OrderingJournalPath()
	b.MetadataRoot = filepath.Join(final.Storage.DataDir, HAMetadataBaselineDirectory, "metadata-root")
	b.MetadataJournal = filepath.Join(final.Storage.DataDir, HAMetadataBaselineDirectory, "metadata.journal")
	if err := b.validate(final, member, authority, b.Evidence); err != nil {
		return err
	}
	plain, err := json.Marshal(b)
	if err != nil || len(plain) > memoryStateBootstrapLimit {
		return errors.Join(authenticatedstate.ErrLimit, err)
	}
	defer clear(plain)
	raw, err := v.EncryptCredential(haMemoryBootstrapCredential, "system", haMemoryBootstrapDomain, plain)
	if err != nil {
		return err
	}
	defer clear(raw)
	if len(raw) > memoryStateBootstrapLimit {
		return authenticatedstate.ErrLimit
	}
	// Preserve the independently approved source credentials; no unknown name
	// is overwritten or deleted during target rebinding.
	old := haMemoryStateBootstrapPath(stage)
	if err = renameMigrationWithoutReplace(old, old+archiveSuffix); err != nil {
		return err
	}
	if err = writeSelectedSeedBoundFile(ctx, old, raw); err != nil {
		return err
	}
	got, err := readBoundedRegularFile(ctx, old, memoryStateBootstrapLimit)
	if err != nil {
		return err
	}
	defer clear(got)
	if !bytes.Equal(raw, got) {
		return ownedfile.ErrReplaced
	}
	return durable.SyncDirectory(stage.Storage.DataDir)
}
