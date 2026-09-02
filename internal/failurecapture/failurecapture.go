// Package failurecapture keeps the request a failed call carried and the answer
// the upstream gave it, so an operator can reproduce the failure instead of
// guessing at it.
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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
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

// captureName is `<unix-millis>-<request-id>.hfc`.
//
// The timestamp is in the name so the sweep can date a record without opening
// it, and so retention is per record rather than per day — day granularity made
// `retain` a floor rather than a ceiling, keeping a record configured for an
// hour the better part of a day. It is not the file's mtime for two reasons: an
// mtime is not governed by this store's clock, so the guarantee could not be
// tested; and a `cp -R` of a data directory resets every mtime forward, which
// would extend a retention window rather than shorten it.
func captureName(capturedAt time.Time, requestID string) string {
	return fmt.Sprintf("%d-%s%s", capturedAt.UnixMilli(), requestID, fileExtension)
}

// capturedRequestID reads back what captureName wrote, or reports that the
// entry is not one of ours.
func capturedRequestID(name string) (string, time.Time, bool) {
	if filepath.Ext(name) != fileExtension {
		return "", time.Time{}, false
	}
	millis, requestID, found := strings.Cut(strings.TrimSuffix(name, fileExtension), "-")
	if !found {
		return "", time.Time{}, false
	}
	stamp, err := strconv.ParseInt(millis, 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return requestID, time.UnixMilli(stamp).UTC(), true
}

// Record is what a capture holds. Every field is either produced by Halro or is
// material the caller or the upstream wrote — and the second kind is exactly
// what makes this package's guarantees necessary.
type Record struct {
	RequestID  string    `json:"request_id"`
	ProjectID  string    `json:"project_id"`
	Outcome    string    `json:"outcome"`
	CapturedAt time.Time `json:"captured_at"`
	// Request is the operation as it went upstream: provider-agnostic, and
	// already through the project's redaction policy, because capture happens
	// after that policy has run. Storing the pre-redaction form would put back
	// exactly what the policy exists to remove.
	Request json.RawMessage `json:"request,omitempty"`
	// RequestTruncated says the ceiling cut the request short, so a reader does
	// not diagnose a malformed body that is only an incomplete one.
	RequestTruncated bool `json:"request_truncated,omitempty"`
	// Response is what came back: the upstream's error body for a refusal, or
	// the answer Halro could not put on the caller's wire for a render failure.
	// Absent when the failure produced nothing to record — a refused dial has no
	// response to keep.
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

	mu sync.Mutex
	// countedDay and dayCount enforce MaxRecordsPerDay without listing the
	// directory on every capture. A restart resets the count, which is the
	// conservative direction only for the day in progress and is bounded by
	// retention either way.
	countedDay string
	dayCount   int
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
	if sealer == nil {
		return nil, errors.New("failure capture requires a sealer")
	}
	if strings.TrimSpace(options.Root) == "" {
		return nil, errors.New("failure capture root is required")
	}
	if options.MaxBytes <= 0 {
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
	return &Store{
		root: options.Root, maxBytes: options.MaxBytes,
		maxRecordsPerDay: options.MaxRecordsPerDay, retain: options.Retain,
		now: now, sealer: sealer,
	}, nil
}

// Put seals one capture. It returns false when the record was dropped rather
// than written — a saturated day, an unwritable directory — so the caller can
// report the degradation once instead of per request.
func (s *Store) Put(record Record) (bool, error) {
	if record.RequestID == "" || record.ProjectID == "" {
		return false, errors.New("a capture must name its request and project")
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
	record.CapturedAt = s.now().UTC()
	record.Request, record.RequestTruncated = truncateJSON(record.Request, s.maxBytes)
	record.Response, record.ResponseTruncated = truncateJSON(record.Response, s.maxBytes)

	day := record.CapturedAt.Format("2006-01-02")
	if !s.reserve(day) {
		return false, nil
	}
	plaintext, err := json.Marshal(record)
	if err != nil {
		return false, fmt.Errorf("encode failure capture: %w", err)
	}
	sealed, err := s.sealer.EncryptFailurePayload(record.RequestID, record.ProjectID, plaintext)
	clear(plaintext)
	if err != nil {
		return false, fmt.Errorf("seal failure capture: %w", err)
	}
	directory := filepath.Join(s.root, day)
	if err := os.MkdirAll(directory, DirPerm); err != nil {
		return false, fmt.Errorf("create failure capture day: %w", err)
	}
	path := filepath.Join(directory, captureName(record.CapturedAt, record.RequestID))
	if err := os.WriteFile(path, sealed, FilePerm); err != nil {
		return false, fmt.Errorf("write failure capture: %w", err)
	}
	return true, nil
}

// reserve takes one slot from the day's budget, reporting false once the day is
// full. It also answers whether this is the first refusal of the day, which is
// what the caller turns into a single degradation notice.
func (s *Store) reserve(day string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.countedDay != day {
		s.countedDay, s.dayCount, s.dropped = day, 0, false
	}
	if s.dayCount >= s.maxRecordsPerDay {
		s.dropped = true
		return false
	}
	s.dayCount++
	return true
}

// Saturated reports whether the day in progress has hit its ceiling, and
// reports it only once per day, so the caller logs one line rather than one per
// dropped capture.
func (s *Store) Saturated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dropped || s.reportedDay == s.countedDay {
		return false
	}
	s.reportedDay = s.countedDay
	return true
}

// Get opens one capture. projectID is not a filter but part of the key: the
// envelope is bound to it, so a record cannot be read under the wrong project
// even by a caller that knows the request ID.
func (s *Store) Get(requestID, projectID string) (Record, bool, error) {
	if requestID == "" || projectID == "" || strings.ContainsAny(requestID, `/\.`) {
		return Record{}, false, nil
	}
	days, err := s.days()
	if err != nil {
		return Record{}, false, err
	}
	for _, day := range days {
		// The name carries a timestamp as well as the request, so the file is
		// found by listing the day rather than by composing a path. A day holds
		// at most MaxRecordsPerDay entries and this is an audited admin read,
		// not a hot path.
		path, found, err := s.locate(day, requestID)
		if err != nil {
			return Record{}, false, err
		}
		if !found {
			continue
		}
		sealed, err := os.ReadFile(path)
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
		return record, true, nil
	}
	return Record{}, false, nil
}

// Purge removes every capture older than the retention window, one record at a
// time. Retention is the promise an operator repeats to their own compliance
// people, so the configured value has to be the number that is true.
//
// A directory that is not a day, and a file that is not a capture, are both
// left alone: this store deletes on a clock, and deleting what it does not
// recognise is how a retention sweep becomes a data loss incident.
func (s *Store) Purge() error {
	days, err := s.days()
	if err != nil {
		return err
	}
	cutoff := s.now().UTC().Add(-s.retain)
	var problems []error
	for _, day := range days {
		directory := filepath.Join(s.root, day)
		entries, err := os.ReadDir(directory)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		remaining := 0
		for _, entry := range entries {
			_, capturedAt, ours := capturedRequestID(entry.Name())
			if entry.IsDir() || !ours || capturedAt.After(cutoff) {
				remaining++
				continue
			}
			if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
				problems = append(problems, err)
				remaining++
			}
		}
		// An emptied day directory goes too, so the sweep is bounded rather
		// than leaving one directory per day forever. Only a directory this
		// store recognises as one of its own days, and only once nothing it
		// does not recognise is left inside: a sweep that removes an empty
		// directory somebody else put here is how retention becomes data loss.
		if _, err := time.Parse("2006-01-02", day); err != nil {
			continue
		}
		if remaining == 0 {
			if err := os.Remove(directory); err != nil {
				problems = append(problems, err)
			}
		}
	}
	return errors.Join(problems...)
}

// locate finds the capture for one request inside a day directory.
func (s *Store) locate(day, requestID string) (string, bool, error) {
	directory := filepath.Join(s.root, day)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("list failure captures: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if name, _, ours := capturedRequestID(entry.Name()); ours && name == requestID {
			return filepath.Join(directory, entry.Name()), true, nil
		}
	}
	return "", false, nil
}

func (s *Store) days() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list failure captures: %w", err)
	}
	days := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			days = append(days, entry.Name())
		}
	}
	// Newest first: a lookup by request ID almost always wants a recent
	// failure, and the days are few.
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	return days, nil
}

// truncateJSON bounds one side of a capture.
//
// An over-long value is replaced by a JSON string holding its bounded prefix,
// rather than being cut mid-structure: a truncated JSON document does not parse,
// and a reader handed one cannot tell "the upstream sent this" from "we cut it
// here". The companion flag says which happened.
func truncateJSON(payload json.RawMessage, maxBytes int) (json.RawMessage, bool) {
	if len(payload) <= maxBytes {
		return payload, false
	}
	prefix := string(payload[:maxBytes])
	encoded, err := json.Marshal(prefix)
	if err != nil {
		return nil, true
	}
	return encoded, true
}
