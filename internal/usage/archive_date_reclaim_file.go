package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	bbolt "go.etcd.io/bbolt"
)

var ErrArchiveDateReferenced = errors.New("date reclaim target is referenced by the current root")

type DateFileReclaimLimits struct {
	FileRows                                        int
	FileBytes, ReadBytes, NativeBytes, IndexIOBytes int64
	MaxDuration                                     time.Duration
}

func DefaultDateFileReclaimLimits() DateFileReclaimLimits {
	return DateFileReclaimLimits{FileRows: 4096, FileBytes: 256 << 20, ReadBytes: 1 << 30, NativeBytes: 64 << 20, IndexIOBytes: 1 << 20, MaxDuration: 10 * time.Second}
}

// LogicalBytesUnlinked is the removed name's logical size, not released physical
// blocks/fees. Catalog pages and the persistent obligation remain unfinished.
type DateFileReclamation struct {
	Path                                       string
	Removed, AlreadyMissing, ObligationPending bool
	RowsVerified                               int
	LogicalBytesUnlinked                       int64
}

// A caller authenticates the native/role/Metadata scope independently. This API
// consumes a fresh published-root comparison proof and current named references;
// an old descriptor, saved phase/Ready, or missing file supplies no authority.
func (e *Exporter) ReclaimRetainedDateFile(ctx context.Context, p *DiskProjection, retainFrom time.Time, entry ManifestFile, limits DateFileReclaimLimits) (DateFileReclamation, error) {
	return e.reclaimRetainedDateFile(ctx, p, retainFrom, entry, limits, nil, nil)
}

// Hooks observe physical boundaries, never supply publication/deletion authority.
func (e *Exporter) reclaimRetainedDateFile(ctx context.Context, p *DiskProjection, retainFrom time.Time, entry ManifestFile, limits DateFileReclaimLimits, beforeUnlink func(*os.File) error, afterUnlink func() error) (out DateFileReclamation, err error) {
	err = e.withDateReclaimAuthority(ctx, p, retainFrom, limits, func(ctx context.Context, a *archiveDateReclaimAuthority) error {
		var err error
		out, err = a.reclaimFile(ctx, entry, limits, beforeUnlink, afterUnlink)
		return err
	})
	if err != nil {
		out = DateFileReclamation{}
	}
	return out, err
}

// One caller-owned physical operation consumes one fresh published proof. Every
// data object in it shares the read/native/index budgets and actual Close join.
type archiveDateReclaimAuthority struct {
	e                                     *Exporter
	p                                     *DiskProjection
	proof                                 *archiveDateProof
	root                                  *archiveManifestInput
	state                                 archiveDateActivationState
	directory                             os.FileInfo
	check                                 func() error
	advance                               func() error
	readBudget, nativeBudget, indexBudget *archiveVerifyReadBudget
	tree                                  *archiveTreeBudget
	keepProof                             bool // Only read-only checkpoint-prefix work without extension or Ready.
	dataComplete                          *archiveDateDataProof
	orphanInspectRemaining                *int // Optional aggregate discovery-row budget for a finite orphan batch.
}

func (a *archiveDateReclaimAuthority) referenced(ctx context.Context, path string) (bool, error) {
	return a.referenceContains(ctx, 2, path)
}

func (a *archiveDateReclaimAuthority) referenceContains(ctx context.Context, tree int, path string) (bool, error) {
	if err := a.readBudget.reserve(a.root.info.Size() + archiveDateBuildBytes); err != nil {
		return false, err
	}
	return a.e.dateReferenceContainsHeld(ctx, a.root, tree, path, a.indexBudget)
}

func (e *Exporter) withDateReclaimAuthority(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DateFileReclaimLimits, action func(context.Context, *archiveDateReclaimAuthority) error) (err error) {
	if p == nil || retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 || limits.FileRows < 1 || limits.FileRows > 1<<20 || limits.FileBytes < 1 || limits.FileBytes > 256<<20 || limits.ReadBytes < 2<<20 || limits.ReadBytes > 1<<30 || limits.NativeBytes < diskUsageMaxValue || limits.NativeBytes > 128<<20 || limits.IndexIOBytes < 1<<20 || limits.IndexIOBytes > 1<<30 || limits.MaxDuration <= 0 || limits.MaxDuration > time.Minute {
		return ErrDiskUsageLimit
	}
	// os.Root cannot promise handle-based confinement on these platforms.
	if runtime.GOOS == "js" || runtime.GOOS == "plan9" {
		return ErrDiskUsageLimit
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	releaseShared, err := e.reserveArchiveDisk(ctx, limits.FileBytes)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, releaseShared()) }()
	return e.withArchiveReclamation(ctx, func(advance func() error) (jobErr error) {
		tree, treeErr := newArchiveTreeBudget(ctx, e.root, e.archiveDiskCeiling)
		if treeErr != nil {
			return treeErr
		}
		proof := e.dateProof
		keepProof := false
		var completed *archiveDateDataProof
		defer func() {
			jobErr = errors.Join(jobErr, e.archiveReadError(ctx))
			if !keepProof || jobErr != nil {
				e.dateProof = nil
			}
			if jobErr != nil {
				jobErr = errors.Join(jobErr, e.discardDateOrphanPages())
				jobErr = errors.Join(jobErr, e.discardDateOrphans())
				e.dateReclaimReplay = nil
				e.dateDataProof = nil
			}
		}()
		if proof == nil || !proof.published || proof.owner != p.generation || proof.position.Retention.RetainFromDate != retainFrom.UTC().Format("2006-01-02") {
			return ErrArchiveDateProof
		}
		sourceLimits := DefaultDateCatalogVerifyLimits().Source
		sourceLimits.MaxDuration = limits.MaxDuration
		_, jobErr = p.archiveJob(ctx, sourceLimits, func(ctx context.Context) (_ DiskArchiveCut, physicalErr error) {
			defer func() {
				physicalErr = errors.Join(physicalErr, e.archiveReadError(ctx))
				if p.retired.Load() {
					physicalErr = errors.Join(physicalErr, ErrDiskUsageClosed)
				}
			}()
			directory, err := os.Lstat(e.root)
			if err != nil || !directory.IsDir() || !os.SameFile(directory, proof.directory) {
				return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
			}
			root, err := openArchiveManifest(e.root)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			defer func() { physicalErr = errors.Join(physicalErr, root.file.Close()) }()
			if !os.SameFile(root.info, proof.identity) || root.info.Size() != proof.identity.Size() || root.info.ModTime() != proof.identity.ModTime() {
				return DiskArchiveCut{}, ErrArchiveDateProof
			}
			readBudget := &archiveVerifyReadBudget{remaining: limits.ReadBytes}
			if err := readBudget.reserve(4*archiveManifestValueLimit + 2*archiveDateBuildBytes + archiveDateActivationBytes + archiveDateActivePolicyBytes); err != nil {
				return DiskArchiveCut{}, err
			}
			position, digest, err := root.walk(ctx, nil)
			if err != nil || position != proof.position || digest != proof.digest {
				return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
			}
			input, err := e.readCurrentDatePolicy(ctx, position, proof.head)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			state, raw, marker, markerInfo, markerPath := input.state, input.raw, input.file, input.info, input.path
			defer func() { physicalErr = errors.Join(physicalErr, marker.Close()) }()
			_, candidate, err := state.positions()
			if err != nil || candidate != proof.candidate || position.Retention != candidate.Retention || sha256.Sum256(raw) != proof.activationSHA {
				return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
			}
			verifyPath := filepath.Join(e.root, archiveDatePublishedVerifyName)
			verify, verifyRaw, verifyFile, verifyInfo, err := readDateVerifyState(ctx, verifyPath)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			defer func() { physicalErr = errors.Join(physicalErr, verifyFile.Close()) }()
			binding, err := dateActivationBinding(state)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			if sha256.Sum256(verifyRaw) != proof.verifySHA || verify.Phase != "ready" || !verify.Published || !verify.AncestorSeen || verify.SourceHead != proof.head || verify.BuildSHA != hex.EncodeToString(proof.buildSHA[:]) || verify.ActivationSHA != hex.EncodeToString(binding[:]) {
				return DiskArchiveCut{}, ErrArchiveDateProof
			}
			check := func() error {
				stats, err := p.ReadStats(ctx)
				if err != nil || stats.Watermark != proof.head || stats.Window != proof.window {
					return errors.Join(err, ErrArchiveDateProof)
				}
				named, err := os.Lstat(e.root)
				if err != nil || !os.SameFile(named, directory) {
					return errors.Join(err, ErrArchiveDateProof)
				}
				policyDigest := sha256.Sum256(raw)
				return errors.Join(root.check(), checkDateBuildState(marker, markerInfo, markerPath), hashDateReclaimFile(ctx, marker, markerInfo.Size(), hex.EncodeToString(policyDigest[:]), nil, readBudget), checkDateBuildState(verifyFile, verifyInfo, verifyPath), e.archiveReadError(ctx))
			}
			if err := check(); err != nil {
				return DiskArchiveCut{}, err
			}
			a := &archiveDateReclaimAuthority{e: e, p: p, proof: proof, root: root, state: state, directory: directory, check: check, advance: advance, readBudget: readBudget, nativeBudget: &archiveVerifyReadBudget{remaining: limits.NativeBytes}, indexBudget: &archiveVerifyReadBudget{remaining: limits.IndexIOBytes}, tree: tree}
			// Repeat intent/current-root barriers before any destructive operation,
			// regardless of the marker's untrusted derived phase.
			if err := marker.Sync(); err != nil {
				return DiskArchiveCut{}, err
			}
			if err := root.file.Sync(); err != nil {
				return DiskArchiveCut{}, err
			}
			if err := syncDateRootDirectory(e.root, directory); err != nil {
				return DiskArchiveCut{}, err
			}
			if err := action(ctx, a); err != nil {
				return DiskArchiveCut{}, err
			}
			keepProof = a.keepProof
			completed = a.dataComplete
			return DiskArchiveCut{}, check()
		})
		// Mint only after the projection job and every actual file Close have
		// succeeded, while the exporter writer/lifetime exclusion is still held.
		if jobErr == nil && completed != nil {
			e.dateDataProof = completed
		}
		return jobErr
	})
}

func (a *archiveDateReclaimAuthority) reclaimFile(ctx context.Context, entry ManifestFile, limits DateFileReclaimLimits, beforeUnlink func(*os.File) error, afterUnlink func() error) (out DateFileReclamation, err error) {
	e, p, proof, directory, check, advance := a.e, a.p, a.proof, a.directory, a.check, a.advance
	defer func() {
		if err != nil {
			out = DateFileReclamation{}
		}
	}()
	if err := e.validateCatalogEntry(entry); err != nil {
		return out, err
	}
	if err := validateArchivePartitionDate(entry); err != nil {
		return out, err
	}
	if len(entry.Path) > 4096 || entry.Date >= proof.position.Retention.RetainFromDate || entry.MaxSequence > proof.position.Retention.PruneThroughSequence {
		return out, ErrArchiveDateProof
	}
	found, err := a.referenced(ctx, entry.Path)
	if err != nil {
		return out, err
	}
	if found {
		return out, ErrArchiveDateReferenced
	}
	parent, leaf, parentsCheck, err := openDateReclaimParent(ctx, e.root, directory, entry.Path)
	if err != nil {
		return out, err
	}
	if parent == nil {
		// Missing parents mean no deletion. Current coverage/authority must
		// still be valid; sync the existing archive directory for retry.
		if err := check(); err != nil {
			return out, err
		}
		if err := syncDateRootDirectory(e.root, directory); err != nil {
			return out, err
		}
		out = DateFileReclamation{Path: entry.Path, AlreadyMissing: true, ObligationPending: true}
		return out, nil
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	named, err := parent.Lstat(leaf)
	if errors.Is(err, os.ErrNotExist) {
		if err := errors.Join(parentsCheck(), check(), syncDateParent(parent)); err != nil {
			return out, err
		}
		out = DateFileReclamation{Path: entry.Path, AlreadyMissing: true, ObligationPending: true}
		return out, nil
	}
	if err != nil || !named.Mode().IsRegular() || named.Size() > limits.FileBytes {
		return out, errors.Join(err, ErrDiskUsageLimit)
	}
	file, err := parent.Open(leaf)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil || !os.SameFile(info, named) || info.Size() != named.Size() {
		return out, errors.Join(err, ErrDiskUsageCorrupt)
	}
	fileCheck := func() error {
		actual, err := file.Stat()
		if err != nil {
			return err
		}
		now, err := parent.Lstat(leaf)
		if err != nil {
			return err
		}
		if !now.Mode().IsRegular() || !os.SameFile(info, actual) || !os.SameFile(info, now) || info.Size() != actual.Size() || info.Size() != now.Size() || info.ModTime() != actual.ModTime() || info.ModTime() != now.ModTime() {
			return ErrDiskUsageCorrupt
		}
		return parentsCheck()
	}
	rows, err := verifyDateReclaimFile(ctx, p, file, info, entry, e.root, limits, a.readBudget, a.nativeBudget, a.tree)
	if err != nil {
		return out, err
	}
	if beforeUnlink != nil {
		if err := beforeUnlink(file); err != nil {
			return out, err
		}
	}
	// Rehash the held object immediately before unlink. Decoder/native
	// work must not turn an earlier content check into later permission.
	if err := hashDateReclaimFile(ctx, file, info.Size(), entry.SHA256, nil, a.readBudget); err != nil {
		return out, err
	}
	if err := errors.Join(fileCheck(), check()); err != nil {
		return out, err
	}
	found, err = a.referenced(ctx, entry.Path)
	if err != nil {
		return out, err
	}
	if found {
		return out, ErrArchiveDateReferenced
	}
	if err := advance(); err != nil {
		return out, err
	}
	if err := parent.Remove(leaf); err != nil {
		return out, err
	}
	if afterUnlink != nil {
		if err := afterUnlink(); err != nil {
			return out, err
		}
	}
	if err := syncDateParent(parent); err != nil {
		return out, err
	}
	if _, err := parent.Lstat(leaf); !errors.Is(err, os.ErrNotExist) {
		return out, errors.Join(err, ErrDiskUsageCorrupt)
	}
	if err := errors.Join(parentsCheck(), check()); err != nil {
		return out, err
	}
	out = DateFileReclamation{Path: entry.Path, Removed: true, ObligationPending: true, RowsVerified: rows, LogicalBytesUnlinked: info.Size()}
	return out, nil
}

func syncDateRootDirectory(path string, identity os.FileInfo) (err error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(identity, actual) {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	return f.Sync()
}

func syncDateParent(parent *os.Root) (err error) {
	f, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return f.Sync()
}

// At most two traversal handles remain live. Root.OpenRoot pins the actual
// parent; Remove receives only one leaf component. All parent names must remain
// directories, never symlinks. The caller additionally owns archive exclusion.
func openDateReclaimParent(ctx context.Context, path string, identity os.FileInfo, relative string) (out *os.Root, leaf string, check func() error, err error) {
	parts := strings.Split(filepath.FromSlash(relative), string(filepath.Separator))
	leaf = parts[len(parts)-1]
	archive, err := os.OpenRoot(path)
	if err != nil {
		return nil, "", nil, err
	}
	defer func() {
		err = errors.Join(err, archive.Close())
		if err != nil && out != nil {
			err = errors.Join(err, out.Close())
			out = nil
		}
	}()
	actual, err := archive.Stat(".")
	if err != nil || !os.SameFile(actual, identity) {
		return nil, "", nil, errors.Join(err, ErrDiskUsageCorrupt)
	}
	current := archive
	prefix := ""
	defer func() {
		if current != archive && current != out {
			err = errors.Join(err, current.Close())
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		if err := ctx.Err(); err != nil {
			return nil, "", nil, err
		}
		prefix = filepath.Join(prefix, part)
		named, err := archive.Lstat(prefix)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, "", nil, nil // Deferred actual Close errors still fail the operation.
			}
			return nil, "", nil, err
		}
		if !named.IsDir() {
			return nil, "", nil, ErrDiskUsageCorrupt
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, "", nil, nil
			}
			return nil, "", nil, err
		}
		actual, statErr := next.Stat(".")
		if statErr != nil || !os.SameFile(actual, named) {
			return nil, "", nil, errors.Join(statErr, next.Close(), ErrDiskUsageCorrupt)
		}
		if current != archive {
			closeErr := current.Close()
			current = archive
			if closeErr != nil {
				return nil, "", nil, errors.Join(closeErr, next.Close())
			}
		}
		current = next
	}
	parentInfo, err := current.Stat(".")
	if err != nil {
		return nil, "", nil, err
	}
	// The closure opens a new confined archive handle for each finite check,
	// rather than retaining an ancestry-sized descriptor array.
	check = func() (err error) {
		if err := ctx.Err(); err != nil {
			return err
		}
		named, err := os.Lstat(path)
		if err != nil || !os.SameFile(named, identity) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		root, err := os.OpenRoot(path)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, root.Close()) }()
		prefix := ""
		for _, part := range parts[:len(parts)-1] {
			if err := ctx.Err(); err != nil {
				return err
			}
			prefix = filepath.Join(prefix, part)
			named, err := root.Lstat(prefix)
			if err != nil || !named.IsDir() {
				return errors.Join(err, ErrDiskUsageCorrupt)
			}
		}
		actual, err := root.Lstat(prefix)
		if err != nil || !os.SameFile(actual, parentInfo) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		return nil
	}
	out = current
	if err := check(); err != nil {
		return out, "", nil, err
	}
	return out, leaf, check, nil
}

func hashDateReclaimFile(ctx context.Context, file *os.File, size int64, want string, copyTo io.Writer, budget *archiveVerifyReadBudget) error {
	if err := budget.reserve(size); err != nil {
		return err
	}
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	reader := io.NewSectionReader(file, 0, size)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			h.Write(buffer[:n])
			if copyTo != nil {
				written, err := copyTo.Write(buffer[:n])
				if err != nil {
					return err
				}
				if written != n {
					return io.ErrShortWrite
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return ErrDiskUsageCorrupt
	}
	return nil
}

func verifyDateReclaimFile(ctx context.Context, p *DiskProjection, file *os.File, info os.FileInfo, entry ManifestFile, directory string, limits DateFileReclaimLimits, readBudget, nativeBudget *archiveVerifyReadBudget, tree *archiveTreeBudget) (rows int, err error) {
	spool, err := os.CreateTemp(directory, ".archive-date-reclaim-spool-*")
	if err != nil {
		return 0, err
	}
	spoolInfo, err := spool.Stat()
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, spool.Close())
			closed = true
		}
		if spoolInfo != nil {
			err = errors.Join(err, cleanupArchiveTemporary(spool, true, spoolInfo))
		}
		if err != nil {
			rows = 0
		}
	}()
	if err != nil {
		return 0, err
	}
	spoolWriter := &archiveWriter{ctx: ctx, out: spool, remaining: limits.FileBytes, work: &archiveWork{remaining: limits.FileBytes, tree: tree}}
	if err := hashDateReclaimFile(ctx, file, info.Size(), entry.SHA256, spoolWriter, readBudget); err != nil {
		return 0, err
	}
	spoolInfo, err = spool.Stat()
	if err != nil {
		return 0, err
	}
	var previous uint64
	var input, output, cost int64
	visit := func(row parquetAttempt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if rows == limits.FileRows || row.SchemaVersion != int32(entry.SchemaVersion) || row.Sequence <= 0 || uint64(row.Sequence) <= previous || uint64(row.Sequence) > entry.MaxSequence || rows == 0 && uint64(row.Sequence) != entry.MinSequence || time.UnixMicro(row.CompletedAtMicros).UTC().Format("2006-01-02") != entry.Date {
			return ErrDiskUsageCorrupt
		}
		var actual AttemptEvent
		if err := p.view(ctx, func(tx *bbolt.Tx) error {
			b := tx.Bucket([]byte("attempts"))
			if b == nil {
				return ErrDiskUsageCorrupt
			}
			raw := b.Get(diskUsageSequence(uint64(row.Sequence)))
			if len(raw) == 0 || len(raw) > diskUsageMaxValue {
				return ErrDiskUsageCorrupt
			}
			if err := nativeBudget.reserve(int64(len(raw))); err != nil {
				return err
			}
			if json.Unmarshal(raw, &actual) != nil || actual.Sequence != uint64(row.Sequence) {
				return ErrDiskUsageCorrupt
			}
			return nil
		}); err != nil {
			return err
		}
		if row != narrowToSchema(toParquetAttempt(actual), row.SchemaVersion) {
			return ErrDiskUsageCorrupt
		}
		for _, v := range []struct {
			target *int64
			value  int64
		}{{&input, row.ProviderInputTokens}, {&output, row.ProviderOutputTokens}, {&cost, row.CostMicrosUSD}} {
			if err := addInt64(v.target, v.value); err != nil {
				return err
			}
		}
		previous = uint64(row.Sequence)
		rows++
		return nil
	}
	if entry.format() == FormatParquet {
		err = archiveReadParquet(ctx, spool, info.Size(), readBudget, visit)
	} else {
		if err = readBudget.reserve(info.Size()); err == nil {
			err = archiveReadNDJSON(ctx, io.NewSectionReader(spool, 0, info.Size()), visit)
		}
	}
	if err != nil {
		return 0, err
	}
	if int64(rows) != entry.Records || previous != entry.MaxSequence || input != entry.InputTokens || output != entry.OutputTokens || cost != entry.CostMicrosUSD {
		return 0, ErrDiskUsageCorrupt
	}
	// Actual spool Close/removal precedes any target unlink. Late failures refuse.
	err = spool.Close()
	closed = true
	if err != nil {
		return 0, err
	}
	return rows, ctx.Err()
}
