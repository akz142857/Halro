package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const VerifiedFileRAMBytes int64 = 4 << 20

// VerifiedFile is an exclusively created derivative of one authenticated backup
// payload. It is a source owner, not a runtime baseline or a restore permission.
// Its descriptor is available only inside WithReadOnly. Callbacks must not
// reenter this owner, close the borrowed descriptor, or let it escape.
type VerifiedFile struct {
	mu                       sync.Mutex
	file                     *os.File
	owner                    *ownedfile.Owner
	identity, parentIdentity os.FileInfo
	path, parent             string
	fact                     File
	budget                   *resourcebudget.Manager
	disk                     resourcebudget.Permit
	ctx                      context.Context
	cancel                   context.CancelFunc
	ready, closed, discarded bool
	retirementErr            error
}

// OpenVerifiedFile revalidates the entire original ciphertext, including its
// authenticated ending, before exposing the selected payload. Only that file
// is materialized. destination must be an absent file in an existing private
// directory under caller exclusion. maxBytes and the same Manager's disk/RAM
// admission are checked before file creation. This logical reservation does not
// reserve filesystem blocks or reconcile production disk inventory.
//
// On failure the result is normally nil. If physical cleanup fails, a non-nil
// cleanup-only owner and the error are returned; retry Discard rather than
// refunding its disk fee. The caller must always inspect both results.
func (h *VerifiedArchive) OpenVerifiedFile(ctx context.Context, name, destination string, maxBytes int64, budget *resourcebudget.Manager) (result *VerifiedFile, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := h.check(); err != nil {
		return nil, err
	}
	if h.active || budget == nil || maxBytes <= 0 || !filepath.IsAbs(destination) {
		return nil, errors.New("invalid verified backup file request")
	}
	clean, err := safeArchivePath(name)
	if err != nil || clean != name {
		return nil, errors.New("invalid selected backup path")
	}
	fact, found, err := h.observed.Get(name)
	if err != nil {
		return nil, err
	}
	if !found || fact.Size < 0 || fact.Size > maxBytes {
		return nil, errors.New("selected backup file missing or exceeds limit")
	}
	parent := filepath.Dir(destination)
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm()&0077 != 0 {
		return nil, errors.Join(errors.New("selected backup file requires a private directory"), err)
	}
	disk, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: fact.Size})
	if err != nil {
		return nil, err
	}
	ram, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: VerifiedFileRAMBytes})
	if err != nil {
		return nil, errors.Join(err, disk.Release())
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, ram.Release())
		}
	}()
	v := &VerifiedFile{path: destination, parent: parent, parentIdentity: parentInfo, fact: fact, budget: budget, disk: disk}
	v.ctx, v.cancel = context.WithCancel(context.Background())
	defer func() {
		if resultErr != nil {
			if cleanup := v.Discard(); cleanup != nil {
				result, resultErr = v, errors.Join(resultErr, cleanup)
			} else {
				result = nil
			}
		}
	}()
	output, owner, err := ownedfile.Create(destination, os.O_WRONLY, 0600)
	if err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) {
			v.retirementErr = err
		}
		return nil, err
	}
	v.file, v.owner = output, owner
	v.identity, err = output.Stat()
	if err != nil {
		return nil, err
	}
	h.active = true
	defer func() { h.active = false }()
	if err := h.extractSelected(ctx, fact, output); err != nil {
		return nil, err
	}
	syncErr, closeErr := output.Sync(), output.Close()
	v.file = nil
	if closeErr != nil {
		v.retirementErr = errors.Join(ownedfile.ErrRetirement, closeErr)
		closeErr = v.retirementErr
	}
	if err := errors.Join(syncErr, closeErr); err != nil {
		return nil, err
	}
	if err := v.checkParent(); err != nil {
		return nil, err
	}
	if err := syncSelectedDirectory(parent); err != nil {
		return nil, err
	}
	v.file, err = os.Open(destination)
	if err != nil {
		return nil, err
	}
	if err := v.verify(ctx); err != nil {
		return nil, err
	}
	v.ready = true
	return v, nil
}

func (h *VerifiedArchive) extractSelected(ctx context.Context, selected File, output io.Writer) error {
	if _, err := h.input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := sha256.New()
	decrypted, err := newDecryptReader(io.TeeReader(backupContextReader{ctx: ctx, reader: h.input}, digest), h.key)
	if err != nil {
		return err
	}
	reader := tar.NewReader(decrypted)
	var count uint64
	manifest, selectedSeen := false, false
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
		if manifest {
			return errors.New("backup contains entries after manifest")
		}
		name, err := safeArchivePath(header.Name)
		if err != nil || header.Typeflag != tar.TypeReg || header.Size < 0 {
			return errors.New("backup contains unsafe entry")
		}
		if name == "manifest.json" || h.root.FormatVersion == pagedManifestVersion && isCatalogPath(name) {
			limit := int64(backupManifestSizeLimit)
			if h.root.FormatVersion == pagedManifestVersion {
				limit = pagedMetadataLimit
			}
			if header.Size > limit {
				return errors.New("backup metadata exceeds limit")
			}
			if _, err := io.Copy(io.Discard, reader); err != nil {
				return err
			}
			manifest = name == "manifest.json"
			continue
		}
		fact, exists, err := h.observed.Get(name)
		if err != nil {
			return err
		}
		if !exists || fact.Size != header.Size || count >= h.root.Catalog.Files {
			return errors.New("backup payload inventory changed")
		}
		count++
		hash := sha256.New()
		var writer io.Writer = hash
		if name == selected.Path {
			if selectedSeen {
				return errors.New("duplicate selected backup file")
			}
			selectedSeen = true
			writer = io.MultiWriter(output, hash)
		}
		size, err := io.Copy(writer, backupContextReader{ctx: ctx, reader: reader})
		if err != nil {
			return err
		}
		if size != fact.Size || hex.EncodeToString(hash.Sum(nil)) != fact.SHA256 {
			return errors.New("backup payload checksum changed")
		}
	}
	if _, err := io.Copy(io.Discard, backupContextReader{ctx: ctx, reader: decrypted}); err != nil {
		return err
	}
	// The exact original ciphertext binds ordering, all catalog entries and the
	// complete payload set. A second unbounded seen-name collection is unnecessary.
	if !manifest || !selectedSeen || count != h.root.Catalog.Files || !bytes.Equal(digest.Sum(nil), h.digest[:]) {
		return errors.New("backup changed between verification and selected extraction")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.check()
}

func (v *VerifiedFile) checkParent() error {
	info, err := os.Lstat(v.parent)
	if err != nil || !os.SameFile(info, v.parentIdentity) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	return nil
}

func (v *VerifiedFile) verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := v.checkParent(); err != nil {
		return err
	}
	info, err := v.file.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(v.path)
	if err != nil || !os.SameFile(info, v.identity) || !os.SameFile(info, named) || info.Size() != v.fact.Size {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, backupContextReader{ctx: ctx, reader: io.NewSectionReader(v.file, 0, v.fact.Size)})
	if err != nil {
		return err
	}
	if size != v.fact.Size || hex.EncodeToString(hash.Sum(nil)) != v.fact.SHA256 {
		return errors.New("verified backup file content changed")
	}
	return ctx.Err()
}

// WithReadOnly holds this owner and its IO RAM fee until the actual consumer
// returns. It verifies the pinned bytes before and after the consumer. Close
// cancels this context and joins the consumer; cancellation alone refunds no fee.
// Any consumer-built candidate stays private until this method succeeds.
func (v *VerifiedFile) WithReadOnly(ctx context.Context, consume func(context.Context, *os.File, File) error) (resultErr error) {
	if v == nil || consume == nil {
		return errors.New("missing verified backup file owner or consumer")
	}
	if !v.mu.TryLock() {
		return errors.New("verified backup file is busy")
	}
	defer v.mu.Unlock()
	if !v.ready || v.closed {
		return errors.New("verified backup file is not available")
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(v.ctx, cancel)
	defer func() { stop(); cancel() }()
	ram, err := v.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: VerifiedFileRAMBytes})
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, ram.Release()) }()
	if err := v.verify(ctx); err != nil {
		v.ready = false
		return err
	}
	if _, err := v.file.Seek(0, io.SeekStart); err != nil {
		v.ready = false
		return err
	}
	resultErr = consume(ctx, v.file, v.fact)
	if err := v.verify(ctx); err != nil {
		v.ready = false
		resultErr = errors.Join(resultErr, err)
	}
	return resultErr
}

// Close retires the descriptor but retains the file's disk fee. Only successful
// identity-checked deletion and directory sync in Discard refund that fee.
func (v *VerifiedFile) Close() error {
	if v == nil {
		return nil
	}
	v.cancel()
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.closeLocked()
}
func (v *VerifiedFile) closeLocked() error {
	v.ready, v.closed = false, true
	if v.retirementErr != nil {
		return v.retirementErr
	}
	if v.file == nil {
		return nil
	}
	err := v.file.Close()
	v.file = nil
	if err != nil {
		v.retirementErr = errors.Join(ownedfile.ErrRetirement, err)
		return v.retirementErr
	}
	return err
}
func (v *VerifiedFile) Discard() error {
	if v == nil {
		return nil
	}
	v.cancel()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.discarded {
		return nil
	}
	if err := v.closeLocked(); err != nil {
		return err
	}
	if v.owner != nil {
		if err := v.checkParent(); err != nil {
			return err
		}
		if err := v.owner.Remove(); err != nil {
			return err
		}
		if err := syncSelectedDirectory(v.parent); err != nil {
			return err
		}
	}
	if err := v.disk.Release(); err != nil {
		return err
	}
	v.discarded = true
	return nil
}
func syncSelectedDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
