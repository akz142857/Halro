package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
)

const maxSeedSnapshotManifestBytes = 1 << 20

// SeedApprovalSnapshotReport proves the MAC on a frozen approval, the exact
// approved file set and the ordering journal prefix. It does not prove that a
// target installed the seed or that a replacement member journal is approved.
type SeedApprovalSnapshotReport struct {
	Version           int                                `json:"version"`
	Status            string                             `json:"status"`
	ClusterID         string                             `json:"cluster_id"`
	Incarnation       string                             `json:"incarnation"`
	SourceNode        string                             `json:"source_node"`
	TargetNode        string                             `json:"target_node"`
	Term              uint64                             `json:"term"`
	Index             uint64                             `json:"index"`
	ApprovedAt        time.Time                          `json:"approved_at"`
	ManifestSHA256    string                             `json:"manifest_sha256"`
	Ordering          replication.OrderingSnapshotReport `json:"ordering"`
	ApprovedFileCount int                                `json:"approved_file_count"`
}

// VerifySeedApprovalSnapshot is read-only. The operator must freeze the seed
// source and keep both the original manifest and approved files for an
// independent archive receipt.
func VerifySeedApprovalSnapshot(ctx context.Context, target config.Config, approvedFilesDir, manifestPath string) (SeedApprovalSnapshotReport, error) {
	if target.Replication == nil || !filepath.IsAbs(approvedFilesDir) || filepath.Clean(approvedFilesDir) != approvedFilesDir ||
		!filepath.IsAbs(manifestPath) || filepath.Clean(manifestPath) != manifestPath {
		return SeedApprovalSnapshotReport{}, errors.New("seed snapshot requires a target configuration and clean absolute paths")
	}
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Size() == 0 || manifestInfo.Size() > maxSeedSnapshotManifestBytes ||
		manifestInfo.Mode().Perm()&0o022 != 0 {
		return SeedApprovalSnapshotReport{}, errors.New("seed manifest must be a bounded regular file without group/world write access")
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil || int64(len(manifestBytes)) != manifestInfo.Size() {
		return SeedApprovalSnapshotReport{}, errors.New("seed manifest changed while being read")
	}
	var manifest SeedManifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return SeedApprovalSnapshotReport{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return SeedApprovalSnapshotReport{}, err
	}
	stageInfo, err := os.Lstat(approvedFilesDir)
	if err != nil || !stageInfo.IsDir() || stageInfo.Mode().Perm()&0o022 != 0 {
		return SeedApprovalSnapshotReport{}, errors.New("approved seed snapshot must be a private directory")
	}
	if err := checkSeedTree(approvedFilesDir, false); err != nil {
		return SeedApprovalSnapshotReport{}, err
	}
	masterKey, err := unlockMemberMasterKey(ctx, target)
	if err != nil {
		return SeedApprovalSnapshotReport{}, err
	}
	defer clear(masterKey)
	clusterKey, err := replication.DeriveClusterKey(masterKey, manifest.Incarnation)
	if err != nil {
		return SeedApprovalSnapshotReport{}, err
	}
	defer clear(clusterKey[:])
	if err := validateSeedManifest(target, approvedFilesDir, manifest, clusterKey[:]); err != nil {
		return SeedApprovalSnapshotReport{}, err
	}
	approved := make(map[string]bool, len(manifest.Files))
	for _, file := range manifest.Files {
		approved[filepath.FromSlash(file.Path)] = true
	}
	seen := 0
	if err := filepath.WalkDir(approvedFilesDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(approvedFilesDir, path)
		if err != nil || !approved[relative] {
			return fmt.Errorf("approved seed snapshot contains an unlisted file: %s", relative)
		}
		seen++
		return nil
	}); err != nil || seen != len(manifest.Files) {
		return SeedApprovalSnapshotReport{}, errors.New("approved seed snapshot has extra or missing files")
	}
	headBytes, err := hex.DecodeString(strings.TrimPrefix(manifest.OrderingHeadMAC, "sha256:"))
	if err != nil || len(headBytes) != sha256.Size || !strings.HasPrefix(manifest.OrderingHeadMAC, "sha256:") {
		return SeedApprovalSnapshotReport{}, errors.New("seed manifest ordering head is invalid")
	}
	var head [sha256.Size]byte
	copy(head[:], headBytes)
	ordering, err := replication.VerifyOrderingSnapshot(filepath.Join(approvedFilesDir, replication.ClusterDirectoryName, "ordering.journal"),
		clusterKey[:], manifest.ClusterID, manifest.Incarnation, manifest.Index, head)
	if err != nil {
		return SeedApprovalSnapshotReport{}, fmt.Errorf("authenticate approved seed ordering prefix: %w", err)
	}
	manifestHash := sha256.Sum256(manifestBytes)
	return SeedApprovalSnapshotReport{Version: 1, Status: "seed_manifest_mac_files_ordering_verified",
		ClusterID: manifest.ClusterID, Incarnation: manifest.Incarnation, SourceNode: manifest.SourceNode,
		TargetNode: manifest.TargetNode, Term: manifest.Term, Index: manifest.Index,
		ApprovedAt: manifest.ApprovedAt, ManifestSHA256: hex.EncodeToString(manifestHash[:]),
		Ordering: ordering, ApprovedFileCount: len(manifest.Files)}, nil
}
