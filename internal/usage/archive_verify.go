package usage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

// ReadBytes counts catalog reads, partition copying, preflight and decoder
// reads; the native projection has its separate Source budget. IndexBytes also
// caps the physical scratch database. These are job caps, not shared RAM fees.
type ArchiveVerifyLimits struct {
	Source                           DiskArchiveLimits
	Files, Rows                      int
	FileBytes, ReadBytes, IndexBytes int64
	MaxDuration                      time.Duration
}

func DefaultArchiveVerifyLimits() ArchiveVerifyLimits {
	return ArchiveVerifyLimits{Source: DefaultDiskExportLimits().Source, Files: 65536, Rows: 1 << 20, FileBytes: 256 << 20, ReadBytes: 1 << 30, IndexBytes: 256 << 20, MaxDuration: time.Minute}
}

type ArchiveVerification struct {
	AfterSequence, FirstRetainedSequence, ThroughSequence uint64
	Files, ArchiveRows, SourceRows                        int
	SourceHead                                            ledger.Watermark
}

// VerifyDiskArchive proves exact schema-narrowed row correspondence in the
// entire native sequence interval, relative to the selected fresh projection.
// It is not native/key-set authentication, restart trust or native trim authority.
// The complete index/spool/callback lifetime joins p.Close, with zero queue.
func (e *Exporter) VerifyDiskArchive(ctx context.Context, p *DiskProjection, limits ArchiveVerifyLimits) (ArchiveVerification, error) {
	return e.VerifyDiskArchiveAfter(ctx, p, 0, limits)
}

// VerifyDiskArchiveAfter selects an explicit retained interval (after, archive H].
// The caller must establish after from its retention/native authority; missing
// catalog entries never establish a floor. All listed rows must be in this range.
// This verifier does not authenticate the caller's floor or authorize deletion.
func (e *Exporter) VerifyDiskArchiveAfter(ctx context.Context, p *DiskProjection, after uint64, limits ArchiveVerifyLimits) (report ArchiveVerification, err error) {
	scoped, err := e.verifyDiskArchive(ctx, p, after, "", limits)
	return scoped.ArchiveVerification, err
}

// VerifyDiskArchiveRetainedDates checks the entire native prefix from genesis,
// requiring every attempt on/after the caller's explicit UTC retention date.
// Listed expired rows still require exact native correspondence. Missing files
// or root metadata never choose this policy; this date-scoped proof cannot mint
// an all-history window witness or authorize physical/native deletion.
func (e *Exporter) VerifyDiskArchiveRetainedDates(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits ArchiveVerifyLimits) (ArchiveDateVerification, error) {
	if retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 {
		return ArchiveDateVerification{}, ErrDiskUsageLimit
	}
	return e.verifyDiskArchive(ctx, p, 0, retainFrom.UTC().Format("2006-01-02"), limits)
}

func (e *Exporter) verifyDiskArchive(ctx context.Context, p *DiskProjection, after uint64, retainFrom string, limits ArchiveVerifyLimits) (report ArchiveDateVerification, err error) {
	if p == nil || limits.Files < 1 || limits.Files > 65536 || limits.Rows < 1 || limits.Rows > 1<<20 || limits.FileBytes < 1 || limits.FileBytes > 256<<20 || limits.ReadBytes < 1 || limits.ReadBytes > 1<<30 || limits.IndexBytes < 1 || limits.IndexBytes > 256<<20 || limits.MaxDuration <= 0 || limits.MaxDuration > time.Minute {
		return report, ErrDiskUsageLimit
	}
	if !e.mu.TryLock() {
		return report, ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	if e.archiveRetired.Load() {
		return report, ErrDiskUsageClosed
	}
	e.windowProof = nil
	e.windowPending = nil
	var tentative *archiveWindowProof
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	defer func() {
		err = errors.Join(err, ctx.Err())
		if p.retired.Load() || e.archiveRetired.Load() {
			err = errors.Join(err, ErrDiskUsageClosed)
		}
		if err != nil {
			report = ArchiveDateVerification{}
			e.windowProof = nil
			e.windowPending = nil
		} else if after == 0 && retainFrom == "" {
			e.windowProof = tentative
		}
	}()
	if limits.IndexBytes > math.MaxInt64-limits.FileBytes {
		return report, ErrDiskUsageLimit
	}
	tree, err := newArchiveTreeBudget(ctx, e.root, e.archiveDiskCeiling)
	if err != nil {
		return report, err
	}
	// The limits are upper bounds, not minimum preallocations. A small archive
	// ceiling must still be able to verify a small catalog. Split its observed
	// remaining space between the whole index and one spool, never enlarging a
	// caller limit. The actual index/file checks still reject oversized history.
	if tree != nil {
		available := tree.ceiling - tree.used
		if available < 2*archiveVerifyNodeBytes {
			return report, ErrDiskUsageLimit
		}
		limits.FileBytes = min(limits.FileBytes, available/2)
		limits.IndexBytes = min(limits.IndexBytes, available-limits.FileBytes)
	}
	// The decoder workspace follows the spool it decodes, so it is reserved
	// only once the ceiling has bounded that spool.
	releaseRAM, err := e.reserveArchiveWorkRAM(ctx, limits.FileBytes+archiveVerifyDecoderRAMBytes)
	if err != nil {
		return report, err
	}
	var cleanupErr error
	defer func() {
		if cleanupErr == nil {
			err = errors.Join(err, releaseRAM())
		}
	}()
	releaseDisk, err := e.reserveArchiveDisk(ctx, limits.IndexBytes+limits.FileBytes)
	if err != nil {
		return report, err
	}
	defer func() {
		if cleanupErr == nil {
			err = errors.Join(err, releaseDisk())
		}
	}()
	if err := tree.takeBytes(limits.IndexBytes + limits.FileBytes); err != nil {
		return report, err
	}

	_, err = p.archiveJob(ctx, limits.Source, func(ctx context.Context) (cut DiskArchiveCut, jobErr error) {
		budget := &archiveVerifyReadBudget{remaining: limits.ReadBytes}
		root, err := openArchiveManifest(e.root)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		defer func() { jobErr = errors.Join(jobErr, root.file.Close()) }()
		if err := budget.reserve(root.info.Size()); err != nil {
			return DiskArchiveCut{}, err
		}
		position, digest, err := root.walk(ctx, nil)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		stats, err := p.ReadStats(ctx)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		if position.LastSequence > stats.Watermark.Sequence || after > position.LastSequence {
			return DiskArchiveCut{}, ErrDiskUsageCursor
		}
		directory, err := os.MkdirTemp(e.root, ".archive-verify-")
		if err != nil {
			return DiskArchiveCut{}, err
		}
		identity, err := os.Lstat(directory)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		scratch := &archiveVerifyScratch{directory: directory, identity: identity, tree: tree}
		defer func() {
			cleanupErr = errors.Join(cleanupErr, scratch.closeScratch())
			jobErr = errors.Join(jobErr, cleanupErr)
		}()
		indexBudget := &archiveVerifyReadBudget{remaining: limits.ReadBytes}
		// The complete index extent was reserved above. Charging each node to
		// the same tree again would count those bytes twice. Its own max still
		// checks every actual write; the prepaid spool has its separate max.
		scratch.index, err = newArchiveVerifyIndex(filepath.Join(directory, "index.bin"), limits.IndexBytes)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		index := scratch.index
		report.AfterSequence, report.ThroughSequence = after, position.LastSequence
		report.RetainFromDate = retainFrom
		visitEntry := func(entry ManifestFile) error {
			if position.CatalogVersion == 0 && entry.SchemaVersion == 0 {
				entry.SchemaVersion = position.SchemaVersion
			}
			if err := e.validateCatalogEntry(entry); err != nil {
				return err
			}
			if err := validateArchivePartitionDate(entry); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if report.Files == limits.Files {
				return ErrDiskUsageLimit
			}
			if err := validateArchiveEntryCoverage(entry, position.LastSequence, position.Retention); err != nil {
				return ErrDiskUsageCorrupt
			}
			pathHash := sha256.Sum256([]byte(entry.Path))
			if err := index.insert(ctx, 2, pathHash, [40]byte{}, indexBudget); err != nil {
				return err
			}
			if report.FirstRetainedSequence == 0 || entry.MinSequence < report.FirstRetainedSequence {
				report.FirstRetainedSequence = entry.MinSequence
			}
			spool, err := copyArchiveVerifySpool(ctx, e, entry, directory, limits.FileBytes, budget)
			if err != nil {
				return err
			}
			err = func() (fileErr error) {
				defer func() {
					retireErr := retireArchiveSpool(spool)
					cleanupErr = errors.Join(cleanupErr, retireErr)
					fileErr = errors.Join(fileErr, retireErr)
				}()
				var rows int64
				var input, output, cost int64
				var previous uint64
				visit := func(row parquetAttempt) error {
					if err := ctx.Err(); err != nil {
						return err
					}
					if report.ArchiveRows == limits.Rows {
						return ErrDiskUsageLimit
					}
					if row.SchemaVersion != int32(entry.SchemaVersion) || row.Sequence <= 0 || uint64(row.Sequence) <= previous || row.EventID == "" || row.RequestID == "" || row.AttemptID == "" {
						return ErrDiskUsageCorrupt
					}
					if time.UnixMicro(row.CompletedAtMicros).UTC().Format("2006-01-02") != entry.Date {
						return fmt.Errorf("%w: archive partition date mismatch", ErrDiskUsageCorrupt)
					}
					seq := uint64(row.Sequence)
					if seq <= after {
						return ErrDiskUsageCorrupt
					}
					if rows == 0 && seq != entry.MinSequence || seq > entry.MaxSequence {
						return ErrDiskUsageCorrupt
					}
					previous = seq
					rows++
					report.ArchiveRows++
					for _, v := range []struct {
						target *int64
						value  int64
					}{{&input, row.ProviderInputTokens}, {&output, row.ProviderOutputTokens}, {&cost, row.CostMicrosUSD}} {
						if err := addInt64(v.target, v.value); err != nil {
							return err
						}
					}
					raw, err := json.Marshal(row)
					if err != nil {
						return err
					}
					if len(raw) > diskUsageMaxValue {
						return ErrDiskUsageLimit
					}
					digest := sha256.Sum256(raw)
					var value [40]byte
					binary.BigEndian.PutUint64(value[:8], uint64(row.SchemaVersion))
					copy(value[8:], digest[:])
					if err := index.insert(ctx, 0, sha256.Sum256([]byte(row.EventID)), value, indexBudget); err != nil {
						return fmt.Errorf("%w: duplicate archive EventID or sequence", err)
					}
					return index.insert(ctx, 1, sha256.Sum256(diskUsageSequence(seq)), [40]byte{}, indexBudget)
				}
				info, err := spool.Stat()
				if err != nil {
					return err
				}
				if entry.format() == FormatParquet {
					err = archiveReadParquet(ctx, spool, info.Size(), budget, visit)
				} else {
					if err := budget.reserve(info.Size()); err != nil {
						return err
					}
					err = archiveReadNDJSON(ctx, io.NewSectionReader(spool, 0, info.Size()), visit)
				}
				if err != nil {
					return err
				}
				if rows != entry.Records || previous != entry.MaxSequence || input != entry.InputTokens || output != entry.OutputTokens || cost != entry.CostMicrosUSD {
					return fmt.Errorf("%w: archive partition summary", ErrDiskUsageCorrupt)
				}
				return nil
			}()
			if err != nil {
				return err
			}
			report.Files++
			return nil
		}
		if position.CatalogVersion == 0 {
			// Legacy directories remain byte-for-byte unchanged. Rewalk the same
			// captured descriptor, normalize only each current entry, and check the
			// complete second document before accepting any tentative rows.
			if err := budget.reserve(root.info.Size()); err != nil {
				return DiskArchiveCut{}, err
			}
			second, secondDigest, err := root.walk(ctx, visitEntry)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			if second != position || secondDigest != digest {
				return DiskArchiveCut{}, ErrDiskUsageCorrupt
			}
		} else {
			ref := position.CatalogHead
			for ref != (ArchivePageRef{}) {
				if err := budget.reserve(int64(ref.Bytes) + 1); err != nil {
					return DiskArchiveCut{}, err
				}
				page, err := e.readCatalogPage(ctx, ref)
				if err != nil {
					return DiskArchiveCut{}, err
				}
				for _, entry := range page.Files {
					if err := visitEntry(entry); err != nil {
						return DiskArchiveCut{}, err
					}
				}
				ref = page.Previous
			}
		}
		if uint64(report.Files) != position.Files {
			return DiskArchiveCut{}, ErrDiskUsageCorrupt
		}
		cut, err = p.streamArchiveBoundedHeld(ctx, after, position.LastSequence, true, limits.Source, func(ctx context.Context, page []AttemptEvent) error {
			for _, attempt := range page {
				required := retainFrom == "" || attempt.CompletedAt.UTC().Format("2006-01-02") >= retainFrom
				if retainFrom != "" && required {
					report.RequiredRows++
				}
				key := sha256.Sum256([]byte(attempt.EventID))
				value, found, err := index.lookup(ctx, 0, key, indexBudget)
				if err != nil {
					return err
				}
				if !found {
					if required {
						return fmt.Errorf("%w: missing archive event", ErrDiskUsageCorrupt)
					}
					report.ExpiredOmittedRows++
				} else {
					schema := int32(binary.BigEndian.Uint64(value[:8]))
					if !supportedManifestSchema(int(schema)) {
						return ErrDiskUsageCorrupt
					}
					raw, err := json.Marshal(narrowToSchema(toParquetAttempt(attempt), schema))
					if err != nil || len(raw) > diskUsageMaxValue {
						return errors.Join(err, ErrDiskUsageLimit)
					}
					digest := sha256.Sum256(raw)
					if !bytes.Equal(value[8:], digest[:]) {
						return fmt.Errorf("%w: archive content mismatch", ErrDiskUsageCorrupt)
					}
					report.MatchedRows++
				}
				report.SourceRows++
			}
			return nil
		}, nil)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		if report.MatchedRows != report.ArchiveRows {
			return DiskArchiveCut{}, fmt.Errorf("%w: extra archive event", ErrDiskUsageCorrupt)
		}
		if err := budget.reserve(root.info.Size()); err != nil {
			return DiskArchiveCut{}, err
		}
		second, secondDigest, err := root.walk(ctx, nil)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		if second != position || secondDigest != digest {
			return DiskArchiveCut{}, ErrDiskUsageCorrupt
		}
		report.SourceHead = cut.Head
		if after == 0 && retainFrom == "" && position.Retention == (ArchiveDateRetention{}) {
			directory, statErr := os.Lstat(e.root)
			if statErr != nil || !directory.IsDir() {
				return DiskArchiveCut{}, errors.Join(statErr, ErrDiskUsageCorrupt)
			}
			tentative = &archiveWindowProof{owner: p.generation, position: position, digest: digest, identity: root.info, directory: directory}
		}
		return cut, nil
	})
	return report, err
}

func copyArchiveVerifySpool(ctx context.Context, e *Exporter, entry ManifestFile, directory string, max int64, budget *archiveVerifyReadBudget) (out *os.File, err error) {
	path, err := e.safeManifestPath(entry.Path)
	if err != nil {
		return nil, err
	}
	named, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !named.Mode().IsRegular() || named.Size() > max {
		return nil, ErrDiskUsageLimit
	}
	if err := budget.reserve(named.Size()); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, f.Close(), ctx.Err())
		if err != nil && out != nil {
			err = errors.Join(err, retireArchiveSpool(out))
			out = nil
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(named, info) || info.Size() != named.Size() {
		return nil, ErrDiskUsageCorrupt
	}
	spool, err := os.CreateTemp(directory, "partition-")
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			err = errors.Join(err, retireArchiveSpool(spool))
		}
	}()
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	reader := io.NewSectionReader(f, 0, info.Size())
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			h.Write(buffer[:n])
			if _, err := spool.Write(buffer[:n]); err != nil {
				return nil, err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != entry.SHA256 {
		return nil, ErrDiskUsageCorrupt
	}
	current, err := f.Stat()
	if err != nil {
		return nil, err
	}
	now, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, now) || info.Size() != current.Size() || info.Size() != now.Size() || info.ModTime() != current.ModTime() || info.ModTime() != now.ModTime() {
		return nil, ErrDiskUsageCorrupt
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	success = true
	return spool, nil
}
func archiveReadNDJSON(ctx context.Context, source io.Reader, visit func(parquetAttempt) error) error {
	input := bufio.NewReaderSize(source, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := archiveSizedValue(input, diskUsageMaxValue)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var row parquetAttempt
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&row) != nil {
			return ErrDiskUsageCorrupt
		}
		if err := visit(row); err != nil {
			return err
		}
	}
}

// Reserve before IO. Parquet may re-read data or request a buffer beyond EOF;
// charging the requested size is conservative. One budget spans every file.
type archiveVerifyReadBudget struct {
	mu        sync.Mutex
	remaining int64
}

func (b *archiveVerifyReadBudget) reserve(size int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if size < 0 || size > b.remaining {
		return ErrDiskUsageLimit
	}
	b.remaining -= size
	return nil
}
