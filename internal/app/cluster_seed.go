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
	"github.com/akz142857/Halro/internal/dirscan"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
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
	Selected        *selectedSeedState          `json:"selected_state,omitempty"`
}

func CreateSeedManifest(ctx context.Context, cfg config.Config, targetNode, outputPath, username string, password []byte, totpCode string) (result SeedManifest, resultErr error) {
	ctx, permit, scopeErr := seedCommandContext(ctx, cfg)
	if scopeErr != nil {
		return SeedManifest{}, scopeErr
	}
	if permit != (resourcebudget.Permit{}) {
		defer func() {
			if !errors.Is(resultErr, errBoundedFileRetirement) && !errors.Is(resultErr, replication.ErrOrderingInspectionRetirement) && !errors.Is(resultErr, ownedfile.ErrRetirement) {
				resultErr = errors.Join(resultErr, permit.Release())
			}
			resultErr = errors.Join(resultErr, ctx.Err())
		}()
	}

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
	defer func() { resultErr = retireSeedGuard(dataLock, resultErr) }()
	if selected, routeErr := selectedSeedRoute(cfg); routeErr != nil {
		return SeedManifest{}, routeErr
	} else if selected {
		if cfg.ResourceBudget == nil {
			// An opaque bootstrap trace cannot route a legacy command into
			// selected owner opening without its explicit bounded profile.
			return SeedManifest{}, errSeedMemoryStateAuthority
		}
		return createSelectedSeedManifestLocked(ctx, cfg, targetNode, outputPath, username, password, totpCode, dataLock)
	}
	if err := refuseLegacySeedMemoryState(ctx, cfg); err != nil {
		return SeedManifest{}, err
	}
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
	files, err := hashSeedFilesContext(ctx, cfg.Storage.DataDir, cfg.Storage.MetadataFile)
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
	if err := writeSeedManifestContext(ctx, cfg, outputPath, manifest); err != nil {
		return SeedManifest{}, err
	}
	return manifest, nil
}

func InstallSeedSnapshot(ctx context.Context, cfg config.Config, stagingData, manifestPath string) (result replication.MemberState, resultErr error) {
	ctx, permit, scopeErr := seedCommandContext(ctx, cfg)
	if scopeErr != nil {
		return replication.MemberState{}, scopeErr
	}
	if permit != (resourcebudget.Permit{}) {
		defer func() {
			if !errors.Is(resultErr, errBoundedFileRetirement) && !errors.Is(resultErr, replication.ErrOrderingInspectionRetirement) && !errors.Is(resultErr, ownedfile.ErrRetirement) {
				resultErr = errors.Join(resultErr, permit.Release())
			}
			resultErr = errors.Join(resultErr, ctx.Err())
		}()
	}

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
	payload0, readErr := readSeedManifest(ctx, manifestPath)
	if readErr != nil {
		return replication.MemberState{}, readErr
	}
	declared, readErr := decodeSeedManifest(payload0)
	clear(payload0)
	if readErr != nil {
		return replication.MemberState{}, readErr
	}
	if declared.Version == selectedSeedManifestVersion {
		return installSelectedSeedSnapshot(ctx, cfg, stagingData, declared)
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
	stageCfg := cfg
	stageCfg.Storage.DataDir = stagingData
	if err := refuseLegacySeedMemoryState(ctx, stageCfg); err != nil {
		return replication.MemberState{}, err
	}
	if err := checkSeedTreeContext(ctx, stagingData, false); err != nil {
		return replication.MemberState{}, fmt.Errorf("validate seed staging tree: %w", err)
	}
	payload, err := readSeedManifest(ctx, manifestPath)
	if err != nil {
		return replication.MemberState{}, err
	}
	manifest, err := decodeSeedManifest(payload)
	if err != nil {
		return replication.MemberState{}, err
	}
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
	if err := validateSeedManifestContext(ctx, cfg, stagingData, manifest, clusterKey[:]); err != nil {
		return replication.MemberState{}, err
	}
	state, err := seedReplicaStateContext(ctx, cfg, stagingData, manifest, clusterKey[:])
	if err != nil {
		return replication.MemberState{}, err
	}
	if err := checkSeedTreeContext(ctx, stagingData, true); err != nil {
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
	if err := ctx.Err(); err != nil {
		return replication.MemberState{}, err
	}
	if err := refuseLegacySeedMemoryState(ctx, stageCfg); err != nil {
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

func checkSeedTree(root string, persist bool) error {
	return checkSeedTreeContext(context.Background(), root, persist)
}

func checkSeedTreeContext(ctx context.Context, root string, persist bool) error {
	return dirscan.Tree(ctx, root, seedTreeDepth, func(relative string, file *os.File, info os.FileInfo) error {
		if !persist {
			return nil
		}
		mode := os.FileMode(0600)
		if info.IsDir() {
			mode = 0700
		}
		if err := file.Chmod(mode); err != nil {
			return err
		}
		current, err := file.Stat()
		if err != nil || !os.SameFile(info, current) || current.Mode().Perm() != mode {
			return errors.Join(err, fmt.Errorf("seed path %s changed or has incorrect permissions", relative))
		}
		return errors.Join(ctx.Err(), file.Sync())
	})
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

func validateSeedManifestContext(ctx context.Context, cfg config.Config, staging string, manifest SeedManifest, key []byte) error {
	if err := seedDeclarationBound(manifest); err != nil {
		return err
	}
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
	err := dirscan.Walk(ctx, clusterDirectory, func(entry os.DirEntry) error {
		if entry.Name() == "ordering.journal" && !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		if entry.Name() == "provider-object-sources" && entry.IsDir() {
			return nil
		}
		return errors.New("seed staging cluster directory may contain only ordering.journal and provider-object-sources")
	})
	if err != nil {
		return fmt.Errorf("seed staging cluster directory refused: %w", err)
	}
	want, err := seedManifestMAC(manifest, key)
	if err != nil {
		return err
	}
	got, err := hex.DecodeString(strings.TrimPrefix(manifest.MAC, "sha256:"))
	if err != nil || !hmac.Equal(got, want[:]) {
		return errors.New("seed manifest MAC is invalid")
	}
	actual, err := hashSeedFilesContext(ctx, staging, cfg.Storage.MetadataFile)
	if err != nil {
		return err
	}
	manifest.MAC = ""
	if !seedFilesEqual(actual, manifest.Files) {
		return errors.New("seed staging files do not match the approved manifest")
	}
	return nil
}

func seedReplicaStateContext(ctx context.Context, cfg config.Config, staging string, manifest SeedManifest, key []byte) (result replication.MemberState, resultErr error) {
	headBytes, err := hex.DecodeString(strings.TrimPrefix(manifest.OrderingHeadMAC, "sha256:"))
	if err != nil || len(headBytes) != sha256.Size || !strings.HasPrefix(manifest.OrderingHeadMAC, "sha256:") {
		return replication.MemberState{}, errors.New("seed manifest ordering head is invalid")
	}
	var head [sha256.Size]byte
	copy(head[:], headBytes)
	pressure, err := commandPressure(ctx, cfg, nil)
	if err != nil {
		return replication.MemberState{}, err
	}
	credit, err := acquireOrderingJournalDisk(ctx, cfg, pressure)
	if err != nil {
		return replication.MemberState{}, err
	}
	journalPath := filepath.Join(staging, replication.ClusterDirectoryName, "ordering.journal")
	_, err = replication.InspectOrderingJournalWithLimit(ctx, journalPath, key, manifest.ClusterID, manifest.Incarnation, manifest.Index, head, manifest.Index, orderingJournalOptions(cfg).MaxDiskBytes)
	// Verification must not repair bytes that the approved declaration just
	// hashed. The read-only scanner refuses partial tails and checks context.
	if !errors.Is(err, replication.ErrOrderingInspectionRetirement) {
		defer func() { resultErr = errors.Join(resultErr, credit.Release()) }()
	}
	if err != nil {
		return replication.MemberState{}, fmt.Errorf("authenticate seeded ordering journal: %w", err)
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
	if err := ctx.Err(); err != nil {
		return replication.MemberState{}, err
	}
	if err := replication.WriteState(filepath.Join(staging, replication.ClusterDirectoryName, "state.json"), state, key); err != nil {
		return replication.MemberState{}, fmt.Errorf("stage seeded Replica state: %w", err)
	}
	return state, nil
}

func seedManifestMAC(manifest SeedManifest, key []byte) ([sha256.Size]byte, error) {
	if err := seedDeclarationBound(manifest); err != nil {
		return [sha256.Size]byte{}, err
	}
	manifest.MAC = ""
	if manifest.Version == selectedSeedManifestVersion && len(manifest.Files) == 0 {
		manifest.Files = []SeedFile{}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	mac := hmac.New(sha256.New, key)
	if manifest.Version == selectedSeedManifestVersion {
		mac.Write([]byte("halro:seed-manifest:v2\x00"))
	} else {
		mac.Write([]byte("halro:seed-manifest:v1\x00"))
	}
	_, _ = mac.Write(encoded)
	var result [sha256.Size]byte
	copy(result[:], mac.Sum(nil))
	return result, nil
}

func hashSeedFiles(root, metadataFile string) ([]SeedFile, error) {
	return hashSeedFilesContext(context.Background(), root, metadataFile)
}

func hashSeedFilesContext(ctx context.Context, root, metadataFile string) (files []SeedFile, resultErr error) {
	var sourceCfg config.Config
	sourceCfg.Storage.DataDir = root
	if err := refuseLegacySeedMemoryState(ctx, sourceCfg); err != nil {
		return nil, err
	}
	pinnedRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, pinnedRoot.Close(), ctx.Err())
		if resultErr != nil {
			files = nil
		}
	}()
	remaining := seedManifestBytes - 4096
	buffer := make([]byte, 32<<10)
	hashFile := func(relative string, file *os.File, info os.FileInfo) error {
		if info.IsDir() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(files) >= seedManifestFiles || len(relative) > 4096 || len(relative) > (remaining-640)/6 {
			return errSeedLimit
		}
		named, err := pinnedRoot.Lstat(relative)
		if err != nil || !os.SameFile(info, named) || !info.Mode().IsRegular() {
			return errors.Join(dirscan.ErrIdentity, err)
		}
		hash := sha256.New()
		n, copyErr := io.CopyBuffer(hash, seedContextReader{ctx, file}, buffer)
		actual, statErr := file.Stat()
		named, nameErr := pinnedRoot.Lstat(relative)
		if err := errors.Join(copyErr, statErr, nameErr); err != nil {
			return err
		}
		if n != info.Size() || actual.Size() != info.Size() || !actual.ModTime().Equal(info.ModTime()) || !os.SameFile(info, named) || named.Size() != info.Size() || !named.ModTime().Equal(info.ModTime()) {
			return dirscan.ErrIdentity
		}
		files = append(files, SeedFile{Path: filepath.ToSlash(relative), Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))})
		remaining -= 6*len(relative) + 640
		return nil
	}
	for _, relativeRoot := range [8]string{metadataFile, "metadata.journal", "ledger", "audit", "governance", "provider-objects", filepath.Join(replication.ClusterDirectoryName, "ordering.journal"), filepath.Join(replication.ClusterDirectoryName, "provider-object-sources")} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := pinnedRoot.Lstat(relativeRoot)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			err = dirscan.Tree(ctx, filepath.Join(root, relativeRoot), seedTreeDepth, func(relative string, file *os.File, info os.FileInfo) error {
				return hashFile(filepath.Join(relativeRoot, relative), file, info)
			})
		} else if info.Mode().IsRegular() {
			file, openErr := pinnedRoot.OpenFile(relativeRoot, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if openErr != nil {
				return nil, openErr
			}
			actual, statErr := file.Stat()
			if statErr != nil || !os.SameFile(info, actual) {
				err = errors.Join(statErr, dirscan.ErrIdentity)
			} else {
				err = hashFile(relativeRoot, file, info)
			}
			err = errors.Join(err, file.Close())
		} else {
			return nil, fmt.Errorf("seed source %s contains a symlink or non-regular file", relativeRoot)
		}
		if err != nil {
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
	return writeSeedManifestContext(context.Background(), config.Config{}, path, manifest)
}

func writeSeedManifestContext(ctx context.Context, cfg config.Config, path string, manifest SeedManifest) (resultErr error) {
	if err := seedDeclarationBound(manifest); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var permit resourcebudget.Permit
	var cleanupErr error
	published := false
	if cfg.ResourceBudget != nil {
		pressure, err := commandPressure(ctx, cfg, nil)
		if err != nil {
			return err
		}
		if pressure.logicalBudget() == nil {
			return resourcebudget.ErrInvalid
		}
		permit, err = pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{Disk: seedManifestBytes})
		if err != nil {
			return err
		}
	}
	defer func() {
		// A published alias keeps the persistent Disk owner, including uncertain
		// directory Sync. Closing the temporary is not deletion of that alias.
		if !published && cleanupErr == nil && permit != (resourcebudget.Permit{}) {
			resultErr = errors.Join(resultErr, permit.Release())
		}
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if len(encoded) > seedManifestBytes {
		return errSeedLimit
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temporary, owner, err := ownedfile.CreateTemporary(filepath.Dir(path), ".seed-manifest-*")
	if err != nil {
		return err
	}
	defer func() {
		cleanupErr = temporary.Close()
		if cleanupErr == nil {
			cleanupErr = owner.Retire()
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		return err
	}
	if n, err := temporary.Write(encoded); err != nil || n != len(encoded) {
		return errors.Join(err, io.ErrShortWrite)
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := owner.VerifyFile(temporary); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(temporary.Name(), path); err != nil {
		return err
	}
	published = true
	current, err := os.Lstat(path)
	actual, statErr := temporary.Stat()
	if err != nil || statErr != nil || !os.SameFile(actual, current) {
		return errors.Join(dirscan.ErrIdentity, err, statErr)
	}
	return durable.SyncDirectory(filepath.Dir(path))
}
