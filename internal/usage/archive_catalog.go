package usage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/durable"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Catalog format and row schemas are separate. Schema 9 deliberately makes
// older binaries refuse this root instead of treating files:null as no history.
const archiveCatalogSchema = 9
const archiveCatalogPageBytes = 64 << 10
const archiveCatalogPageFiles = 64

var ErrArchivePagedCatalog = errors.New("paged Usage catalog requires bounded catalog readers; legacy snapshot/export/verify/prune is unavailable")

type ArchivePageRef struct {
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int    `json:"bytes,omitempty"`
	Pages  uint64 `json:"pages,omitempty"`
	Files  uint64 `json:"files,omitempty"`
}

func (r ArchivePageRef) valid() bool {
	if r == (ArchivePageRef{}) {
		return true
	}
	raw, err := hex.DecodeString(r.SHA256)
	return err == nil && len(raw) == 32 && r.SHA256 == strings.ToLower(r.SHA256) && r.Bytes > 0 && r.Bytes <= archiveCatalogPageBytes && r.Pages > 0 && r.Files >= r.Pages
}
func (r ArchivePageRef) path(root string) string {
	return filepath.Join(root, "catalog", "v1", "page-"+r.SHA256+".json")
}

type archiveCatalogRoot struct {
	Version int            `json:"version"`
	Head    ArchivePageRef `json:"head"`
}
type archiveCatalogPage struct {
	Version  int            `json:"version"`
	Previous ArchivePageRef `json:"previous"`
	Files    []ManifestFile `json:"files"`
}

type ArchiveFilesPage struct {
	Files        []ManifestFile `json:"files"`
	NextCursor   string         `json:"next_cursor,omitempty"`
	LastSequence uint64         `json:"last_sequence"`
}
type archiveFilesCursor struct {
	Version      uint8
	Epoch        uint64
	Reference    ArchivePageRef
	Index        int
	LastSequence uint64
	Retention    ArchiveDateRetention
}

func (e *Exporter) readCatalogPage(ctx context.Context, ref ArchivePageRef) (result archiveCatalogPage, err error) {
	if err := ctx.Err(); err != nil {
		return archiveCatalogPage{}, err
	}
	if !ref.valid() || ref == (ArchivePageRef{}) {
		return archiveCatalogPage{}, ErrDiskUsageCorrupt
	}
	path := ref.path(e.root)
	named, err := os.Lstat(path)
	if err != nil {
		return archiveCatalogPage{}, err
	}
	if !named.Mode().IsRegular() || named.Size() != int64(ref.Bytes) {
		return archiveCatalogPage{}, ErrDiskUsageCorrupt
	}
	f, err := os.Open(path)
	if err != nil {
		return archiveCatalogPage{}, err
	}
	defer func() {
		err = errors.Join(err, f.Close(), ctx.Err())
		if err != nil {
			result = archiveCatalogPage{}
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return archiveCatalogPage{}, err
	}
	if !os.SameFile(named, info) || info.Size() != int64(ref.Bytes) {
		return archiveCatalogPage{}, ErrDiskUsageCorrupt
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(ref.Bytes)+1))
	if err != nil {
		return archiveCatalogPage{}, err
	}
	digest := sha256.Sum256(raw)
	if len(raw) != ref.Bytes || hex.EncodeToString(digest[:]) != ref.SHA256 {
		return archiveCatalogPage{}, ErrDiskUsageCorrupt
	}
	page, decodeErr := decodeArchiveCatalogPage(raw)
	if decodeErr != nil || page.Version != 1 || !page.Previous.valid() || len(page.Files) == 0 || len(page.Files) > archiveCatalogPageFiles {
		return archiveCatalogPage{}, ErrDiskUsageCorrupt
	}
	if page.Previous.Pages == math.MaxUint64 || page.Previous.Files > math.MaxUint64-uint64(len(page.Files)) || ref.Pages != page.Previous.Pages+1 || ref.Files != page.Previous.Files+uint64(len(page.Files)) {
		return archiveCatalogPage{}, ErrDiskUsageCorrupt
	}
	for _, entry := range page.Files {
		if err := e.validateCatalogEntry(entry); err != nil {
			return archiveCatalogPage{}, err
		}
	}
	current, err := f.Stat()
	if err != nil {
		return archiveCatalogPage{}, err
	}
	now, err := os.Lstat(path)
	if err != nil {
		return archiveCatalogPage{}, err
	}
	if !os.SameFile(info, now) || info.Size() != current.Size() || info.Size() != now.Size() || info.ModTime() != current.ModTime() || info.ModTime() != now.ModTime() {
		return archiveCatalogPage{}, ErrDiskUsageCorrupt
	}
	return page, ctx.Err()
}
func (e *Exporter) validateCatalogEntry(entry ManifestFile) error {
	digest, err := hex.DecodeString(entry.SHA256)
	if err != nil || len(digest) != 32 || entry.Records < 1 || entry.MinSequence == 0 || entry.MaxSequence < entry.MinSequence || !supportedManifestSchema(entry.SchemaVersion) || entry.format() != FormatParquet && entry.format() != FormatNDJSON {
		return ErrDiskUsageCorrupt
	}
	_, err = e.safeManifestPath(entry.Path)
	return err
}

// ReadArchiveFiles returns at most one authenticated immutable page (or part of
// it), newest page first. It never presents that physical order as row order.
// A signed cursor is scoped to this Exporter lifetime and its captured frontier;
// appends leave old pages available. Reclamation invalidates the old epoch.
// Returned descriptors do not pin a caller's later, independent file opens.
func (e *Exporter) ReadArchiveFiles(ctx context.Context, cursor string, limit int) (result ArchiveFilesPage, err error) {
	finish, err := e.beginArchiveRead(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		err = errors.Join(err, finish())
		if err != nil {
			result = ArchiveFilesPage{}
		}
	}()
	return e.readArchiveFilesHeld(ctx, cursor, limit)
}

func (e *Exporter) readArchiveFilesHeld(ctx context.Context, cursor string, limit int) (result ArchiveFilesPage, err error) {
	if limit < 1 || limit > archiveCatalogPageFiles {
		return result, ErrDiskUsageLimit
	}
	var state archiveFilesCursor
	if cursor == "" {
		position, err := e.readArchivePositionHeld(ctx)
		if err != nil {
			return result, err
		}
		if position.CatalogVersion != 1 {
			return result, ErrArchivePagedCatalog
		}
		state = archiveFilesCursor{Version: archiveFilesCursorVersion, Epoch: e.archiveEpoch, Reference: position.CatalogHead, LastSequence: position.LastSequence, Retention: position.Retention}
	} else {
		if len(cursor) > 2732 {
			return result, ErrDiskUsageCursor
		}
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(raw) > 2048 || len(raw) <= sha256.Size {
			return result, ErrDiskUsageCursor
		}
		payload, mac := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
		h := hmac.New(sha256.New, e.cursorKey[:])
		h.Write(payload)
		if !hmac.Equal(mac, h.Sum(nil)) || json.Unmarshal(payload, &state) != nil || state.Version != archiveFilesCursorVersion || state.Epoch != e.archiveEpoch || !state.Reference.valid() || state.Index < 0 || state.Retention != (ArchiveDateRetention{}) && !state.Retention.valid(state.LastSequence) {
			return result, ErrDiskUsageCursor
		}
	}
	result.LastSequence = state.LastSequence
	if state.Reference == (ArchivePageRef{}) {
		if state.Index != 0 {
			return ArchiveFilesPage{}, ErrDiskUsageCursor
		}
		return result, ctx.Err()
	}
	page, err := e.readCatalogPage(ctx, state.Reference)
	if err != nil {
		return ArchiveFilesPage{}, err
	}
	if state.Index >= len(page.Files) {
		return ArchiveFilesPage{}, ErrDiskUsageCursor
	}
	end := min(len(page.Files), state.Index+limit)
	result.Files = page.Files[state.Index:end]
	for _, entry := range result.Files {
		if err := validateArchiveEntryCoverage(entry, state.LastSequence, state.Retention); err != nil {
			return ArchiveFilesPage{}, ErrDiskUsageCorrupt
		}
	}
	if end == len(page.Files) {
		state.Reference = page.Previous
		state.Index = 0
	} else {
		state.Index = end
	}
	if state.Reference != (ArchivePageRef{}) {
		payload, err := json.Marshal(state)
		if err != nil {
			return ArchiveFilesPage{}, err
		}
		h := hmac.New(sha256.New, e.cursorKey[:])
		h.Write(payload)
		result.NextCursor = base64.RawURLEncoding.EncodeToString(append(payload, h.Sum(nil)...))
	}
	if err := ctx.Err(); err != nil {
		return ArchiveFilesPage{}, err
	}
	return result, nil
}

// Validation checks catalog reachability/content/counters/descriptor structure.
// It does not prove exact canonical row sets or authenticate native source lineage.
func (e *Exporter) ValidateArchiveCatalog(ctx context.Context) (err error) {
	finish, err := e.beginArchiveRead(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, finish()) }()
	return e.validateArchiveCatalogHeld(ctx)
}

func (e *Exporter) validateArchiveCatalogHeld(ctx context.Context) error {
	position, err := e.readArchivePositionHeld(ctx)
	if err != nil {
		return err
	}
	if position.CatalogVersion != 1 {
		return ErrArchivePagedCatalog
	}
	ref := position.CatalogHead
	for pages := uint64(0); ref != (ArchivePageRef{}); pages++ {
		if pages >= 1<<20 {
			return ErrDiskUsageLimit
		}
		page, err := e.readCatalogPage(ctx, ref)
		if err != nil {
			return err
		}
		for _, entry := range page.Files {
			if err := validateArchiveEntryCoverage(entry, position.LastSequence, position.Retention); err != nil {
				return ErrDiskUsageCorrupt
			}
		}
		ref = page.Previous
	}
	return ctx.Err()
}

// VerifyArchiveFileChecksums streams data files. It deliberately does not claim
// the stronger legacy Verify/Reconcile row/snapshot/duplicate-set semantics.
func (e *Exporter) VerifyArchiveFileChecksums(ctx context.Context) error {
	return e.verifyArchiveFileChecksumsWithHook(ctx, nil)
}

// The private hook exercises actual descriptor lifetime, not just cancellation
// of a queued callback. It runs after open, before hashing/Close, under the permit.
func (e *Exporter) verifyArchiveFileChecksumsWithHook(ctx context.Context, opened func(*os.File) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	finish, err := e.beginArchiveRead(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, finish()) }()
	remaining := int64(16 << 30)
	cursor := ""
	for pages := 0; ; pages++ {
		if pages >= 1<<20 {
			return ErrDiskUsageLimit
		}
		page, err := e.readArchiveFilesHeld(ctx, cursor, archiveCatalogPageFiles)
		if err != nil {
			return err
		}
		for _, entry := range page.Files {
			path, err := e.safeManifestPath(entry.Path)
			if err != nil {
				return err
			}
			named, err := os.Lstat(path)
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
			err = func() (err error) {
				defer func() { err = errors.Join(err, f.Close(), ctx.Err()) }()
				if opened != nil {
					if err := opened(f); err != nil {
						return err
					}
				}
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() || !os.SameFile(named, info) {
					return ErrDiskUsageCorrupt
				}
				if info.Size() > remaining {
					return ErrDiskUsageLimit
				}
				remaining -= info.Size()
				reader := io.NewSectionReader(f, 0, info.Size())
				h := sha256.New()
				buffer := make([]byte, 64<<10)
				for {
					if err := ctx.Err(); err != nil {
						return err
					}
					n, err := reader.Read(buffer)
					h.Write(buffer[:n])
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						return err
					}
				}
				if hex.EncodeToString(h.Sum(nil)) != entry.SHA256 {
					return fmt.Errorf("%w: checksum %s", ErrDiskUsageCorrupt, entry.Path)
				}
				now, err := os.Lstat(path)
				if err != nil {
					return err
				}
				current, err := f.Stat()
				if err != nil {
					return err
				}
				if !os.SameFile(info, now) || info.Size() != now.Size() || info.Size() != current.Size() || info.ModTime() != now.ModTime() || info.ModTime() != current.ModTime() {
					return ErrDiskUsageCorrupt
				}
				return nil
			}()
			if err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}

// Count/cap array entries before decoding ManifestFile strings/structs. Content
// pages have one canonical encoding, avoiding duplicate-field ambiguity.
func decodeArchiveCatalogPage(raw []byte) (archiveCatalogPage, error) {
	var header struct {
		Version  int             `json:"version"`
		Previous ArchivePageRef  `json:"previous"`
		Files    json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return archiveCatalogPage{}, err
	}
	page := archiveCatalogPage{Version: header.Version, Previous: header.Previous}
	input := bufio.NewReader(bytes.NewReader(header.Files))
	if err := archiveExpect(input, '['); err != nil {
		return page, err
	}
	c, err := archiveNonSpace(input)
	if err != nil {
		return page, err
	}
	if c == ']' {
		return page, ErrDiskUsageCorrupt
	}
	input.UnreadByte()
	for {
		if len(page.Files) == archiveCatalogPageFiles {
			return archiveCatalogPage{}, ErrDiskUsageLimit
		}
		value, err := archiveValue(input)
		if err != nil {
			return archiveCatalogPage{}, err
		}
		var entry ManifestFile
		if json.Unmarshal(value, &entry) != nil {
			return archiveCatalogPage{}, ErrDiskUsageCorrupt
		}
		page.Files = append(page.Files, entry)
		c, err := archiveNonSpace(input)
		if err != nil {
			return archiveCatalogPage{}, err
		}
		if c == ']' {
			break
		}
		if c != ',' {
			return archiveCatalogPage{}, ErrDiskUsageCorrupt
		}
	}
	canonical, err := json.Marshal(page)
	if err != nil || !bytes.Equal(raw, canonical) {
		return archiveCatalogPage{}, ErrDiskUsageCorrupt
	}
	return page, nil
}

type archiveCatalogBuilder struct {
	exporter *Exporter
	ctx      context.Context
	work     *archiveWork
	head     ArchivePageRef
	pending  []ManifestFile
}

func (b *archiveCatalogBuilder) add(entry ManifestFile) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if err := b.exporter.validateCatalogEntry(entry); err != nil {
		return err
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if len(raw) > archiveCatalogPageBytes-1024 {
		return ErrDiskUsageLimit
	}
	if len(b.pending) == archiveCatalogPageFiles {
		if err := b.flush(); err != nil {
			return err
		}
	}
	candidate := append(b.pending, entry)
	encoded, err := json.Marshal(archiveCatalogPage{Version: 1, Previous: b.head, Files: candidate})
	if err != nil {
		return err
	}
	if len(encoded) > archiveCatalogPageBytes {
		if err := b.flush(); err != nil {
			return err
		}
		b.pending = append(b.pending, entry)
	} else {
		b.pending = candidate
	}
	return nil
}
func (b *archiveCatalogBuilder) flush() (err error) {
	if len(b.pending) == 0 {
		return nil
	}
	if b.head.Pages == math.MaxUint64 || b.head.Files > math.MaxUint64-uint64(len(b.pending)) {
		return ErrDiskUsageLimit
	}
	raw, err := json.Marshal(archiveCatalogPage{Version: 1, Previous: b.head, Files: b.pending})
	if err != nil {
		return err
	}
	if len(raw) > archiveCatalogPageBytes {
		return ErrDiskUsageLimit
	}
	if err := b.exporter.ensureArchiveWorkTree(b.ctx, b.work); err != nil {
		return err
	}
	hash := sha256.Sum256(raw)
	ref := ArchivePageRef{SHA256: hex.EncodeToString(hash[:]), Bytes: len(raw), Pages: b.head.Pages + 1, Files: b.head.Files + uint64(len(b.pending))}
	directory := filepath.Join(b.exporter.root, "catalog", "v1")
	for _, path := range []string{b.exporter.root, filepath.Dir(directory), directory} {
		if err := archiveDirectory(path); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(directory, ".archive-catalog-*.tmp")
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
	writer := &archiveWriter{ctx: b.ctx, out: f, remaining: archiveCatalogPageBytes, work: b.work}
	if _, err := writer.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	identity, err = f.Stat()
	if err != nil {
		return err
	}
	if err := checkArchiveTemporary(f, identity); err != nil {
		return err
	}
	if err := os.Link(f.Name(), ref.path(b.exporter.root)); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := checkArchiveOrphan(b.ctx, ref.path(b.exporter.root), int64(len(raw)), ref.SHA256); err != nil {
			return err
		}
	}
	err = f.Close()
	closed = true
	if err != nil {
		return err
	}
	for _, path := range []string{directory, filepath.Dir(directory), b.exporter.root} {
		if err := durable.SyncDirectory(path); err != nil {
			return err
		}
	}
	b.head = ref
	b.pending = nil
	return nil
}

func (e *Exporter) commitPagedArchive(ctx context.Context, old *archiveManifestInput, missing bool, position ArchivePosition, digest [32]byte, stage *os.File, partitions uint64, cut DiskArchiveCut, work *archiveWork) error {
	return e.commitPagedArchiveWithProof(ctx, old, missing, position, digest, stage, partitions, cut, work, nil)
}

func (e *Exporter) commitPagedArchiveWithProof(ctx context.Context, old *archiveManifestInput, missing bool, position ArchivePosition, digest [32]byte, stage *os.File, partitions uint64, cut DiskArchiveCut, work *archiveWork, publication *archiveWindowPublication) error {
	if cut.AfterSequence != position.LastSequence || cut.ThroughSequence < position.LastSequence || cut.ThroughSequence > cut.Head.Sequence {
		return ErrDiskUsageCorrupt
	}
	builder := archiveCatalogBuilder{exporter: e, ctx: ctx, work: work, head: position.CatalogHead}
	if !missing {
		if position.CatalogVersion == 1 {
			// The existing immutable tail was validated at owner construction. Check
			// its head again; appending does not re-scan every historical page. Full
			// revalidation/malicious mutation detection remains an explicit operation.
			if position.CatalogHead != (ArchivePageRef{}) {
				if _, err := e.readCatalogPage(ctx, position.CatalogHead); err != nil {
					return err
				}
			}
		} else {
			second, secondDigest, err := old.walk(ctx, func(entry ManifestFile) error {
				if entry.MaxSequence > position.LastSequence {
					return ErrDiskUsageCorrupt
				}
				if entry.SchemaVersion == 0 {
					entry.SchemaVersion = position.SchemaVersion
				}
				return builder.add(entry)
			})
			if err != nil {
				return err
			}
			if second != position || secondDigest != digest {
				return ErrDiskUsageCorrupt
			}
		}
	}
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		return err
	}
	decoder := json.NewDecoder(stage)
	var appended uint64
	for {
		var entry ManifestFile
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if entry.MinSequence <= position.LastSequence || entry.MaxSequence > cut.ThroughSequence {
			return ErrDiskUsageCorrupt
		}
		if err := builder.add(entry); err != nil {
			return err
		}
		appended++
	}
	if appended != partitions {
		return ErrDiskUsageCorrupt
	}
	if err := builder.flush(); err != nil {
		return err
	}
	if position.Files > math.MaxUint64-partitions || builder.head.Files != position.Files+partitions {
		return ErrDiskUsageCorrupt
	}
	return e.publishArchiveCatalogRootWithRetention(ctx, old, missing, cut.ThroughSequence, true, builder.head, position.Retention, work, publication)
}

func (e *Exporter) publishArchiveCatalogRoot(ctx context.Context, old *archiveManifestInput, missing bool, through uint64, complete bool, head ArchivePageRef, work *archiveWork) error {
	return e.publishArchiveCatalogRootWithProof(ctx, old, missing, through, complete, head, work, nil)
}

func (e *Exporter) publishArchiveCatalogRootWithProof(ctx context.Context, old *archiveManifestInput, missing bool, through uint64, complete bool, head ArchivePageRef, work *archiveWork, publication *archiveWindowPublication) (err error) {
	var retention ArchiveDateRetention
	if !missing && old.info.Size() <= archiveManifestValueLimit {
		if err := work.take(int(old.info.Size())); err != nil {
			return err
		}
		position, _, err := old.walk(ctx, nil)
		if err != nil {
			return err
		}
		retention = position.Retention
	}
	return e.publishArchiveCatalogRootWithRetention(ctx, old, missing, through, complete, head, retention, work, publication)
}

func (e *Exporter) publishArchiveCatalogRootWithRetention(ctx context.Context, old *archiveManifestInput, missing bool, through uint64, complete bool, head ArchivePageRef, retention ArchiveDateRetention, work *archiveWork, publication *archiveWindowPublication) (err error) {
	return e.publishArchiveCatalogRootWithRetentionHook(ctx, old, missing, through, complete, head, retention, work, publication, nil, nil)
}

func (e *Exporter) publishArchiveCatalogRootWithRetentionHook(ctx context.Context, old *archiveManifestInput, missing bool, through uint64, complete bool, head ArchivePageRef, retention ArchiveDateRetention, work *archiveWork, publication *archiveWindowPublication, beforeRename, afterRename func() error) (err error) {
	if err := e.ensureArchiveWorkTree(ctx, work); err != nil {
		return err
	}
	raw, expected, err := archiveCatalogRootBytes(through, complete, head, retention)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(e.root, ".archive-root-*.tmp")
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
	writer := &archiveWriter{ctx: ctx, out: f, remaining: archiveManifestValueLimit, work: work}
	if _, err := writer.Write(raw); err != nil {
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
	if err := publication.checkDirectory(e.root); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if missing {
		if _, err := os.Lstat(filepath.Join(e.root, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
	} else if err := old.check(); err != nil {
		return err
	}
	if beforeRename != nil {
		if err := beforeRename(); err != nil {
			return err
		}
	}
	if err := os.Rename(f.Name(), filepath.Join(e.root, "manifest.json")); err != nil {
		return err
	}
	if err := publication.renamed(expected, sha256.Sum256(raw), identity); err != nil {
		return err
	}
	if afterRename != nil {
		if err := afterRename(); err != nil {
			return err
		}
	}
	return durable.SyncDirectory(e.root)
}

// MigrateArchiveCatalog preserves a legacy frontier and physical data files.
// Publication is all-or-nothing at the root. Interrupted pages remain harmless
// content-addressed orphans for retry; bulk orphan reclamation is separate work.
func (e *Exporter) MigrateArchiveCatalog(ctx context.Context, maxDuration time.Duration) (result ArchivePosition, err error) {
	if maxDuration <= 0 || maxDuration > time.Minute {
		return ArchivePosition{}, ErrDiskUsageLimit
	}
	if !e.mu.TryLock() {
		return ArchivePosition{}, ErrDiskUsageBusy
	}
	defer e.mu.Unlock()
	if e.archiveRetired.Load() {
		return result, ErrDiskUsageClosed
	}
	ctx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()
	defer func() {
		err = errors.Join(err, e.archiveReadError(ctx))
		if err != nil {
			result = ArchivePosition{}
		}
	}()
	old, err := openArchiveManifest(e.root)
	if err != nil {
		return ArchivePosition{}, err
	}
	defer func() { err = errors.Join(err, old.file.Close()) }()
	position, digest, err := old.walk(ctx, nil)
	if err != nil {
		return ArchivePosition{}, err
	}
	if position.CatalogVersion == 1 {
		return position, e.ValidateArchiveCatalog(ctx)
	}
	releaseShared, err := e.reserveArchiveDisk(ctx, 1<<30)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, releaseShared()) }()
	work, err := e.newArchiveWork(ctx, 1<<30)
	if err != nil {
		return result, err
	}
	builder := archiveCatalogBuilder{exporter: e, ctx: ctx, work: work}
	second, secondDigest, err := old.walk(ctx, func(entry ManifestFile) error {
		if entry.MaxSequence > position.LastSequence {
			return ErrDiskUsageCorrupt
		}
		if entry.SchemaVersion == 0 {
			entry.SchemaVersion = position.SchemaVersion
		}
		return builder.add(entry)
	})
	if err != nil {
		return ArchivePosition{}, err
	}
	if second != position || digest != secondDigest {
		return ArchivePosition{}, ErrDiskUsageCorrupt
	}
	if err := builder.flush(); err != nil {
		return ArchivePosition{}, err
	}
	if err := e.publishArchiveCatalogRoot(ctx, old, false, position.LastSequence, position.CompletePrefix, builder.head, work); err != nil {
		return ArchivePosition{}, err
	}
	return e.ReadArchivePosition(ctx)
}
