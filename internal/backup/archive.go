package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/dirscan"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

const manifestVersion = 4

type SourceFile struct {
	ArchivePath string
	LocalPath   string
}

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type StoreHead struct {
	Generation uint64 `json:"generation"`
	Sequence   uint64 `json:"sequence"`
}

type Manifest struct {
	FormatVersion              int                    `json:"format_version"`
	BackupID                   string                 `json:"backup_id"`
	CreatedAt                  time.Time              `json:"created_at"`
	Encrypted                  bool                   `json:"encrypted"`
	Metadata                   boltstore.MetadataInfo `json:"metadata"`
	LedgerWatermark            ledger.Watermark       `json:"ledger_watermark"`
	CheckpointWatermark        ledger.Watermark       `json:"checkpoint_watermark"`
	UsageManifestVersion       int                    `json:"usage_manifest_version"`
	LedgerFeatureEpoch         uint8                  `json:"ledger_feature_epoch,omitempty"`
	MinimumLedgerReaderVersion string                 `json:"minimum_ledger_reader_version,omitempty"`
	// LedgerChainHead is the epoch-4 chain head (ADR 0016) as of the moment
	// this backup's Ledger snapshot was taken. LedgerChainVerified is false
	// when the source Ledger had not yet written any epoch-4 frame — a chain
	// head of all zeros in that case is "nothing to verify yet", not a
	// tampered or empty chain, and restore must not confuse the two.
	LedgerChainHeadSequence uint64               `json:"ledger_chain_head_sequence,omitempty"`
	LedgerChainHeadOffset   int64                `json:"ledger_chain_head_offset,omitempty"`
	LedgerChainHeadHash     [32]byte             `json:"ledger_chain_head_hash,omitempty"`
	LedgerChainVerified     bool                 `json:"ledger_chain_verified,omitempty"`
	GovernanceSequence      uint64               `json:"governance_sequence,omitempty"`
	GovernanceOffset        int64                `json:"governance_offset,omitempty"`
	GovernanceHeadHash      [32]byte             `json:"governance_head_hash,omitempty"`
	GovernanceFormatVersion int                  `json:"governance_format_version,omitempty"`
	PricingStateSHA256      string               `json:"pricing_state_sha256,omitempty"`
	PendingIntentSHA256     string               `json:"pending_intent_sha256,omitempty"`
	PendingIntents          int                  `json:"pending_intents,omitempty"`
	MasterKeyFingerprint    string               `json:"master_key_fingerprint"`
	KeySlotDescriptorSHA256 string               `json:"key_slot_descriptor_sha256,omitempty"`
	RestoreDrillVerified    bool                 `json:"restore_drill_verified"`
	Build                   buildinfo.Info       `json:"build"`
	ClusterID               string               `json:"cluster_id,omitempty"`
	ClusterIncarnation      string               `json:"cluster_incarnation,omitempty"`
	SourceNodeID            string               `json:"source_node_id,omitempty"`
	SourceRole              string               `json:"source_role,omitempty"`
	Term                    uint64               `json:"term,omitempty"`
	AppliedIndex            uint64               `json:"applied_index,omitempty"`
	MetadataJournalEpoch    uint64               `json:"metadata_journal_epoch,omitempty"`
	MetadataJournalSequence uint64               `json:"metadata_journal_sequence,omitempty"`
	PerStoreHead            map[string]StoreHead `json:"per_store_head,omitempty"`
	Files                   []File               `json:"files"`
}

type CreateOptions struct {
	OutputPath string
	BackupKey  []byte
	Files      []SourceFile
	// OutputDiskBytes and ReserveOutputDisk must be enabled together. The
	// producer charges the complete finite encrypted-output ceiling before
	// creating its inode; publication/unknown exits never refund the token.
	OutputDiskBytes   int64
	ReserveOutputDisk func(int64) (ledger.NativeFileDiskReservation, error)
	// SourceCatalog is exclusive with Files; Create consumes its ownership.
	SourceCatalog              *SourceCatalog
	Context                    context.Context
	Metadata                   boltstore.MetadataInfo
	LedgerWatermark            ledger.Watermark
	CheckpointWatermark        ledger.Watermark
	UsageManifestVersion       int
	LedgerFeatureEpoch         uint8
	MinimumLedgerReaderVersion string
	LedgerChainHeadSequence    uint64
	LedgerChainHeadOffset      int64
	LedgerChainHeadHash        [32]byte
	LedgerChainVerified        bool
	GovernanceSequence         uint64
	GovernanceOffset           int64
	GovernanceHeadHash         [32]byte
	GovernanceFormatVersion    int
	PricingStateSHA256         string
	PendingIntentSHA256        string
	PendingIntents             int
	MasterKeyFingerprint       string
	KeySlotDescriptorSHA256    string
	ClusterID                  string
	ClusterIncarnation         string
	SourceNodeID               string
	SourceRole                 string
	Term                       uint64
	AppliedIndex               uint64
	MetadataJournalEpoch       uint64
	MetadataJournalSequence    uint64
	PerStoreHead               map[string]StoreHead
	Build                      buildinfo.Info
	Now                        func() time.Time
}

func Create(options CreateOptions) (Manifest, error) { return createArchive(options, nil) }

func createArchive(options CreateOptions, paged *PagedManifest) (result Manifest, resultErr error) {
	catalog := options.SourceCatalog
	defer func() {
		if err := catalog.Close(); err != nil {
			resultErr = &archiveCatalogDebt{catalog: catalog, cause: errors.Join(resultErr, err)}
			result = Manifest{}
		}
	}()
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if catalog != nil && len(options.Files) != 0 {
		return Manifest{}, errors.New("backup sources must use either Files or SourceCatalog")
	}
	if !filepath.IsAbs(options.OutputPath) {
		return Manifest{}, errors.New("backup output path must be absolute")
	}
	if len(options.BackupKey) != 32 {
		return Manifest{}, errors.New("backup key must be exactly 32 bytes")
	}
	if catalog == nil && len(options.Files) == 0 {
		return Manifest{}, errors.New("backup files are required")
	}
	if options.ClusterID != "" {
		if options.ClusterIncarnation == "" || options.SourceNodeID == "" || options.SourceRole != "replica" ||
			options.Term == 0 || len(options.PerStoreHead) != 4 {
			return Manifest{}, errors.New("Replica backup cluster metadata is incomplete")
		}
		metadataHead, ok := options.PerStoreHead["metadata"]
		if !ok || metadataHead.Generation != options.MetadataJournalEpoch || metadataHead.Sequence != options.MetadataJournalSequence {
			return Manifest{}, errors.New("Replica backup metadata journal head is inconsistent")
		}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if _, err := os.Lstat(options.OutputPath); err == nil {
		return Manifest{}, errors.New("backup output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	if err := os.MkdirAll(filepath.Dir(options.OutputPath), 0o700); err != nil {
		return Manifest{}, err
	}
	if catalog == nil {
		var err error
		catalog, err = NewSourceCatalogWithContext(ctx, filepath.Dir(options.OutputPath), 0)
		if err != nil {
			return Manifest{}, err
		}
		for _, source := range options.Files {
			if err := ctx.Err(); err != nil {
				return Manifest{}, err
			}
			if err := catalog.Add(source); err != nil {
				return Manifest{}, err
			}
		}
	}
	if catalog.index == nil || catalog.index.count == 0 {
		return Manifest{}, errors.New("backup files are required")
	}
	backupID, err := randomID()
	if err != nil {
		return Manifest{}, err
	}
	formatVersion := 2
	if options.GovernanceFormatVersion > 0 {
		formatVersion = 3
	}
	if options.ClusterID != "" {
		formatVersion = manifestVersion
	}
	manifest := Manifest{
		FormatVersion: formatVersion, BackupID: backupID,
		CreatedAt: options.Now().UTC(), Encrypted: true,
		Metadata: options.Metadata, LedgerWatermark: options.LedgerWatermark,
		CheckpointWatermark:  options.CheckpointWatermark,
		UsageManifestVersion: options.UsageManifestVersion,
		LedgerFeatureEpoch:   options.LedgerFeatureEpoch, MinimumLedgerReaderVersion: options.MinimumLedgerReaderVersion,
		LedgerChainHeadSequence: options.LedgerChainHeadSequence, LedgerChainHeadOffset: options.LedgerChainHeadOffset,
		LedgerChainHeadHash: options.LedgerChainHeadHash, LedgerChainVerified: options.LedgerChainVerified,
		GovernanceSequence: options.GovernanceSequence, GovernanceOffset: options.GovernanceOffset,
		GovernanceHeadHash: options.GovernanceHeadHash, GovernanceFormatVersion: options.GovernanceFormatVersion,
		PricingStateSHA256: options.PricingStateSHA256, PendingIntentSHA256: options.PendingIntentSHA256, PendingIntents: options.PendingIntents,
		MasterKeyFingerprint:    options.MasterKeyFingerprint,
		KeySlotDescriptorSHA256: options.KeySlotDescriptorSHA256,
		RestoreDrillVerified:    false,
		Build:                   options.Build,
		ClusterID:               options.ClusterID, ClusterIncarnation: options.ClusterIncarnation,
		SourceNodeID: options.SourceNodeID, SourceRole: options.SourceRole,
		Term: options.Term, AppliedIndex: options.AppliedIndex,
		MetadataJournalEpoch: options.MetadataJournalEpoch, MetadataJournalSequence: options.MetadataJournalSequence,
		PerStoreHead: options.PerStoreHead,
	}
	manifestBytes := 0
	if paged == nil {
		baseManifest, err := json.Marshal(manifest)
		if err != nil {
			return Manifest{}, err
		}
		if len(baseManifest) > backupManifestSizeLimit {
			return Manifest{}, errors.New("backup manifest exceeds size limit")
		}
		manifestBytes = len(baseManifest) - 2
	} else {
		paged.Manifest = manifest
		paged.CompatibilityVersion = manifest.FormatVersion
		paged.FormatVersion = pagedManifestVersion
		if paged.ClusterID == "" {
			paged.SourceRole = "standalone"
		}
		paged.Catalog = FileCatalogSummary{Version: 1, SHA256: strings.Repeat("0", 64)}
		if err := validatePagedManifest(*paged); err != nil {
			return Manifest{}, err
		}
		encoded, err := json.Marshal(paged)
		if err != nil {
			return Manifest{}, err
		}
		if len(encoded) > pagedMetadataLimit-128 {
			return Manifest{}, errors.New("paged backup root exceeds size limit")
		}
		if err := validatePagedSources(ctx, *paged, catalog); err != nil {
			return Manifest{}, err
		}
	}
	output, err := newArchiveOutput(options)
	if err != nil {
		return Manifest{}, err
	}
	defer func() {
		resultErr = output.finish(resultErr)
		if resultErr != nil {
			result = Manifest{}
		}
	}()
	encrypted, err := newEncryptWriter(output, options.BackupKey)
	if err != nil {
		return Manifest{}, err
	}
	archive := tar.NewWriter(encrypted)
	pages := newCatalogWriter(archive, manifest.CreatedAt)
	err = catalog.Walk(ctx, func(source SourceFile) error {
		if paged != nil && isCatalogPath(source.ArchivePath) {
			return errors.New("backup source uses reserved catalog path")
		}
		entry, err := writeSource(ctx, archive, source)
		if err != nil {
			return err
		}
		if paged != nil {
			return pages.Add(entry)
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		size := len(encoded)
		if len(manifest.Files) > 0 {
			size++
		}
		if size > backupManifestSizeLimit-manifestBytes {
			return errors.New("backup manifest exceeds size limit")
		}
		manifestBytes += size
		manifest.Files = append(manifest.Files, entry)
		return nil
	})
	if err != nil {
		return Manifest{}, errors.Join(err, archive.Close(), encrypted.Close())
	}
	var encodedManifest []byte
	if paged == nil {
		encodedManifest, err = json.Marshal(manifest)
		if err == nil && len(encodedManifest) > backupManifestSizeLimit {
			err = errors.New("backup manifest exceeds size limit")
		}
	} else {
		paged.Catalog, err = pages.Finish()
		if err == nil {
			encodedManifest, err = json.Marshal(paged)
		}
		if err == nil && len(encodedManifest) > pagedMetadataLimit {
			err = errors.New("paged backup root exceeds size limit")
		}
	}
	if err == nil {
		err = archive.WriteHeader(&tar.Header{
			Name: "manifest.json", Mode: 0o600, Size: int64(len(encodedManifest)),
			ModTime: manifest.CreatedAt, Typeflag: tar.TypeReg,
		})
	}
	if err == nil {
		_, err = archive.Write(encodedManifest)
	}
	if closeErr := archive.Close(); err == nil {
		err = closeErr
	}
	if closeErr := encrypted.Close(); err == nil {
		err = closeErr
	}
	if syncErr := output.file.Sync(); err == nil {
		err = syncErr
	}
	if verifyErr := output.verify(); err == nil {
		err = verifyErr
	}
	if closeErr := output.closeFile(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Manifest{}, err
	}
	if err := catalog.Close(); err != nil {
		return Manifest{}, err
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	// Hard-link publication is atomic and, unlike Rename, cannot overwrite an
	// output created by a racing process after the initial existence check.
	if err := output.publish(options.OutputPath); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func Verify(archivePath string, backupKey []byte) (manifest Manifest, err error) {
	return VerifyContext(context.Background(), archivePath, backupKey)
}

func VerifyContext(ctx context.Context, archivePath string, backupKey []byte) (manifest Manifest, err error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	input, err := os.Open(archivePath)
	if err != nil {
		return Manifest{}, err
	}
	defer func() {
		if closeErr := input.Close(); closeErr != nil {
			err = errors.Join(err, ownedfile.ErrRetirement, closeErr)
		}
		if err != nil {
			manifest = Manifest{}
		}
	}()
	return verifyArchiveContext(ctx, input, backupKey)
}

// input lifetime belongs to the caller; verification retires its own index.
func verifyArchive(input io.Reader, backupKey []byte) (manifest Manifest, err error) {
	return verifyArchiveContext(context.Background(), input, backupKey)
}

func verifyArchiveContext(ctx context.Context, input io.Reader, backupKey []byte) (manifest Manifest, err error) {
	decrypted, err := newDecryptReader(backupContextReader{ctx: ctx, reader: input}, backupKey)
	if err != nil {
		return Manifest{}, err
	}
	reader := tar.NewReader(decrypted)
	observed, err := newObservationIndexForWorkspace(ctx, os.TempDir(), 0)
	if err != nil {
		return Manifest{}, err
	}
	defer func() {
		err = errors.Join(err, observed.Close())
		if err != nil {
			manifest = Manifest{}
		}
	}()
	foundManifest := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("read backup archive: %w", err)
		}
		if foundManifest {
			return Manifest{}, errors.New("backup contains entries after manifest")
		}
		name, err := safeArchivePath(header.Name)
		if err != nil || header.Typeflag != tar.TypeReg || header.Size < 0 {
			return Manifest{}, errors.New("backup contains an unsafe archive entry")
		}
		if name == "manifest.json" {
			if header.Size > backupManifestSizeLimit {
				return Manifest{}, errors.New("backup manifest exceeds size limit")
			}
			payload, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
			if err != nil || int64(len(payload)) != header.Size {
				return Manifest{}, errors.New("read backup manifest")
			}
			if err := json.Unmarshal(payload, &manifest); err != nil {
				return Manifest{}, fmt.Errorf("decode backup manifest: %w", err)
			}
			foundManifest = true
			continue
		}
		if _, duplicate, err := observed.Get(name); err != nil {
			return Manifest{}, err
		} else if duplicate {
			return Manifest{}, fmt.Errorf("duplicate backup entry %q", name)
		}
		hash := sha256.New()
		count, err := io.Copy(hash, reader)
		if err != nil || count != header.Size {
			return Manifest{}, fmt.Errorf("read backup entry %q", name)
		}
		if err := observed.Add(File{
			Path: name, Size: count, SHA256: hex.EncodeToString(hash.Sum(nil)),
		}); err != nil {
			return Manifest{}, err
		}
	}
	if _, err := io.Copy(io.Discard, decrypted); err != nil {
		return Manifest{}, fmt.Errorf("verify authenticated backup ending: %w", err)
	}
	if !foundManifest {
		return Manifest{}, errors.New("backup manifest is invalid")
	}
	if err := validateManifestHeader(manifest); err != nil {
		return Manifest{}, err
	}
	if err := matchManifestObservations(manifest, observed); err != nil {
		return Manifest{}, err
	}
	for _, required := range []string{
		"config/config.yaml", "data/metadata.db",
		"data/ledger/ledger.wal", "data/audit/audit.log",
	} {
		if _, exists := expectedFile(manifest.Files, required); !exists {
			return Manifest{}, fmt.Errorf("backup is missing required file %q", required)
		}
	}
	if manifest.FormatVersion >= 3 {
		if _, exists := expectedFile(manifest.Files, "data/governance/governance.journal"); !exists {
			return Manifest{}, errors.New("backup is missing Governance Journal")
		}
	}
	if manifest.FormatVersion >= 4 {
		for _, required := range []string{"data/cluster/state.json", "data/cluster/ordering.journal", "data/metadata.journal"} {
			if _, exists := expectedFile(manifest.Files, required); !exists {
				return Manifest{}, fmt.Errorf("Replica backup is missing required file %q", required)
			}
		}
	}
	return manifest, nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validSHA256Label(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validSHA256(strings.TrimPrefix(value, "sha256:"))
}

// Extract verifies the complete authenticated archive before writing regular
// files into an empty staging directory. Callers must validate staged domain
// data before publishing it as the live data directory.
func Extract(archivePath string, backupKey []byte, destination string) (Manifest, error) {
	return extractArchive(archivePath, backupKey, destination, nil)
}

// between is a per-operation test boundary after complete authentication.
func extractArchive(archivePath string, backupKey []byte, destination string, between func() error) (manifest Manifest, err error) {
	input, err := os.Open(archivePath)
	if err != nil {
		return Manifest{}, err
	}
	defer func() {
		if closeErr := input.Close(); closeErr != nil {
			err = errors.Join(err, ownedfile.ErrRetirement, closeErr)
		}
		if err != nil {
			manifest = Manifest{}
		}
	}()
	identity, err := input.Stat()
	if err != nil {
		return Manifest{}, err
	}
	firstDigest := sha256.New()
	manifest, err = verifyArchive(io.TeeReader(input, firstDigest), backupKey)
	if err != nil {
		return Manifest{}, err
	}
	if between != nil {
		if err := between(); err != nil {
			return Manifest{}, err
		}
	}
	if err := checkArchiveInput(input, identity, archivePath); err != nil {
		return Manifest{}, err
	}
	if !filepath.IsAbs(destination) {
		return Manifest{}, errors.New("backup extraction destination must be absolute")
	}
	if err := dirscan.Empty(context.Background(), destination); errors.Is(err, dirscan.ErrNotEmpty) {
		return Manifest{}, errors.New("backup extraction destination must be empty")
	} else if err != nil && !dirscan.Missing(err) {
		return Manifest{}, err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return Manifest{}, err
	}
	expected, err := newObservationIndex(os.TempDir())
	if err != nil {
		return Manifest{}, err
	}
	defer func() {
		err = errors.Join(err, expected.Close())
		if err != nil {
			manifest = Manifest{}
		}
	}()
	for _, file := range manifest.Files {
		if err := expected.Add(file); err != nil {
			return Manifest{}, err
		}
	}
	extracted, err := newObservationIndex(os.TempDir())
	if err != nil {
		return Manifest{}, err
	}
	defer func() {
		err = errors.Join(err, extracted.Close())
		if err != nil {
			manifest = Manifest{}
		}
	}()
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return Manifest{}, err
	}
	secondDigest := sha256.New()
	decrypted, err := newDecryptReader(io.TeeReader(input, secondDigest), backupKey)
	if err != nil {
		return Manifest{}, err
	}
	reader := tar.NewReader(decrypted)
	foundManifest := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("read backup archive: %w", err)
		}
		if foundManifest {
			return Manifest{}, errors.New("backup contains entries after manifest")
		}
		name, pathErr := safeArchivePath(header.Name)
		if pathErr != nil || header.Typeflag != tar.TypeReg || header.Size < 0 {
			return Manifest{}, errors.New("backup contains an unsafe archive entry")
		}
		if name == "manifest.json" {
			if _, err := io.Copy(io.Discard, reader); err != nil {
				return Manifest{}, err
			}
			foundManifest = true
			continue
		}
		file, exists, err := expected.Get(name)
		if err != nil {
			return Manifest{}, err
		}
		if !exists {
			return Manifest{}, fmt.Errorf("backup contains unmanifested file %q", name)
		}
		if _, duplicate, err := extracted.Get(name); err != nil {
			return Manifest{}, err
		} else if duplicate {
			return Manifest{}, fmt.Errorf("duplicate backup entry %q", name)
		}
		localPath := filepath.Join(destination, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
			return Manifest{}, err
		}
		actual, err := writeExtractedPayload(context.Background(), localPath, name, reader, nil)
		if err != nil {
			return Manifest{}, err
		}
		if actual != file {
			return Manifest{}, fmt.Errorf("backup checksum mismatch for %q", name)
		}
		if err := extracted.Add(actual); err != nil {
			return Manifest{}, err
		}
	}
	if _, err := io.Copy(io.Discard, decrypted); err != nil {
		return Manifest{}, fmt.Errorf("verify authenticated backup ending: %w", err)
	}
	if !foundManifest || extracted.count != expected.count {
		return Manifest{}, errors.New("extracted backup file set does not match manifest")
	}
	if !bytes.Equal(firstDigest.Sum(nil), secondDigest.Sum(nil)) {
		return Manifest{}, errors.New("backup archive changed between verification and extraction")
	}
	if err := checkArchiveInput(input, identity, archivePath); err != nil {
		return Manifest{}, err
	}
	return manifest, durable.SyncDirectory(destination)
}

func checkArchiveInput(input *os.File, identity os.FileInfo, archivePath string) error {
	opened, err := input.Stat()
	if err != nil {
		return err
	}
	named, err := os.Stat(archivePath)
	if err != nil {
		return err
	}
	if !identity.Mode().IsRegular() || !os.SameFile(identity, opened) || !os.SameFile(identity, named) || identity.Size() != opened.Size() || identity.Size() != named.Size() || !identity.ModTime().Equal(opened.ModTime()) || !identity.ModTime().Equal(named.ModTime()) {
		return errors.New("backup archive changed during extraction")
	}
	return nil
}

func validFingerprint(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil
}

func expectedFile(files []File, name string) (File, bool) {
	for _, file := range files {
		if file.Path == name {
			return file, true
		}
	}
	return File{}, false
}

func writeSource(ctx context.Context, archive *tar.Writer, source SourceFile) (File, error) {
	return writeSourceWithClose(ctx, archive, source, ownedfile.CloseFile)
}

func writeSourceWithClose(ctx context.Context, archive *tar.Writer, source SourceFile, closeInput func(*os.File) error) (result File, resultErr error) {
	info, err := os.Lstat(source.LocalPath)
	if err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() {
		return File{}, fmt.Errorf("backup source %q is not a regular file", source.LocalPath)
	}
	if hasMultipleLinks(info) {
		return File{}, fmt.Errorf("backup source %q has multiple hard links", source.LocalPath)
	}
	input, err := os.OpenFile(source.LocalPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return File{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeInput(input))
		if resultErr != nil {
			result = File{}
		}
	}()
	opened, err := input.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || hasMultipleLinks(opened) {
		return File{}, errors.Join(err, ownedfile.ErrReplaced)
	}
	header := &tar.Header{
		Name: source.ArchivePath, Mode: 0o600, Size: info.Size(),
		ModTime: info.ModTime().UTC(), Typeflag: tar.TypeReg,
	}
	if err := archive.WriteHeader(header); err != nil {
		return File{}, err
	}
	hash := sha256.New()
	var buffer [32 << 10]byte
	count, err := io.CopyBuffer(io.MultiWriter(archive, hash), backupContextReader{ctx: ctx, reader: io.LimitReader(input, info.Size())}, buffer[:])
	if err != nil || count != info.Size() {
		return File{}, errors.Join(fmt.Errorf("copy backup source %q", source.LocalPath), err, io.ErrUnexpectedEOF)
	}
	current, err := input.Stat()
	named, nameErr := os.Lstat(source.LocalPath)
	if err != nil || nameErr != nil || !os.SameFile(info, current) || !os.SameFile(info, named) || info.Size() != current.Size() || !info.ModTime().Equal(current.ModTime()) {
		return File{}, errors.Join(err, nameErr, ownedfile.ErrReplaced)
	}
	return File{
		Path: source.ArchivePath, Size: count,
		SHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func safeArchivePath(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\\') || path.IsAbs(value) {
		return "", errors.New("backup archive path is unsafe")
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return "", errors.New("backup archive path is unsafe")
	}
	return clean, nil
}

func randomID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "bkp_" + hex.EncodeToString(random[:]), nil
}

// IO remains owned until Read returns; cancellation never releases resources
// around a still-running filesystem read.
type backupContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r backupContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
