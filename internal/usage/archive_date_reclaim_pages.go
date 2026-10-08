package usage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

const archiveDatePagesName = ".archive-date-pages-v1.json"

// This witness is minted only after the data job and its actual Closes succeed.
// It has no historical array, page, index, key or descriptor ownership.
type archiveDateDataProof struct {
	owner                  [16]byte
	head                   ledger.Watermark
	window                 DiskWindowState
	current                ArchivePosition
	currentSHA, bindingSHA [32]byte
	old                    ArchivePageRef
	prefixSHA              string
	root, directory        os.FileInfo
}

type DatePageReclaimLimits struct {
	Pages                   int
	ReadBytes, IndexIOBytes int64
	MaxDuration             time.Duration
}

func DefaultDatePageReclaimLimits() DatePageReclaimLimits {
	return DatePageReclaimLimits{Pages: 4, ReadBytes: 16 << 20, IndexIOBytes: 16 << 20, MaxDuration: 10 * time.Second}
}

// Ready covers the original old-page chain only. The policy receipt, obligation,
// orphans, native facts and physical-space fees are not reclaimed here.
type DatePageReclaimProgress struct {
	Phase                                                      string
	PagesProcessed, PagesRemoved, PagesMissing, PagesProtected int
	Visited                                                    uint64
	LogicalBytesUnlinked                                       int64
	ObligationPending                                          bool
	Current                                                    ArchivePosition
}

// Authenticated irreversible transition evidence. Ordinary saved data Ready is
// never signed. A real data-completion witness seeds Next at the original head;
// each intent is signed after reading/closing that authentic page and BEFORE its
// unlink, preserving the verified next hop when the current page is gone.
type archiveDatePagesState struct {
	Version                   int
	BindingSHA, DataPrefixSHA string
	Old, Next                 ArchivePageRef
	DataEntries, Processed    uint64
	Pending                   bool
	Previous                  ArchivePageRef
	MAC                       string
}

func (s archiveDatePagesState) valid() bool {
	if s.Version != 1 || !s.Old.valid() || !s.Next.valid() || !s.Previous.valid() || s.DataEntries != s.Old.Files || s.Processed > s.Old.Pages || s.Next.Pages != s.Old.Pages-s.Processed || s.Next.Files > s.Old.Files {
		return false
	}
	if s.Pending {
		if s.Next == (ArchivePageRef{}) || s.Previous.Pages != s.Next.Pages-1 || s.Previous.Files >= s.Next.Files || s.Next.Files-s.Previous.Files > archiveCatalogPageFiles {
			return false
		}
	} else if s.Previous != (ArchivePageRef{}) {
		return false
	}
	for _, value := range []string{s.BindingSHA, s.DataPrefixSHA, s.MAC} {
		raw, err := hex.DecodeString(value)
		if err != nil || len(raw) != sha256.Size || hex.EncodeToString(raw) != value {
			return false
		}
	}
	return true
}

func (e *Exporter) signDatePages(s *archiveDatePagesState) error {
	if !e.datePolicyReady {
		return ErrArchiveDateProof
	}
	s.MAC = ""
	raw, err := json.Marshal(struct {
		Domain string
		State  archiveDatePagesState
	}{"halro/usage/date-pages/transition/v1", *s})
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, e.datePolicyKey[:])
	_, _ = mac.Write(raw)
	s.MAC = hex.EncodeToString(mac.Sum(nil))
	if !s.valid() {
		return ErrDiskUsageCorrupt
	}
	return nil
}

func (e *Exporter) verifyDatePages(s archiveDatePagesState) error {
	if !s.valid() || !e.datePolicyReady {
		return ErrArchiveDateProof
	}
	got, _ := hex.DecodeString(s.MAC)
	if err := e.signDatePages(&s); err != nil {
		return err
	}
	want, _ := hex.DecodeString(s.MAC)
	if !hmac.Equal(got, want) {
		return ErrArchiveDateProof
	}
	return nil
}

func readDatePagesState(ctx context.Context, path string) (s archiveDatePagesState, raw []byte, f *os.File, info os.FileInfo, err error) {
	named, err := os.Lstat(path)
	if err != nil {
		return s, nil, nil, nil, err
	}
	if !named.Mode().IsRegular() || named.Size() > archiveDateBuildBytes {
		return s, nil, nil, nil, ErrDiskUsageCorrupt
	}
	f, err = os.Open(path)
	if err != nil {
		return s, nil, nil, nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.Close())
			f = nil
		}
	}()
	info, err = f.Stat()
	if err != nil || !os.SameFile(named, info) {
		return s, nil, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	raw, err = io.ReadAll(io.LimitReader(f, archiveDateBuildBytes+1))
	if err != nil || len(raw) > archiveDateBuildBytes || json.Unmarshal(raw, &s) != nil {
		return s, raw, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	canonical, err := json.Marshal(s)
	if err != nil || !bytes.Equal(canonical, raw) {
		return s, raw, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	return s, raw, f, info, errors.Join(checkDateBuildState(f, info, path), ctx.Err())
}

func (e *Exporter) ReclaimRetainedDatePagesStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DatePageReclaimLimits) (DatePageReclaimProgress, error) {
	return e.reclaimRetainedDatePagesStep(ctx, p, retainFrom, limits, nil, nil)
}

// Presence chooses bootstrap versus authenticated resume only. It confers no
// saved-state or deletion authority; the step must verify the record MAC/scope.
func (e *Exporter) HasDatePageReclamation(ctx context.Context) (present bool, err error) {
	finish, err := e.beginArchiveRead(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, finish())
		if err != nil {
			present = false
		}
	}()
	info, err := os.Lstat(filepath.Join(e.root, archiveDatePagesName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > archiveDateBuildBytes {
		return false, ErrDiskUsageCorrupt
	}
	return true, nil
}

// Select signed progress for the freshly compared current policy, rather than
// treating an older completed policy's page file as progress for its successor.
func (e *Exporter) HasCurrentDatePageReclamation(ctx context.Context, p *DiskProjection, retainFrom time.Time) (present bool, err error) {
	err = e.withDateReclaimAuthority(ctx, p, retainFrom, DefaultDateFileReclaimLimits(), func(ctx context.Context, a *archiveDateReclaimAuthority) (jobErr error) {
		state, raw, file, info, err := readDatePagesState(ctx, filepath.Join(e.root, archiveDatePagesName))
		if errors.Is(err, os.ErrNotExist) {
			a.keepProof = true
			return nil
		}
		if err != nil {
			return err
		}
		defer func() { jobErr = errors.Join(jobErr, file.Close()) }()
		if err := e.verifyDatePages(state); err != nil {
			return err
		}
		old, _, err := a.state.positions()
		if err != nil {
			return err
		}
		binding, err := dateActivationBinding(a.state)
		if err != nil {
			return err
		}
		present = state.BindingSHA == hex.EncodeToString(binding[:]) && state.Old == old.CatalogHead
		if !present {
			prior, err := e.readDateCompletion(ctx, old, a.proof.head)
			if err != nil {
				return err
			}
			defer func() { jobErr = errors.Join(jobErr, prior.file.Close()) }()
			if err := e.verifyDateSuccession(ctx, a.state, prior.state, prior.raw, a.proof.head); err != nil {
				return err
			}
			if state != prior.state.OriginalChain {
				return ErrArchiveDateProof
			}
			if err := prior.check(ctx); err != nil {
				return err
			}
		}
		actual, err := io.ReadAll(io.NewSectionReader(file, 0, archiveDateBuildBytes+1))
		if err != nil || !bytes.Equal(actual, raw) {
			return errors.Join(err, ErrArchiveDateProof)
		}
		if err := checkDateBuildState(file, info, filepath.Join(e.root, archiveDatePagesName)); err != nil {
			return err
		}
		a.keepProof = true
		return a.check()
	})
	if err != nil {
		present = false
	}
	return present, err
}

func (e *Exporter) reclaimRetainedDatePagesStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DatePageReclaimLimits, afterIntent, afterUnlink func() error) (out DatePageReclaimProgress, err error) {
	if limits.Pages < 1 || limits.Pages > 16 {
		return out, ErrDiskUsageLimit
	}
	fileLimits := DefaultDateFileReclaimLimits()
	fileLimits.ReadBytes, fileLimits.IndexIOBytes, fileLimits.MaxDuration = limits.ReadBytes, limits.IndexIOBytes, limits.MaxDuration
	err = e.withDateReclaimAuthority(ctx, p, retainFrom, fileLimits, func(ctx context.Context, a *archiveDateReclaimAuthority) (jobErr error) {
		witness := e.dateDataProof
		e.dateDataProof = nil // One admitted bootstrap, including failed publication.
		old, _, err := a.state.positions()
		if err != nil {
			return err
		}
		binding, err := dateActivationBinding(a.state)
		if err != nil {
			return err
		}
		bindingSHA := hex.EncodeToString(binding[:])
		path := filepath.Join(e.root, archiveDatePagesName)
		if err := a.readBudget.reserve(archiveDateBuildBytes); err != nil {
			return err
		}
		state, stateRaw, file, info, err := readDatePagesState(ctx, path)
		missing := errors.Is(err, os.ErrNotExist)
		if err != nil && !missing {
			return err
		}
		defer func() {
			if file != nil {
				jobErr = errors.Join(jobErr, file.Close())
			}
		}()
		bootstrap := missing
		var previous *archiveDateCompletionInput
		if !missing {
			if err := e.verifyDatePages(state); err != nil {
				return err
			}
			if state.BindingSHA != bindingSHA || state.Old != old.CatalogHead {
				previous, err = e.readDateCompletion(ctx, old, a.proof.head)
				if err != nil {
					return err
				}
				defer func() { jobErr = errors.Join(jobErr, previous.file.Close()) }()
				if err := e.verifyDateSuccession(ctx, a.state, previous.state, previous.raw, a.proof.head); err != nil {
					return err
				}
				if state != previous.state.OriginalChain {
					return ErrArchiveDateProof
				}
				actual, err := io.ReadAll(io.NewSectionReader(file, 0, archiveDateBuildBytes+1))
				if err != nil || !bytes.Equal(actual, stateRaw) {
					return errors.Join(err, ErrArchiveDateProof)
				}
				bootstrap = true
			}
		}
		if bootstrap {
			if witness == nil || witness.owner != p.generation || witness.head != a.proof.head || witness.window != a.proof.window || witness.current != a.proof.position || witness.currentSHA != a.proof.digest || witness.bindingSHA != binding || witness.old != old.CatalogHead || !os.SameFile(witness.root, a.proof.identity) || !os.SameFile(witness.directory, a.directory) {
				return ErrArchiveDateProof
			}
			state = archiveDatePagesState{Version: 1, BindingSHA: bindingSHA, DataPrefixSHA: witness.prefixSHA, Old: old.CatalogHead, Next: old.CatalogHead, DataEntries: old.Files}
		}
		if state.BindingSHA != bindingSHA || state.Old != old.CatalogHead {
			return ErrArchiveDateProof
		}
		// Every new transition has one closed temporary and one current state FD.
		// Reopen the committed record before using its next hop; never retain an
		// array of replaced checkpoint FDs across the finite loop.
		save := func() error {
			if previous != nil {
				if err := previous.check(ctx); err != nil {
					return err
				}
			}
			if err := a.check(); err != nil {
				return err
			}
			if err := e.signDatePages(&state); err != nil {
				return err
			}
			raw, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if err := a.advance(); err != nil {
				return err
			}
			if err := e.saveDateBuildState(ctx, path, raw, file, info, missing, &archiveWork{remaining: archiveDateBuildBytes}, nil); err != nil {
				return err
			}
			if file != nil {
				closeErr := file.Close()
				file = nil
				if closeErr != nil {
					return closeErr
				}
			}
			if err := a.readBudget.reserve(archiveDateBuildBytes); err != nil {
				return err
			}
			actual, actualRaw, nextFile, nextInfo, err := readDatePagesState(ctx, path)
			if err != nil {
				return err
			}
			file, info, missing = nextFile, nextInfo, false
			if actual != state || !bytes.Equal(actualRaw, raw) {
				return ErrArchiveDateProof
			}
			return a.check()
		}
		if bootstrap {
			if err := save(); err != nil {
				return err
			}
		}
		for state.Next != (ArchivePageRef{}) && out.PagesProcessed < limits.Pages {
			if err := a.check(); err != nil {
				return err
			}
			if err := checkDateBuildState(file, info, path); err != nil {
				return err
			}
			if !state.Pending {
				if err := a.readBudget.reserve(int64(state.Next.Bytes)); err != nil {
					return err
				}
				page, err := e.readCatalogPage(ctx, state.Next)
				if err != nil {
					return err
				}
				state.Pending, state.Previous = true, page.Previous
				if err := save(); err != nil {
					return err
				}
				if afterIntent != nil {
					if err := afterIntent(); err != nil {
						return err
					}
				}
			}
			removed, absent, size, err := a.reclaimPage(ctx, state.Next, afterUnlink)
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
			state.Next, state.Previous, state.Pending = state.Previous, ArchivePageRef{}, false
			state.Processed++
			if err := save(); err != nil {
				return err
			}
			out.PagesProcessed++
		}
		out.Phase, out.Visited, out.ObligationPending = "pages", state.Processed, true
		if state.Next == (ArchivePageRef{}) {
			out.Phase, out.Current = "ready", a.proof.position
		}
		return a.check()
	})
	if err != nil {
		out = DatePageReclaimProgress{}
	}
	return out, err
}

func (a *archiveDateReclaimAuthority) reclaimPage(ctx context.Context, ref ArchivePageRef, afterUnlink func() error) (removed, absent bool, bytesUnlinked int64, err error) {
	return a.reclaimPageWithHook(ctx, ref, nil, afterUnlink)
}

func (a *archiveDateReclaimAuthority) reclaimPageWithHook(ctx context.Context, ref ArchivePageRef, beforeUnlink func(*os.File) error, afterUnlink func() error) (removed, absent bool, bytesUnlinked int64, err error) {
	defer func() {
		if err != nil {
			removed, absent, bytesUnlinked = false, false, 0
		}
	}()
	relative := filepath.ToSlash(filepath.Join("catalog", "v1", "page-"+ref.SHA256+".json"))
	found, err := a.referenceContains(ctx, 0, relative)
	if err != nil {
		return false, false, 0, err
	}
	parent, leaf, parentsCheck, err := openDateReclaimParent(ctx, a.e.root, a.directory, relative)
	if err != nil {
		return false, false, 0, err
	}
	if parent == nil {
		if found {
			return false, false, 0, ErrDiskUsageCorrupt
		}
		return false, true, 0, errors.Join(a.check(), syncDateRootDirectory(a.e.root, a.directory))
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	named, err := parent.Lstat(leaf)
	if errors.Is(err, os.ErrNotExist) {
		if found {
			return false, false, 0, ErrDiskUsageCorrupt
		}
		return false, true, 0, errors.Join(parentsCheck(), a.check(), syncDateParent(parent))
	}
	if err != nil || !named.Mode().IsRegular() || named.Size() != int64(ref.Bytes) {
		return false, false, 0, errors.Join(err, ErrDiskUsageCorrupt)
	}
	file, err := parent.Open(leaf)
	if err != nil {
		return false, false, 0, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil || !os.SameFile(named, info) {
		return false, false, 0, errors.Join(err, ErrDiskUsageCorrupt)
	}
	checkFile := func() error {
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
		return errors.Join(parentsCheck(), a.check())
	}
	if err := hashDateReclaimFile(ctx, file, info.Size(), ref.SHA256, nil, a.readBudget); err != nil {
		return false, false, 0, err
	}
	if err := checkFile(); err != nil {
		return false, false, 0, err
	}
	if found {
		return false, false, 0, nil
	}
	if beforeUnlink != nil {
		if err := beforeUnlink(file); err != nil {
			return false, false, 0, err
		}
	}
	if err := hashDateReclaimFile(ctx, file, info.Size(), ref.SHA256, nil, a.readBudget); err != nil {
		return false, false, 0, err
	}
	found, err = a.referenceContains(ctx, 0, relative)
	if err != nil {
		return false, false, 0, err
	}
	if found {
		return false, false, 0, ErrArchiveDateReferenced
	}
	if err := checkFile(); err != nil {
		return false, false, 0, err
	}
	if err := a.advance(); err != nil {
		return false, false, 0, err
	}
	if err := parent.Remove(leaf); err != nil {
		return false, false, 0, err
	}
	if afterUnlink != nil {
		if err := afterUnlink(); err != nil {
			return false, false, 0, err
		}
	}
	if err := syncDateParent(parent); err != nil {
		return false, false, 0, err
	}
	if _, err := parent.Lstat(leaf); !errors.Is(err, os.ErrNotExist) {
		return false, false, 0, errors.Join(err, ErrDiskUsageCorrupt)
	}
	return true, false, info.Size(), errors.Join(parentsCheck(), a.check())
}
