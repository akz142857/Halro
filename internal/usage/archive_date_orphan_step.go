package usage

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

type DateOrphanReclaimLimits struct {
	Entries, Files, InspectRows int
	File                        DateFileReclaimLimits
}

func DefaultDateOrphanReclaimLimits() DateOrphanReclaimLimits {
	return DateOrphanReclaimLimits{Entries: 128, Files: 4, InspectRows: 8192, File: DefaultDateFileReclaimLimits()}
}

// Scan-complete means this live directory stream reached EOF, not a proof of a
// currently orphan-free filesystem. Unknown objects and outside-policy rows are
// preserved. It grants no obligation completion, next-policy or disk-fee authority.
type DateOrphanReclaimProgress struct {
	Phase                                                 string
	EntriesScanned, EntriesIgnored                        int
	FilesExamined, FilesRemoved, FilesMissing             int
	FilesProtected, FilesOutsidePolicy, FilesUnrecognized int
	RowsInspected, RowsVerified                           int
	LogicalBytesUnlinked                                  int64
	Visited                                               uint64
	ObligationPending                                     bool
	Current                                               ArchivePosition
}

// At most two confined roots and two directory stream FDs. No persisted offset,
// directory/name array, old-page list or saved Ready is accepted on reopen.
type archiveDateOrphanSession struct {
	owner                  [16]byte
	head                   ledger.Watermark
	window                 DiskWindowState
	position               ArchivePosition
	digest, binding        [32]byte
	directory, partitionID os.FileInfo
	root, partition        *os.Root
	scan, partitionScan    *os.File
	partitionName, pending string
	visited                uint64
}

func (e *Exporter) discardDateOrphans() error {
	s := e.dateOrphans
	e.dateOrphans = nil
	if s == nil {
		return nil
	}
	return errors.Join(s.closePartition(), s.closeRoot())
}

func (s *archiveDateOrphanSession) closePartition() error {
	var err error
	if s.partitionScan != nil {
		err = s.partitionScan.Close()
		s.partitionScan = nil
	}
	if s.partition != nil {
		err = errors.Join(err, s.partition.Close())
		s.partition = nil
	}
	s.partitionID, s.partitionName = nil, ""
	return err
}

func (s *archiveDateOrphanSession) closeRoot() error {
	var err error
	if s.scan != nil {
		err = s.scan.Close()
		s.scan = nil
	}
	if s.root != nil {
		err = errors.Join(err, s.root.Close())
		s.root = nil
	}
	s.pending = ""
	return err
}

// Reset only withdraws the live scan and its process proof, joining its actual
// directory FDs. It removes no archive/checkpoint/native files and grants no auth.
func (e *Exporter) ResetRetainedDateOrphans(ctx context.Context) error {
	return e.withArchiveReclamation(ctx, func(func() error) error {
		e.dateProof = nil
		return e.discardDateOrphans()
	})
}

func (s *archiveDateOrphanSession) check(a *archiveDateReclaimAuthority) error {
	if s.owner != a.p.generation || s.head != a.proof.head || s.window != a.proof.window || s.position != a.proof.position || s.digest != a.proof.digest || !os.SameFile(s.directory, a.directory) {
		return ErrArchiveDateBuildStale
	}
	binding, err := dateActivationBinding(a.state)
	if err != nil || binding != s.binding {
		return errors.Join(err, ErrArchiveDateBuildStale)
	}
	info, err := s.root.Stat(".")
	if err != nil || !os.SameFile(s.directory, info) {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	info, err = s.scan.Stat()
	if err != nil || !os.SameFile(s.directory, info) {
		return errors.Join(err, ErrDiskUsageCorrupt)
	}
	if s.partition != nil {
		named, err := s.root.Lstat(s.partitionName)
		if err != nil || !named.IsDir() || !os.SameFile(named, s.partitionID) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		actual, err := s.partition.Stat(".")
		if err != nil || !os.SameFile(s.partitionID, actual) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
		actual, err = s.partitionScan.Stat()
		if err != nil || !os.SameFile(s.partitionID, actual) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
	}
	return a.check()
}

func (a *archiveDateReclaimAuthority) newOrphanSession() (_ *archiveDateOrphanSession, err error) {
	binding, err := dateActivationBinding(a.state)
	if err != nil {
		return nil, err
	}
	s := &archiveDateOrphanSession{owner: a.p.generation, head: a.proof.head, window: a.proof.window, position: a.proof.position, digest: a.proof.digest, binding: binding, directory: a.directory}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.closeRoot())
		}
	}()
	s.root, err = os.OpenRoot(a.e.root)
	if err != nil {
		return nil, err
	}
	s.scan, err = s.root.Open(".")
	if err != nil {
		return nil, err
	}
	if err := s.check(a); err != nil {
		return nil, err
	}
	return s, nil
}

// One bounded directory entry at a time. Charge a conservative complete 8 KiB
// directory buffer before each read, including EOF. Partition opening/closing is
// serial; a stream error never leaves a replacement/root handle unowned.
func (s *archiveDateOrphanSession) next(ctx context.Context, a *archiveDateReclaimAuthority, out *DateOrphanReclaimProgress, entries int) (path string, complete bool, err error) {
	if s.pending != "" {
		return s.pending, false, s.check(a)
	}
	for out.EntriesScanned < entries {
		if err := s.check(a); err != nil {
			return "", false, err
		}
		if err := a.readBudget.reserve(8 << 10); err != nil {
			return "", false, err
		}
		stream := s.scan
		if s.partitionScan != nil {
			stream = s.partitionScan
		}
		names, readErr := stream.Readdirnames(1)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", false, readErr
		}
		if len(names) == 0 {
			if !errors.Is(readErr, io.EOF) {
				return "", false, io.ErrNoProgress
			}
			if s.partitionScan != nil {
				if err := s.closePartition(); err != nil {
					return "", false, err
				}
				continue
			}
			return "", true, s.check(a)
		}
		name := names[0]
		if len(name) > 4096 || strings.ContainsAny(name, "/\\") {
			return "", false, ErrDiskUsageCorrupt
		}
		out.EntriesScanned++
		if s.visited == ^uint64(0) {
			return "", false, ErrDiskUsageLimit
		}
		s.visited++
		if s.partitionScan != nil {
			path = s.partitionName + "/" + name
			if _, err := dateOrphanPath(path); err != nil {
				out.FilesUnrecognized++
				continue
			}
			s.pending = path
			return path, false, s.check(a)
		}
		if !strings.HasPrefix(name, "date=") {
			out.EntriesIgnored++
			continue
		}
		date := strings.TrimPrefix(name, "date=")
		if err := validateArchivePartitionDate(ManifestFile{Date: date, Path: name + "/inventory.ndjson"}); err != nil {
			return "", false, err
		}
		if date >= a.proof.position.Retention.RetainFromDate {
			out.EntriesIgnored++
			continue
		}
		info, err := s.root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			out.EntriesIgnored++
			continue
		}
		if err != nil || !info.IsDir() {
			return "", false, errors.Join(err, ErrDiskUsageCorrupt)
		}
		s.partition, err = s.root.OpenRoot(name)
		if err != nil {
			return "", false, err
		}
		s.partitionName, s.partitionID = name, info
		s.partitionScan, err = s.partition.Open(".")
		if err != nil {
			return "", false, err
		}
	}
	return "", false, nil
}

func (e *Exporter) ReclaimRetainedDateOrphansStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DateOrphanReclaimLimits) (DateOrphanReclaimProgress, error) {
	return e.reclaimRetainedDateOrphansStep(ctx, p, retainFrom, limits, nil)
}

func (e *Exporter) reclaimRetainedDateOrphansStep(ctx context.Context, p *DiskProjection, retainFrom time.Time, limits DateOrphanReclaimLimits, afterUnlink func() error) (out DateOrphanReclaimProgress, err error) {
	if limits.Entries < 1 || limits.Entries > 256 || limits.Files < 1 || limits.Files > 16 || limits.InspectRows < limits.File.FileRows || limits.InspectRows > 2<<20 {
		return out, ErrDiskUsageLimit
	}
	err = e.withDateReclaimAuthority(ctx, p, retainFrom, limits.File, func(ctx context.Context, a *archiveDateReclaimAuthority) (jobErr error) {
		if e.dateOrphans == nil {
			e.dateOrphans, jobErr = a.newOrphanSession()
			if jobErr != nil {
				return jobErr
			}
		}
		s := e.dateOrphans
		if err := s.check(a); err != nil {
			return err
		}
		remaining := limits.InspectRows
		a.orphanInspectRemaining = &remaining
		complete := false
		for out.FilesExamined < limits.Files {
			path, finished, err := s.next(ctx, a, &out, limits.Entries)
			if err != nil {
				return err
			}
			if finished || path == "" {
				complete = finished
				break
			}
			if remaining == 0 || out.RowsVerified == limits.File.FileRows {
				break // The one pending name is repeated with fresh budgets next call.
			}
			entry, err := dateOrphanPath(path)
			if err != nil {
				return err
			}
			found, err := a.referenced(ctx, path)
			if err != nil {
				return err
			}
			if found {
				out.FilesProtected++
			} else {
				actual, missing, err := a.describeOrphanFile(ctx, entry, limits.File)
				// An out-of-scope row is a conservative preservation hint, not a
				// native content claim. Never suppress actual cleanup/Close errors.
				if err != nil {
					if onlyDateOutsidePolicy(err) {
						out.FilesOutsidePolicy++
					} else {
						return err
					}
				} else if missing {
					out.FilesMissing++
				} else {
					if actual.Records > int64(limits.File.FileRows-out.RowsVerified) {
						break
					}
					fileLimits := limits.File
					fileLimits.FileRows -= out.RowsVerified
					reclaimed, err := a.reclaimFile(ctx, actual, fileLimits, nil, afterUnlink)
					if err != nil {
						return err
					}
					if reclaimed.Removed {
						out.FilesRemoved++
					} else {
						out.FilesMissing++
					}
					out.RowsVerified += reclaimed.RowsVerified
					if err := addInt64(&out.LogicalBytesUnlinked, reclaimed.LogicalBytesUnlinked); err != nil {
						return err
					}
				}
			}
			out.FilesExamined++
			s.pending = ""
		}
		out.Phase, out.Current, out.Visited, out.ObligationPending = "scan", a.proof.position, s.visited, true
		out.RowsInspected = limits.InspectRows - remaining
		if complete {
			out.Phase = "scan-complete"
			// EOF is delivered only after both actual stream/root Close joins.
			if err := e.discardDateOrphans(); err != nil {
				return err
			}
		} else {
			if err := s.check(a); err != nil {
				return err
			}
			a.keepProof = out.FilesRemoved == 0 // Read-only finite scan may continue on this owner.
		}
		return a.check()
	})
	if err != nil {
		out = DateOrphanReclaimProgress{}
	}
	return out, err
}

// errors.Join cleanup retains every physical error. Only the two policy leaves
// may be treated as conservative preservation, never a Close/identity/IO error.
func onlyDateOutsidePolicy(err error) bool {
	if !errors.Is(err, errArchiveDateOrphanOutsidePolicy) {
		return false
	}
	var allowed func(error) bool
	allowed = func(err error) bool {
		if err == ErrArchiveDateProof || err == errArchiveDateOrphanOutsidePolicy {
			return true
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !allowed(child) {
					return false
				}
			}
			return true
		}
		return false
	}
	return allowed(err)
}
