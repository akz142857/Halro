package usage

import (
	"context"
	"crypto/sha256"
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

	"github.com/akz142857/Halro/internal/durable"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
)

// Limits are local hard stops, not calibrated shared RAM/disk reservations.
// The legacy JSON catalog is streamed; its IO still grows with archive history.
type DiskExportLimits struct {
	Source         DiskArchiveLimits
	PartitionBytes int64
	WriteBytes     int64
	WorkBytes      int64
	MaxDuration    time.Duration
}

func DefaultDiskExportLimits() DiskExportLimits {
	return DiskExportLimits{
		Source:         DiskArchiveLimits{Pages: 4096, Rows: 1 << 20, InputBytes: 128 << 20, OutputBytes: 128 << 20, PageRows: 160, PageInputBytes: 4 << 20, PageOutputBytes: 4 << 20, MaxDuration: time.Minute},
		PartitionBytes: 16 << 20, WriteBytes: 256 << 20, WorkBytes: 1 << 30, MaxDuration: time.Minute,
	}
}

// ExportDisk publishes only a complete frozen source prefix. Failures before
// the atomic manifest rename leave the previous catalog in place;
// failure of its final directory barrier may leave a visible, unacknowledged
// publication. An idempotent retry repeats the manifest durability barriers;
// successfully written partitions are retryable orphans. It never takes a
// historical Snapshot, date map, row array or manifest file list.
func (e *Exporter) ExportDisk(ctx context.Context, p *DiskProjection, limits DiskExportLimits) (DiskArchiveCut, error) {
	return e.exportDisk(ctx, p, limits, false, false)
}

// ExportDiskStep publishes a finite complete prefix, retaining the source's
// real frozen Head separately from ThroughSequence. Reopen/retry starts at the
// committed root. Errors never turn an unfinished sink into a prefix receipt.
func (e *Exporter) ExportDiskStep(ctx context.Context, p *DiskProjection, limits DiskExportLimits) (DiskArchiveCut, error) {
	return e.exportDisk(ctx, p, limits, true, false)
}

// ExportDiskStepPreservingCatalog keeps a legacy catalog (or creates one when
// missing) until default Runtime startup supports paged catalogs. Existing paged
// catalogs remain paged. The same finite source/encoder/orphan/physical lifetime
// applies; legacy root rewriting is streamed but retains historical IO cost.
func (e *Exporter) ExportDiskStepPreservingCatalog(ctx context.Context, p *DiskProjection, limits DiskExportLimits) (DiskArchiveCut, error) {
	return e.exportDisk(ctx, p, limits, true, true)
}

// These fixed local controls cap default selected Runtime maintenance work.
// They are not calibrated RAM/disk fees or a production throughput promise.
func DefaultDiskExportStepLimits() DiskExportLimits {
	limits := DefaultDiskExportLimits()
	limits.Source.Pages = 16
	limits.Source.Rows = 256
	limits.Source.InputBytes = 8 << 20
	limits.Source.OutputBytes = 8 << 20
	return limits
}

func (e *Exporter) exportDisk(ctx context.Context, p *DiskProjection, limits DiskExportLimits, step, preserveCatalog bool) (result DiskArchiveCut, err error) {
	return e.exportDiskWithRootHook(ctx, p, limits, step, preserveCatalog, nil)
}

// The hook runs after the actual rename and before the directory barrier, while
// archiveJob still owns the physical lifetime. It is private fault-injection.
func (e *Exporter) exportDiskWithRootHook(ctx context.Context, p *DiskProjection, limits DiskExportLimits, step, preserveCatalog bool, afterRename func() error) (result DiskArchiveCut, err error) {
	if p == nil || limits.PartitionBytes < 1 || limits.PartitionBytes > 16<<20 || limits.WriteBytes < 1 || limits.WriteBytes > 1<<30 || limits.WorkBytes < 1 || limits.WorkBytes > 1<<30 || limits.MaxDuration <= 0 || limits.MaxDuration > time.Minute {
		return result, ErrDiskUsageLimit
	}
	if !e.mu.TryLock() {
		return result, ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	if e.archiveRetired.Load() {
		return result, ErrDiskUsageClosed
	}
	// Withdraw usable authority while the publisher is running. An unchanged
	// previous proof is retained only as a private retry receipt, not a permit.
	base := e.windowProof
	if base == nil {
		base = e.windowPending
	}
	e.windowProof = nil
	e.windowPending = base
	var candidate *archiveWindowProof
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	releaseDisk, err := e.reserveArchiveDisk(ctx, limits.WriteBytes)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, releaseDisk()) }()
	tree, err := newArchiveTreeBudget(ctx, e.root, e.archiveDiskCeiling)
	if err != nil {
		return result, fmt.Errorf("usage archive disk ceiling: %w", err)
	}
	defer func() {
		err = errors.Join(err, ctx.Err())
		if p.retired.Load() || e.archiveRetired.Load() {
			err = errors.Join(err, ErrDiskUsageClosed)
		}
		if err != nil {
			result = DiskArchiveCut{}
			return
		}
		e.windowProof = candidate
		e.windowPending = nil
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return p.archiveJob(ctx, limits.Source, func(ctx context.Context) (result DiskArchiveCut, err error) {
		old, err := openArchiveManifest(e.root)
		missing := errors.Is(err, os.ErrNotExist)
		if err != nil && !missing {
			return result, err
		}
		position := ArchivePosition{SchemaVersion: parquetSchemaVersion}
		var digest [32]byte
		if !missing {
			defer func() { err = errors.Join(err, old.file.Close()) }()
			position, digest, err = old.walk(ctx, nil)
			if err != nil {
				return result, err
			}
		}
		if err := archiveDirectory(e.root); err != nil {
			return result, err
		}
		directory, err := os.Lstat(e.root)
		if err != nil {
			return result, err
		}
		var publication *archiveWindowPublication
		if missing || base.matches(p.generation, old, position, digest, directory) {
			if !missing {
				// A visible unacknowledged root is usable only after its exact receipt
				// matches and its file and directory barriers have been repeated.
				if err := old.file.Sync(); err != nil {
					return result, err
				}
				if err := durable.SyncDirectory(e.root); err != nil {
					return result, err
				}
				if err := old.check(); err != nil {
					return result, err
				}
				candidate = base
			}
			publication = &archiveWindowPublication{owner: p.generation, directory: directory, retention: position.Retention, record: func(proof *archiveWindowProof) error {
				candidate = proof
				e.windowPending = proof
				if afterRename != nil {
					return afterRename()
				}
				return nil
			}}
		} else {
			// Publication can continue, but an unknown/replaced prefix cannot gain
			// deletion authority by observing a larger sequence number.
			e.windowPending = nil
		}
		stage, err := os.CreateTemp(e.root, ".archive-entries-*.tmp")
		if err != nil {
			return result, err
		}
		var stageIdentity os.FileInfo
		defer func() {
			err = errors.Join(err, stage.Close())
			named, statErr := os.Lstat(stage.Name())
			if errors.Is(statErr, os.ErrNotExist) {
				return
			}
			if statErr != nil || stageIdentity == nil || !os.SameFile(stageIdentity, named) {
				err = errors.Join(err, statErr, ErrDiskUsageCorrupt)
				return
			}
			err = errors.Join(err, os.Remove(stage.Name()))
		}()
		stageIdentity, err = stage.Stat()
		if err != nil {
			return result, err
		}
		if err := stage.Chmod(0o600); err != nil {
			return result, err
		}
		work := &archiveWork{remaining: limits.WorkBytes, tree: tree}
		writes := &archiveWork{remaining: limits.WriteBytes, tree: tree}
		entryWriter := &archiveWriter{ctx: ctx, out: stage, remaining: 64 << 20, work: work}
		encoder := json.NewEncoder(entryWriter)
		var partitions uint64
		stream := p.streamArchiveHeld
		if step {
			stream = p.streamArchiveStepHeld
		}
		result, err = stream(ctx, position.LastSequence, limits.Source, func(ctx context.Context, page []AttemptEvent) error {
			// Consecutive date runs preserve native order even when old dates appear
			// after new dates. Each run is no larger than this bounded source page.
			for start := 0; start < len(page); {
				date := page[start].CompletedAt.UTC().Format("2006-01-02")
				end := start + 1
				for end < len(page) && page[end].CompletedAt.UTC().Format("2006-01-02") == date {
					end++
				}
				entry, err := e.publishDiskPartition(ctx, date, page[start:end], limits.PartitionBytes, writes, work)
				if err != nil {
					return err
				}
				if err := encoder.Encode(entry); err != nil {
					return err
				}
				partitions++
				start = end
			}
			return nil
		}, func(ctx context.Context, cut DiskArchiveCut) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !missing && position.LastSequence == cut.ThroughSequence && (position.CompletePrefix || position.Retention != (ArchiveDateRetention{})) && position.CatalogVersion == 1 {
				if position.CatalogHead != (ArchivePageRef{}) {
					if _, err := e.readCatalogPage(ctx, position.CatalogHead); err != nil {
						return err
					}
				}
				if err := old.check(); err != nil {
					return err
				}
				if err := old.file.Sync(); err != nil {
					return err
				}
				if err := durable.SyncDirectory(e.root); err != nil {
					return err
				}
				return old.check()
			}
			if preserveCatalog && position.CatalogVersion == 0 {
				return e.commitLegacyArchiveWithProof(ctx, old, missing, position, digest, stage, partitions, cut, writes, work, publication)
			}
			return e.commitPagedArchiveWithProof(ctx, old, missing, position, digest, stage, partitions, cut, work, publication)
		})
		if err == nil && candidate != nil {
			check, openErr := openArchiveManifest(e.root)
			if openErr != nil {
				return result, openErr
			}
			if feeErr := work.take(int(check.info.Size())); feeErr != nil {
				return result, errors.Join(feeErr, check.file.Close())
			}
			checked, hash, walkErr := check.walk(ctx, nil)
			currentDirectory, directoryErr := os.Lstat(e.root)
			if walkErr == nil && directoryErr == nil && !candidate.matches(p.generation, check, checked, hash, currentDirectory) {
				walkErr = ErrArchiveWindowProof
			}
			err = errors.Join(walkErr, directoryErr, check.file.Close())
			if err != nil {
				e.windowPending = nil
			}
		}
		return result, err
	})
}

func archiveDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrDiskUsageCorrupt
	}
	return nil
}

// Counts actual writes across all sinks, including Parquet column swap files.
// No buffers or file handles are pooled beyond a partition's lifetime.
type archiveWork struct {
	mu        sync.Mutex
	remaining int64
	tree      *archiveTreeBudget
}

func (q *archiveWork) take(n int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if int64(n) > q.remaining {
		return ErrDiskUsageLimit
	}
	q.remaining -= int64(n)
	return nil
}

type archiveWriter struct {
	ctx       context.Context
	out       io.Writer
	remaining int64
	work      *archiveWork
}

func (w *archiveWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.remaining {
		return 0, ErrDiskUsageLimit
	}
	if err := w.work.take(len(p)); err != nil {
		return 0, err
	}
	if err := w.work.tree.take(len(p)); err != nil {
		return 0, err
	}
	w.remaining -= int64(len(p))
	return w.out.Write(p)
}

type archivePagePool struct {
	mu        sync.Mutex
	ctx       context.Context
	directory string
	work      *archiveWork
	live      map[*archivePageBuffer]struct{}
	err       error
}
type archivePageBuffer struct {
	*os.File
	writer   *archiveWriter
	identity os.FileInfo
}

func (b *archivePageBuffer) Write(p []byte) (int, error) { return b.writer.Write(p) }
func (p *archivePagePool) GetBuffer() io.ReadWriteSeeker {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.live) >= 128 {
		p.err = ErrDiskUsageLimit
		return &archiveErrorBuffer{p.err}
	}
	f, err := os.CreateTemp(p.directory, ".archive-column-*.tmp")
	if err != nil {
		p.err = err
		return &archiveErrorBuffer{err}
	}
	identity, err := f.Stat()
	if err != nil {
		p.err = errors.Join(err, f.Close(), os.Remove(f.Name()))
		return &archiveErrorBuffer{p.err}
	}
	b := &archivePageBuffer{File: f, writer: &archiveWriter{ctx: p.ctx, out: f, remaining: 16 << 20, work: p.work}, identity: identity}
	if p.live == nil {
		p.live = make(map[*archivePageBuffer]struct{})
	}
	p.live[b] = struct{}{}
	return b
}
func (p *archivePagePool) PutBuffer(buffer io.ReadWriteSeeker) {
	b, ok := buffer.(*archivePageBuffer)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, live := p.live[b]; !live {
		return
	}
	delete(p.live, b)
	p.err = errors.Join(p.err, cleanupArchiveTemporary(b.File, false, b.identity))
}

// release retires every buffer the writer did not return. A failed write
// (ENOSPC among others) abandons the column buffers it holds, so their files
// are the pool's to close and unlink. It reports how many were abandoned.
func (p *archivePagePool) release() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	abandoned := len(p.live)
	for b := range p.live {
		delete(p.live, b)
		p.err = errors.Join(p.err, cleanupArchiveTemporary(b.File, false, b.identity))
	}
	return abandoned, p.err
}

type archiveErrorBuffer struct{ err error }

func (b *archiveErrorBuffer) Read([]byte) (int, error)       { return 0, b.err }
func (b *archiveErrorBuffer) Write([]byte) (int, error)      { return 0, b.err }
func (b *archiveErrorBuffer) Seek(int64, int) (int64, error) { return 0, b.err }

func (e *Exporter) publishDiskPartition(ctx context.Context, date string, attempts []AttemptEvent, limit int64, writes, work *archiveWork) (entry ManifestFile, err error) {
	if len(attempts) == 0 || len(attempts) > 256 {
		return entry, ErrDiskUsageLimit
	}
	rows := make([]parquetAttempt, len(attempts))
	entry = ManifestFile{SchemaVersion: parquetSchemaVersion, Date: date, MinSequence: attempts[0].Sequence, MaxSequence: attempts[len(attempts)-1].Sequence, Records: int64(len(attempts)), Format: e.format}
	var previous uint64
	for i, a := range attempts {
		if a.Sequence == 0 || a.Sequence > math.MaxInt64 || a.Sequence <= previous || a.EventID == "" {
			return ManifestFile{}, ErrDiskUsageCorrupt
		}
		if a.CompletedAt.UTC().Format("2006-01-02") != date {
			return ManifestFile{}, fmt.Errorf("%w: archive source partition date mismatch", ErrDiskUsageCorrupt)
		}
		previous = a.Sequence
		rows[i] = toParquetAttempt(a)
		if err := addInt64(&entry.InputTokens, a.ProviderInputTokens); err != nil {
			return ManifestFile{}, err
		}
		if err := addInt64(&entry.OutputTokens, a.ProviderOutputTokens); err != nil {
			return ManifestFile{}, err
		}
		if cost, ok := a.KnownCostMicrosUSD(); ok {
			if err := addInt64(&entry.CostMicrosUSD, cost); err != nil {
				return ManifestFile{}, err
			}
		}
	}
	entry.Path = filepath.ToSlash(filepath.Join("date="+date, fmt.Sprintf("usage-bounded-v1-%020d-%020d.%s", entry.MinSequence, entry.MaxSequence, partitionExtension(e.format))))
	if err := validateArchivePartitionDate(entry); err != nil {
		return ManifestFile{}, err
	}
	path, err := e.safeManifestPath(entry.Path)
	if err != nil {
		return ManifestFile{}, err
	}
	if err := archiveDirectory(filepath.Dir(path)); err != nil {
		return ManifestFile{}, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".archive-partition-*.tmp")
	if err != nil {
		return ManifestFile{}, err
	}
	closed := false
	var identity os.FileInfo
	defer func() {
		err = errors.Join(err, cleanupArchiveTemporary(f, closed, identity))
		if err != nil {
			entry = ManifestFile{}
		}
	}()
	identity, err = f.Stat()
	if err != nil {
		return ManifestFile{}, err
	}
	if err := f.Chmod(0o600); err != nil {
		return ManifestFile{}, err
	}
	hash := sha256.New()
	out := &archiveWriter{ctx: ctx, out: io.MultiWriter(f, hash), remaining: limit, work: writes}
	if e.format == FormatNDJSON {
		encoder := json.NewEncoder(out)
		for _, row := range rows {
			if err := encoder.Encode(row); err != nil {
				return ManifestFile{}, err
			}
		}
	} else {
		pool := &archivePagePool{ctx: ctx, directory: filepath.Dir(path), work: work}
		// One bounded row group per file, fixed page/write buffers, disk column
		// buffers, and a private single-worker codec. Shared fee/peak calibration
		// still belongs to I-03/D-04; row count alone is not that evidence.
		writer := parquet.NewGenericWriter[parquetAttempt](out, parquet.Compression(&zstd.Codec{Concurrency: 1}), parquet.PageBufferSize(32<<10), parquet.WriteBufferSize(32<<10), parquet.MaxRowsPerRowGroup(256), parquet.ColumnPageBuffers(pool))
		_, writeErr := writer.Write(rows)
		closeErr := writer.Close()
		abandoned, poolErr := pool.release()
		// A writer that succeeded must have returned every buffer.
		if abandoned != 0 && writeErr == nil && closeErr == nil {
			poolErr = errors.Join(poolErr, ErrDiskUsageCorrupt)
		}
		if err := errors.Join(writeErr, closeErr, poolErr); err != nil {
			return ManifestFile{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return ManifestFile{}, err
	}
	if err := f.Sync(); err != nil {
		return ManifestFile{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return ManifestFile{}, err
	}
	identity = info
	if err := checkArchiveTemporary(f, identity); err != nil {
		return ManifestFile{}, err
	}
	entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
	// Link refuses to overwrite another publisher's or orphan's bytes.
	if err := os.Link(f.Name(), path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return ManifestFile{}, err
		}
		if err := checkArchiveOrphan(ctx, path, info.Size(), entry.SHA256); err != nil {
			return ManifestFile{}, err
		}
	}
	err = f.Close()
	closed = true
	if err != nil {
		return ManifestFile{}, err
	}
	if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
		return ManifestFile{}, err
	}
	// A newly created date directory also needs its parent publication barrier.
	if err := durable.SyncDirectory(e.root); err != nil {
		return ManifestFile{}, err
	}
	return entry, nil
}

func checkArchiveOrphan(ctx context.Context, path string, size int64, digest string) (err error) {
	named, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !named.Mode().IsRegular() || named.Size() != size {
		return fmt.Errorf("%w: orphan partition conflicts: %s", ErrDiskUsageCorrupt, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close(), ctx.Err()) }()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(named, opened) {
		return ErrDiskUsageCorrupt
	}
	hash := sha256.New()
	buf := make([]byte, 64<<10)
	var read int64
	for read < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := io.ReadFull(f, buf[:min(int64(len(buf)), size-read)])
		if err != nil {
			return err
		}
		hash.Write(buf[:n])
		read += int64(n)
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("%w: orphan partition conflicts: %s", ErrDiskUsageCorrupt, path)
	}
	current, err := f.Stat()
	if err != nil {
		return err
	}
	namedNow, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(opened, namedNow) || opened.Size() != current.Size() || opened.ModTime() != current.ModTime() || opened.Size() != namedNow.Size() || opened.ModTime() != namedNow.ModTime() {
		return ErrDiskUsageCorrupt
	}
	// Matching bytes are not proof that an earlier rename/link barrier survived.
	return f.Sync()
}

// Cleanup reports physical close and ownership-checked unlink failures. A
// publisher may not acknowledge a prefix while one of its temporaries failed
// to retire, nor unlink a replacement belonging to someone else.
func cleanupArchiveTemporary(f *os.File, closed bool, identity os.FileInfo) error {
	var err error
	if !closed {
		err = f.Close()
	}
	named, statErr := os.Lstat(f.Name())
	if errors.Is(statErr, os.ErrNotExist) {
		return err
	}
	if statErr != nil || identity == nil || !os.SameFile(identity, named) {
		return errors.Join(err, statErr, ErrDiskUsageCorrupt)
	}
	return errors.Join(err, os.Remove(f.Name()))
}

func checkArchiveTemporary(f *os.File, identity os.FileInfo) error {
	named, err := os.Lstat(f.Name())
	if err != nil || identity == nil || !os.SameFile(identity, named) || identity.Size() != named.Size() || identity.ModTime() != named.ModTime() {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	return nil
}
