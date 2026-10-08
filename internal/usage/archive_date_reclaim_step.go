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

	"github.com/akz142857/Halro/internal/ledger"
)

const archiveDateReclaimName = ".archive-date-reclaim-v1.json"

type DateReclaimStepLimits struct {
	Entries, Pages, Files int
	File                  DateFileReclaimLimits // Rows/read/native/index bytes are aggregate per call.
}

func DefaultDateReclaimStepLimits() DateReclaimStepLimits {
	l := DefaultDateFileReclaimLimits()
	l.IndexIOBytes = 64 << 20
	return DateReclaimStepLimits{Entries: 64, Pages: 4, Files: 4, File: l}
}

// Ready means the old data-descriptor chain has been processed, not that pages,
// orphans, native facts, the obligation or physical-space fees have been reclaimed.
type DateReclaimProgress struct {
	Phase                                                                 string
	EntriesProcessed, PagesRead, FilesRemoved, FilesMissing, RowsVerified int
	Visited, Eligible, ReplayEntries                                      uint64
	LogicalBytesUnlinked                                                  int64
	ObligationPending                                                     bool
	Current                                                               ArchivePosition
}

type archiveDateReclaimState struct {
	Version                  int
	Old, Current             ArchivePosition
	CurrentSHA, BindingSHA   string
	Head                     ledger.Watermark
	Window                   DiskWindowState
	Next                     ArchivePageRef
	Offset                   int
	Pages, Visited, Eligible uint64
	PrefixSHA                string
}

func (s archiveDateReclaimState) valid() bool {
	if s.Version != 1 || s.Old.CatalogVersion != 1 || s.Old.Files != s.Old.CatalogHead.Files || !s.Old.CatalogHead.valid() || s.Current.SchemaVersion != archiveDateCatalogSchema || !s.Current.Retention.valid(s.Current.LastSequence) || s.Current.CompletePrefix || !s.Current.CatalogHead.valid() || !s.Next.valid() || s.Offset < 0 || s.Offset >= archiveCatalogPageFiles || s.Pages > s.Old.CatalogHead.Pages || s.Next.Pages != s.Old.CatalogHead.Pages-s.Pages || s.Next.Files > s.Old.Files || uint64(s.Offset) > s.Next.Files || s.Visited != s.Old.Files-s.Next.Files+uint64(s.Offset) || s.Eligible > s.Visited || s.Next == (ArchivePageRef{}) && s.Offset != 0 {
		return false
	}
	for _, h := range []string{s.CurrentSHA, s.BindingSHA, s.PrefixSHA} {
		d, err := hex.DecodeString(h)
		if err != nil || len(d) != sha256.Size || hex.EncodeToString(d) != h {
			return false
		}
	}
	return true
}

// Only fixed-size, process-owned progress is cached. There are no retained old
// pages, descriptors, deletion lists, index objects or file handles here.
type archiveDateReclaimReplay struct {
	state           archiveDateReclaimState
	stateSHA        [32]byte
	owner           [16]byte
	root, directory os.FileInfo
	replaying       bool
}

func readDateReclaimState(ctx context.Context, path string) (s archiveDateReclaimState, raw []byte, f *os.File, info os.FileInfo, err error) {
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
	if err != nil || len(raw) > archiveDateBuildBytes || json.Unmarshal(raw, &s) != nil || !s.valid() {
		return s, raw, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	canonical, err := json.Marshal(s)
	if err != nil || !bytes.Equal(canonical, raw) {
		return s, raw, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	return s, raw, f, info, errors.Join(checkDateBuildState(f, info, path), ctx.Err())
}

func (e *Exporter) ReclaimRetainedDateStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DateReclaimStepLimits) (DateReclaimProgress, error) {
	return e.reclaimRetainedDateStep(ctx, p, retainFrom, limits, nil, nil)
}

func (e *Exporter) reclaimRetainedDateStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DateReclaimStepLimits, beforeSave, afterRename func() error) (out DateReclaimProgress, err error) {
	if limits.Entries < 1 || limits.Entries > 128 || limits.Pages < 1 || limits.Pages > 16 || limits.Files < 1 || limits.Files > 16 {
		return out, ErrDiskUsageLimit
	}
	err = e.withDateReclaimAuthority(ctx, p, retainFrom, limits.File, func(ctx context.Context, a *archiveDateReclaimAuthority) (stepErr error) {
		e.dateDataProof = nil
		// Cleanup and cursor-cache invalidation run while holding the writer gate.
		defer func() {
			if stepErr != nil {
				e.dateReclaimReplay = nil
			}
		}()
		old, _, err := a.state.positions()
		if err != nil {
			return err
		}
		binding, err := dateActivationBinding(a.state)
		if err != nil {
			return err
		}
		initial := archiveDateReclaimState{Version: 1, Old: old, Current: a.proof.position, CurrentSHA: hex.EncodeToString(a.proof.digest[:]), BindingSHA: hex.EncodeToString(binding[:]), Head: a.proof.head, Window: a.proof.window, Next: old.CatalogHead, PrefixSHA: hex.EncodeToString(make([]byte, sha256.Size))}
		if err := a.readBudget.reserve(archiveDateBuildBytes); err != nil {
			return err
		}
		path := filepath.Join(e.root, archiveDateReclaimName)
		target, raw, file, info, err := readDateReclaimState(ctx, path)
		missing := errors.Is(err, os.ErrNotExist)
		if err != nil && !missing {
			return err
		}
		if file != nil {
			defer func() {
				stepErr = errors.Join(stepErr, file.Close())
				if stepErr != nil {
					e.dateReclaimReplay = nil
				}
			}()
		}
		if missing {
			target = initial
			raw, err = json.Marshal(target)
			if err != nil {
				return err
			}
		}
		if target.Old != initial.Old || target.Current != initial.Current || target.CurrentSHA != initial.CurrentSHA || target.BindingSHA != initial.BindingSHA || target.Head != initial.Head || target.Window != initial.Window {
			return ErrArchiveDateBuildStale
		}
		digest := sha256.Sum256(raw)
		s := e.dateReclaimReplay
		if s == nil || s.stateSHA != digest || s.owner != p.generation || !os.SameFile(s.root, a.root.info) || !os.SameFile(s.directory, a.directory) {
			s = &archiveDateReclaimReplay{state: initial, stateSHA: digest, owner: p.generation, root: a.root.info, directory: a.directory, replaying: true}
			e.dateReclaimReplay = s
		}
		acceptReplay := func() error {
			if !s.replaying {
				return nil
			}
			if s.state.Visited > target.Visited {
				return ErrDiskUsageCorrupt
			}
			if s.state.Visited == target.Visited {
				if s.state != target {
					return ErrDiskUsageCorrupt
				}
				s.replaying = false
			}
			return nil
		}
		for out.EntriesProcessed < limits.Entries && s.state.Next != (ArchivePageRef{}) {
			if err := acceptReplay(); err != nil {
				return err
			}
			if out.PagesRead == limits.Pages {
				break
			}
			// Reserve the immutable input page before decoding any of it.
			if a.readBudget.remaining < int64(s.state.Next.Bytes) {
				break
			}
			if err := a.readBudget.reserve(int64(s.state.Next.Bytes)); err != nil {
				return err
			}
			page, err := e.readCatalogPage(ctx, s.state.Next)
			if err != nil {
				return err
			}
			out.PagesRead++
			if s.state.Offset >= len(page.Files) {
				return ErrDiskUsageCorrupt
			}
			for s.state.Offset < len(page.Files) && out.EntriesProcessed < limits.Entries {
				if err := acceptReplay(); err != nil {
					return err
				}
				if err := a.check(); err != nil {
					return err
				}
				entry := page.Files[s.state.Offset]
				if err := validateArchiveEntryCoverage(entry, old.LastSequence, old.Retention); err != nil {
					return err
				}
				if err := validateArchivePartitionDate(entry); err != nil {
					return err
				}
				if len(entry.Path) > 4096 {
					return ErrDiskUsageLimit
				}
				eligible := entry.Date < initial.Current.Retention.RetainFromDate && entry.MaxSequence <= initial.Current.Retention.PruneThroughSequence
				if eligible {
					// One classification plus two destructive-boundary lookups.
					if a.indexBudget.remaining < 3*257*archiveVerifyNodeBytes || a.readBudget.remaining < 3*(a.root.info.Size()+archiveDateBuildBytes) {
						goto finished
					}
					found, err := a.referenced(ctx, entry.Path)
					if err != nil {
						return err
					}
					eligible = !found
				}
				if eligible {
					if s.replaying {
						// A claimed processed prefix must not skip a remaining file.
						if err := a.requireMissing(ctx, entry.Path); err != nil {
							return err
						}
					} else {
						if out.FilesRemoved+out.FilesMissing == limits.Files {
							goto finished
						}
						fileLimits := limits.File
						fileLimits.FileRows -= out.RowsVerified
						if entry.Records > int64(fileLimits.FileRows) {
							missing, err := a.targetMissing(ctx, entry.Path)
							if err != nil {
								return err
							}
							if !missing {
								if out.RowsVerified == 0 {
									return ErrDiskUsageLimit
								}
								goto finished
							}
							// No decoder work for an absent name. reclaimFile repeats
							// ownership/absence and real durability checks before success.
						}
						reclaimed, err := a.reclaimFile(ctx, entry, fileLimits, nil, nil)
						if err != nil {
							return err
						}
						if reclaimed.Removed {
							out.FilesRemoved++
						}
						if reclaimed.AlreadyMissing {
							out.FilesMissing++
						}
						out.RowsVerified += reclaimed.RowsVerified
						if err := addInt64(&out.LogicalBytesUnlinked, reclaimed.LogicalBytesUnlinked); err != nil {
							return err
						}
					}
				}
				encoded, err := json.Marshal(struct {
					Entry    ManifestFile
					Eligible bool
				}{entry, eligible})
				if err != nil {
					return err
				}
				prefix, err := hex.DecodeString(s.state.PrefixSHA)
				if err != nil {
					return err
				}
				h := sha256.New()
				h.Write(prefix)
				h.Write(encoded)
				s.state.PrefixSHA = hex.EncodeToString(h.Sum(nil))
				s.state.Visited++
				if eligible {
					s.state.Eligible++
				}
				s.state.Offset++
				out.EntriesProcessed++
				if s.replaying {
					out.ReplayEntries++
				}
				if s.state.Offset == len(page.Files) {
					s.state.Next = page.Previous
					s.state.Offset = 0
					s.state.Pages++
					break
				}
			}
		}
	finished:
		if err := acceptReplay(); err != nil {
			return err
		}
		// If replay reaches its saved boundary exactly at a work cap, hand the
		// same read-only owner to another finite chunk. Returning ordinary data
		// progress here would let fresh offline calls repeat that prefix forever.
		if s.replaying || out.ReplayEntries > 0 && out.ReplayEntries == uint64(out.EntriesProcessed) && s.state.Next != (ArchivePageRef{}) {
			out.Phase = "revalidating"
			out.ObligationPending = true
			a.keepProof = true // No unlink/checkpoint write occurred; final Close/checks still govern reuse.
			return a.check()
		}
		if !s.state.valid() {
			return ErrDiskUsageCorrupt
		}
		if beforeSave != nil {
			if err := beforeSave(); err != nil {
				return err
			}
		}
		if err := a.check(); err != nil {
			return err
		}
		newRaw, err := json.Marshal(s.state)
		if err != nil {
			return err
		}
		if missing || !bytes.Equal(newRaw, raw) {
			err = e.saveDateBuildState(ctx, path, newRaw, file, info, missing, &archiveWork{remaining: archiveDateBuildBytes}, afterRename)
		} else {
			err = syncDateBuildState(ctx, file, info, path)
		}
		if err != nil {
			return err
		}
		if err := a.check(); err != nil {
			return err
		}
		s.stateSHA = sha256.Sum256(newRaw)
		out.Phase, out.Visited, out.Eligible, out.ObligationPending = "data", s.state.Visited, s.state.Eligible, true
		if s.state.Next == (ArchivePageRef{}) {
			out.Phase, out.Current = "ready", initial.Current
			a.dataComplete = &archiveDateDataProof{owner: p.generation, head: a.proof.head, window: a.proof.window, current: a.proof.position, currentSHA: a.proof.digest, bindingSHA: binding, old: old.CatalogHead, prefixSHA: s.state.PrefixSHA, root: a.proof.identity, directory: a.directory}
		}
		return nil
	})
	if err != nil {
		out = DateReclaimProgress{}
	}
	return out, err
}

// A missing path is only prefix revalidation. Any existing owner (including a
// symlink/replacement) refuses a claimed completed position, without unlinking.
func (a *archiveDateReclaimAuthority) requireMissing(ctx context.Context, relative string) (err error) {
	missing, err := a.targetMissing(ctx, relative)
	if err != nil {
		return err
	}
	if !missing {
		return ErrDiskUsageCorrupt
	}
	return nil
}

func (a *archiveDateReclaimAuthority) targetMissing(ctx context.Context, relative string) (missing bool, err error) {
	parent, leaf, check, err := openDateReclaimParent(ctx, a.e.root, a.directory, relative)
	if err != nil {
		return false, err
	}
	if parent == nil {
		return true, a.check()
	}
	defer func() {
		err = errors.Join(err, parent.Close())
		if err != nil {
			missing = false
		}
	}()
	_, statErr := parent.Lstat(leaf)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	return errors.Is(statErr, os.ErrNotExist), errors.Join(check(), a.check())
}

func (e *Exporter) ResetRetainedDateReclamation(ctx context.Context) error {
	return e.resetArchiveDateCheckpoint(ctx, archiveDateReclaimName, false)
}
