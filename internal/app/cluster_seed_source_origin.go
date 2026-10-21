package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

// SourceSeedOriginReport binds an authenticated stopped Primary snapshot to
// the exact seed approval and authoritative file set it signed.
type SourceSeedOriginReport struct {
	Version        int                                  `json:"version"`
	Status         string                               `json:"status"`
	Approval       SeedApprovalSnapshotReport           `json:"approval"`
	SourceMember   replication.TransitionSnapshotReport `json:"source_member"`
	SourceOrdering replication.OrderingSnapshotReport   `json:"source_ordering"`
}

func VerifySourceSeedOrigin(ctx context.Context, sourceCfg config.Config, sourceSnapshotDir string,
	targetCfg config.Config, approvedFilesDir, manifestPath string) (SourceSeedOriginReport, error) {
	if sourceCfg.Replication == nil || targetCfg.Replication == nil {
		return SourceSeedOriginReport{}, errors.New("seed source origin requires source and target replication configurations")
	}
	approval, err := VerifySeedApprovalSnapshot(ctx, targetCfg, approvedFilesDir, manifestPath)
	if err != nil {
		return SourceSeedOriginReport{}, err
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return SourceSeedOriginReport{}, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != approval.ManifestSHA256 {
		return SourceSeedOriginReport{}, errors.New("seed approval manifest changed after authentication")
	}
	var manifest SeedManifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return SourceSeedOriginReport{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return SourceSeedOriginReport{}, err
	}
	seeded, err := expectedSeedReplicaState(targetCfg, manifest)
	if err != nil {
		return SourceSeedOriginReport{}, err
	}
	result := SourceSeedOriginReport{Version: 1, Approval: approval}
	err = withFrozenMemberTransitionSnapshot(ctx, sourceCfg, sourceSnapshotDir,
		func(snapshotCfg config.Config, state replication.MemberState, key []byte) error {
			if state.ClusterID != approval.ClusterID || state.Incarnation != approval.Incarnation || state.NodeID != approval.SourceNode ||
				state.Role != replication.RolePrimary || state.Term != approval.Term || state.PromisedTerm != approval.Term ||
				state.DurableIndex != approval.Index || state.ConfirmedIndex != approval.Index || state.AppliedIndex != approval.Index ||
				state.OrderingHeadMAC != seeded.OrderingHeadMAC || !reflect.DeepEqual(state.Projection, manifest.Projection) {
				return errors.New("frozen source Primary state does not match seed approval")
			}
			files, err := hashSeedFiles(sourceSnapshotDir, sourceCfg.Storage.MetadataFile)
			if err != nil || !seedFilesEqual(files, manifest.Files) {
				return errors.New("frozen source authoritative files do not match approved seed")
			}
			member, err := replication.VerifyTransitionSnapshot(snapshotCfg.ReplicationStatePath(), key, state)
			if err != nil {
				return err
			}
			ordering, err := replication.VerifyOrderingSnapshot(snapshotCfg.OrderingJournalPath(), key,
				state.ClusterID, state.Incarnation, state.DurableIndex, state.OrderingHeadMAC)
			if err != nil || ordering.FileSHA256 != approval.Ordering.FileSHA256 {
				return errors.New("frozen source ordering file differs from the approved seed")
			}
			result.Status, result.SourceMember, result.SourceOrdering = "source_primary_seed_files_mac_verified", member, ordering
			return nil
		})
	return result, err
}
