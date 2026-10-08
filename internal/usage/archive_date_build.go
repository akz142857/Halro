package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/durable"
)

var ErrArchiveDateBuildStale = errors.New("date catalog candidate no longer matches the live root or policy")

const archiveDateBuildBytes = 8 << 10
const archiveDateBuildName = ".archive-date-build-v1.json"

type DateCatalogBuildLimits struct {
	Pages       int
	WorkBytes   int64
	MaxDuration time.Duration
}

func DefaultDateCatalogBuildLimits() DateCatalogBuildLimits {
	return DateCatalogBuildLimits{Pages: 4, WorkBytes: 1 << 20, MaxDuration: 10 * time.Second}
}

// ResetRetainedDateCatalogBuild explicitly discards only the saved candidate
// checkpoint. Immutable pages remain retryable orphans; no live root/data file
// is removed, and this action confers no deletion permission. It is never an
// automatic response to corrupt or stale progress.
func (e *Exporter) ResetRetainedDateCatalogBuild(ctx context.Context) (err error) {
	return e.resetArchiveDateCheckpoint(ctx, archiveDateBuildName, false)
}

// Explicitly discard only the comparison checkpoint and this process's private
// scratch. Live roots, immutable pages and data are never removed by reset.
func (e *Exporter) ResetRetainedDateCatalogVerification(ctx context.Context) error {
	return e.resetArchiveDateCheckpoint(ctx, archiveDateVerifyName, true)
}

func (e *Exporter) ResetPublishedDateCatalogVerification(ctx context.Context) error {
	return e.resetArchiveDateCheckpoint(ctx, archiveDatePublishedVerifyName, true)
}

func (e *Exporter) resetArchiveDateCheckpoint(ctx context.Context, name string, verification bool) (err error) {
	if err := e.archiveReadError(ctx); err != nil {
		return err
	}
	if !e.mu.TryLock() {
		return ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	e.dateProof = nil
	defer func() {
		err = errors.Join(err, e.archiveReadError(ctx))
		e.dateBuildReplay = nil
		e.dateReclaimReplay = nil
		e.dateDataProof = nil
	}()
	if verification {
		if err := e.discardDateVerification(); err != nil {
			return err
		}
	}
	if name == archiveDateReferenceName {
		if err := e.discardDateReferences(); err != nil {
			return err
		}
	}
	if err := e.archiveReadError(ctx); err != nil {
		return err
	}
	directory, err := os.Lstat(e.root)
	if err != nil || !directory.IsDir() {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	path := filepath.Join(e.root, name)
	named, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !named.Mode().IsRegular() {
		return ErrDiskUsageCorrupt
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil || !os.SameFile(named, info) {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	if err := checkDateBuildState(f, info, path); err != nil {
		return err
	}
	now, err := os.Lstat(e.root)
	if err != nil || !os.SameFile(directory, now) {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return durable.SyncDirectory(e.root)
}

// This is a transformation receipt only. Neither Ready nor RootJSON proves the
// native row set or authorizes root publication, unlink, window or native trim.
type DateCatalogBuildProgress struct {
	Phase          string
	PagesProcessed int
	ScannedPages   uint64
	ReplayPages    uint64
	Candidate      ArchivePageRef
	RootJSON       json.RawMessage
}

type archiveDateBuildState struct {
	Version      int
	Baseline     ArchivePosition
	BaselineSHA  string
	RetainFrom   string
	Next         ArchivePageRef
	Output       ArchivePageRef
	ScannedPages uint64
}

// Reopening discards this cache. Persisted progress is never trusted: replay
// reconstructs its deterministic output from the still-matching baseline.
type archiveDateBuildReplay struct {
	stateSHA [32]byte
	next     ArchivePageRef
	output   ArchivePageRef
	scanned  uint64
}

func (s archiveDateBuildState) valid() bool {
	d, err := time.Parse("2006-01-02", s.RetainFrom)
	return s.Version == 1 && err == nil && !d.IsZero() && d.Format("2006-01-02") == s.RetainFrom &&
		s.Baseline.CatalogVersion == 1 && s.Baseline.CatalogHead.valid() && s.Next.valid() && s.Output.valid() &&
		s.Baseline.CatalogHead.Pages <= 1<<20 && s.ScannedPages <= s.Baseline.CatalogHead.Pages &&
		s.Next.Pages == s.Baseline.CatalogHead.Pages-s.ScannedPages && s.Output.Files <= s.Baseline.Files &&
		len(s.BaselineSHA) == 64
}

// PrepareRetainedDateCatalogStep builds immutable candidate pages in finite
// steps without altering the live root or deleting files. A root/policy change
// refuses the saved job; a reopen revalidates its saved prefix across finite
// calls before extending it. Activation needs independent native verification.
func (e *Exporter) PrepareRetainedDateCatalogStep(ctx context.Context, retainFrom time.Time, limits DateCatalogBuildLimits) (DateCatalogBuildProgress, error) {
	return e.prepareRetainedDateCatalogStep(ctx, retainFrom, limits, nil)
}

func (e *Exporter) prepareRetainedDateCatalogStep(ctx context.Context, retainFrom time.Time, limits DateCatalogBuildLimits, afterStateRename func() error) (out DateCatalogBuildProgress, err error) {
	if retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 || limits.Pages < 1 || limits.Pages > 16 || limits.WorkBytes < 320<<10 || limits.WorkBytes > 8<<20 || limits.MaxDuration <= 0 || limits.MaxDuration > time.Minute {
		return out, ErrDiskUsageLimit
	}
	if err := e.archiveReadError(ctx); err != nil {
		return out, err
	}
	if !e.mu.TryLock() {
		return out, ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	releaseShared, err := e.reserveArchiveDisk(ctx, limits.WorkBytes)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, releaseShared()) }()
	defer func() {
		err = errors.Join(err, e.archiveReadError(ctx))
		if err != nil {
			out = DateCatalogBuildProgress{}
			e.dateBuildReplay = nil
		}
	}()
	if err := e.archiveReadError(ctx); err != nil {
		return out, err
	}
	directory, err := os.Lstat(e.root)
	if err != nil || !directory.IsDir() {
		return out, errors.Join(err, ErrDiskUsageCorrupt)
	}
	defer func() {
		now, statErr := os.Lstat(e.root)
		if statErr != nil || !os.SameFile(directory, now) {
			err = errors.Join(err, statErr, ErrDiskUsageCorrupt)
		}
	}()
	root, err := openArchiveManifest(e.root)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, root.file.Close()) }()
	// Legacy JSON must first undergo the existing bounded-value migration.
	if root.info.Size() > archiveManifestValueLimit {
		return out, ErrArchivePagedCatalog
	}
	work, err := e.newArchiveWork(ctx, limits.WorkBytes)
	if err != nil {
		return out, err
	}
	if err := work.take(int(root.info.Size())); err != nil {
		return out, err
	}
	position, digest, err := root.walk(ctx, nil)
	if err != nil {
		return out, err
	}
	if position.CatalogVersion != 1 {
		return out, ErrArchivePagedCatalog
	}
	date := retainFrom.UTC().Format("2006-01-02")
	if position.Retention != (ArchiveDateRetention{}) && date < position.Retention.RetainFromDate {
		return out, ErrDiskUsageLimit
	}
	path := filepath.Join(e.root, archiveDateBuildName)
	state, raw, stateFile, stateInfo, err := readDateBuildState(ctx, path)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return out, err
	}
	if stateFile != nil {
		defer func() { err = errors.Join(err, stateFile.Close()) }()
	}
	if err := work.take(len(raw)); err != nil {
		return out, err
	}
	if missing {
		state = archiveDateBuildState{Version: 1, Baseline: position, BaselineSHA: hex.EncodeToString(digest[:]), RetainFrom: date, Next: position.CatalogHead}
		raw, err = json.Marshal(state)
		if err != nil {
			return out, err
		}
	} else if state.Baseline != position || state.BaselineSHA != hex.EncodeToString(digest[:]) || state.RetainFrom != date {
		return out, ErrArchiveDateBuildStale
	}
	if !state.valid() {
		return out, ErrDiskUsageCorrupt
	}
	stateSHA := sha256.Sum256(raw)
	replay := e.dateBuildReplay
	if replay == nil || replay.stateSHA != stateSHA {
		replay = &archiveDateBuildReplay{stateSHA: stateSHA, next: position.CatalogHead}
	}
	used := 0
	// Reserve the next input plus at most two encoded output pages and the
	// checkpoint. Stop between pages, committing finite progress instead of
	// repeatedly exhausting a valid smaller budget midway through the batch.
	canTransform := func(ref ArchivePageRef) bool {
		return work.remaining >= int64(ref.Bytes+2*archiveCatalogPageBytes+archiveDateBuildBytes)
	}
	transform := func(ref ArchivePageRef, output ArchivePageRef) (ArchivePageRef, ArchivePageRef, error) {
		if ref == (ArchivePageRef{}) {
			return ArchivePageRef{}, output, ErrDiskUsageCorrupt
		}
		if err := work.take(ref.Bytes); err != nil {
			return ArchivePageRef{}, output, err
		}
		page, err := e.readCatalogPage(ctx, ref)
		if err != nil {
			return ArchivePageRef{}, output, err
		}
		builder := archiveCatalogBuilder{exporter: e, ctx: ctx, work: work, head: output}
		for _, entry := range page.Files {
			if err := validateArchiveEntryCoverage(entry, position.LastSequence, position.Retention); err != nil {
				return ArchivePageRef{}, output, err
			}
			if err := validateArchivePartitionDate(entry); err != nil {
				return ArchivePageRef{}, output, err
			}
			if entry.Date >= date {
				if err := builder.add(entry); err != nil {
					return ArchivePageRef{}, output, err
				}
			}
		}
		if err := builder.flush(); err != nil {
			return ArchivePageRef{}, output, err
		}
		used++
		return page.Previous, builder.head, nil
	}
	for replay.scanned < state.ScannedPages && used < limits.Pages && canTransform(replay.next) {
		next, output, err := transform(replay.next, replay.output)
		if err != nil {
			return out, err
		}
		replay.next, replay.output, replay.scanned = next, output, replay.scanned+1
	}
	if replay.scanned == state.ScannedPages && (replay.next != state.Next || replay.output != state.Output) {
		return out, ErrDiskUsageCorrupt
	}
	if replay.scanned < state.ScannedPages {
		if err := root.check(); err != nil {
			return out, err
		}
		e.dateBuildReplay = replay
		return DateCatalogBuildProgress{Phase: "revalidate", PagesProcessed: used, ScannedPages: state.ScannedPages, ReplayPages: replay.scanned, Candidate: state.Output}, nil
	}
	for state.Next != (ArchivePageRef{}) && used < limits.Pages && canTransform(state.Next) {
		next, output, err := transform(state.Next, state.Output)
		if err != nil {
			return out, err
		}
		state.Next, state.Output, state.ScannedPages = next, output, state.ScannedPages+1
	}
	if !state.valid() {
		return out, ErrDiskUsageCorrupt
	}
	if err := root.check(); err != nil {
		return out, err
	}
	newRaw, err := json.Marshal(state)
	if err != nil {
		return out, err
	}
	if missing || !bytes.Equal(raw, newRaw) {
		if err := e.saveDateBuildState(ctx, path, newRaw, stateFile, stateInfo, missing, work, afterStateRename); err != nil {
			return out, err
		}
	} else if err := syncDateBuildState(ctx, stateFile, stateInfo, path); err != nil {
		return out, err
	}
	if err := root.check(); err != nil {
		return out, err
	}
	e.dateBuildReplay = &archiveDateBuildReplay{stateSHA: sha256.Sum256(newRaw), next: state.Next, output: state.Output, scanned: state.ScannedPages}
	out = DateCatalogBuildProgress{Phase: "build", PagesProcessed: used, ScannedPages: state.ScannedPages, ReplayPages: replay.scanned, Candidate: state.Output}
	if state.Next == (ArchivePageRef{}) {
		out.Phase = "ready"
		out.RootJSON, _, err = archiveCatalogRootBytes(position.LastSequence, false, state.Output, ArchiveDateRetention{Version: 1, RetainFromDate: date, PruneThroughSequence: position.LastSequence})
	}
	return out, err
}

func readDateBuildState(ctx context.Context, path string) (state archiveDateBuildState, raw []byte, f *os.File, info os.FileInfo, err error) {
	named, err := os.Lstat(path)
	if err != nil {
		return state, nil, nil, nil, err
	}
	if !named.Mode().IsRegular() || named.Size() > archiveDateBuildBytes {
		return state, nil, nil, nil, ErrDiskUsageCorrupt
	}
	f, err = os.Open(path)
	if err != nil {
		return state, nil, nil, nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.Close())
			f = nil
		}
	}()
	info, err = f.Stat()
	if err != nil || !os.SameFile(named, info) {
		return state, nil, f, nil, errors.Join(err, ErrDiskUsageCorrupt)
	}
	raw, err = io.ReadAll(io.LimitReader(f, archiveDateBuildBytes+1))
	if err != nil || len(raw) > archiveDateBuildBytes {
		return state, nil, f, nil, errors.Join(err, ErrDiskUsageCorrupt)
	}
	if json.Unmarshal(raw, &state) != nil || !state.valid() {
		return state, raw, f, info, ErrDiskUsageCorrupt
	}
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(raw, canonical) {
		return state, raw, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	if err := checkDateBuildState(f, info, path); err != nil {
		return state, raw, f, info, err
	}
	return state, raw, f, info, ctx.Err()
}

func checkDateBuildState(f *os.File, info os.FileInfo, path string) error {
	current, err := f.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) || !os.SameFile(info, named) || info.Size() != current.Size() || info.Size() != named.Size() || info.ModTime() != current.ModTime() || info.ModTime() != named.ModTime() {
		return ErrDiskUsageCorrupt
	}
	return nil
}

func syncDateBuildState(ctx context.Context, f *os.File, info os.FileInfo, path string) error {
	if err := checkDateBuildState(f, info, path); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return errors.Join(checkDateBuildState(f, info, path), ctx.Err())
}

func (e *Exporter) saveDateBuildState(ctx context.Context, path string, raw []byte, old *os.File, oldInfo os.FileInfo, missing bool, work *archiveWork, afterRename func() error) (err error) {
	if len(raw) > archiveDateBuildBytes {
		return ErrDiskUsageLimit
	}
	if err := e.ensureArchiveWorkTree(ctx, work); err != nil {
		return err
	}
	directory, err := os.Lstat(e.root)
	if err != nil || !directory.IsDir() {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	f, err := os.CreateTemp(e.root, ".archive-date-state-*.tmp")
	if err != nil {
		return err
	}
	closed := false
	var identity os.FileInfo
	defer func() { err = errors.Join(err, cleanupArchiveTemporary(f, closed, identity)) }()
	identity, err = f.Stat()
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	w := &archiveWriter{ctx: ctx, out: f, remaining: archiveDateBuildBytes, work: work}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	identity, err = f.Stat()
	if err != nil {
		return err
	}
	err = f.Close()
	closed = true
	if err != nil {
		return err
	}
	if err := checkArchiveTemporary(f, identity); err != nil {
		return err
	}
	if missing {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
	} else if err := checkDateBuildState(old, oldInfo, path); err != nil {
		return err
	}
	if err := e.archiveReadError(ctx); err != nil {
		return err
	}
	namedDirectory, err := os.Lstat(e.root)
	if err != nil || !os.SameFile(directory, namedDirectory) {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	if afterRename != nil {
		if err := afterRename(); err != nil {
			return err
		}
	}
	return durable.SyncDirectory(e.root)
}
