package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/store/lock"
)

const seedManifestVersion = 1

type SeedFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type SeedManifest struct {
	Version         int                         `json:"version"`
	ClusterID       string                      `json:"cluster_id"`
	Incarnation     string                      `json:"incarnation"`
	SourceNode      string                      `json:"source_node"`
	TargetNode      string                      `json:"target_node"`
	Term            uint64                      `json:"term"`
	Index           uint64                      `json:"index"`
	OrderingHeadMAC string                      `json:"ordering_head_mac"`
	Projection      replication.ProjectionState `json:"projection"`
	ApprovedBy      string                      `json:"approved_by"`
	ApprovedAt      time.Time                   `json:"approved_at"`
	Files           []SeedFile                  `json:"files"`
	MAC             string                      `json:"mac"`
}

func CreateSeedManifest(ctx context.Context, cfg config.Config, targetNode, outputPath, username string, password []byte, totpCode string) (SeedManifest, error) {
	if cfg.Replication == nil || targetNode == "" || !filepath.IsAbs(outputPath) {
		return SeedManifest{}, errors.New("seed approval requires replication config, target node and absolute output path")
	}
	if pathWithin(outputPath, cfg.Storage.DataDir) {
		return SeedManifest{}, errors.New("seed approval manifest must be outside storage.data_dir")
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return SeedManifest{}, fmt.Errorf("seed approval requires the source member to be stopped: %w", err)
	}
	defer dataLock.Close()
	masterKey, err := unlockMemberMasterKey(ctx, cfg)
	if err != nil {
		return SeedManifest{}, err
	}
	defer clear(masterKey)
	state, clusterKey, err := readMemberState(cfg, masterKey)
	if err != nil {
		return SeedManifest{}, err
	}
	defer clear(clusterKey[:])
	if state.Role != replication.RolePrimary || state.DurableIndex != state.ConfirmedIndex || state.ConfirmedIndex != state.AppliedIndex {
		return SeedManifest{}, errors.New("seed approval requires a stopped, fully confirmed and applied Primary")
	}
	if !stateHasPeer(state, targetNode) {
		return SeedManifest{}, errors.New("seed target is not a configured peer")
	}
	if err := authenticateClusterOperator(ctx, cfg, masterKey, username, password, totpCode); err != nil {
		return SeedManifest{}, err
	}
	files, err := hashSeedFiles(cfg.Storage.DataDir, cfg.Storage.MetadataFile)
	if err != nil {
		return SeedManifest{}, err
	}
	projection := state.Projection
	projection.Index = state.AppliedIndex
	manifest := SeedManifest{
		Version: seedManifestVersion, ClusterID: state.ClusterID, Incarnation: state.Incarnation,
		SourceNode: state.NodeID, TargetNode: targetNode, Term: state.Term, Index: state.AppliedIndex,
		OrderingHeadMAC: "sha256:" + hex.EncodeToString(state.OrderingHeadMAC[:]), Projection: projection,
		ApprovedBy: username, ApprovedAt: time.Now().UTC(), Files: files,
	}
	mac, err := seedManifestMAC(manifest, clusterKey[:])
	if err != nil {
		return SeedManifest{}, err
	}
	manifest.MAC = "sha256:" + hex.EncodeToString(mac[:])
	if err := writeSeedManifest(outputPath, manifest); err != nil {
		return SeedManifest{}, err
	}
	return manifest, nil
}

func InstallSeedSnapshot(ctx context.Context, cfg config.Config, stagingData, manifestPath string) (replication.MemberState, error) {
	if cfg.Replication == nil || !filepath.IsAbs(stagingData) || !filepath.IsAbs(manifestPath) {
		return replication.MemberState{}, errors.New("seed install requires replication config and absolute staging/manifest paths")
	}
	stagingData = filepath.Clean(stagingData)
	dataDir := filepath.Clean(cfg.Storage.DataDir)
	publicationDirectory := filepath.Dir(dataDir)
	if stagingData == dataDir || filepath.Dir(stagingData) != publicationDirectory {
		return replication.MemberState{}, errors.New("seed staging directory must be a sibling of storage.data_dir for atomic publication")
	}
	publicationInfo, err := os.Lstat(publicationDirectory)
	if err != nil {
		return replication.MemberState{}, err
	}
	if publicationInfo.Mode()&os.ModeSymlink != 0 || !publicationInfo.IsDir() {
		return replication.MemberState{}, errors.New("seed publication directory must be a real directory, not a symlink")
	}
	stagingInfo, err := os.Lstat(stagingData)
	if err != nil {
		return replication.MemberState{}, err
	}
	if !stagingInfo.IsDir() || stagingInfo.Mode()&os.ModeSymlink != 0 {
		return replication.MemberState{}, errors.New("seed staging must be a real directory, not a symlink")
	}
	if !sameSeedFilesystem(stagingInfo, publicationInfo) {
		return replication.MemberState{}, errors.New("seed staging and storage.data_dir must be on the same filesystem")
	}
	publicationLock, err := lock.AcquireInitialization(cfg.Storage.DataDir)
	if err != nil {
		return replication.MemberState{}, fmt.Errorf("acquire seed publication lock: %w", err)
	}
	defer publicationLock.Close()
	if _, err := os.Lstat(cfg.Storage.DataDir); err == nil {
		return replication.MemberState{}, errors.New("seed install requires storage.data_dir to be absent")
	} else if !errors.Is(err, os.ErrNotExist) {
		return replication.MemberState{}, err
	}
	// Reject links and special files before any key or authenticated store is
	// opened through the staging tree. The final pass below repeats these checks
	// through a root descriptor immediately before publication.
	if err := checkSeedTree(stagingData, false); err != nil {
		return replication.MemberState{}, fmt.Errorf("validate seed staging tree: %w", err)
	}
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		return replication.MemberState{}, err
	}
	var manifest SeedManifest
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return replication.MemberState{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return replication.MemberState{}, err
	}
	stageCfg := cfg
	stageCfg.Storage.DataDir = stagingData
	masterKey, err := unlockMemberMasterKey(ctx, stageCfg)
	if err != nil {
		return replication.MemberState{}, err
	}
	defer clear(masterKey)
	clusterKey, err := replication.DeriveClusterKey(masterKey, manifest.Incarnation)
	if err != nil {
		return replication.MemberState{}, err
	}
	defer clear(clusterKey[:])
	if err := validateSeedManifest(cfg, stagingData, manifest, clusterKey[:]); err != nil {
		return replication.MemberState{}, err
	}
	state, err := seedReplicaState(cfg, stagingData, manifest, clusterKey[:])
	if err != nil {
		return replication.MemberState{}, err
	}
	if err := syncSeedTree(stagingData); err != nil {
		return replication.MemberState{}, fmt.Errorf("persist seed staging tree: %w", err)
	}
	currentInfo, err := os.Lstat(stagingData)
	if err != nil || !os.SameFile(stagingInfo, currentInfo) {
		return replication.MemberState{}, errors.New("seed staging directory changed during verification")
	}
	currentPublicationInfo, err := os.Lstat(publicationDirectory)
	if err != nil || !os.SameFile(publicationInfo, currentPublicationInfo) || !sameSeedFilesystem(currentInfo, currentPublicationInfo) {
		return replication.MemberState{}, errors.New("seed publication directory changed or crossed filesystems during verification")
	}
	if _, err := os.Lstat(dataDir); err == nil {
		return replication.MemberState{}, errors.New("seed install requires storage.data_dir to remain absent")
	} else if !errors.Is(err, os.ErrNotExist) {
		return replication.MemberState{}, err
	}
	if err := os.Rename(stagingData, dataDir); err != nil {
		return replication.MemberState{}, fmt.Errorf("publish seed snapshot: %w", err)
	}
	if err := durable.SyncDirectory(publicationDirectory); err != nil {
		return replication.MemberState{}, fmt.Errorf("persist seed publication: %w", err)
	}
	return state, nil
}

func syncSeedTree(root string) error {
	return checkSeedTree(root, true)
}

type seedDirectory struct {
	name string
	info os.FileInfo
}

func checkSeedTree(root string, persist bool) error {
	pinnedRoot, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer pinnedRoot.Close()
	var directories []seedDirectory
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := pinnedRoot.Lstat(relative)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("seed path %s is a symlink", path)
		}
		if info.IsDir() {
			directories = append(directories, seedDirectory{name: relative, info: info})
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("seed path %s is not a regular file", path)
		}
		file, err := pinnedRoot.Open(relative)
		if err != nil {
			return err
		}
		openedInfo, statErr := file.Stat()
		if statErr != nil || !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
			file.Close()
			return errors.Join(statErr, fmt.Errorf("seed path %s changed during verification", path))
		}
		var chmodErr, syncErr error
		if persist {
			chmodErr = file.Chmod(0o600)
			if chmodErr == nil {
				openedInfo, statErr = file.Stat()
				if statErr == nil && openedInfo.Mode().Perm() != 0o600 {
					statErr = fmt.Errorf("seed file %s does not have mode 0600", path)
				}
			}
			if chmodErr == nil && statErr == nil {
				syncErr = file.Sync()
			}
		}
		closeErr := file.Close()
		return errors.Join(chmodErr, statErr, syncErr, closeErr)
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		directory := directories[index]
		handle, err := pinnedRoot.Open(directory.name)
		if err != nil {
			return err
		}
		openedInfo, statErr := handle.Stat()
		if statErr != nil || !os.SameFile(directory.info, openedInfo) || !openedInfo.IsDir() {
			handle.Close()
			return errors.Join(statErr, fmt.Errorf("seed directory %s changed during verification", directory.name))
		}
		var chmodErr, syncErr error
		if persist {
			chmodErr = handle.Chmod(0o700)
			if chmodErr == nil {
				openedInfo, statErr = handle.Stat()
				if statErr == nil && openedInfo.Mode().Perm() != 0o700 {
					statErr = fmt.Errorf("seed directory %s does not have mode 0700", directory.name)
				}
			}
			if chmodErr == nil && statErr == nil {
				syncErr = handle.Sync()
			}
		}
		if err := errors.Join(chmodErr, statErr, syncErr, handle.Close()); err != nil {
			return err
		}
	}
	return nil
}

func sameSeedFilesystem(left, right os.FileInfo) bool {
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && leftStat.Dev == rightStat.Dev
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("seed manifest contains trailing JSON")
		}
		return err
	}
	return nil
}

func validateSeedManifest(cfg config.Config, staging string, manifest SeedManifest, key []byte) error {
	if manifest.Version != seedManifestVersion || manifest.ClusterID != cfg.Replication.ClusterID ||
		manifest.TargetNode != cfg.Replication.NodeID || manifest.SourceNode == "" || manifest.SourceNode == manifest.TargetNode ||
		manifest.Incarnation == "" || manifest.Term == 0 || manifest.ApprovedBy == "" || manifest.ApprovedAt.IsZero() || len(manifest.Files) == 0 ||
		manifest.Projection.Index != manifest.Index {
		return errors.New("seed manifest identity or approval is invalid")
	}
	foundSource := false
	for _, peer := range cfg.Replication.Peers {
		if peer.Name == manifest.SourceNode {
			foundSource = true
		}
	}
	if !foundSource {
		return errors.New("seed source is not a configured peer")
	}
	clusterDirectory := filepath.Join(staging, replication.ClusterDirectoryName)
	entries, err := os.ReadDir(clusterDirectory)
	if err != nil {
		return fmt.Errorf("seed staging must carry the authenticated ordering journal: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == "ordering.journal" && !entry.IsDir() {
			continue
		}
		if entry.Name() == "provider-object-sources" && entry.IsDir() {
			continue
		}
		return errors.New("seed staging cluster directory may contain only ordering.journal and provider-object-sources")
	}
	want, err := seedManifestMAC(manifest, key)
	if err != nil {
		return err
	}
	got, err := hex.DecodeString(strings.TrimPrefix(manifest.MAC, "sha256:"))
	if err != nil || !hmac.Equal(got, want[:]) {
		return errors.New("seed manifest MAC is invalid")
	}
	actual, err := hashSeedFiles(staging, cfg.Storage.MetadataFile)
	if err != nil {
		return err
	}
	manifest.MAC = ""
	if !seedFilesEqual(actual, manifest.Files) {
		return errors.New("seed staging files do not match the approved manifest")
	}
	return nil
}

func seedReplicaState(cfg config.Config, staging string, manifest SeedManifest, key []byte) (replication.MemberState, error) {
	headBytes, err := hex.DecodeString(strings.TrimPrefix(manifest.OrderingHeadMAC, "sha256:"))
	if err != nil || len(headBytes) != sha256.Size || !strings.HasPrefix(manifest.OrderingHeadMAC, "sha256:") {
		return replication.MemberState{}, errors.New("seed manifest ordering head is invalid")
	}
	var head [sha256.Size]byte
	copy(head[:], headBytes)
	journalPath := filepath.Join(staging, replication.ClusterDirectoryName, "ordering.journal")
	journal, err := replication.OpenExistingOrderingJournal(journalPath, key, manifest.ClusterID, manifest.Incarnation, manifest.Index, head)
	if err != nil {
		return replication.MemberState{}, fmt.Errorf("authenticate seeded ordering journal: %w", err)
	}
	if err := journal.Close(); err != nil {
		return replication.MemberState{}, err
	}
	peers := make([]replication.StatePeer, 0, len(cfg.Replication.Peers))
	for _, peer := range cfg.Replication.Peers {
		peers = append(peers, replication.StatePeer{Name: peer.Name, Address: peer.Address, SPKISHA256: peer.SPKISHA256})
	}
	state := replication.MemberState{
		Version: replication.StateVersion, ClusterID: manifest.ClusterID, Incarnation: manifest.Incarnation,
		NodeID: cfg.Replication.NodeID, Role: replication.RoleReplica, Term: manifest.Term, PromisedTerm: manifest.Term,
		DurableIndex: manifest.Index, ConfirmedIndex: manifest.Index, AppliedIndex: manifest.Index,
		OrderingHeadMAC: head, Projection: manifest.Projection, Peers: peers,
	}
	if err := replication.WriteState(filepath.Join(staging, replication.ClusterDirectoryName, "state.json"), state, key); err != nil {
		return replication.MemberState{}, fmt.Errorf("stage seeded Replica state: %w", err)
	}
	return state, nil
}

func seedManifestMAC(manifest SeedManifest, key []byte) ([sha256.Size]byte, error) {
	manifest.MAC = ""
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("halro:seed-manifest:v1\x00"))
	_, _ = mac.Write(encoded)
	var result [sha256.Size]byte
	copy(result[:], mac.Sum(nil))
	return result, nil
}

func hashSeedFiles(root, metadataFile string) ([]SeedFile, error) {
	var files []SeedFile
	for _, relativeRoot := range []string{
		metadataFile, "metadata.journal", "ledger", "audit", "governance", "provider-objects",
		filepath.Join(replication.ClusterDirectoryName, "ordering.journal"),
		filepath.Join(replication.ClusterDirectoryName, "provider-object-sources"),
	} {
		path := filepath.Join(root, relativeRoot)
		err := filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("seed source contains non-regular file %s", current)
			}
			file, err := os.Open(current)
			if err != nil {
				return err
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			relative, err := filepath.Rel(root, current)
			if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
				return errors.New("seed source escaped its data directory")
			}
			files = append(files, SeedFile{Path: filepath.ToSlash(relative), Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))})
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func seedFilesEqual(left, right []SeedFile) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func writeSeedManifest(path string, manifest SeedManifest) error {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".seed-manifest-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return err
	}
	return durable.SyncDirectory(filepath.Dir(path))
}
