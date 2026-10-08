package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"encoding/json"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/vault"
)

const haMemoryBootstrapCredential = "system:ha-memory-state-bootstrap"
const haMemoryBootstrapDomain = "halro:ha:memory-state:bootstrap:v1"

// This independently encrypted record binds one member's explicitly prepared
// namespace. It supplies no role, term, ordering cut, readiness or root grant:
// each use must obtain those from the actual Member/Ordering/native owners.
type haMemoryStateBootstrap struct {
	Version                                             uint8
	ClusterID, Incarnation, NodeID                      string
	SourceAuthorityDigest                               [32]byte
	Evidence                                            metadatastate.BaselineEvidence
	Binding                                             metadatajournal.BaselineBinding
	LedgerPath, AuditPath, GovernancePath, OrderingPath string
	MetadataRoot, MetadataJournal                       string
	LedgerKey, AuditKey, GovernanceKey                  []byte
	LegacyMetadataKey                                   []byte
}

func haMemoryStateBootstrapPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "ha-memory-state.bootstrap")
}

func haMemoryStateDirectory(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "ha-memory-state")
}

func (b *haMemoryStateBootstrap) clearKeys() {
	if b != nil {
		clear(b.LedgerKey)
		clear(b.AuditKey)
		clear(b.GovernanceKey)
		clear(b.LegacyMetadataKey)
	}
}

func (b *haMemoryStateBootstrap) validate(cfg config.Config, member replication.MemberState, authority *replication.MetadataBaselineAuthority, evidence metadatastate.BaselineEvidence) error {
	if b == nil || cfg.Replication == nil || authority == nil || member.Validate() != nil {
		return authenticatedstate.ErrSealIneligible
	}
	claims := authority.Claims()
	if b.Version != 2 || b.ClusterID != cfg.Replication.ClusterID || b.NodeID != cfg.Replication.NodeID || b.ClusterID != member.ClusterID || b.Incarnation != member.Incarnation || b.NodeID != member.NodeID || b.ClusterID != claims.ClusterID || b.Incarnation != claims.Incarnation || b.SourceAuthorityDigest != authority.Descriptor().AuthorityDigest || b.Evidence != evidence || b.Evidence.Identity != claims.New.Baseline.Identity || b.Evidence.KeyGeneration != claims.New.Baseline.KeyGeneration || b.Evidence.InstanceID == "" || b.Binding != claims.New.Baseline || b.LedgerPath != cfg.LedgerPath() || b.AuditPath != cfg.AuditPath() || b.GovernancePath != cfg.GovernancePath() || b.OrderingPath != cfg.OrderingJournalPath() || b.MetadataRoot != filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "metadata-root") || b.MetadataJournal != filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory, "metadata.journal") || len(b.LedgerKey) != 32 || len(b.AuditKey) != 32 || len(b.GovernanceKey) != 32 || len(b.LegacyMetadataKey) != 32 {
		return authenticatedstate.ErrSealIneligible
	}
	// Configuration paths are bounded before allocating an encoded bootstrap.
	for _, path := range []string{b.LedgerPath, b.AuditPath, b.GovernancePath, b.OrderingPath, b.MetadataRoot, b.MetadataJournal} {
		if len(path) < 1 || len(path) > 4096 {
			return authenticatedstate.ErrLimit
		}
	}
	return nil
}

// An unknown actual FD Close permanently retains its original bounded read
// owner. The encrypted bytes are never decrypted on that failure path.
type haMemoryBootstrapReadFailure struct {
	Cause error
	file  *os.File
	fee   resourcebudget.Permit
	raw   []byte
}

func (e *haMemoryBootstrapReadFailure) Error() string {
	return fmt.Sprintf("HA bootstrap reader retirement: %v", e.Cause)
}
func (e *haMemoryBootstrapReadFailure) Unwrap() error { return e.Cause }

func readHAMemoryStateBootstrap(ctx context.Context, cfg config.Config, v *vault.Vault, manager *resourcebudget.Manager, member replication.MemberState, authority *replication.MetadataBaselineAuthority, evidence metadatastate.BaselineEvidence) (result *haMemoryStateBootstrap, resultErr error) {
	return readHAMemoryStateBootstrapAt(ctx, cfg, haMemoryStateBootstrapPath(cfg), v, manager, member, authority, evidence)
}

func readHAMemoryStateBootstrapAt(ctx context.Context, cfg config.Config, path string, v *vault.Vault, manager *resourcebudget.Manager, member replication.MemberState, authority *replication.MetadataBaselineAuthority, evidence metadatastate.BaselineEvidence) (result *haMemoryStateBootstrap, resultErr error) {
	if path != haMemoryStateBootstrapPath(cfg) && path != haMemoryStateBootstrapPath(cfg)+".pending" {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if cfg.Replication == nil || v == nil || manager == nil || authority == nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > memoryStateBootstrapLimit {
		return nil, errors.Join(authenticatedstate.ErrCorrupt, err)
	}
	fee, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4 * memoryStateBootstrapLimit})
	if err != nil {
		return nil, err
	}
	debt := &haMemoryBootstrapReadFailure{fee: fee}
	defer func() {
		if debt.Cause != nil {
			result = nil
			resultErr = debt
			return
		}
		clear(debt.raw)
		resultErr = errors.Join(resultErr, fee.Release())
		if resultErr != nil && result != nil {
			result.clearKeys()
			result = nil
		}
	}()
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	debt.file = file
	actual, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, actual) {
		err = errors.Join(ownedfile.ErrReplaced, statErr)
	} else {
		debt.raw, err = io.ReadAll(io.LimitReader(file, memoryStateBootstrapLimit+1))
	}
	// Do not advance to any key/decoder use until the real read descriptor joined.
	if closeErr := ownedfile.CloseFile(file); closeErr != nil {
		debt.Cause = errors.Join(err, closeErr, ownedfile.ErrRetirement)
		return nil, debt
	}
	debt.file = nil
	if err != nil {
		return nil, err
	}
	named, nameErr := os.Lstat(path)
	if nameErr != nil || !named.Mode().IsRegular() || !os.SameFile(info, named) || named.Size() != info.Size() || !named.ModTime().Equal(info.ModTime()) || len(debt.raw) > memoryStateBootstrapLimit {
		return nil, errors.Join(ownedfile.ErrReplaced, nameErr)
	}
	plain, err := v.DecryptCredential(haMemoryBootstrapCredential, "system", haMemoryBootstrapDomain, debt.raw)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	if len(plain) > memoryStateBootstrapLimit {
		return nil, authenticatedstate.ErrLimit
	}
	b := &haMemoryStateBootstrap{}
	defer func() {
		if resultErr != nil {
			b.clearKeys()
		}
	}()
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(b); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, authenticatedstate.ErrCorrupt
	}
	if err := b.validate(cfg, member, authority, evidence); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b, nil
}
