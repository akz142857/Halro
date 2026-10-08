package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
)

const selectedSeedManifestVersion = 2

// These fields are independently authenticated approval facts. The complete
// source member/cut/root commitment stays immutable; target role is obtained
// later from its own authenticated Member and actual cold source verification.
type selectedSeedState struct {
	SourceDataDir      string                           `json:"source_data_dir"`
	SourceMetadataFile string                           `json:"source_metadata_file"`
	SourceMember       replication.MemberState          `json:"source_member"`
	SourceSeal         authenticatedstate.CandidateSeal `json:"source_seal"`
	MetadataHead       metadatajournal.Head             `json:"metadata_head"`
	BaselineDigest     [32]byte                         `json:"baseline_digest"`
	Tree               selectedSeedTree                 `json:"tree"`
}

func createSelectedSeedManifestLocked(ctx context.Context, cfg config.Config, target, output, user string, password []byte, totp string, guard *lock.Lock) (result SeedManifest, resultErr error) {
	resultErr = withSelectedSeedSampling(ctx, cfg, func(ctx context.Context) error {
		var err error
		result, err = createSelectedSeedManifestSampled(ctx, cfg, target, output, user, password, totp, guard)
		return err
	})
	return result, resultErr
}

func createSelectedSeedManifestSampled(ctx context.Context, cfg config.Config, target, output, user string, password []byte, totp string, guard *lock.Lock) (result SeedManifest, resultErr error) {
	pressure, err := commandPressure(ctx, cfg, nil)
	if err != nil {
		return result, err
	}
	manager := pressure.logicalBudget()
	if manager == nil {
		return result, authenticatedstate.ErrSealIneligible
	}
	master, unlock, err := unlockSelectedSeedMaster(ctx, cfg, manager)
	if err != nil {
		return result, err
	}
	defer clear(master)
	if unlock != nil {
		defer func() { resultErr = errors.Join(resultErr, unlock.Close()) }()
	}
	member, key, err := readMemberState(cfg, master)
	if err != nil {
		return result, err
	}
	defer clear(key[:])
	if member.Role != replication.RolePrimary || member.DurableIndex != member.ConfirmedIndex || member.ConfirmedIndex != member.AppliedIndex || !stateHasPeer(member, target) {
		return result, replication.ErrMemberAuthority
	}
	// Reauthenticate through the original selected DAO before publishing any
	// new source checkpoint. The reader joins before the cold full-cut owner.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reader, err := openHASelectedRuntimeFoundation(ctx, cfg, master, guard, manager, logger)
	if err != nil {
		return result, err
	}
	authErr := reader.prepareSelectedRoots(ctx)
	if authErr == nil {
		reader.metadataStore, authErr = reader.reopened.OpenHAMetadataStore(ctx)
	}
	if authErr == nil && unlock != nil {
		authErr = unlock.validateSelected(ctx, reader)
	}
	if authErr == nil {
		authErr = authenticateClusterOperatorMetadata(ctx, cfg, master, reader.metadataStore, user, password, totp)
	}
	if e := reader.Close(); e != nil {
		return result, &haSelectedRuntimeFoundationFailure{Owner: reader, Cause: errors.Join(authErr, e)}
	}
	if authErr != nil {
		return result, authErr
	}
	checkpoint, err := CheckpointHASelectedRuntime(ctx, cfg, master, guard, manager, logger)
	if err != nil {
		return result, err
	}
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, checkpoint.Close())
		}
	}()
	f := checkpoint.owner
	c := f.reopened.Owners.Components
	selection := selectedSeedState{SourceDataDir: cfg.Storage.DataDir, SourceMetadataFile: cfg.Storage.MetadataFile, SourceMember: f.replication.publisher.Snapshot(), SourceSeal: checkpoint.Publication.Seal, BaselineDigest: f.owners.Baseline.Descriptor().AuthorityDigest}
	selection.MetadataHead, err = c.Metadata.EpochHead()
	if err != nil {
		return result, err
	}
	if selection.SourceMember.DurableIndex != selection.SourceMember.AppliedIndex || selection.SourceSeal.OrderingIndex != selection.SourceMember.AppliedIndex {
		return result, authenticatedstate.ErrSealIneligible
	}
	if err = checkpoint.Close(); err != nil {
		return result, err
	}
	closed = true
	workspace, err := os.MkdirTemp(filepath.Dir(output), "halro-seed-catalog-")
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.Remove(workspace)) }()
	selection.Tree, err = hashSelectedSeedTree(ctx, cfg, cfg.Storage.DataDir, workspace, manager)
	if err != nil {
		return result, err
	}
	result = SeedManifest{Version: selectedSeedManifestVersion, ClusterID: member.ClusterID, Incarnation: member.Incarnation, SourceNode: member.NodeID, TargetNode: target, Term: member.Term, Index: member.AppliedIndex, OrderingHeadMAC: "sha256:" + hex.EncodeToString(member.OrderingHeadMAC[:]), Projection: member.Projection, ApprovedBy: user, ApprovedAt: time.Now().UTC(), Files: []SeedFile{}, Selected: &selection}
	mac, err := seedManifestMAC(result, key[:])
	if err != nil {
		return result, err
	}
	result.MAC = "sha256:" + hex.EncodeToString(mac[:])
	return result, writeSeedManifestContext(ctx, cfg, output, result)
}

func validateSelectedSeedDeclaration(cfg config.Config, m SeedManifest, key []byte) error {
	s := m.Selected
	if cfg.Replication == nil || cfg.ResourceBudget == nil || m.Version != 2 || s == nil || len(m.Files) != 0 || m.ClusterID != cfg.Replication.ClusterID || m.TargetNode != cfg.Replication.NodeID || m.TargetNode == m.SourceNode || m.ApprovedBy == "" || m.ApprovedAt.IsZero() || !filepath.IsAbs(s.SourceDataDir) || len(s.SourceDataDir) > 4096 || s.SourceMetadataFile != cfg.Storage.MetadataFile || s.Tree.Files == 0 || s.Tree.Digest == ([32]byte{}) || s.BaselineDigest == ([32]byte{}) {
		return authenticatedstate.ErrSealIneligible
	}
	member := s.SourceMember
	if member.Validate() != nil || member.Role != replication.RolePrimary || member.NodeID != m.SourceNode || member.ClusterID != m.ClusterID || member.Incarnation != m.Incarnation || member.Term != m.Term || member.PromisedTerm != m.Term || member.DurableIndex != m.Index || member.ConfirmedIndex != m.Index || member.AppliedIndex != m.Index || member.Projection != m.Projection || !stateHasPeer(member, m.TargetNode) || s.SourceSeal.Role != authenticatedstate.SealPrimary || s.SourceSeal.Member != sha256.Sum256([]byte(m.SourceNode)) || s.SourceSeal.Cluster != sha256.Sum256([]byte(m.ClusterID)) || s.SourceSeal.Term != m.Term || s.SourceSeal.OrderingIndex != m.Index || s.SourceSeal.OrderingMAC != member.OrderingHeadMAC || s.SourceSeal.RootCount != 7 {
		return authenticatedstate.ErrSealIneligible
	}
	found := false
	for _, p := range cfg.Replication.Peers {
		found = found || p.Name == m.SourceNode
	}
	if !found {
		return replication.ErrMemberAuthority
	}
	want, err := seedManifestMAC(m, key)
	if err != nil {
		return err
	}
	got, err := hex.DecodeString(m.MAC[min(len(m.MAC), len("sha256:")):])
	if err != nil || !hmac.Equal(got, want[:]) || m.MAC != "sha256:"+hex.EncodeToString(want[:]) {
		return fmt.Errorf("selected seed approval MAC: %w", authenticatedstate.ErrSealIneligible)
	}
	return nil
}

func selectedSeedRoute(cfg config.Config) (bool, error) {
	_, err := os.Lstat(haMemoryStateBootstrapPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
