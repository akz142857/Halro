package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

// MemberSeedOriginReport proves a frozen replacement member's transition
// baseline was built from the exact version-2 Replica state defined by one
// authenticated seed approval. It does not join the old member's journal.
type MemberSeedOriginReport struct {
	Version             int                                  `json:"version"`
	Status              string                               `json:"status"`
	Approval            SeedApprovalSnapshotReport           `json:"approval"`
	Member              replication.TransitionSnapshotReport `json:"member"`
	TargetOrdering      replication.OrderingSnapshotReport   `json:"target_ordering"`
	CurrentTerm         uint64                               `json:"current_term"`
	CurrentDurableIndex uint64                               `json:"current_durable_index"`
}

func VerifyMemberSeedOrigin(ctx context.Context, cfg config.Config, targetSnapshotDir, approvedFilesDir, manifestPath string) (MemberSeedOriginReport, error) {
	if cfg.Replication == nil {
		return MemberSeedOriginReport{}, errors.New("seed origin requires target replication configuration")
	}
	seedCfg := cfg
	seedCfg.Storage.DataDir = targetSnapshotDir // key-slot unlock must read the frozen target
	approval, err := VerifySeedApprovalSnapshot(ctx, seedCfg, approvedFilesDir, manifestPath)
	if err != nil {
		return MemberSeedOriginReport{}, err
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return MemberSeedOriginReport{}, err
	}
	digest := sha256.Sum256(manifestBytes)
	if hex.EncodeToString(digest[:]) != approval.ManifestSHA256 {
		return MemberSeedOriginReport{}, errors.New("seed approval manifest changed after authentication")
	}
	var manifest SeedManifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return MemberSeedOriginReport{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return MemberSeedOriginReport{}, err
	}
	seeded, err := expectedSeedReplicaState(cfg, manifest)
	if err != nil {
		return MemberSeedOriginReport{}, err
	}
	result := MemberSeedOriginReport{Version: 1, Approval: approval}
	err = withFrozenMemberTransitionSnapshot(ctx, cfg, targetSnapshotDir,
		func(snapshotCfg config.Config, state replication.MemberState, key []byte) error {
			if state.ClusterID != approval.ClusterID || state.Incarnation != approval.Incarnation || state.NodeID != approval.TargetNode ||
				state.Term < approval.Term || state.DurableIndex < approval.Index || state.ConfirmedIndex < approval.Index || state.AppliedIndex < approval.Index {
				return errors.New("replacement member state is behind or outside its authenticated seed approval")
			}
			member, err := replication.VerifyTransitionSnapshotSeedOrigin(snapshotCfg.ReplicationStatePath(), key, state, seeded)
			if err != nil {
				return err
			}
			ordering, err := replication.VerifyOrderingSnapshot(snapshotCfg.OrderingJournalPath(), key,
				approval.ClusterID, approval.Incarnation, approval.Index, seeded.OrderingHeadMAC)
			if err != nil {
				return err
			}
			currentOrdering, err := replication.VerifyOrderingSnapshot(snapshotCfg.OrderingJournalPath(), key,
				state.ClusterID, state.Incarnation, state.DurableIndex, state.OrderingHeadMAC)
			if err != nil || ordering.FileSHA256 != currentOrdering.FileSHA256 {
				return errors.New("replacement member ordering journal changed or disagrees with current state")
			}
			result.Status = "replacement_seed_origin_mac_verified"
			result.Member, result.TargetOrdering = member, ordering
			result.CurrentTerm, result.CurrentDurableIndex = state.Term, state.DurableIndex
			return nil
		})
	return result, err
}
