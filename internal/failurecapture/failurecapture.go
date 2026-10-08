// Package failurecapture keeps the request a failed call carried and a safe,
// structured description of the answer, so an operator can reproduce the
// failure without persisting untrusted upstream prose.
//
// It is the only place Halro stores what a caller wrote. Everything else the
// gateway persists is metadata it produced itself — identifiers, counts, classes
// and costs — and the rule that prompts and response bodies never reach a log, a
// metric or an audit record still holds without exception. This is a different
// act from logging: the material is encrypted under the master key and bound to
// the request and project it belongs to, it expires on a clock, it is bounded in
// size and in count, and reading it is an audited admin action rather than a
// side effect of tailing a file.
//
// Four properties are load-bearing, and each of them exists because the obvious
// version of this feature is a data breach waiting for a disk image:
//
//   - Failures only. A successful call is never captured, so the store holds a
//     small tail of traffic rather than a copy of it.
//   - Bounded. Every record is truncated to a byte ceiling and every day to a
//     record ceiling; past either, capture stops and says so once. A store that
//     grows with traffic is one that fills the disk the accounting depends on.
//   - Expiring. Records live in day directories and whole days are removed past
//     the retention window, so "how long do we keep customer prompts" has an
//     answer that is enforced rather than promised.
//   - Best-effort. A capture that cannot be written is dropped, never retried
//     and never allowed to change what the caller is told. Diagnostics must not
//     be able to fail a request that already failed.
package failurecapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/dirscan"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/provider"
)

// DirPerm and FilePerm match the log sink's and the data directory's. The
// content here is more sensitive than either.
const (
	DirPerm  os.FileMode = 0o700
	FilePerm os.FileMode = 0o600
)

// fileExtension marks a sealed capture. The bytes inside are a vault envelope,
// so the extension is a hint for a human listing the directory rather than
// anything the reader trusts.
const fileExtension = ".hfc"

const captureMetadataBytes = 4096

func captureFileCeiling(maxBytes int) int64 {
	return 18*int64(maxBytes) + 6*captureMetadataBytes + 4096
}

// ReadRAMBytes covers ciphertext, plaintext, decoded JSON and response encoding
// plus fixed directory batches. This conservative logical envelope is retained
// by the response owner through audited output; it is not an RSS guarantee.
func (s *Store) ReadRAMBytes() int64 { return 4*captureFileCeiling(s.maxBytes) + 64<<10 }

func validRecordMetadata(record Record) bool {
	remaining := captureMetadataBytes
	for _, value := range []string{record.RequestID, record.ProjectID, record.Outcome, string(record.OfferingID), string(record.ProfileID), string(record.AccountRegionID), record.ProviderCode, string(record.ProviderFailureReason), record.ProviderRequestID, record.FailurePhase} {
		if len(value) > remaining {
			return false
		}
		remaining -= len(value)
	}
	return true
}

// captureName is `<unix-millis>-<request-id>.hfc`.
//
// The timestamp is in the name so the sweep can date a record without opening
// it, and so retention is per record rather than per day — day granularity made
// `retain` a floor rather than a ceiling, keeping a record configured for an
// hour the better part of a day. It is not the file's mtime for two reasons: an
// mtime is not governed by this store's clock, so the guarantee could not be
// tested; and a `cp -R` of a data directory resets every mtime forward, which
// would extend a retention window rather than shorten it.
// captureName carries the project as well as the request, and the project is
// there for a reason worth stating: the envelope is sealed against
// (request, project), so a reader has to know the project before it can open
// anything. That used to be answered by the usage aggregate, which holds a
// bounded console window — so a capture whose retention outlived that window
// was still on disk, still within its promised life, and no longer openable,
// and the endpoint reported it as a missing usage request. Kept but unreadable
// is the worst side of a retention promise to land on.
//
// Neither identifier may contain the separator, which is checked on the way in;
// both are Halro-minted and use an alphabet that has none.
func captureName(capturedAt time.Time, requestID, projectID string) string {
	return fmt.Sprintf("%d-%s-%s%s", capturedAt.UnixMilli(), requestID, projectID, fileExtension)
}

// capturedIdentity reads back what captureName wrote, or reports that the entry
// is not one of ours.
func capturedIdentity(name string) (requestID, projectID string, capturedAt time.Time, ok bool) {
	if filepath.Ext(name) != fileExtension {
		return "", "", time.Time{}, false
	}
	millis, rest, found := strings.Cut(strings.TrimSuffix(name, fileExtension), "-")
	if !found {
		return "", "", time.Time{}, false
	}
	request, project, found := strings.Cut(rest, "-")
	if !found || request == "" || project == "" {
		return "", "", time.Time{}, false
	}
	stamp, err := strconv.ParseInt(millis, 10, 64)
	if err != nil {
		return "", "", time.Time{}, false
	}
	return request, project, time.UnixMilli(stamp).UTC(), true
}

// Record is what a capture holds. Every field is either produced by Halro or is
// material the caller or the upstream wrote — and the second kind is exactly
// what makes this package's guarantees necessary.
type Record struct {
	RequestID  string    `json:"request_id"`
	ProjectID  string    `json:"project_id"`
	Outcome    string    `json:"outcome"`
	CapturedAt time.Time `json:"captured_at"`
	// OfferingID and ProfileID identify the exact upstream product surface that
	// produced the captured failure. Older captures decode with both empty.
	OfferingID               domain.ProviderOfferingID `json:"offering_id,omitempty"`
	ProfileID                domain.ProviderProfileID  `json:"profile_id,omitempty"`
	AccountRegionID          domain.ProviderRegionID   `json:"account_region_id,omitempty"`
	ProviderCode             string                    `json:"provider_code,omitempty"`
	ProviderFailureReason    provider.FailureReason    `json:"provider_failure_reason,omitempty"`
	ProviderRequestID        string                    `json:"provider_request_id,omitempty"`
	FailurePhase             string                    `json:"failure_phase,omitempty"`
	Retryable                bool                      `json:"retryable,omitempty"`
	Ambiguous                bool                      `json:"ambiguous,omitempty"`
	FailureSemanticsRecorded bool                      `json:"failure_semantics_recorded,omitempty"`
	// GatewayRequest is the decoded body accepted at Halro's public API
	// boundary, before protocol translation changes field names or structure.
	// It is retained only in this encrypted, bounded and audited store. Headers
	// are never included, so a Gateway credential cannot enter the capture.
	GatewayRequest json.RawMessage `json:"gateway_request,omitempty"`
	// GatewayRequestTruncated distinguishes an incomplete diagnostic copy from
	// a malformed request sent by the caller.
	GatewayRequestTruncated bool `json:"gateway_request_truncated,omitempty"`
	// Request is Halro's provider-neutral operation after the project's
	// redaction policy has run. Comparing it with GatewayRequest explains which
	// translation produced the field the provider ultimately accepted or
	// refused.
	Request json.RawMessage `json:"request,omitempty"`
	// RequestTruncated says the ceiling cut the request short, so a reader does
	// not diagnose a malformed body that is only an incomplete one.
	RequestTruncated bool `json:"request_truncated,omitempty"`
	// Response is Halro's structured status/class for an upstream refusal, or a
	// prose-free shape summary for an answer the wire renderer refused. Raw
	// upstream values are never stored: they may echo the Provider credential.
	Response          json.RawMessage `json:"response,omitempty"`
	ResponseTruncated bool            `json:"response_truncated,omitempty"`
}

// Sealer is the encryption this store depends on, kept as an interface so the
// store can be built and tested without a master key in the process.
type Sealer interface {
	EncryptFailurePayload(requestID, projectID string, plaintext []byte) ([]byte, error)
	DecryptFailurePayload(requestID, projectID string, envelope []byte) ([]byte, error)
}

type Options struct {
	Root string
	// MaxBytes bounds one captured side — the request and the response are
	// bounded separately, because a large answer must not cost the request that
	// explains it.
	MaxBytes int
	// MaxRecordsPerDay bounds the store's growth against an upstream that is
	// failing everything. Past it, capture stops for the day rather than
	// competing with the ledger for the same disk.
	MaxRecordsPerDay int
	// Retain is how long a day's captures live. Zero is refused: a store of
	// customer prompts with no expiry is the thing this package is written to
	// avoid.
	Retain time.Duration
	Now    func() time.Time
}

type Store struct {
	root             string
	maxBytes         int
	maxRecordsPerDay int
	retain           time.Duration
	now              func() time.Time
	sealer           Sealer
	write            func(context.Context, string, string, []byte) error
	writeMu          sync.Mutex
	writeWait        sync.WaitGroup
	writeClosed      bool

	mu sync.Mutex
	// countedDay and dayCount enforce MaxRecordsPerDay without listing the
	// directory on every capture. Open seeds the count from the current day's
	// directory, so a restart cannot replenish a ceiling intended to bound the
	// whole day rather than one process lifetime.
	countedDay string
	dayCount   int
	dayPending int
	// dropped says a capture was actually refused today, which is not the same
	// as the count having reached the ceiling: the record that fills the last
	// slot is written, and reporting saturation for it would announce a
	// degradation that has not happened.
	dropped bool
	// reportedDay is the day the operator has already been told about, so a
	// runaway upstream produces one line rather than one per dropped capture.
	reportedDay string
}

func Open(sealer Sealer, options Options) (*Store, error) {
	return OpenContext(context.Background(), sealer, options)
}

func OpenContext(ctx context.Context, sealer Sealer, options Options) (*Store, error) {
	if ctx == nil {
		return nil, errors.New("failure capture context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sealer == nil {
		return nil, errors.New("failure capture requires a sealer")
	}
	if strings.TrimSpace(options.Root) == "" {
		return nil, errors.New("failure capture root is required")
	}
	if options.MaxBytes <= 0 || int64(options.MaxBytes) > ((math.MaxInt64-(64<<10))/4-6*captureMetadataBytes-4097)/18 || captureFileCeiling(options.MaxBytes) >= int64(math.MaxInt) {
		return nil, errors.New("failure capture byte ceiling is required")
	}
	if options.MaxRecordsPerDay <= 0 {
		return nil, errors.New("failure capture record ceiling is required")
	}
	if options.Retain <= 0 {
		return nil, errors.New("failure capture retention is required")
	}
	if err := os.MkdirAll(options.Root, DirPerm); err != nil {
		return nil, fmt.Errorf("create failure capture directory: %w", err)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	day := now().UTC().Format("2006-01-02")
	dayCount, err := countCaptures(ctx, filepath.Join(options.Root, day), day)
	if err != nil {
		return nil, fmt.Errorf("count current failure captures: %w", err)
	}
	return &Store{
		root: options.Root, maxBytes: options.MaxBytes,
		maxRecordsPerDay: options.MaxRecordsPerDay, retain: options.Retain,
		now: now, sealer: sealer, countedDay: day, dayCount: dayCount,
		write: writeSealedCapture,
	}, nil
}

var ErrClosed = errors.New("failure capture store is closed for writes")

// Close refuses new puts and joins both accepted puts and their sealed writers.
// PutContext cancellation can return before a regular-file syscall completes;
// Close deliberately waits for that owner before the vault/data lock is closed.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.writeMu.Lock()
	s.writeClosed = true
	s.writeMu.Unlock()
	s.writeWait.Wait()
	return nil
}

func (s *Store) beginPut() bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeClosed {
		return false
	}
	s.writeWait.Add(1)
	return true
}

func countCaptures(ctx context.Context, directory, day string) (int, error) {
	count := 0
	err := dirscan.Walk(ctx, directory, func(entry os.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		_, _, capturedAt, ok := capturedIdentity(entry.Name())
		if ok && capturedAt.UTC().Format("2006-01-02") == day {
			if count == math.MaxInt {
				return errors.New("failure capture count overflow")
			}
			count++
		}
		return nil
	})
	if dirscan.Missing(err) {
		return 0, nil
	}
	return count, err
}

// Put seals one capture. It returns false when the record was dropped rather
// than written — a saturated day, an unwritable directory — so the caller can
// report the degradation once instead of per request.
func (s *Store) Put(record Record) (bool, error) {
	return s.PutContext(context.Background(), record)
}

// PrepareRecord applies the payload byte ceiling before a record is handed to
// an asynchronous owner. PutContext applies it again so direct Store callers
// get the same guarantee; the operation is idempotent.
func (s *Store) PrepareRecord(record Record) Record {
	if s == nil {
		return Record{}
	}
	record.GatewayRequest, record.GatewayRequestTruncated = truncateJSON(record.GatewayRequest, s.maxBytes)
	record.Request, record.RequestTruncated = truncateJSON(record.Request, s.maxBytes)
	record.Response, record.ResponseTruncated = truncateJSON(record.Response, s.maxBytes)
	return record
}

// PutContext is Put with a shutdown boundary. Regular-file syscalls cannot be
// interrupted once the kernel is executing them, but every user-space phase is
// cancellable so an async capture worker does not begin another expensive step
// after its owner has started shutting down.
func (s *Store) PutContext(ctx context.Context, record Record) (bool, error) {
	return s.putContext(ctx, record, resourcebudget.Permit{})
}

// PutContextWithPermit consumes one explicitly retained owner. The caller drops
// the old handle and its payload references after the call. The calibrated
// envelope already includes raw JSON, plaintext and encryption copies; retaining
// a writer keeps that entire cost live if cancellation returns ahead of IO.
func (s *Store) PutContextWithPermit(ctx context.Context, record Record, permit resourcebudget.Permit) (bool, error) {
	owner, err := permit.Transfer()
	if err != nil {
		return false, fmt.Errorf("own failure capture RAM: %w", err)
	}
	return s.putContext(ctx, record, owner)
}

func (s *Store) putContext(ctx context.Context, record Record, permit resourcebudget.Permit) (bool, error) {
	var plaintext, sealed []byte
	begun := false
	defer func() {
		record = Record{}
		clear(plaintext)
		plaintext, sealed = nil, nil
		_ = permit.Release()
		if begun {
			s.writeWait.Done()
		}
	}()
	if ctx == nil {
		return false, errors.New("failure capture context is required")
	}
	if !s.beginPut() {
		return false, ErrClosed
	}
	begun = true
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if record.RequestID == "" || record.ProjectID == "" {
		return false, errors.New("a capture must name its request and project")
	}
	if !validRecordMetadata(record) {
		return false, errors.New("failure capture metadata exceeds byte ceiling")
	}
	// The separator is part of the name format now, so neither identifier may
	// contain one: a project called "a-b" would be read back as a request that
	// ends in "a" and a project that starts at "b", and the reader would ask the
	// vault to open an envelope sealed against a different pair.
	if strings.Contains(record.RequestID, "-") || strings.Contains(record.ProjectID, "-") {
		return false, errors.New("request and project IDs may not contain the capture name separator")
	}
	if strings.ContainsAny(record.ProjectID, `/\.`) {
		return false, errors.New("project ID is not a safe file name")
	}
	if strings.ContainsAny(record.RequestID, `/\.`) {
		// The request ID becomes a filename. Every ID the gateway holds today
		// is one it minted itself — gatewayapi generates it and never reads a
		// client header for it — so nothing reaches this line that could
		// contain a separator. That is exactly why the check is here: this
		// package must not inherit that property by assumption, because the
		// day an operator-supplied or upstream-supplied ID is threaded through
		// is the day "../../master.key" would choose where a file lands.
		return false, errors.New("request ID is not a safe file name")
	}
	if record.ProviderCode != provider.SafeProviderIdentifier(record.ProviderCode) ||
		record.ProviderRequestID != provider.SafeProviderIdentifier(record.ProviderRequestID) {
		return false, errors.New("provider identifiers must be bounded identifiers")
	}
	if !record.ProviderFailureReason.Valid() {
		return false, errors.New("provider failure reason is invalid")
	}
	if record.OfferingID != "" || record.ProfileID != "" {
		identity, ok := domain.IdentityForProfile(record.ProfileID)
		if !ok || record.OfferingID == "" || identity.Offering != record.OfferingID {
			return false, errors.New("provider offering and profile attribution must match")
		}
		if !domain.AccountRegionBelongsToSurface(identity.Surface, record.AccountRegionID) {
			return false, errors.New("provider account region does not belong to the profile surface")
		}
	} else if record.AccountRegionID != domain.RegionNone {
		return false, errors.New("provider account region requires provider profile attribution")
	}
	record.CapturedAt = s.now().UTC()
	record = s.PrepareRecord(record)

	day := record.CapturedAt.Format("2006-01-02")
	if !s.reserve(day) {
		return false, nil
	}
	reservationOwned := true
	defer func() {
		if reservationOwned {
			s.finishReservation(day, false)
		}
	}()
	var err error
	plaintext, err = json.Marshal(record)
	if err != nil {
		return false, fmt.Errorf("encode failure capture: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	sealed, err = s.sealer.EncryptFailurePayload(record.RequestID, record.ProjectID, plaintext)
	clear(plaintext)
	plaintext = nil
	if err != nil {
		return false, fmt.Errorf("seal failure capture: %w", err)
	}
	if int64(len(sealed)) > captureFileCeiling(s.maxBytes) {
		return false, errors.New("sealed failure capture exceeds byte ceiling")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	directory := filepath.Join(s.root, day)
	if err := os.MkdirAll(directory, DirPerm); err != nil {
		return false, fmt.Errorf("create failure capture day: %w", err)
	}
	path := filepath.Join(directory, captureName(record.CapturedAt, record.RequestID, record.ProjectID))
	type result struct {
		committed bool
		err       error
	}
	write := func(payload []byte) result {
		defer clear(payload)
		err := s.write(ctx, directory, path, payload)
		committed := err == nil
		if err != nil {
			// rename is the commit point. A following directory fsync can fail
			// even though the record is visible and consumes the day's budget.
			_, statErr := os.Stat(path)
			committed = statErr == nil
		}
		s.finishReservation(day, committed)
		return result{committed: committed, err: err}
	}
	// Put is still synchronous. A cancellable worker gets one extra goroutine
	// only while its single active filesystem operation is in progress; this
	// lets queue cancellation return while the store's Close still joins IO.
	if ctx.Done() == nil {
		reservationOwned = false
		completed := write(sealed)
		if completed.err != nil {
			return false, fmt.Errorf("write failure capture: %w", completed.err)
		}
		return completed.committed, nil
	}
	var writerPermit resourcebudget.Permit
	if permit != (resourcebudget.Permit{}) {
		writerPermit, err = permit.Retain()
		if err != nil {
			return false, fmt.Errorf("retain failure capture writer RAM: %w", err)
		}
	}
	reservationOwned = false
	// This child is registered while its accepted parent still has a positive
	// wait count. Close may already be waiting; it cannot see a false zero.
	s.writeWait.Add(1)
	completed := make(chan result, 1)
	go func(payload []byte, owner resourcebudget.Permit) {
		defer s.writeWait.Done()
		defer func() { payload = nil; _ = owner.Release() }()
		completed <- write(payload)
	}(sealed, writerPermit)
	select {
	case outcome := <-completed:
		if outcome.err != nil {
			return false, fmt.Errorf("write failure capture: %w", outcome.err)
		}
		return outcome.committed, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// writeSealedCapture puts the envelope on disk whole or not at all.
//
// A plain write can be interrupted, and the moment it is most likely to be is
// exactly when captures are densest — an upstream failing under load is also
// when a process is most likely to be killed. What that leaves is a truncated
// GCM envelope, and every reader of it fails authentication: the admin endpoint
// reports "no captured payload", so the operator is told nothing was captured
// while the file sits there until its retention expires. Told nothing was kept
// is worse than told the capture is damaged.
//
// This is the same temp-write-fsync-rename-fsync sequence every other durable
// write here uses; internal/durable exists so that there is one of them rather
// than six, and this was the one place that had grown its own.
func writeSealedCapture(ctx context.Context, directory, path string, sealed []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".capture-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(FilePerm); err != nil {
		temporary.Close()
		return err
	}
	if err := ctx.Err(); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(sealed); err != nil {
		temporary.Close()
		return err
	}
	if err := ctx.Err(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	return durable.SyncDirectory(directory)
}

// reserve takes one slot from the day's budget, reporting false once the day is
// full. It also answers whether this is the first refusal of the day, which is
// what the caller turns into a single degradation notice.
// Saturation is what the day's budget looks like from outside: how much of it
// has been spent, what it is, and whether it has already been hit. The last is
// the one worth alerting on — past it, real failures stop being captured, and
// the process log says so exactly once.
type Saturation struct {
	Day       string
	Captured  int
	DayLimit  int
	Saturated bool
}

// Saturation reports the day's capture budget so it can be watched rather than
// discovered afterwards in a log line.
func (s *Store) Saturation() Saturation {
	if s == nil {
		return Saturation{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDayLocked(s.now().UTC().Format("2006-01-02"))
	return Saturation{
		Day: s.countedDay, Captured: s.dayCount,
		DayLimit: s.maxRecordsPerDay, Saturated: s.dropped,
	}
}

func (s *Store) reserve(day string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDayLocked(day)
	if s.dayCount+s.dayPending >= s.maxRecordsPerDay {
		s.dropped = true
		return false
	}
	s.dayPending++
	return true
}

func (s *Store) rollDayLocked(day string) {
	if s.countedDay != day {
		s.countedDay, s.dayCount, s.dayPending, s.dropped = day, 0, 0, false
	}
}

func (s *Store) finishReservation(day string, committed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.countedDay != day || s.dayPending == 0 {
		return
	}
	s.dayPending--
	if committed {
		s.dayCount++
	}
}

// Saturated reports whether the day in progress has hit its ceiling, and
// reports it only once per day, so the caller logs one line rather than one per
// dropped capture.
func (s *Store) Saturated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDayLocked(s.now().UTC().Format("2006-01-02"))
	if !s.dropped || s.reportedDay == s.countedDay {
		return false
	}
	s.reportedDay = s.countedDay
	return true
}

// Get opens one capture. projectID is not a filter but part of the key: the
// envelope is bound to it, so a record cannot be read under the wrong project
// even by a caller that knows the request ID.
// ProjectOf answers which project a capture was sealed against, so a reader can
// open it without already knowing.
//
// The endpoint used to take the project from the usage aggregate, which holds a
// bounded console window: past it a capture that was still on disk and still
// inside its retention became unopenable, and the caller was told the usage
// request did not exist. The project comes from the name now, and the envelope
// is still sealed against it — this makes the capture findable, not readable.
func (s *Store) ProjectOf(requestID string) (string, bool, error) {
	return s.ProjectOfContext(context.Background(), requestID)
}

func (s *Store) ProjectOfContext(ctx context.Context, requestID string) (string, bool, error) {
	if requestID == "" || strings.ContainsAny(requestID, `/\.`) {
		return "", false, nil
	}
	_, project, _, _, found, err := s.find(ctx, requestID, "")
	return project, found, err
}

func (s *Store) Get(requestID, projectID string) (Record, bool, error) {
	return s.GetContext(context.Background(), requestID, projectID)
}

func (s *Store) GetContext(ctx context.Context, requestID, projectID string) (Record, bool, error) {
	if requestID == "" || projectID == "" || strings.ContainsAny(requestID, `/\.`) {
		return Record{}, false, nil
	}
	before := ""
	for {
		path, _, capturedAt, day, found, err := s.find(ctx, requestID, before)
		if err != nil {
			return Record{}, false, err
		}
		if !found {
			return Record{}, false, nil
		}
		before = day
		// Expiry is a read-time authorization boundary, not a promise that the
		// periodic physical sweep always wins its scheduling race. At the exact
		// TTL boundary the payload is already unavailable even if deletion is
		// delayed or repeatedly fails.
		if !s.now().UTC().Before(capturedAt.Add(s.retain)) {
			return Record{}, false, nil
		}
		if err := ctx.Err(); err != nil {
			return Record{}, false, err
		}
		sealed, err := s.readCapture(ctx, path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Record{}, false, fmt.Errorf("read failure capture: %w", err)
		}
		plaintext, err := s.sealer.DecryptFailurePayload(requestID, projectID, sealed)
		if err != nil {
			// The envelope is bound to the request and project. A failure here
			// is a record that does not belong to what was asked for, or one
			// that was tampered with; both are "not found" to the caller and a
			// refusal rather than a best guess.
			return Record{}, false, fmt.Errorf("open failure capture: %w", err)
		}
		var record Record
		err = json.Unmarshal(plaintext, &record)
		clear(plaintext)
		if err != nil {
			return Record{}, false, fmt.Errorf("decode failure capture: %w", err)
		}
		if !validRecordMetadata(record) {
			return Record{}, false, errors.New("stored failure capture metadata exceeds byte ceiling")
		}
		return record, true, nil
	}
}

func (s *Store) readCapture(ctx context.Context, path string) (raw []byte, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	limit := captureFileCeiling(s.maxBytes)
	if !identity.Mode().IsRegular() || identity.Size() < 0 || identity.Size() > limit {
		return nil, errors.New("failure capture file exceeds its byte ceiling or is not regular")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, f.Close())
		if resultErr != nil {
			clear(raw)
			raw = nil
		}
	}()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(identity, actual) {
		return nil, errors.Join(dirscan.ErrIdentity, err)
	}
	raw, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.Join(errors.New("read failure capture exceeds byte ceiling"), err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}

// Purge removes every capture older than the retention window, one record at a
// time. Retention is the promise an operator repeats to their own compliance
// people, so the configured value has to be the number that is true.
//
// A directory that is not a day, and a file that is not a capture, are both
// left alone: this store deletes on a clock, and deleting what it does not
// recognise is how a retention sweep becomes a data loss incident.
func (s *Store) Purge() error { return s.PurgeContext(context.Background()) }

func (s *Store) PurgeContext(ctx context.Context) error {
	cutoff := s.now().UTC().Add(-s.retain)
	var first error
	var failures uint64
	note := func(err error) {
		if err != nil {
			if first == nil {
				first = err
			}
			if failures != ^uint64(0) {
				failures++
			}
		}
	}
	err := dirscan.Walk(ctx, s.root, func(dayEntry os.DirEntry) error {
		if !dayEntry.IsDir() {
			return nil
		}
		day := dayEntry.Name()
		directory := filepath.Join(s.root, day)
		remaining := false
		err := dirscan.Walk(ctx, directory, func(entry os.DirEntry) error {
			_, _, capturedAt, ours := capturedIdentity(entry.Name())
			if entry.IsDir() || !ours || capturedAt.After(cutoff) {
				remaining = true
				return nil
			}
			if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
				note(err)
				remaining = true
			}
			return nil
		})
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err != nil {
			note(err)
			return nil
		}
		if _, err := time.Parse("2006-01-02", day); err == nil && !remaining {
			note(os.Remove(directory))
		}
		return nil
	})
	if dirscan.Missing(err) {
		err = nil
	}
	if first != nil {
		err = errors.Join(err, fmt.Errorf("failure capture purge had %d errors: %w", failures, first))
	}
	return err
}

// find preserves the old newest-directory/first-filename ordering without
// collecting days or entries. A vanished selected file can retry older days.
func (s *Store) find(ctx context.Context, requestID, before string) (path, project string, capturedAt time.Time, day string, found bool, resultErr error) {
	resultErr = dirscan.Walk(ctx, s.root, func(entry os.DirEntry) error {
		name := entry.Name()
		if !entry.IsDir() || before != "" && name >= before || found && name <= day {
			return nil
		}
		candidate, candidateProject, stamp, ok, err := s.locateContext(ctx, name, requestID)
		if err != nil {
			return err
		}
		if ok {
			path, project, capturedAt, day, found = candidate, candidateProject, stamp, name, true
		}
		return nil
	})
	if dirscan.Missing(resultErr) {
		resultErr = nil
	}
	if resultErr != nil {
		return "", "", time.Time{}, "", false, resultErr
	}
	return
}

func (s *Store) locateContext(ctx context.Context, day, requestID string) (path, project string, capturedAt time.Time, found bool, err error) {
	directory := filepath.Join(s.root, day)
	var chosen string
	err = dirscan.Walk(ctx, directory, func(entry os.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		id, candidateProject, stamp, ours := capturedIdentity(name)
		if ours && id == requestID && (!found || name < chosen) {
			chosen, path, project, capturedAt, found = name, filepath.Join(directory, name), candidateProject, stamp, true
		}
		return nil
	})
	if dirscan.Missing(err) {
		return "", "", time.Time{}, false, nil
	}
	if err != nil {
		return "", "", time.Time{}, false, fmt.Errorf("list failure captures: %w", err)
	}
	return
}

// truncateJSON bounds one side of a capture.
//
// An over-long value is replaced by a JSON string holding its bounded prefix,
// rather than being cut mid-structure: a truncated JSON document does not parse,
// and a reader handed one cannot tell "the upstream sent this" from "we cut it
// here". The companion flag says which happened.
//
// The bound is applied to what is *stored*, not to the prefix taken before
// escaping. Cutting the raw bytes first and marshalling afterwards let a
// control-character payload out at roughly six times the configured size —
// every byte becoming `\u00XX` — which is not a ceiling an operator can size a
// disk against.
func truncateJSON(payload json.RawMessage, maxBytes int) (json.RawMessage, bool) {
	if len(payload) <= maxBytes {
		return payload, false
	}
	// Escaping can only grow a string, never shrink it, so shrinking the prefix
	// until the encoded form fits terminates: each pass cuts by at least the
	// overshoot, and the loop is bounded by the payload's length.
	prefix := payload[:maxBytes]
	for {
		encoded, err := json.Marshal(string(prefix))
		if err != nil {
			return nil, true
		}
		if len(encoded) <= maxBytes || len(prefix) == 0 {
			return encoded, true
		}
		overshoot := len(encoded) - maxBytes
		if overshoot >= len(prefix) {
			prefix = prefix[:0]
			continue
		}
		prefix = prefix[:len(prefix)-overshoot]
	}
}
