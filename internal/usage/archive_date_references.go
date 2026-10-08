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
	"strings"
	"time"
)

const archiveDateReferenceName = ".archive-date-references-v1.json"

type DateCatalogReferenceLimits struct {
	Pages                               int
	ReadBytes, IndexIOBytes, IndexBytes int64
	MaxDuration                         time.Duration
}

func DefaultDateCatalogReferenceLimits() DateCatalogReferenceLimits {
	return DateCatalogReferenceLimits{Pages: 4, ReadBytes: 1 << 20, IndexIOBytes: 64 << 20, IndexBytes: 4 << 30, MaxDuration: 10 * time.Second}
}

// This is current-root named-path membership, never native/deletion authority.
// A ready index remains privately owned until reset, failure or Exporter Close.
type DateCatalogReferenceProgress struct {
	Phase                                   string
	PagesProcessed, PathsProcessed          int
	PagesIndexed, FilesIndexed, ReplayPages uint64
	Root                                    ArchivePosition
}

type archiveDateReferenceState struct {
	Version      int
	Root         ArchivePosition
	RootSHA      string
	Next         ArchivePageRef
	Pages, Files uint64
}

func (s archiveDateReferenceState) valid() bool {
	digest, err := hex.DecodeString(s.RootSHA)
	return s.Version == 1 && err == nil && len(digest) == 32 && strings.ToLower(s.RootSHA) == s.RootSHA && s.Root.SchemaVersion == archiveDateCatalogSchema && s.Root.CatalogVersion == 1 && !s.Root.CompletePrefix && s.Root.Retention.valid(s.Root.LastSequence) && s.Root.CatalogHead.valid() && s.Root.Files == s.Root.CatalogHead.Files && s.Next.valid() && s.Pages <= s.Root.CatalogHead.Pages && s.Files <= s.Root.Files && s.Next.Pages == s.Root.CatalogHead.Pages-s.Pages && s.Next.Files == s.Root.Files-s.Files && (s.Pages != 0 || s.Next == s.Root.CatalogHead) && (s.Next != (ArchivePageRef{}) || s.Files == s.Root.Files)
}

type archiveDateReferenceSession struct {
	archiveVerifyScratch
	stateSHA                        [32]byte
	state                           archiveDateReferenceState
	replaying                       bool
	rootIdentity, directoryIdentity os.FileInfo
}

func (e *Exporter) discardDateReferences() error {
	s := e.dateReferences
	e.dateReferences = nil
	if s == nil {
		return nil
	}
	return s.closeScratch()
}

func readDateReferenceState(ctx context.Context, path string) (state archiveDateReferenceState, raw []byte, f *os.File, info os.FileInfo, err error) {
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
	if err != nil || !os.SameFile(info, named) {
		return state, nil, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	raw, err = io.ReadAll(io.LimitReader(f, archiveDateBuildBytes+1))
	if err != nil || len(raw) > archiveDateBuildBytes || json.Unmarshal(raw, &state) != nil || !state.valid() {
		return state, raw, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(raw, canonical) {
		return state, raw, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	return state, raw, f, info, errors.Join(checkDateBuildState(f, info, path), ctx.Err())
}

func (e *Exporter) PrepareDateCatalogReferencesStep(ctx context.Context, retainFrom time.Time, limits DateCatalogReferenceLimits) (DateCatalogReferenceProgress, error) {
	return e.prepareDateCatalogReferencesStep(ctx, retainFrom, limits, nil)
}

func (e *Exporter) prepareDateCatalogReferencesStep(ctx context.Context, retainFrom time.Time, limits DateCatalogReferenceLimits, beforeSave func() error) (out DateCatalogReferenceProgress, err error) {
	if retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 || limits.Pages < 1 || limits.Pages > 16 || limits.ReadBytes < 2*archiveCatalogPageBytes+archiveDateBuildBytes || limits.ReadBytes > 8<<20 || limits.IndexIOBytes < 16<<20 || limits.IndexIOBytes > 1<<30 || limits.IndexBytes < 16*archiveVerifyNodeBytes || limits.IndexBytes > 1<<40 || limits.MaxDuration <= 0 || limits.MaxDuration > time.Minute {
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
	tree, err := newArchiveTreeBudget(ctx, e.root, e.archiveDiskCeiling)
	if err != nil {
		return out, err
	}
	defer func() {
		err = errors.Join(err, e.archiveReadError(ctx))
		if err != nil {
			out = DateCatalogReferenceProgress{}
			err = errors.Join(err, e.discardDateReferences())
		}
	}()
	directory, err := os.Lstat(e.root)
	if err != nil || !directory.IsDir() {
		return out, errors.Join(err, ErrDiskUsageCorrupt)
	}
	root, err := openArchiveManifest(e.root)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, root.file.Close()) }()
	if root.info.Size() > archiveManifestValueLimit {
		return out, ErrDiskUsageLimit
	}
	readBudget := &archiveVerifyReadBudget{remaining: limits.ReadBytes}
	if err := readBudget.reserve(root.info.Size() + archiveDateBuildBytes); err != nil {
		return out, err
	}
	position, digest, err := root.walk(ctx, nil)
	if err != nil {
		return out, err
	}
	if position.SchemaVersion != archiveDateCatalogSchema || position.Retention.RetainFromDate != retainFrom.UTC().Format("2006-01-02") {
		return out, ErrArchiveDateProof
	}
	initial := archiveDateReferenceState{Version: 1, Root: position, RootSHA: hex.EncodeToString(digest[:]), Next: position.CatalogHead}
	path := filepath.Join(e.root, archiveDateReferenceName)
	target, raw, stateFile, stateInfo, err := readDateReferenceState(ctx, path)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return out, err
	}
	if stateFile != nil {
		defer func() { err = errors.Join(err, stateFile.Close()) }()
	}
	if missing {
		target = initial
		raw, err = json.Marshal(target)
		if err != nil {
			return out, err
		}
	}
	if target.Root != position || target.RootSHA != initial.RootSHA {
		return out, ErrArchiveDateBuildStale
	}
	stateSHA := sha256.Sum256(raw)
	s := e.dateReferences
	if s != nil && (!os.SameFile(s.rootIdentity, root.info) || !os.SameFile(s.directoryIdentity, directory)) {
		return out, ErrArchiveDateBuildStale
	}
	if s == nil || s.stateSHA != stateSHA {
		if err := e.discardDateReferences(); err != nil {
			return out, err
		}
		releaseShared, err := e.reserveArchiveDisk(ctx, limits.IndexBytes)
		if err != nil {
			return out, err
		}
		scratch, err := os.MkdirTemp(e.root, ".archive-date-ref-index-*")
		if err != nil {
			return out, errors.Join(err, releaseShared())
		}
		s = &archiveDateReferenceSession{archiveVerifyScratch: archiveVerifyScratch{directory: scratch, tree: tree, releaseShared: releaseShared}, stateSHA: stateSHA, state: initial, replaying: true, rootIdentity: root.info, directoryIdentity: directory}
		e.dateReferences = s
		s.identity, err = os.Lstat(scratch)
		if err != nil {
			return out, err
		}
		s.index, err = newArchiveVerifyIndexWithTree(filepath.Join(scratch, "index.bin"), limits.IndexBytes, tree)
		if err != nil {
			return out, err
		}
	}
	if s.index.size > limits.IndexBytes {
		return out, ErrDiskUsageLimit
	}
	s.tree = tree
	s.index.tree = tree
	s.index.max = limits.IndexBytes
	if err := s.checkScratch(); err != nil {
		return out, err
	}
	indexBudget := &archiveVerifyReadBudget{remaining: limits.IndexIOBytes}
	if err := indexBudget.reserve(archiveVerifyNodeBytes); err != nil {
		return out, err
	}
	acceptReplay := func() error {
		if !s.replaying {
			return nil
		}
		if s.state.Pages > target.Pages {
			return ErrDiskUsageCorrupt
		}
		if s.state.Pages == target.Pages {
			if s.state != target {
				return ErrDiskUsageCorrupt
			}
			s.replaying = false
		}
		return nil
	}
	for out.PagesProcessed < limits.Pages && s.state.Next != (ArchivePageRef{}) {
		if err := acceptReplay(); err != nil {
			return out, err
		}
		if readBudget.remaining < int64(s.state.Next.Bytes) || indexBudget.remaining < (archiveCatalogPageFiles+1)*archiveVerifyIndexRowWork {
			break
		}
		if err := readBudget.reserve(int64(s.state.Next.Bytes)); err != nil {
			return out, err
		}
		ref := s.state.Next
		page, err := e.readCatalogPage(ctx, ref)
		if err != nil {
			return out, err
		}
		// Page and canonical data paths use separate fixed-node trees.
		pagePath := filepath.ToSlash(filepath.Join("catalog", "v1", "page-"+ref.SHA256+".json"))
		if err := s.index.insert(ctx, 0, sha256.Sum256([]byte(pagePath)), [40]byte{}, indexBudget); err != nil {
			return out, err
		}
		for _, entry := range page.Files {
			if err := validateArchiveEntryCoverage(entry, position.LastSequence, position.Retention); err != nil {
				return out, err
			}
			if err := s.index.insert(ctx, 2, sha256.Sum256([]byte(entry.Path)), [40]byte{}, indexBudget); err != nil {
				return out, err
			}
			out.PathsProcessed++
		}
		s.state.Pages++
		s.state.Files += uint64(len(page.Files))
		s.state.Next = page.Previous
		out.PagesProcessed++
	}
	if err := acceptReplay(); err != nil {
		return out, err
	}
	if !s.state.valid() {
		return out, ErrDiskUsageCorrupt
	}
	if err := s.index.file.Sync(); err != nil {
		return out, err
	}
	if beforeSave != nil {
		if err := beforeSave(); err != nil {
			return out, err
		}
	}
	check := func() error {
		named, err := os.Lstat(e.root)
		if err != nil || !os.SameFile(named, directory) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		return errors.Join(root.check(), s.checkScratch(), e.archiveReadError(ctx))
	}
	if err := check(); err != nil {
		return out, err
	}
	out.PagesIndexed, out.FilesIndexed, out.ReplayPages = s.state.Pages, s.state.Files, s.state.Pages
	if s.replaying {
		out.Phase = "revalidate"
		return out, nil
	}
	newRaw, err := json.Marshal(s.state)
	if err != nil {
		return out, err
	}
	if missing || !bytes.Equal(newRaw, raw) {
		err = e.saveDateBuildState(ctx, path, newRaw, stateFile, stateInfo, missing, &archiveWork{remaining: archiveDateBuildBytes}, nil)
	} else {
		err = syncDateBuildState(ctx, stateFile, stateInfo, path)
	}
	if err != nil {
		return out, err
	}
	if err := check(); err != nil {
		return out, err
	}
	s.stateSHA = sha256.Sum256(newRaw)
	out.Phase = "index"
	if s.state.Next == (ArchivePageRef{}) {
		out.Phase = "ready"
		out.Root = position
	}
	return out, nil
}

// Caller holds writer/reclamation lifetime and supplies a current physical root.
// Membership alone grants no native authority, file identity or unlink right.
func (e *Exporter) dateReferenceContainsHeld(ctx context.Context, root *archiveManifestInput, tree int, path string, budget *archiveVerifyReadBudget) (found bool, err error) {
	s := e.dateReferences
	if s == nil || s.replaying || s.state.Next != (ArchivePageRef{}) || root == nil || root.file == nil || budget == nil || tree != 0 && tree != 2 {
		return false, ErrArchiveDateProof
	}
	if path == "" || filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))) != path || strings.Contains(path, "\\") || path == ".." || strings.HasPrefix(path, "../") {
		return false, ErrDiskUsageCorrupt
	}
	if err := e.archiveReadError(ctx); err != nil {
		return false, err
	}
	position, digest, err := root.walk(ctx, nil)
	if err != nil || position != s.state.Root || hex.EncodeToString(digest[:]) != s.state.RootSHA || !os.SameFile(root.info, s.rootIdentity) {
		return false, errors.Join(err, ErrArchiveDateBuildStale)
	}
	named, err := os.Lstat(e.root)
	if err != nil || !os.SameFile(named, s.directoryIdentity) {
		return false, errors.Join(err, ErrArchiveDateBuildStale)
	}
	_, raw, file, info, err := readDateReferenceState(ctx, filepath.Join(e.root, archiveDateReferenceName))
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, file.Close(), e.archiveReadError(ctx))
		if err != nil {
			found = false
		}
	}()
	if sha256.Sum256(raw) != s.stateSHA {
		return false, ErrArchiveDateBuildStale
	}
	if err := s.checkScratch(); err != nil {
		return false, err
	}
	_, found, err = s.index.lookup(ctx, tree, sha256.Sum256([]byte(path)), budget)
	if err != nil {
		return false, err
	}
	return found, errors.Join(root.check(), checkDateBuildState(file, info, filepath.Join(e.root, archiveDateReferenceName)), s.checkScratch())
}

func (e *Exporter) ResetDateCatalogReferences(ctx context.Context) error {
	return e.resetArchiveDateCheckpoint(ctx, archiveDateReferenceName, false)
}
