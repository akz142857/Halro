package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errArchiveDateOrphanOutsidePolicy = errors.New("orphan row is outside the original date policy baseline")

// Entry is a bounded description derived from the actual object, not a catalog
// receipt. It is empty for a missing retry. Reclamation still requires exact
// native rows, current references and the independently authenticated policy.
type DateOrphanFileReclamation struct {
	Entry       ManifestFile
	Reclamation DateFileReclamation
}

// ReclaimRetainedDateOrphanFile handles one explicit partition name, including
// an object recreated after the old descriptor/page chains were reclaimed.
// It neither enumerates the directory nor completes the old-root obligation.
func (e *Exporter) ReclaimRetainedDateOrphanFile(ctx context.Context, p *DiskProjection, retainFrom time.Time, path string, limits DateFileReclaimLimits) (DateOrphanFileReclamation, error) {
	return e.reclaimRetainedDateOrphanFile(ctx, p, retainFrom, path, limits, nil, nil)
}

func (e *Exporter) reclaimRetainedDateOrphanFile(ctx context.Context, p *DiskProjection, retainFrom time.Time, path string, limits DateFileReclaimLimits, beforeUnlink func(*os.File) error, afterUnlink func() error) (out DateOrphanFileReclamation, err error) {
	entry, err := dateOrphanPath(path)
	if err != nil {
		return out, err
	}
	err = e.withDateReclaimAuthority(ctx, p, retainFrom, limits, func(ctx context.Context, a *archiveDateReclaimAuthority) error {
		if entry.Date >= a.proof.position.Retention.RetainFromDate {
			return ErrArchiveDateProof
		}
		found, err := a.referenced(ctx, entry.Path)
		if err != nil {
			return err
		}
		if found {
			return ErrArchiveDateReferenced
		}
		actual, missing, err := a.describeOrphanFile(ctx, entry, limits)
		if err != nil {
			return fmt.Errorf("inspect orphan %q: %w", entry.Path, err)
		}
		if missing {
			out.Reclamation = DateFileReclamation{Path: entry.Path, AlreadyMissing: true, ObligationPending: true}
			return a.check()
		}
		// Discovery is not authority: reopen the target, verify its frozen digest
		// and all rows against the fresh native projection, then repeat identity,
		// checksum and reference fences at the actual confined unlink.
		out.Entry = actual
		out.Reclamation, err = a.reclaimFile(ctx, actual, limits, beforeUnlink, afterUnlink)
		if err != nil {
			return fmt.Errorf("reclaim orphan %q: %w", entry.Path, err)
		}
		return nil
	})
	if err != nil {
		out = DateOrphanFileReclamation{}
	}
	return out, err
}

func dateOrphanPath(path string) (ManifestFile, error) {
	var entry ManifestFile
	parts := strings.Split(path, "/")
	if len(path) > 4096 || len(parts) != 2 || !strings.HasPrefix(parts[0], "date=") || parts[1] == "" || strings.HasPrefix(parts[1], ".") {
		return entry, ErrDiskUsageCorrupt
	}
	entry.Path, entry.Date = path, strings.TrimPrefix(parts[0], "date=")
	switch filepath.Ext(parts[1]) {
	case ".ndjson":
		entry.Format = FormatNDJSON
	case ".parquet":
		entry.Format = FormatParquet
	default:
		return ManifestFile{}, ErrDiskUsageCorrupt
	}
	if err := validateArchivePartitionDate(entry); err != nil {
		return ManifestFile{}, err
	}
	return entry, nil
}

// Fixed counters and bounded decoder buffers only. All original target, pinned
// parent and private spool descriptors are actually closed before returning a
// candidate to the independent native verifier. No discovery state is saved.
func (a *archiveDateReclaimAuthority) describeOrphanFile(ctx context.Context, entry ManifestFile, limits DateFileReclaimLimits) (out ManifestFile, missing bool, err error) {
	defer func() {
		if err != nil {
			out, missing = ManifestFile{}, false
		}
	}()
	parent, leaf, parentsCheck, err := openDateReclaimParent(ctx, a.e.root, a.directory, entry.Path)
	if err != nil {
		return out, false, err
	}
	if parent == nil {
		return out, true, errors.Join(a.check(), syncDateRootDirectory(a.e.root, a.directory))
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	named, err := parent.Lstat(leaf)
	if errors.Is(err, os.ErrNotExist) {
		return out, true, errors.Join(parentsCheck(), a.check(), syncDateParent(parent))
	}
	if err != nil || !named.Mode().IsRegular() || named.Size() <= 0 {
		return out, false, errors.Join(err, ErrDiskUsageCorrupt)
	}
	if named.Size() > limits.FileBytes {
		return out, false, ErrDiskUsageLimit
	}
	file, err := parent.Open(leaf)
	if err != nil {
		return out, false, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil || !os.SameFile(named, info) || named.Size() != info.Size() {
		return out, false, errors.Join(err, ErrDiskUsageCorrupt)
	}
	spool, err := os.CreateTemp(a.e.root, ".archive-date-reclaim-spool-*")
	if err != nil {
		return out, false, err
	}
	spoolInfo, err := spool.Stat()
	defer func() { err = errors.Join(err, cleanupArchiveTemporary(spool, false, spoolInfo)) }()
	if err != nil {
		return out, false, err
	}
	if err := a.readBudget.reserve(info.Size()); err != nil {
		return out, false, err
	}
	hash := sha256.New()
	writer := &archiveWriter{ctx: ctx, out: io.MultiWriter(spool, hash), remaining: limits.FileBytes, work: &archiveWork{remaining: limits.FileBytes, tree: a.tree}}
	n, err := io.CopyBuffer(writer, io.NewSectionReader(file, 0, info.Size()), make([]byte, 64<<10))
	if err != nil || n != info.Size() {
		return out, false, errors.Join(err, ErrDiskUsageCorrupt)
	}
	entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
	visit := func(row parquetAttempt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Records == int64(limits.FileRows) {
			return ErrDiskUsageLimit
		}
		if a.orphanInspectRemaining != nil {
			if *a.orphanInspectRemaining == 0 {
				return ErrDiskUsageLimit
			}
			*a.orphanInspectRemaining--
		}
		if !supportedManifestSchema(int(row.SchemaVersion)) || row.Sequence <= 0 || uint64(row.Sequence) <= entry.MaxSequence || time.UnixMicro(row.CompletedAtMicros).UTC().Format("2006-01-02") != entry.Date {
			return ErrDiskUsageCorrupt
		}
		if uint64(row.Sequence) > a.proof.position.Retention.PruneThroughSequence {
			return errors.Join(ErrArchiveDateProof, errArchiveDateOrphanOutsidePolicy)
		}
		if entry.Records == 0 {
			entry.MinSequence, entry.SchemaVersion = uint64(row.Sequence), int(row.SchemaVersion)
		} else if int(row.SchemaVersion) != entry.SchemaVersion {
			return ErrDiskUsageCorrupt
		}
		for _, v := range []struct {
			target *int64
			value  int64
		}{{&entry.InputTokens, row.ProviderInputTokens}, {&entry.OutputTokens, row.ProviderOutputTokens}, {&entry.CostMicrosUSD, row.CostMicrosUSD}} {
			if err := addInt64(v.target, v.value); err != nil {
				return err
			}
		}
		entry.MaxSequence, entry.Records = uint64(row.Sequence), entry.Records+1
		return nil
	}
	if entry.Format == FormatParquet {
		err = archiveReadParquet(ctx, spool, info.Size(), a.readBudget, visit)
	} else if err = a.readBudget.reserve(info.Size()); err == nil {
		err = archiveReadNDJSON(ctx, io.NewSectionReader(spool, 0, info.Size()), visit)
	}
	if err != nil {
		return out, false, err
	}
	if entry.Records == 0 {
		return out, false, ErrDiskUsageCorrupt
	}
	if err := a.e.validateCatalogEntry(entry); err != nil {
		return out, false, err
	}
	if err := errors.Join(checkDateBuildState(file, info, filepath.Join(a.e.root, filepath.FromSlash(entry.Path))), parentsCheck(), a.check()); err != nil {
		return out, false, err
	}
	return entry, false, nil
}
