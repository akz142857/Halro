package usage

import (
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
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

const archiveDateVerifyName = ".archive-date-verify-v1.json"

// Limits apply to each call, not to all historical files/rows combined. Each
// partition remains an atomic decoding unit with the existing bounded codec
// preflight. IndexBytes caps disk space, not a history-sized RAM allocation.
type DateCatalogVerifyLimits struct {
	Source                                                        DiskArchiveLimits
	Files, FileRows                                               int
	FileBytes, FileReadBytes, ReadBytes, IndexIOBytes, IndexBytes int64
	MaxDuration                                                   time.Duration
}

func DefaultDateCatalogVerifyLimits() DateCatalogVerifyLimits {
	s := DefaultDiskExportStepLimits().Source
	s.Pages, s.Rows, s.PageRows = 1, 128, 128
	return DateCatalogVerifyLimits{Source: s, Files: 4, FileRows: 4096, FileBytes: 256 << 20, FileReadBytes: 256 << 20, ReadBytes: 1 << 30, IndexIOBytes: 1 << 30, IndexBytes: 4 << 30, MaxDuration: 10 * time.Second}
}

// A tentative comparison relative to the caller's selected fresh projection.
// Ready does not authenticate that projection's native/member/Metadata source,
// authorize activation, grant saved-state trust or permit deletion/native trim.
type DateCatalogVerifyProgress struct {
	Phase                               string
	FilesProcessed, SourceRowsProcessed int
	ReplayUnits                         uint64
	Report                              ArchiveDateVerification
}

type archiveDateVerifyState struct {
	Published     bool           `json:",omitempty"`
	ActivationSHA string         `json:",omitempty"`
	Ancestor      ArchivePageRef `json:",omitzero"`
	AncestorSeen  bool           `json:",omitempty"`
	Version       int
	BuildSHA      string
	SourceHead    ledger.Watermark
	Phase         string
	Next          ArchivePageRef
	Index         int
	After         uint64
	Units         uint64
	Report        ArchiveDateVerification
}

type archiveDateVerifySession struct {
	archiveVerifyScratch
	stateSHA     [32]byte
	source       [16]byte
	window       DiskWindowState
	pruneThrough uint64
	state        archiveDateVerifyState
	replaying    bool
}

// Shared physical ownership only; the two indexes grant different permissions.
// One comparison scratch and one reference scratch can exist per Exporter.
type archiveVerifyScratch struct {
	directory     string
	identity      os.FileInfo
	index         *archiveVerifyIndex
	tree          *archiveTreeBudget
	releaseShared func() error
	releaseRAM    func() error
	workRAM       int64
	workDisk      int64
}

func (s *archiveVerifyScratch) checkScratch() error {
	named, err := os.Lstat(s.directory)
	if err != nil || !named.IsDir() || !os.SameFile(named, s.identity) {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	return s.index.check()
}

func (s *archiveVerifyScratch) closeScratch() (err error) {
	if s.index != nil {
		err = s.index.discard()
		if err != nil {
			return err
		}
		s.index = nil
	}
	if s.directory != "" {
		named, statErr := os.Lstat(s.directory)
		if statErr != nil || s.identity == nil || !os.SameFile(named, s.identity) {
			return errors.Join(err, statErr, ErrDiskUsageCorrupt)
		}
		removeErr := os.Remove(s.directory)
		err = errors.Join(err, removeErr)
		if removeErr == nil {
			s.directory = ""
			s.identity = nil
			s.tree = nil
		}
	}
	if s.directory == "" && s.releaseShared != nil {
		if releaseErr := s.releaseShared(); releaseErr != nil {
			return errors.Join(err, releaseErr)
		}
		s.releaseShared = nil
	}
	if s.directory == "" && s.releaseRAM != nil {
		if releaseErr := s.releaseRAM(); releaseErr != nil {
			return errors.Join(err, releaseErr)
		}
		s.releaseRAM = nil
	}
	return err
}

func (e *Exporter) discardDateVerification() error {
	s := e.dateVerification
	e.dateVerification = nil
	if s == nil {
		return nil
	}
	return s.closeScratch()
}

func initialDateVerifyState(build archiveDateBuildState, buildSHA [32]byte, head ledger.Watermark) archiveDateVerifyState {
	return archiveDateVerifyState{Version: 1, BuildSHA: hex.EncodeToString(buildSHA[:]), SourceHead: head, Phase: "archive", Next: build.Output,
		Report: ArchiveDateVerification{ArchiveVerification: ArchiveVerification{ThroughSequence: build.Baseline.LastSequence, SourceHead: head}, RetainFromDate: build.RetainFrom}}
}

func (s archiveDateVerifyState) valid(build archiveDateBuildState, buildSHA [32]byte, head ledger.Watermark) bool {
	r := s.Report
	if !s.Published && (s.ActivationSHA != "" || s.Ancestor != (ArchivePageRef{}) || s.AncestorSeen) || s.Published && (len(s.ActivationSHA) != 64 || !s.Ancestor.valid() || build.Baseline.Retention == (ArchiveDateRetention{})) {
		return false
	}
	if s.Version != 1 || s.BuildSHA != hex.EncodeToString(buildSHA[:]) || s.SourceHead != head || r.SourceHead != head || r.RetainFromDate != build.RetainFrom || r.AfterSequence != 0 || r.ThroughSequence != build.Baseline.LastSequence || !s.Next.valid() || s.Index < 0 || s.Index >= archiveCatalogPageFiles || s.After > r.ThroughSequence || r.Files < 0 || uint64(r.Files) > build.Output.Files || r.ArchiveRows < 0 || r.SourceRows < 0 || r.RequiredRows < 0 || r.MatchedRows < 0 || r.ExpiredOmittedRows < 0 || r.MatchedRows > r.ArchiveRows || r.MatchedRows+r.ExpiredOmittedRows != r.SourceRows || r.RequiredRows > r.MatchedRows {
		return false
	}
	switch s.Phase {
	case "archive":
		return s.After == 0 && r.SourceRows == 0 && s.Units == uint64(r.Files) && (s.Next != (ArchivePageRef{}) || s.Index == 0)
	case "source":
		return s.Next == (ArchivePageRef{}) && s.Index == 0 && uint64(r.Files) == build.Output.Files && s.Units == uint64(r.Files)+uint64(r.SourceRows)+1
	case "ready":
		return (!s.Published || s.AncestorSeen) && s.Next == (ArchivePageRef{}) && s.Index == 0 && s.After == r.ThroughSequence && uint64(r.Files) == build.Output.Files && r.MatchedRows == r.ArchiveRows && s.Units == uint64(r.Files)+uint64(r.SourceRows)+2
	}
	return false
}

func readDateVerifyState(ctx context.Context, path string) (state archiveDateVerifyState, raw []byte, f *os.File, info os.FileInfo, err error) {
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
	if err != nil || len(raw) > archiveDateBuildBytes || json.Unmarshal(raw, &state) != nil {
		return state, raw, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(canonical, raw) {
		return state, raw, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	return state, raw, f, info, errors.Join(checkDateBuildState(f, info, path), ctx.Err())
}

func (e *Exporter) VerifyRetainedDateCatalogStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DateCatalogVerifyLimits) (DateCatalogVerifyProgress, error) {
	return e.verifyRetainedDateCatalogStep(ctx, p, retainFrom, limits, nil)
}

// The hook runs after the actual scratch Sync and before the checkpoint rename,
// within both owner lifetimes. It provides no production source authority.
func (e *Exporter) verifyRetainedDateCatalogStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DateCatalogVerifyLimits, beforeSave func() error) (out DateCatalogVerifyProgress, err error) {
	return e.verifyDateCatalogStep(ctx, p, retainFrom, limits, false, beforeSave)
}

// Rebuild the comparison of a published date root and prove its immutable-page
// ancestry relative to one pending obligation. The marker is not native trust.
// This supports barrier recovery after reopen and ordinary late-date appends.
func (e *Exporter) VerifyPublishedDateCatalogStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DateCatalogVerifyLimits) (DateCatalogVerifyProgress, error) {
	return e.verifyDateCatalogStep(ctx, p, retainFrom, limits, true, nil)
}

func (e *Exporter) verifyDateCatalogStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DateCatalogVerifyLimits, published bool, beforeSave func() error) (out DateCatalogVerifyProgress, err error) {
	if p == nil || retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 || limits.Files < 1 || limits.Files > 16 || limits.FileRows < 1 || limits.FileRows > 1<<20 || limits.FileBytes < 1 || limits.FileBytes > 256<<20 || limits.ReadBytes < 2<<20 || limits.ReadBytes > 1<<30 || limits.FileReadBytes < 1<<20 || limits.FileReadBytes > limits.ReadBytes-2*archiveCatalogPageBytes-2*archiveDateBuildBytes || limits.IndexIOBytes < 1<<20 || limits.IndexIOBytes > 1<<40 || limits.IndexBytes < 16*archiveVerifyNodeBytes || limits.IndexBytes > 1<<40 || limits.MaxDuration <= 0 || limits.MaxDuration > time.Minute {
		return out, ErrDiskUsageLimit
	}
	limits.FileBytes = clampArchiveScratch(limits.FileBytes, e.archiveDiskCeiling)
	limits.IndexBytes = clampArchiveScratch(limits.IndexBytes, e.archiveDiskCeiling)
	if err := e.archiveReadError(ctx); err != nil {
		return out, err
	}
	if !e.mu.TryLock() {
		return out, ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	e.dateProof = nil
	e.windowProof = nil
	e.windowPending = nil
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	tree, err := newArchiveTreeBudget(ctx, e.root, e.archiveDiskCeiling)
	if err != nil {
		return out, err
	}
	defer func() {
		err = errors.Join(err, e.archiveReadError(ctx))
		if p.retired.Load() {
			err = errors.Join(err, ErrDiskUsageClosed)
		}
		if err != nil {
			e.dateProof = nil
			e.windowProof = nil
			e.windowPending = nil
			out = DateCatalogVerifyProgress{}
			err = errors.Join(err, e.discardDateVerification())
		}
	}()
	var tentative *archiveDateProof
	_, err = p.archiveJob(ctx, limits.Source, func(ctx context.Context) (_ DiskArchiveCut, jobErr error) {
		readBudget := &archiveVerifyReadBudget{remaining: limits.ReadBytes}
		indexBudget := &archiveVerifyReadBudget{remaining: limits.IndexIOBytes}
		// Every physical close/checkpoint/index cleanup is inside archiveJob;
		// neither owner may return Close before an accepted callback exits.
		defer func() {
			jobErr = errors.Join(jobErr, e.archiveReadError(ctx))
			if p.retired.Load() {
				jobErr = errors.Join(jobErr, ErrDiskUsageClosed)
			}
			if jobErr != nil {
				jobErr = errors.Join(jobErr, e.discardDateVerification())
			}
		}()
		directory, err := os.Lstat(e.root)
		if err != nil || !directory.IsDir() {
			return DiskArchiveCut{}, errors.Join(err, ErrDiskUsageCorrupt)
		}
		defer func() {
			now, statErr := os.Lstat(e.root)
			if statErr != nil || !os.SameFile(directory, now) {
				jobErr = errors.Join(jobErr, statErr, ErrDiskUsageCorrupt)
			}
		}()
		root, err := openArchiveManifest(e.root)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		defer func() { jobErr = errors.Join(jobErr, root.file.Close()) }()
		if root.info.Size() > archiveManifestValueLimit {
			return DiskArchiveCut{}, ErrArchivePagedCatalog
		}
		if err := readBudget.reserve(root.info.Size()); err != nil {
			return DiskArchiveCut{}, err
		}
		position, digest, err := root.walk(ctx, nil)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		if position.CatalogVersion != 1 {
			return DiskArchiveCut{}, ErrArchivePagedCatalog
		}
		buildPath := filepath.Join(e.root, archiveDateBuildName)
		var build archiveDateBuildState
		var buildRaw []byte
		var buildFile *os.File
		var buildInfo os.FileInfo
		var activationSHA [32]byte
		var activationBinding [32]byte
		var ancestor ArchivePageRef
		var candidate ArchivePosition
		checkBuild := func() error { return checkDateBuildState(buildFile, buildInfo, buildPath) }
		if published {
			if position.Retention == (ArchiveDateRetention{}) || position.Retention.RetainFromDate != retainFrom.UTC().Format("2006-01-02") {
				return DiskArchiveCut{}, ErrArchiveDateProof
			}
			if limits.FileReadBytes > limits.ReadBytes-2*archiveCatalogPageBytes-2*archiveDateBuildBytes-archiveDateActivationBytes-archiveDateActivePolicyBytes {
				return DiskArchiveCut{}, ErrDiskUsageLimit
			}
			if err := readBudget.reserve(archiveDateActivationBytes + archiveDateActivePolicyBytes); err != nil {
				return DiskArchiveCut{}, err
			}
			stats, err := p.ReadStats(ctx)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			input, err := e.readCurrentDatePolicy(ctx, position, stats.Watermark)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			state, raw, file, info := input.state, input.raw, input.file, input.info
			defer func() { jobErr = errors.Join(jobErr, file.Close()) }()
			_, candidate, err = state.positions()
			if err != nil || candidate.Retention != position.Retention || candidate.LastSequence > position.LastSequence {
				return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
			}
			ancestor = candidate.CatalogHead
			activationSHA = sha256.Sum256(raw)
			activationBinding, err = dateActivationBinding(state)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			build = archiveDateBuildState{Version: 1, Baseline: position, BaselineSHA: hex.EncodeToString(digest[:]), RetainFrom: retainFrom.UTC().Format("2006-01-02"), Output: position.CatalogHead, ScannedPages: position.CatalogHead.Pages}
			buildRaw, err = json.Marshal(build)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			checkBuild = func() error { return checkDateBuildState(file, info, input.path) }
		} else {
			if err := readBudget.reserve(archiveDateBuildBytes); err != nil {
				return DiskArchiveCut{}, err
			}
			build, buildRaw, buildFile, buildInfo, err = readDateBuildState(ctx, buildPath)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			defer func() { jobErr = errors.Join(jobErr, buildFile.Close()) }()
			_, candidate, err = archiveCatalogRootBytes(position.LastSequence, false, build.Output, ArchiveDateRetention{Version: 1, RetainFromDate: build.RetainFrom, PruneThroughSequence: position.LastSequence})
			if err != nil {
				return DiskArchiveCut{}, err
			}
		}
		if build.Next != (ArchivePageRef{}) || build.ScannedPages != position.CatalogHead.Pages {
			return DiskArchiveCut{}, ErrDiskUsageCursor
		}
		if position != build.Baseline || hex.EncodeToString(digest[:]) != build.BaselineSHA || build.RetainFrom != retainFrom.UTC().Format("2006-01-02") {
			return DiskArchiveCut{}, ErrArchiveDateBuildStale
		}
		stats, err := p.ReadStats(ctx)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		if stats.Watermark.Sequence < position.LastSequence || stats.Window.AttemptAfter != 0 {
			return DiskArchiveCut{}, ErrDiskUsageCursor
		}
		buildSHA := sha256.Sum256(buildRaw)
		path := filepath.Join(e.root, archiveDateVerifyName)
		if published {
			path = filepath.Join(e.root, archiveDatePublishedVerifyName)
		}
		if err := readBudget.reserve(archiveDateBuildBytes); err != nil {
			return DiskArchiveCut{}, err
		}
		target, raw, stateFile, stateInfo, err := readDateVerifyState(ctx, path)
		missing := errors.Is(err, os.ErrNotExist)
		if err != nil && !missing {
			return DiskArchiveCut{}, err
		}
		if stateFile != nil {
			defer func() { jobErr = errors.Join(jobErr, stateFile.Close()) }()
		}
		if missing {
			target = initialDateVerifyState(build, buildSHA, stats.Watermark)
			target.Published = published
			if published {
				target.ActivationSHA = hex.EncodeToString(activationBinding[:])
				target.Ancestor = ancestor
				target.AncestorSeen = ancestor == (ArchivePageRef{}) || ancestor == position.CatalogHead
			}
			raw, err = json.Marshal(target)
			if err != nil {
				return DiskArchiveCut{}, err
			}
		}
		if target.Published != published || published && (target.ActivationSHA != hex.EncodeToString(activationBinding[:]) || target.Ancestor != ancestor) || target.BuildSHA != hex.EncodeToString(buildSHA[:]) || target.SourceHead != stats.Watermark {
			return DiskArchiveCut{}, ErrArchiveDateBuildStale
		}
		if !target.valid(build, buildSHA, stats.Watermark) {
			return DiskArchiveCut{}, ErrDiskUsageCorrupt
		}
		stateSHA := sha256.Sum256(raw)
		s := e.dateVerification
		if s == nil || s.stateSHA != stateSHA || s.source != p.generation || s.window != stats.Window || s.workRAM < limits.FileBytes+archiveVerifyDecoderRAMBytes || s.workDisk < limits.IndexBytes+limits.FileBytes {
			if err := e.discardDateVerification(); err != nil {
				return DiskArchiveCut{}, err
			}
			if limits.IndexBytes > math.MaxInt64-limits.FileBytes {
				return DiskArchiveCut{}, ErrDiskUsageLimit
			}
			releaseRAM, err := e.reserveArchiveWorkRAM(ctx, limits.FileBytes+archiveVerifyDecoderRAMBytes)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			releaseShared, err := e.reserveArchiveDisk(ctx, limits.IndexBytes+limits.FileBytes)
			if err != nil {
				return DiskArchiveCut{}, errors.Join(err, releaseRAM())
			}
			directory, err := os.MkdirTemp(e.root, ".archive-date-index-*")
			if err != nil {
				return DiskArchiveCut{}, errors.Join(err, releaseShared(), releaseRAM())
			}
			s = &archiveDateVerifySession{archiveVerifyScratch: archiveVerifyScratch{directory: directory, tree: tree, releaseShared: releaseShared, releaseRAM: releaseRAM, workRAM: limits.FileBytes + archiveVerifyDecoderRAMBytes, workDisk: limits.IndexBytes + limits.FileBytes}, stateSHA: stateSHA, source: p.generation, window: stats.Window, pruneThrough: position.Retention.PruneThroughSequence, state: initialDateVerifyState(build, buildSHA, stats.Watermark), replaying: true}
			s.state.Published = published
			if published {
				s.state.ActivationSHA = hex.EncodeToString(activationBinding[:])
				s.state.Ancestor = ancestor
				s.state.AncestorSeen = ancestor == (ArchivePageRef{}) || ancestor == position.CatalogHead
			}
			e.dateVerification = s
			s.identity, err = os.Lstat(directory)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			if err := indexBudget.reserve(archiveVerifyNodeBytes); err != nil {
				return DiskArchiveCut{}, err
			}
			s.index, err = newArchiveVerifyIndexWithTree(filepath.Join(directory, "index.bin"), limits.IndexBytes, tree)
			if err != nil {
				return DiskArchiveCut{}, err
			}
		}
		// A changed cap applies explicitly; an already larger scratch is refused.
		if s.index != nil {
			s.tree = tree
			s.index.tree = tree
			if s.index.size > limits.IndexBytes {
				return DiskArchiveCut{}, ErrDiskUsageLimit
			}
			s.index.max = limits.IndexBytes
			if err := s.checkScratch(); err != nil {
				return DiskArchiveCut{}, err
			}
		}
		acceptReplay := func() error {
			if !s.replaying {
				return nil
			}
			if s.state.Units > target.Units {
				return ErrDiskUsageCorrupt
			}
			if s.state.Units == target.Units {
				if s.state != target {
					return ErrDiskUsageCorrupt
				}
				s.replaying = false
			}
			return nil
		}
		for s.state.Phase == "archive" {
			if err := acceptReplay(); err != nil {
				return DiskArchiveCut{}, err
			}
			if s.state.Next == (ArchivePageRef{}) {
				if uint64(s.state.Report.Files) != build.Output.Files {
					return DiskArchiveCut{}, ErrDiskUsageCorrupt
				}
				s.state.Phase = "source"
				s.state.Units++
				break
			}
			if out.FilesProcessed == limits.Files {
				break
			}
			ref := s.state.Next
			if readBudget.remaining < int64(ref.Bytes)+limits.FileReadBytes {
				break
			}
			if err := readBudget.reserve(int64(ref.Bytes)); err != nil {
				return DiskArchiveCut{}, err
			}
			page, err := e.readCatalogPage(ctx, ref)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			if s.state.Index >= len(page.Files) {
				return DiskArchiveCut{}, ErrDiskUsageCorrupt
			}
			entry := page.Files[s.state.Index]
			if entry.Records > int64(limits.FileRows) {
				return DiskArchiveCut{}, ErrDiskUsageLimit
			}
			// Reserve worst radix work before entering an atomic partition. A low
			// budget stops between files with durable progress, never half an index.
			need := (entry.Records + 1) * archiveVerifyIndexRowWork
			if need > limits.IndexIOBytes {
				return DiskArchiveCut{}, ErrDiskUsageLimit
			}
			if need > indexBudget.remaining {
				break
			}
			fileBudget := &archiveVerifyReadBudget{remaining: limits.FileReadBytes}
			if err := readBudget.reserve(limits.FileReadBytes); err != nil {
				return DiskArchiveCut{}, err
			}
			if err := e.indexDateCandidateFile(ctx, s, entry, build, limits, fileBudget, indexBudget); err != nil {
				return DiskArchiveCut{}, err
			}
			readBudget.remaining += fileBudget.remaining
			s.state.Report.Files++
			s.state.Units++
			out.FilesProcessed++
			s.state.Index++
			if s.state.Index == len(page.Files) {
				s.state.Index = 0
				s.state.Next = page.Previous
			}
			if published && s.state.Next == ancestor {
				s.state.AncestorSeen = true
			}
		}
		if err := acceptReplay(); err != nil {
			return DiskArchiveCut{}, err
		}
		if s.state.Phase == "source" {
			sourceLimits := limits.Source
			if s.replaying && uint64(sourceLimits.Rows) > target.Units-s.state.Units {
				sourceLimits.Rows = int(target.Units - s.state.Units)
			}
			// A lookup has at most 257 fixed nodes; cap rows before entering IO.
			available := int(indexBudget.remaining / (257 * archiveVerifyNodeBytes))
			if sourceLimits.Rows > available {
				sourceLimits.Rows = available
			}
			if sourceLimits.Rows > 0 {
				cut, err := p.streamArchiveIntervalHeld(ctx, s.state.After, position.LastSequence, true, true, sourceLimits, func(ctx context.Context, rows []AttemptEvent) error {
					for _, a := range rows {
						if err := compareDateCandidateAttempt(ctx, s, a, indexBudget); err != nil {
							return err
						}
						s.state.After = a.Sequence
						s.state.Units++
						out.SourceRowsProcessed++
						if err := acceptReplay(); err != nil {
							return err
						}
					}
					return nil
				}, nil)
				if err != nil {
					return DiskArchiveCut{}, err
				}
				if cut.Head != stats.Watermark {
					return DiskArchiveCut{}, ErrArchiveDateBuildStale
				}
				if cut.ThroughSequence == position.LastSequence {
					if s.state.Report.MatchedRows != s.state.Report.ArchiveRows {
						return DiskArchiveCut{}, fmt.Errorf("%w: extra candidate archive event", ErrDiskUsageCorrupt)
					}
					s.state.After = position.LastSequence
					s.state.Phase = "ready"
					s.state.Units++
				}
			}
		}
		if err := acceptReplay(); err != nil {
			return DiskArchiveCut{}, err
		}
		if !s.state.valid(build, buildSHA, stats.Watermark) {
			return DiskArchiveCut{}, ErrDiskUsageCorrupt
		}
		if err := root.check(); err != nil {
			return DiskArchiveCut{}, err
		}
		if err := checkBuild(); err != nil {
			return DiskArchiveCut{}, err
		}
		finalStats, err := p.ReadStats(ctx)
		if err != nil || finalStats.Watermark != stats.Watermark || finalStats.Window != stats.Window {
			return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateBuildStale)
		}
		if s.index != nil {
			if err := s.checkScratch(); err != nil {
				return DiskArchiveCut{}, err
			}
			if err := s.index.file.Sync(); err != nil {
				return DiskArchiveCut{}, err
			}
		}
		if beforeSave != nil {
			if err := beforeSave(); err != nil {
				return DiskArchiveCut{}, err
			}
		}
		if err := ctx.Err(); err != nil {
			return DiskArchiveCut{}, err
		}
		if err := root.check(); err != nil {
			return DiskArchiveCut{}, err
		}
		if err := checkBuild(); err != nil {
			return DiskArchiveCut{}, err
		}
		now, statErr := os.Lstat(e.root)
		if statErr != nil || !os.SameFile(directory, now) {
			return DiskArchiveCut{}, errors.Join(statErr, ErrDiskUsageCorrupt)
		}
		if s.replaying {
			out.Phase, out.ReplayUnits = "revalidate", s.state.Units
			return DiskArchiveCut{}, nil
		}
		if s.state.Phase == "ready" {
			if err := s.closeScratch(); err != nil {
				return DiskArchiveCut{}, err
			}
		}
		newRaw, err := json.Marshal(s.state)
		if err != nil {
			return DiskArchiveCut{}, err
		}
		if missing || !bytes.Equal(newRaw, raw) {
			if err := e.saveDateBuildState(ctx, path, newRaw, stateFile, stateInfo, missing, &archiveWork{remaining: archiveDateBuildBytes}, nil); err != nil {
				return DiskArchiveCut{}, err
			}
		} else if err := syncDateBuildState(ctx, stateFile, stateInfo, path); err != nil {
			return DiskArchiveCut{}, err
		}
		if err := root.check(); err != nil {
			return DiskArchiveCut{}, err
		}
		if err := checkBuild(); err != nil {
			return DiskArchiveCut{}, err
		}
		finalStats, err = p.ReadStats(ctx)
		if err != nil || finalStats.Watermark != stats.Watermark || finalStats.Window != stats.Window {
			return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateBuildStale)
		}
		s.stateSHA = sha256.Sum256(newRaw)
		out.Phase, out.Report = s.state.Phase, s.state.Report
		// Successful Ready has no retained index or cached verification shortcut.
		// Another call must reconstruct the saved comparison, even in this process.
		if s.state.Phase == "ready" {
			tentative = &archiveDateProof{owner: p.generation, head: stats.Watermark, window: stats.Window, position: position, candidate: candidate, digest: digest, buildSHA: buildSHA, verifySHA: sha256.Sum256(newRaw), activationSHA: activationSHA, identity: root.info, directory: directory, published: published}
			e.dateVerification = nil
		}
		return DiskArchiveCut{}, nil
	})
	if err == nil {
		e.dateProof = tentative
		if tentative != nil && tentative.published {
			// Only a complete fresh published-root comparison can authorize
			// window removal: every native row is present or its omission is
			// allowed by the independently authenticated date policy/baseline.
			// Candidate Ready and the general scoped verifier do not mint this.
			e.windowProof = &archiveWindowProof{owner: tentative.owner, position: tentative.position, digest: tentative.digest, identity: tentative.identity, directory: tentative.directory}
		}
	}
	return out, err
}

func (e *Exporter) indexDateCandidateFile(ctx context.Context, s *archiveDateVerifySession, entry ManifestFile, build archiveDateBuildState, limits DateCatalogVerifyLimits, readBudget, indexBudget *archiveVerifyReadBudget) (err error) {
	if err := validateArchivePartitionDate(entry); err != nil {
		return err
	}
	if entry.Date < build.RetainFrom && !s.state.Published || validateArchiveEntryCoverage(entry, build.Baseline.LastSequence, build.Baseline.Retention) != nil {
		return ErrDiskUsageCorrupt
	}
	pathHash := sha256.Sum256([]byte(entry.Path))
	if err := s.index.insert(ctx, 2, pathHash, [40]byte{}, indexBudget); err != nil {
		return err
	}
	spool, err := copyArchiveVerifySpool(ctx, e, entry, s.directory, limits.FileBytes, readBudget)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, retireArchiveSpool(spool)) }()
	var rows, input, output, cost int64
	var previous uint64
	visit := func(row parquetAttempt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if rows == int64(limits.FileRows) || row.SchemaVersion != int32(entry.SchemaVersion) || row.Sequence <= 0 || uint64(row.Sequence) <= previous || row.EventID == "" || row.RequestID == "" || row.AttemptID == "" || time.UnixMicro(row.CompletedAtMicros).UTC().Format("2006-01-02") != entry.Date {
			return ErrDiskUsageCorrupt
		}
		seq := uint64(row.Sequence)
		if rows == 0 && seq != entry.MinSequence || seq > entry.MaxSequence {
			return ErrDiskUsageCorrupt
		}
		previous = seq
		rows++
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
		if err := s.index.insert(ctx, 0, sha256.Sum256([]byte(row.EventID)), value, indexBudget); err != nil {
			return err
		}
		if err := s.index.insert(ctx, 1, sha256.Sum256(diskUsageSequence(seq)), [40]byte{}, indexBudget); err != nil {
			return err
		}
		return nil
	}
	info, err := spool.Stat()
	if err != nil {
		return err
	}
	if entry.format() == FormatParquet {
		err = archiveReadParquet(ctx, spool, info.Size(), readBudget, visit)
	} else {
		if err := readBudget.reserve(info.Size()); err != nil {
			return err
		}
		err = archiveReadNDJSON(ctx, io.NewSectionReader(spool, 0, info.Size()), visit)
	}
	if err != nil {
		return err
	}
	if rows != entry.Records || previous != entry.MaxSequence || input != entry.InputTokens || output != entry.OutputTokens || cost != entry.CostMicrosUSD {
		return fmt.Errorf("%w: candidate partition summary", ErrDiskUsageCorrupt)
	}
	if err := addDateVerifyCount(&s.state.Report.ArchiveRows, rows); err != nil {
		return err
	}
	if s.state.Report.FirstRetainedSequence == 0 || entry.MinSequence < s.state.Report.FirstRetainedSequence {
		s.state.Report.FirstRetainedSequence = entry.MinSequence
	}
	return nil
}

func addDateVerifyCount(target *int, delta int64) error {
	if delta < 0 || uint64(delta) > uint64(^uint(0)>>1)-uint64(*target) {
		return ErrDiskUsageLimit
	}
	*target += int(delta)
	return nil
}

func compareDateCandidateAttempt(ctx context.Context, s *archiveDateVerifySession, a AttemptEvent, budget *archiveVerifyReadBudget) error {
	value, found, err := s.index.lookup(ctx, 0, sha256.Sum256([]byte(a.EventID)), budget)
	if err != nil {
		return err
	}
	// Expiry only authorizes omissions at/below the published sweep baseline.
	// Every later native attempt is newly appended coverage, including old dates.
	required := a.CompletedAt.UTC().Format("2006-01-02") >= s.state.Report.RetainFromDate || s.state.Published && a.Sequence > s.pruneThrough
	if !found {
		if required {
			return fmt.Errorf("%w: missing candidate event", ErrDiskUsageCorrupt)
		}
		if err := addDateVerifyCount(&s.state.Report.ExpiredOmittedRows, 1); err != nil {
			return err
		}
	} else {
		schema := int32(binary.BigEndian.Uint64(value[:8]))
		if !supportedManifestSchema(int(schema)) {
			return ErrDiskUsageCorrupt
		}
		raw, err := json.Marshal(narrowToSchema(toParquetAttempt(a), schema))
		if err != nil {
			return err
		}
		if len(raw) > diskUsageMaxValue {
			return ErrDiskUsageLimit
		}
		digest := sha256.Sum256(raw)
		if !bytes.Equal(value[8:], digest[:]) {
			return fmt.Errorf("%w: candidate archive content mismatch", ErrDiskUsageCorrupt)
		}
		if err := addDateVerifyCount(&s.state.Report.MatchedRows, 1); err != nil {
			return err
		}
	}
	if required {
		if err := addDateVerifyCount(&s.state.Report.RequiredRows, 1); err != nil {
			return err
		}
	}
	return addDateVerifyCount(&s.state.Report.SourceRows, 1)
}
