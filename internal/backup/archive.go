package backup

import (
	"archive/tar"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/buildinfo"
	"github.com/akz142857/Heimdall/internal/ledger"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

const manifestVersion = 1

type SourceFile struct {
	ArchivePath string
	LocalPath   string
}

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	FormatVersion        int                    `json:"format_version"`
	BackupID             string                 `json:"backup_id"`
	CreatedAt            time.Time              `json:"created_at"`
	Encrypted            bool                   `json:"encrypted"`
	Metadata             boltstore.MetadataInfo `json:"metadata"`
	LedgerWatermark      ledger.Watermark       `json:"ledger_watermark"`
	CheckpointWatermark  ledger.Watermark       `json:"checkpoint_watermark"`
	UsageManifestVersion int                    `json:"usage_manifest_version"`
	MasterKeyFingerprint string                 `json:"master_key_fingerprint"`
	Build                buildinfo.Info         `json:"build"`
	Files                []File                 `json:"files"`
}

type CreateOptions struct {
	OutputPath           string
	BackupKey            []byte
	Files                []SourceFile
	Metadata             boltstore.MetadataInfo
	LedgerWatermark      ledger.Watermark
	CheckpointWatermark  ledger.Watermark
	UsageManifestVersion int
	MasterKeyFingerprint string
	Build                buildinfo.Info
	Now                  func() time.Time
}

func Create(options CreateOptions) (Manifest, error) {
	if !filepath.IsAbs(options.OutputPath) {
		return Manifest{}, errors.New("backup output path must be absolute")
	}
	if len(options.BackupKey) != 32 {
		return Manifest{}, errors.New("backup key must be exactly 32 bytes")
	}
	if len(options.Files) == 0 {
		return Manifest{}, errors.New("backup files are required")
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
	files := slices.Clone(options.Files)
	slices.SortFunc(files, func(left, right SourceFile) int {
		return strings.Compare(left.ArchivePath, right.ArchivePath)
	})
	seen := make(map[string]struct{}, len(files))
	for index := range files {
		clean, err := safeArchivePath(files[index].ArchivePath)
		if err != nil {
			return Manifest{}, err
		}
		files[index].ArchivePath = clean
		if _, exists := seen[clean]; exists || clean == "manifest.json" {
			return Manifest{}, fmt.Errorf("duplicate or reserved backup path %q", clean)
		}
		seen[clean] = struct{}{}
	}
	backupID, err := randomID()
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{
		FormatVersion: manifestVersion, BackupID: backupID,
		CreatedAt: options.Now().UTC(), Encrypted: true,
		Metadata: options.Metadata, LedgerWatermark: options.LedgerWatermark,
		CheckpointWatermark:  options.CheckpointWatermark,
		UsageManifestVersion: options.UsageManifestVersion,
		MasterKeyFingerprint: options.MasterKeyFingerprint,
		Build:                options.Build,
	}
	temp, err := os.CreateTemp(filepath.Dir(options.OutputPath), ".heimdall-backup-*.tmp")
	if err != nil {
		return Manifest{}, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return Manifest{}, err
	}
	encrypted, err := newEncryptWriter(temp, options.BackupKey)
	if err != nil {
		temp.Close()
		return Manifest{}, err
	}
	archive := tar.NewWriter(encrypted)
	for _, source := range files {
		entry, err := writeSource(archive, source)
		if err != nil {
			archive.Close()
			encrypted.Close()
			temp.Close()
			return Manifest{}, err
		}
		manifest.Files = append(manifest.Files, entry)
	}
	encodedManifest, err := json.Marshal(manifest)
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
	if syncErr := temp.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Manifest{}, err
	}
	// Hard-link publication is atomic and, unlike Rename, cannot overwrite an
	// output created by a racing process after the initial existence check.
	if err := os.Link(tempPath, options.OutputPath); err != nil {
		return Manifest{}, err
	}
	if err := os.Remove(tempPath); err != nil {
		return Manifest{}, err
	}
	if err := syncDirectory(filepath.Dir(options.OutputPath)); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func Verify(archivePath string, backupKey []byte) (Manifest, error) {
	input, err := os.Open(archivePath)
	if err != nil {
		return Manifest{}, err
	}
	defer input.Close()
	decrypted, err := newDecryptReader(input, backupKey)
	if err != nil {
		return Manifest{}, err
	}
	reader := tar.NewReader(decrypted)
	observed := make(map[string]File)
	var manifest Manifest
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
			if header.Size > 1<<20 {
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
		if _, duplicate := observed[name]; duplicate {
			return Manifest{}, fmt.Errorf("duplicate backup entry %q", name)
		}
		hash := sha256.New()
		count, err := io.Copy(hash, reader)
		if err != nil || count != header.Size {
			return Manifest{}, fmt.Errorf("read backup entry %q", name)
		}
		observed[name] = File{
			Path: name, Size: count, SHA256: hex.EncodeToString(hash.Sum(nil)),
		}
	}
	if _, err := io.Copy(io.Discard, decrypted); err != nil {
		return Manifest{}, fmt.Errorf("verify authenticated backup ending: %w", err)
	}
	if !foundManifest || manifest.FormatVersion != manifestVersion || !manifest.Encrypted ||
		manifest.BackupID == "" || manifest.CreatedAt.IsZero() ||
		manifest.Metadata.SchemaVersion == 0 ||
		manifest.Metadata.SchemaVersion > boltstore.CurrentSchemaVersion() ||
		manifest.Metadata.TxID == 0 ||
		manifest.CheckpointWatermark.Sequence > manifest.LedgerWatermark.Sequence ||
		manifest.CheckpointWatermark.Offset > manifest.LedgerWatermark.Offset ||
		!validFingerprint(manifest.MasterKeyFingerprint) {
		return Manifest{}, errors.New("backup manifest is invalid")
	}
	if len(observed) != len(manifest.Files) {
		return Manifest{}, errors.New("backup file set does not match manifest")
	}
	for _, expected := range manifest.Files {
		actual, exists := observed[expected.Path]
		if !exists || actual != expected {
			return Manifest{}, fmt.Errorf("backup checksum mismatch for %q", expected.Path)
		}
		delete(observed, expected.Path)
	}
	if len(observed) != 0 {
		return Manifest{}, errors.New("backup contains unmanifested files")
	}
	for _, required := range []string{
		"config/config.yaml", "data/metadata.db",
		"data/ledger/ledger.wal", "data/audit/audit.log",
	} {
		if _, exists := expectedFile(manifest.Files, required); !exists {
			return Manifest{}, fmt.Errorf("backup is missing required file %q", required)
		}
	}
	return manifest, nil
}

// Extract verifies the complete authenticated archive before writing regular
// files into an empty staging directory. Callers must validate staged domain
// data before publishing it as the live data directory.
func Extract(archivePath string, backupKey []byte, destination string) (Manifest, error) {
	manifest, err := Verify(archivePath, backupKey)
	if err != nil {
		return Manifest{}, err
	}
	if !filepath.IsAbs(destination) {
		return Manifest{}, errors.New("backup extraction destination must be absolute")
	}
	if entries, err := os.ReadDir(destination); err == nil && len(entries) != 0 {
		return Manifest{}, errors.New("backup extraction destination must be empty")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return Manifest{}, err
	}
	input, err := os.Open(archivePath)
	if err != nil {
		return Manifest{}, err
	}
	defer input.Close()
	decrypted, err := newDecryptReader(input, backupKey)
	if err != nil {
		return Manifest{}, err
	}
	reader := tar.NewReader(decrypted)
	extracted := make(map[string]struct{}, len(manifest.Files))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("read backup archive: %w", err)
		}
		name, pathErr := safeArchivePath(header.Name)
		if pathErr != nil || header.Typeflag != tar.TypeReg || header.Size < 0 {
			return Manifest{}, errors.New("backup contains an unsafe archive entry")
		}
		if name == "manifest.json" {
			if _, err := io.Copy(io.Discard, reader); err != nil {
				return Manifest{}, err
			}
			continue
		}
		expected, exists := expectedFile(manifest.Files, name)
		if !exists {
			return Manifest{}, fmt.Errorf("backup contains unmanifested file %q", name)
		}
		if _, duplicate := extracted[name]; duplicate {
			return Manifest{}, fmt.Errorf("duplicate backup entry %q", name)
		}
		localPath := filepath.Join(destination, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
			return Manifest{}, err
		}
		output, err := os.OpenFile(localPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return Manifest{}, err
		}
		hash := sha256.New()
		count, copyErr := io.Copy(io.MultiWriter(output, hash), reader)
		syncErr := output.Sync()
		closeErr := output.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			return Manifest{}, err
		}
		actualHash := hex.EncodeToString(hash.Sum(nil))
		if count != expected.Size || actualHash != expected.SHA256 {
			return Manifest{}, fmt.Errorf("backup checksum mismatch for %q", name)
		}
		extracted[name] = struct{}{}
	}
	if _, err := io.Copy(io.Discard, decrypted); err != nil {
		return Manifest{}, fmt.Errorf("verify authenticated backup ending: %w", err)
	}
	if len(extracted) != len(manifest.Files) {
		return Manifest{}, errors.New("extracted backup file set does not match manifest")
	}
	return manifest, syncDirectory(destination)
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

func writeSource(archive *tar.Writer, source SourceFile) (File, error) {
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
	input, err := os.Open(source.LocalPath)
	if err != nil {
		return File{}, err
	}
	defer input.Close()
	header := &tar.Header{
		Name: source.ArchivePath, Mode: 0o600, Size: info.Size(),
		ModTime: info.ModTime().UTC(), Typeflag: tar.TypeReg,
	}
	if err := archive.WriteHeader(header); err != nil {
		return File{}, err
	}
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(archive, hash), input)
	if err != nil || count != info.Size() {
		return File{}, fmt.Errorf("copy backup source %q", source.LocalPath)
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

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
