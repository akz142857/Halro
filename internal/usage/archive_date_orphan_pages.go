package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Candidate pages are indexed in a disposable fixed-node disk tree before the
// live catalog inventory starts. No directory array or persisted EOF is trusted.
type DateOrphanPageReclaimLimits struct {
	Entries, Pages, CandidatePages      int
	ReadBytes, IndexIOBytes, IndexBytes int64
	MaxDuration                         time.Duration
}

func DefaultDateOrphanPageReclaimLimits() DateOrphanPageReclaimLimits {
	return DateOrphanPageReclaimLimits{Entries: 128, Pages: 4, CandidatePages: 4, ReadBytes: 16 << 20, IndexIOBytes: 16 << 20, IndexBytes: 4 << 30, MaxDuration: 10 * time.Second}
}

type DateOrphanPageReclaimProgress struct {
	Phase                                                     string
	EntriesScanned, EntriesUnrecognized                       int
	PagesExamined, PagesRemoved, PagesMissing, PagesProtected int
	CandidatePagesProcessed                                   int
	CandidatePagesIndexed, Visited                            uint64
	LogicalBytesUnlinked                                      int64
	ObligationPending                                         bool
	Current                                                   ArchivePosition
}

type archiveDateOrphanPageSession struct {
	archiveVerifyScratch
	proof                         archiveDateProof
	pagesSHA, buildSHA            [32]byte
	buildMissing                  bool
	next                          ArchivePageRef
	candidatePages, visited       uint64
	root, catalogRoot             *os.Root
	scan                          *os.File
	directory, catalog, catalogV1 os.FileInfo
}

func (e *Exporter) discardDateOrphanPages() error {
	s := e.dateOrphanPages
	e.dateOrphanPages = nil
	if s == nil {
		return nil
	}
	var err error
	if s.scan != nil {
		err = s.scan.Close()
		s.scan = nil
	}
	if s.catalogRoot != nil {
		err = errors.Join(err, s.catalogRoot.Close())
		s.catalogRoot = nil
	}
	if s.root != nil {
		err = errors.Join(err, s.root.Close())
		s.root = nil
	}
	return errors.Join(err, s.closeScratch())
}

// Withdraw only this process's inventory and comparison permission. The signed
// old-chain transitions, activation receipt, candidates and physical files stay.
func (e *Exporter) ResetRetainedDateOrphanPages(ctx context.Context) error {
	return e.withArchiveReclamation(ctx, func(func() error) error {
		e.dateProof = nil
		return e.discardDateOrphanPages()
	})
}

func (s *archiveDateOrphanPageSession) check(a *archiveDateReclaimAuthority, pagesSHA, buildSHA [32]byte, missing bool) error {
	p := a.proof
	if s.proof.owner != p.owner || s.proof.head != p.head || s.proof.window != p.window || s.proof.position != p.position || s.proof.digest != p.digest || s.proof.activationSHA != p.activationSHA || s.pagesSHA != pagesSHA || s.buildSHA != buildSHA || s.buildMissing != missing || !os.SameFile(s.directory, a.directory) || !os.SameFile(s.proof.identity, p.identity) {
		return ErrArchiveDateBuildStale
	}
	if err := s.checkScratch(); err != nil {
		return err
	}
	actual, err := s.root.Stat(".")
	if err != nil || !os.SameFile(actual, s.directory) {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	if s.scan != nil {
		named, err := s.root.Lstat("catalog")
		if err != nil || !named.IsDir() || !os.SameFile(named, s.catalog) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		named, err = s.root.Lstat("catalog/v1")
		if err != nil || !named.IsDir() || !os.SameFile(named, s.catalogV1) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		actual, err = s.catalogRoot.Stat(".")
		if err != nil || !os.SameFile(actual, s.catalogV1) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		actual, err = s.scan.Stat()
		if err != nil || !os.SameFile(actual, s.catalogV1) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
	}
	return a.check()
}

// Old pages still needed for an irreversible transition are never inventoried
// as orphans. An authentic completed transition is a precondition; its saved
// Ready alone and an ordinary data checkpoint supply no permission.
func (a *archiveDateReclaimAuthority) orphanPageGuards(ctx context.Context) (pagesSHA, buildSHA [32]byte, missing bool, output ArchivePageRef, guard func() error, close func() error, err error) {
	close = func() error { return nil }
	if err = a.readBudget.reserve(2 * archiveDateBuildBytes); err != nil {
		return
	}
	pagesPath := filepath.Join(a.e.root, archiveDatePagesName)
	pages, raw, pagesFile, pagesInfo, err := readDatePagesState(ctx, pagesPath)
	if err != nil {
		return
	}
	close = func() error { return pagesFile.Close() }
	old, candidate, err := a.state.positions()
	if err != nil {
		return
	}
	binding, err := dateActivationBinding(a.state)
	if err != nil {
		return
	}
	if err = a.e.verifyDatePages(pages); err != nil {
		return
	}
	if pages.BindingSHA != hex.EncodeToString(binding[:]) || pages.Old != old.CatalogHead || pages.Next != (ArchivePageRef{}) || pages.Pending || pages.Processed != old.CatalogHead.Pages {
		err = ErrArchiveDateProof
		return
	}
	pagesSHA = sha256.Sum256(raw)
	buildPath := filepath.Join(a.e.root, archiveDateBuildName)
	build, buildRaw, buildFile, buildInfo, readErr := readDateBuildState(ctx, buildPath)
	missing = errors.Is(readErr, os.ErrNotExist)
	if readErr != nil && !missing {
		err = readErr
		return
	}
	if buildFile != nil {
		close = func() error { return errors.Join(buildFile.Close(), pagesFile.Close()) }
		buildSHA = sha256.Sum256(buildRaw)
		// A future candidate against the actual current root stays protected even
		// when its unsigned saved progress is not a source of publishing authority.
		// The completed original candidate is also legal after its baseline vanished.
		current := build.Baseline == a.proof.position && build.BaselineSHA == hex.EncodeToString(a.proof.digest[:]) && build.RetainFrom >= a.proof.position.Retention.RetainFromDate
		oldSHA := sha256.Sum256(a.state.OldRoot)
		completedOriginal := build.Baseline == old && build.BaselineSHA == hex.EncodeToString(oldSHA[:]) && build.RetainFrom == candidate.Retention.RetainFromDate && build.Next == (ArchivePageRef{}) && build.ScannedPages == old.CatalogHead.Pages && build.Output == candidate.CatalogHead
		if !current && !completedOriginal {
			err = ErrArchiveDateBuildStale
			return
		}
		output = build.Output
	}
	// Both actual checkpoint FDs remain held only for this finite job. Re-hash
	// exact bytes as well as checking identities; restoration of mtime is no bypass.
	guard = func() error {
		if err := checkDateBuildState(pagesFile, pagesInfo, pagesPath); err != nil {
			return err
		}
		if err := hashDateReclaimFile(ctx, pagesFile, pagesInfo.Size(), hex.EncodeToString(pagesSHA[:]), nil, a.readBudget); err != nil {
			return err
		}
		if buildFile != nil {
			if err := checkDateBuildState(buildFile, buildInfo, buildPath); err != nil {
				return err
			}
			if err := hashDateReclaimFile(ctx, buildFile, buildInfo.Size(), hex.EncodeToString(buildSHA[:]), nil, a.readBudget); err != nil {
				return err
			}
		} else if _, err := os.Lstat(buildPath); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, ErrArchiveDateBuildStale)
		}
		return nil
	}
	return
}

func (a *archiveDateReclaimAuthority) newOrphanPageSession(ctx context.Context, pagesSHA, buildSHA [32]byte, missing bool, output ArchivePageRef, max int64) (s *archiveDateOrphanPageSession, err error) {
	s = &archiveDateOrphanPageSession{proof: *a.proof, pagesSHA: pagesSHA, buildSHA: buildSHA, buildMissing: missing, next: output, directory: a.directory}
	a.e.dateOrphanPages = s // Every partial resource now belongs to the admitted job.
	s.archiveVerifyScratch.tree = a.tree
	s.releaseShared, err = a.e.reserveArchiveDisk(ctx, max)
	if err != nil {
		return s, err
	}
	s.archiveVerifyScratch.directory, err = os.MkdirTemp(a.e.root, ".archive-date-orphan-page-index-*")
	if err != nil {
		return
	}
	s.identity, err = os.Lstat(s.archiveVerifyScratch.directory)
	if err != nil {
		return
	}
	s.index, err = newArchiveVerifyIndexWithTree(filepath.Join(s.archiveVerifyScratch.directory, "index.bin"), max, a.tree)
	if err != nil {
		return
	}
	s.root, err = os.OpenRoot(a.e.root)
	return
}

func (s *archiveDateOrphanPageSession) openScan() (missing bool, err error) {
	s.catalog, err = s.root.Lstat("catalog")
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil || !s.catalog.IsDir() {
		return false, errors.Join(err, ErrDiskUsageCorrupt)
	}
	s.catalogV1, err = s.root.Lstat("catalog/v1")
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil || !s.catalogV1.IsDir() {
		return false, errors.Join(err, ErrDiskUsageCorrupt)
	}
	s.catalogRoot, err = s.root.OpenRoot("catalog/v1")
	if err != nil {
		return false, err
	}
	s.scan, err = s.catalogRoot.Open(".")
	return false, err
}

func orphanPageSHA(name string) (string, bool) {
	if !strings.HasPrefix(name, "page-") || !strings.HasSuffix(name, ".json") {
		return "", false
	}
	digest := strings.TrimSuffix(strings.TrimPrefix(name, "page-"), ".json")
	raw, err := hex.DecodeString(digest)
	return digest, err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == digest
}

// Describe a single actual bounded metadata object. Counters are derived from
// the whole page, never supplied by its name/checkpoint. Missing predecessors are
// allowed for detached orphan metadata; this does not authenticate native rows.
func (a *archiveDateReclaimAuthority) describeOrphanPage(ctx context.Context, digest string) (ref ArchivePageRef, missing bool, err error) {
	relative := "catalog/v1/page-" + digest + ".json"
	parent, leaf, check, err := openDateReclaimParent(ctx, a.e.root, a.directory, relative)
	if err != nil {
		return ref, false, err
	}
	if parent == nil {
		return ref, true, a.check()
	}
	defer func() {
		err = errors.Join(err, parent.Close())
		if err != nil {
			ref = ArchivePageRef{}
			missing = false
		}
	}()
	named, err := parent.Lstat(leaf)
	if errors.Is(err, os.ErrNotExist) {
		return ref, true, errors.Join(check(), a.check())
	}
	if err != nil || !named.Mode().IsRegular() || named.Size() < 1 || named.Size() > archiveCatalogPageBytes {
		return ref, false, errors.Join(err, ErrDiskUsageCorrupt)
	}
	f, err := parent.Open(leaf)
	if err != nil {
		return ref, false, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil || !os.SameFile(info, named) || info.Size() != named.Size() {
		return ref, false, errors.Join(err, ErrDiskUsageCorrupt)
	}
	if err := a.readBudget.reserve(info.Size() + 1); err != nil {
		return ref, false, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, archiveCatalogPageBytes+1))
	sum := sha256.Sum256(raw)
	if err != nil || int64(len(raw)) != info.Size() || hex.EncodeToString(sum[:]) != digest {
		return ref, false, errors.Join(err, ErrDiskUsageCorrupt)
	}
	page, err := decodeArchiveCatalogPage(raw)
	if err != nil || page.Version != 1 || !page.Previous.valid() || len(page.Files) < 1 || len(page.Files) > archiveCatalogPageFiles || page.Previous.Pages == math.MaxUint64 || page.Previous.Files > math.MaxUint64-uint64(len(page.Files)) {
		return ref, false, errors.Join(err, ErrDiskUsageCorrupt)
	}
	for _, entry := range page.Files {
		if err := a.e.validateCatalogEntry(entry); err != nil {
			return ref, false, err
		}
	}
	ref = ArchivePageRef{SHA256: digest, Bytes: len(raw), Pages: page.Previous.Pages + 1, Files: page.Previous.Files + uint64(len(page.Files))}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) || actual.Size() != info.Size() || actual.ModTime() != info.ModTime() {
		return ref, false, errors.Join(err, ErrDiskUsageCorrupt)
	}
	return ref, false, errors.Join(checkDateBuildState(f, info, filepath.Join(a.e.root, relative)), check(), a.check())
}

func (e *Exporter) ReclaimRetainedDateOrphanPagesStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DateOrphanPageReclaimLimits) (DateOrphanPageReclaimProgress, error) {
	return e.reclaimRetainedDateOrphanPagesStep(ctx, p, retainFrom, limits, nil, nil)
}

func (e *Exporter) reclaimRetainedDateOrphanPagesStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DateOrphanPageReclaimLimits, beforeUnlink func(*os.File) error, afterUnlink func() error) (out DateOrphanPageReclaimProgress, err error) {
	if limits.Entries < 1 || limits.Entries > 256 || limits.Pages < 1 || limits.Pages > 16 || limits.CandidatePages < 1 || limits.CandidatePages > 16 || limits.IndexBytes < 16*archiveVerifyNodeBytes || limits.IndexBytes > 1<<40 {
		return out, ErrDiskUsageLimit
	}
	fileLimits := DefaultDateFileReclaimLimits()
	fileLimits.ReadBytes, fileLimits.IndexIOBytes, fileLimits.MaxDuration = limits.ReadBytes, limits.IndexIOBytes, limits.MaxDuration
	err = e.withDateReclaimAuthority(ctx, p, retainFrom, fileLimits, func(ctx context.Context, a *archiveDateReclaimAuthority) (jobErr error) {
		pagesSHA, buildSHA, missing, output, guard, close, err := a.orphanPageGuards(ctx)
		defer func() { jobErr = errors.Join(jobErr, close()) }()
		if err != nil {
			return err
		}
		originalCheck := a.check
		a.check = func() error { return errors.Join(originalCheck(), guard()) }
		s := e.dateOrphanPages
		if s == nil {
			s, err = a.newOrphanPageSession(ctx, pagesSHA, buildSHA, missing, output, limits.IndexBytes)
			if err != nil {
				return err
			}
		}
		if s.index.size > limits.IndexBytes {
			return ErrDiskUsageLimit
		}
		s.tree = a.tree
		s.index.tree = a.tree
		s.index.max = limits.IndexBytes
		check := func() error { return s.check(a, pagesSHA, buildSHA, missing) }
		if err := check(); err != nil {
			return err
		}
		for s.next != (ArchivePageRef{}) && out.CandidatePagesProcessed < limits.CandidatePages {
			if err := a.readBudget.reserve(int64(s.next.Bytes)); err != nil {
				return err
			}
			page, err := e.readCatalogPage(ctx, s.next)
			if err != nil {
				return err
			}
			path := "catalog/v1/page-" + s.next.SHA256 + ".json"
			if err := s.index.insert(ctx, 0, sha256.Sum256([]byte(path)), [40]byte{}, a.indexBudget); err != nil {
				return err
			}
			s.next = page.Previous
			s.candidatePages++
			out.CandidatePagesProcessed++
			if err := check(); err != nil {
				return err
			}
		}
		out.Current, out.CandidatePagesIndexed, out.ObligationPending = a.proof.position, s.candidatePages, true
		if s.next != (ArchivePageRef{}) {
			out.Phase = "protect-candidate"
			a.keepProof = true
			return check()
		}
		if err := s.index.file.Sync(); err != nil {
			return err
		}
		complete := false
		if s.scan == nil {
			complete, err = s.openScan()
			if err != nil {
				return err
			}
			if !complete {
				if err := check(); err != nil {
					return err
				}
			}
		}
		for !complete && out.EntriesScanned < limits.Entries && out.PagesExamined < limits.Pages {
			if err := check(); err != nil {
				return err
			}
			if err := a.readBudget.reserve(8 << 10); err != nil {
				return err
			}
			names, readErr := s.scan.Readdirnames(1)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			if len(names) == 0 {
				if !errors.Is(readErr, io.EOF) {
					return io.ErrNoProgress
				}
				complete = true
				break
			}
			name := names[0]
			out.EntriesScanned++
			if s.visited == math.MaxUint64 {
				return ErrDiskUsageLimit
			}
			s.visited++
			digest, recognized := orphanPageSHA(name)
			if !recognized {
				out.EntriesUnrecognized++
				continue
			}
			relative := "catalog/v1/" + name
			current, err := a.referenceContains(ctx, 0, relative)
			if err != nil {
				return err
			}
			_, candidate, err := s.index.lookup(ctx, 0, sha256.Sum256([]byte(relative)), a.indexBudget)
			if err != nil {
				return err
			}
			out.PagesExamined++
			if current || candidate {
				out.PagesProtected++
				continue
			}
			ref, absent, err := a.describeOrphanPage(ctx, digest)
			if err != nil {
				return err
			}
			if absent {
				out.PagesMissing++
				continue
			}
			// Every physical check also observes the actual scan/index/parent ownership.
			authorityCheck := a.check
			a.check = func() error { return errors.Join(authorityCheck(), s.checkPhysical()) }
			removed, absent, size, err := a.reclaimPageWithHook(ctx, ref, beforeUnlink, afterUnlink)
			a.check = authorityCheck
			if err != nil {
				return err
			}
			if removed {
				out.PagesRemoved++
			} else if absent {
				out.PagesMissing++
			} else {
				out.PagesProtected++
			}
			if err := addInt64(&out.LogicalBytesUnlinked, size); err != nil {
				return err
			}
		}
		out.Phase, out.Visited = "scan", s.visited
		if err := check(); err != nil {
			return err
		}
		if complete {
			out.Phase = "scan-complete"
			if err := e.discardDateOrphanPages(); err != nil {
				return err
			}
		} else {
			a.keepProof = out.PagesRemoved == 0
		}
		return a.check()
	})
	if err != nil {
		out = DateOrphanPageReclaimProgress{}
	}
	return out, err
}

func (s *archiveDateOrphanPageSession) checkPhysical() error {
	if err := s.checkScratch(); err != nil {
		return err
	}
	if s.scan == nil || s.catalogRoot == nil {
		return ErrDiskUsageCorrupt
	}
	for _, path := range []string{"catalog", "catalog/v1"} {
		named, err := s.root.Lstat(path)
		expected := s.catalog
		if path == "catalog/v1" {
			expected = s.catalogV1
		}
		if err != nil || !named.IsDir() || !os.SameFile(named, expected) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
	}
	actual, err := s.scan.Stat()
	if err != nil || !os.SameFile(actual, s.catalogV1) {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	actual, err = s.catalogRoot.Stat(".")
	if err != nil || !os.SameFile(actual, s.catalogV1) {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	return nil
}
