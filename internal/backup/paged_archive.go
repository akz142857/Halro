package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/dirscan"
	"github.com/akz142857/Halro/internal/ownedfile"
	"io"
	"maps"
	"os"
	"path/filepath"
)

// CreatePaged writes format 5 without collecting Manifest.Files. The App summary
// and CLI paths select it; collecting Create remains for the bounded legacy API.
// It consumes SourceCatalog just like Create.
func CreatePaged(options CreateOptions) (PagedManifest, error) {
	var root PagedManifest
	if _, err := createArchive(options, &root); err != nil {
		return PagedManifest{}, err
	}
	return root, nil
}

// VerifiedPagedArchive owns a pinned authenticated ciphertext, private file
// facts and a key copy. All methods are serial; callbacks must not reenter it.
// A caller must Close before publishing staged domain data. No live-data
// publication or saved-index trust is granted by this file-format handle.
type VerifiedPagedArchive struct {
	root      PagedManifest
	input     *os.File
	identity  os.FileInfo
	path      string
	key       []byte
	digest    [32]byte
	observed  *observationIndex
	active    bool
	closed    bool
	closeErr  error
	workspace *WorkspaceBudget
}

// VerifiedArchive is the common lifetime owner for formats 1-5.
type VerifiedArchive = VerifiedPagedArchive

func OpenVerified(ctx context.Context, path string, key []byte) (*VerifiedArchive, error) {
	return openVerified(ctx, path, key, false)
}
func OpenPaged(ctx context.Context, path string, key []byte) (*VerifiedPagedArchive, error) {
	return openVerified(ctx, path, key, true)
}
func openVerified(ctx context.Context, path string, key []byte, pagedOnly bool) (result *VerifiedArchive, err error) {
	if len(key) != 32 {
		return nil, errors.New("backup key must be exactly 32 bytes")
	}
	h := &VerifiedPagedArchive{path: path, key: bytes.Clone(key), workspace: backupWorkspace(ctx)}
	defer func() {
		if err != nil {
			err = errors.Join(err, h.Close())
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	h.input, err = os.Open(path)
	if err != nil {
		return nil, err
	}
	h.identity, err = h.input.Stat()
	if err != nil {
		return nil, err
	}
	if !h.identity.Mode().IsRegular() {
		return nil, errors.New("backup input is not a regular file")
	}
	digest := sha256.New()
	h.root, h.observed, err = verifyAnyArchive(ctx, io.TeeReader(backupContextReader{ctx: ctx, reader: h.input}, digest), key)
	if err != nil {
		return nil, err
	}
	if pagedOnly && h.root.FormatVersion != pagedManifestVersion {
		return nil, errors.New("OpenPaged requires format 5")
	}
	copy(h.digest[:], digest.Sum(nil))
	if err = checkArchiveInput(h.input, h.identity, path); err != nil {
		return nil, err
	}
	return h, nil
}
func (h *VerifiedPagedArchive) Summary() PagedManifest {
	if h == nil || h.closed {
		return PagedManifest{}
	}
	root := h.root
	root.PerStoreHead = maps.Clone(root.PerStoreHead)
	return root
}

// PayloadBytes returns the authenticated sum of extracted file sizes from the
// complete verification pass. It excludes format-5 catalog pages.
func (h *VerifiedPagedArchive) PayloadBytes() (int64, error) {
	if err := h.check(); err != nil {
		return 0, err
	}
	return h.root.contentBytes, nil
}

func (h *VerifiedPagedArchive) check() error {
	if h == nil || h.closed || h.input == nil || h.observed == nil {
		return errors.New("paged backup handle is closed")
	}
	if err := h.observed.check(); err != nil {
		return err
	}
	return checkArchiveInput(h.input, h.identity, h.path)
}
func (h *VerifiedPagedArchive) FilesEach(ctx context.Context, consume func(File) error) error {
	if err := h.check(); err != nil {
		return err
	}
	if h.active || consume == nil {
		return errors.New("paged backup handle operation is active or consumer is absent")
	}
	h.active = true
	defer func() { h.active = false }()
	if err := h.observed.Walk(ctx, consume); err != nil {
		return err
	}
	return h.check()
}
func (h *VerifiedPagedArchive) Close() error {
	if h == nil {
		return nil
	}
	if h.closed {
		return h.closeErr
	}
	if h.active {
		return errors.New("paged backup handle operation is active")
	}
	h.closed = true
	var err error
	if h.input != nil {
		if closeErr := h.input.Close(); closeErr != nil {
			err = errors.Join(ownedfile.ErrRetirement, closeErr)
		}
		h.input = nil
	}
	err = errors.Join(err, h.observed.Close())
	h.observed = nil
	clear(h.key)
	h.key = nil
	h.closeErr = err
	return err
}

func readPagedMetadata(reader io.Reader, size int64) ([]byte, error) {
	if size < 0 || size > pagedMetadataLimit {
		return nil, errors.New("paged backup metadata exceeds limit")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, size+1))
	if err != nil || int64(len(raw)) != size {
		return nil, errors.Join(errors.New("read backup metadata"), err)
	}
	return raw, nil
}
func pagedRequiredNames(version int) []string {
	required := []string{"config/config.yaml", "data/metadata.db", "data/ledger/ledger.wal", "data/audit/audit.log"}
	if version >= 3 {
		required = append(required, "data/governance/governance.journal")
	}
	if version == 4 {
		required = append(required, "data/cluster/state.json", "data/cluster/ordering.journal", "data/metadata.journal")
	}
	return required
}
func validatePagedSources(ctx context.Context, root PagedManifest, catalog *SourceCatalog) error {
	required := pagedRequiredNames(root.CompatibilityVersion)
	var found [8]bool
	if err := catalog.Walk(ctx, func(source SourceFile) error {
		if isCatalogPath(source.ArchivePath) {
			return errors.New("backup source uses reserved catalog path")
		}
		for n, name := range required {
			if source.ArchivePath == name {
				found[n] = true
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for n, name := range required {
		if !found[n] {
			return fmt.Errorf("backup is missing required file %q", name)
		}
	}
	return nil
}
func requiredPagedFiles(root PagedManifest, actual *observationIndex) error {
	for _, name := range pagedRequiredNames(root.CompatibilityVersion) {
		if _, found, err := actual.Get(name); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("backup is missing required file %q", name)
		}
	}
	return actual.check()
}

// Extract requires an empty private staging directory. It compares the entire
// second ciphertext stream to the successful first verification on the same
// descriptor and validates every actual payload against immutable disk facts.
// Failure may leave a staging prefix; the caller owns its cleanup.
func (h *VerifiedPagedArchive) Extract(ctx context.Context, destination string) (err error) {
	if err := h.check(); err != nil {
		return err
	}
	var finish func()
	ctx, finish, err = h.workspace.methodContext(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if h.active {
		return errors.New("paged backup handle operation is active")
	}
	h.active = true
	defer func() { h.active = false }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(destination) {
		return errors.New("backup extraction destination must be absolute")
	}
	if err := dirscan.Empty(ctx, destination); errors.Is(err, dirscan.ErrNotEmpty) {
		return errors.New("backup extraction destination must be empty")
	} else if err != nil && !dirscan.Missing(err) {
		return err
	}
	extracted, err := newObservationIndexForWorkspace(ctx, os.TempDir(), 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, extracted.Close()) }()
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	if _, err := h.input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := sha256.New()
	decrypted, err := newDecryptReader(io.TeeReader(backupContextReader{ctx: ctx, reader: h.input}, digest), h.key)
	if err != nil {
		return err
	}
	reader := tar.NewReader(decrypted)
	found := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if found {
			return errors.New("backup contains entries after manifest")
		}
		name, pathErr := safeArchivePath(header.Name)
		if pathErr != nil || header.Typeflag != tar.TypeReg || header.Size < 0 {
			return errors.New("backup contains unsafe entry")
		}
		if name == "manifest.json" || h.root.FormatVersion == pagedManifestVersion && isCatalogPath(name) {
			limit := int64(backupManifestSizeLimit)
			if h.root.FormatVersion == pagedManifestVersion {
				limit = pagedMetadataLimit
			}
			if header.Size > limit {
				return errors.New("paged backup metadata exceeds limit")
			}
			if _, err := io.Copy(io.Discard, reader); err != nil {
				return err
			}
			if name == "manifest.json" {
				found = true
			}
			continue
		}
		file, exists, err := h.observed.Get(name)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("backup contains undeclared payload")
		}
		if _, duplicate, err := extracted.Get(name); err != nil {
			return err
		} else if duplicate {
			return errors.New("duplicate extracted file")
		}
		filename := filepath.Join(destination, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			return err
		}
		actual, err := writeExtractedPayload(ctx, filename, name, reader, nil)
		if err != nil {
			return err
		}
		if actual != file {
			return errors.New("extracted backup checksum mismatch")
		}
		if err := extracted.Add(actual); err != nil {
			return err
		}
	}
	if _, err := io.Copy(io.Discard, backupContextReader{ctx: ctx, reader: decrypted}); err != nil {
		return err
	}
	if !found || extracted.count != h.root.Catalog.Files || !bytes.Equal(h.digest[:], digest.Sum(nil)) {
		return errors.New("backup changed between verification and extraction")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.check()
}

// ManifestDigest preserves the canonical old Manifest digest and binds the
// complete format5 catalog through its authenticated fixed root.
func (h *VerifiedPagedArchive) ManifestDigest() (string, error) {
	if err := h.check(); err != nil {
		return "", err
	}
	if h.root.FormatVersion < pagedManifestVersion {
		return h.root.legacyManifestDigest, nil
	}
	raw, err := json.Marshal(h.root)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// CiphertextDigest binds the exact complete archive stream authenticated by
// this handle. It is source evidence, not authority to restore a role or term.
func (h *VerifiedArchive) CiphertextDigest() ([32]byte, error) {
	if err := h.check(); err != nil {
		return [32]byte{}, err
	}
	return h.digest, nil
}
