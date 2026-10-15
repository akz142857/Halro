package governance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

const (
	frameMagic            = "HGOV"
	frameVersion          = 1
	frameHeaderSize       = 52
	frameMACSize          = sha256.Size
	maxPayloadSize        = 64 << 10
	governanceHMACKeySize = 32

	// maxTrackedEventIDs bounds the in-memory dedup index. An event ID is only
	// ever re-appended within a short window of its first append — a retry
	// inside one request, or the startup drain re-delivering an intent that was
	// in flight when the process died — so a tail window answers every real
	// duplicate. Retaining the whole log instead would let an append-only file
	// pin every historical event, and its payload, in memory for the life of
	// the process.
	maxTrackedEventIDs = 4096
)

var ErrCorrupt = errors.New("governance journal is corrupt")

type Event struct {
	EventID             string    `json:"event_id"`
	ProjectID           string    `json:"project_id"`
	WorkUnitID          string    `json:"work_unit_id"`
	DefinitionID        string    `json:"definition_id"`
	DefinitionVersion   uint64    `json:"definition_version"`
	OutcomeID           string    `json:"outcome_id"`
	Value               string    `json:"value"`
	ReporterKeyID       string    `json:"reporter_key_id"`
	EvidenceSHA256      string    `json:"evidence_sha256,omitempty"`
	EvidenceRef         string    `json:"evidence_ref,omitempty"`
	ObservedAt          time.Time `json:"observed_at"`
	IngestedAt          time.Time `json:"ingested_at"`
	SupersedesOutcomeID string    `json:"supersedes_outcome_id,omitempty"`
	Revision            uint64    `json:"revision"`
	IdempotencyKeyHash  string    `json:"idempotency_key_hash"`
	RequestFingerprint  string    `json:"request_fingerprint"`
}

func (e Event) Validate() error {
	outcome := domain.Outcome{
		ID: e.OutcomeID, ProjectID: e.ProjectID, WorkUnitID: e.WorkUnitID,
		DefinitionID: e.DefinitionID, DefinitionVersion: e.DefinitionVersion,
		Value: e.Value, ReporterKeyID: e.ReporterKeyID,
		EvidenceSHA256: e.EvidenceSHA256, EvidenceRef: e.EvidenceRef,
		ObservedAt: e.ObservedAt, IngestedAt: e.IngestedAt,
		SupersedesOutcomeID: e.SupersedesOutcomeID, Revision: e.Revision,
	}
	if strings.TrimSpace(e.EventID) == "" || !domain.ValidSHA256Label(e.IdempotencyKeyHash) ||
		!domain.ValidSHA256Label(e.RequestFingerprint) {
		return errors.New("governance event identity is invalid")
	}
	if e.IngestedAt.Location() != time.UTC {
		return errors.New("ingested_at must be UTC")
	}
	return outcome.Validate()
}

type Record struct {
	Sequence uint64   `json:"sequence"`
	Offset   int64    `json:"offset"`
	Event    Event    `json:"event"`
	Hash     [32]byte `json:"hash"`
}

type Summary struct {
	Records  uint64   `json:"records"`
	LastHash [32]byte `json:"last_hash"`
	Bytes    int64    `json:"bytes"`
}

const (
	journalAnchorVersion  = 1
	checkpointAuthVersion = 1
)

// JournalAnchorAuth authenticates a durable high-water mark kept outside the
// Journal. The external anchor is what makes deletion of an otherwise valid
// suffix observable; the per-frame chain alone can only authenticate the
// prefix that remains on disk.
func JournalAnchorAuth(key []byte, sequence uint64, offset int64, hash [32]byte) [32]byte {
	return governanceMAC(key, "halro:governance-journal-anchor:v1", journalAnchorVersion, sequence, offset, hash, [32]byte{})
}

func VerifyJournalAnchorAuth(key []byte, sequence uint64, offset int64, hash, authentication [32]byte) bool {
	expected := JournalAnchorAuth(key, sequence, offset, hash)
	return hmac.Equal(expected[:], authentication[:])
}

// CheckpointAuth binds a rebuildable snapshot to both its bytes and the exact
// authenticated Journal frame it projects through.
func CheckpointAuth(key []byte, sequence uint64, offset int64, journalHash, payloadHash [32]byte) [32]byte {
	return governanceMAC(key, "halro:governance-checkpoint:v1", checkpointAuthVersion, sequence, offset, journalHash, payloadHash)
}

func VerifyCheckpointAuth(key []byte, sequence uint64, offset int64, journalHash, payloadHash, authentication [32]byte) bool {
	expected := CheckpointAuth(key, sequence, offset, journalHash, payloadHash)
	return hmac.Equal(expected[:], authentication[:])
}

func governanceMAC(key []byte, domain string, version int, sequence uint64, offset int64, first, second [32]byte) [32]byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain))
	var encoded [24]byte
	binary.BigEndian.PutUint64(encoded[0:8], uint64(version))
	binary.BigEndian.PutUint64(encoded[8:16], sequence)
	binary.BigEndian.PutUint64(encoded[16:24], uint64(offset))
	mac.Write(encoded[:])
	mac.Write(first[:])
	mac.Write(second[:])
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

type Log struct {
	mu               sync.Mutex
	file             *os.File
	key              []byte
	sequence         uint64
	offset           int64
	lastHash         [32]byte
	recordsByEventID map[string]Record
	trackedOrder     []string
}

// remember indexes a record for duplicate detection and evicts the oldest
// entries past maxTrackedEventIDs. Callers must hold l.mu, or hold the only
// reference to l as Open does while building it.
func (l *Log) remember(record Record) {
	eventID := record.Event.EventID
	if _, tracked := l.recordsByEventID[eventID]; !tracked {
		l.trackedOrder = append(l.trackedOrder, eventID)
	}
	l.recordsByEventID[eventID] = record
	for len(l.trackedOrder) > maxTrackedEventIDs {
		delete(l.recordsByEventID, l.trackedOrder[0])
		l.trackedOrder = l.trackedOrder[1:]
	}
}

func Open(path string, key []byte) (*Log, error) {
	if len(key) != governanceHMACKeySize {
		return nil, fmt.Errorf("audit HMAC key must be %d bytes", governanceHMACKeySize)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open governance journal: %w", err)
	}
	log := &Log{file: file, recordsByEventID: make(map[string]Record)}
	summary, partial, err := scan(file, key, func(record Record) error { log.remember(record); return nil })
	if err != nil {
		file.Close()
		return nil, err
	}
	if partial {
		if err := file.Truncate(summary.Bytes); err != nil {
			file.Close()
			return nil, fmt.Errorf("truncate partial audit tail: %w", err)
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return nil, fmt.Errorf("sync repaired audit tail: %w", err)
		}
	}
	if _, err := file.Seek(summary.Bytes, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	log.key = append([]byte(nil), key...)
	log.sequence = summary.Records
	log.offset = summary.Bytes
	log.lastHash = summary.LastHash
	return log, nil
}

func Verify(path string, key []byte) (Summary, error) {
	if len(key) != governanceHMACKeySize {
		return Summary{}, fmt.Errorf("audit HMAC key must be %d bytes", governanceHMACKeySize)
	}
	file, err := os.Open(path)
	if err != nil {
		return Summary{}, err
	}
	defer file.Close()
	summary, partial, err := scan(file, key, nil)
	if err != nil {
		return Summary{}, err
	}
	if partial {
		return Summary{}, fmt.Errorf("%w: partial final record", ErrCorrupt)
	}
	return summary, nil
}

func (l *Log) Append(ctx context.Context, event Event) (Record, error) {
	records, err := l.AppendBatch(ctx, []Event{event})
	if err != nil {
		return Record{}, err
	}
	return records[0], nil
}

// AppendBatch writes a consecutive hash-chain segment and makes it durable
// with one fsync. Either every returned record is durable or an error is
// returned and the log must be treated as unavailable by the caller.
func (l *Log) AppendBatch(ctx context.Context, events []Event) ([]Record, error) {
	if len(events) == 0 {
		return nil, nil
	}
	payloads := make([][]byte, len(events))
	for index, event := range events {
		if err := event.Validate(); err != nil {
			return nil, err
		}
		payload, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		if len(payload) > maxPayloadSize {
			return nil, errors.New("governance event exceeds maximum size")
		}
		payloads[index] = payload
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil, errors.New("governance journal is closed")
	}
	records := make([]Record, len(events))
	sequence, offset, previous := l.sequence, l.offset, l.lastHash
	for index, payload := range payloads {
		if existing, ok := l.recordsByEventID[events[index].EventID]; ok {
			if !reflect.DeepEqual(existing.Event, events[index]) {
				return nil, errors.New("governance event ID conflicts with a different payload")
			}
			records[index] = existing
			continue
		}
		sequence++
		frame, hash := encodeFrame(l.key, sequence, previous, payload)
		if err := writeFull(l.file, frame); err != nil {
			return nil, fmt.Errorf("append governance journal: %w", err)
		}
		offset += int64(len(frame))
		previous = hash
		records[index] = Record{Sequence: sequence, Offset: offset, Event: events[index], Hash: hash}
	}
	if err := l.file.Sync(); err != nil {
		return nil, fmt.Errorf("sync governance journal: %w", err)
	}
	l.sequence, l.offset, l.lastHash = sequence, offset, previous
	for index := range records {
		l.remember(records[index])
	}
	return records, nil
}

func (l *Log) Replay(visit func(Record) error) (Summary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return Summary{}, errors.New("governance journal is closed")
	}
	summary, partial, err := scan(l.file, l.key, visit)
	if err != nil {
		return Summary{}, err
	}
	if partial {
		return Summary{}, fmt.Errorf("%w: partial final record", ErrCorrupt)
	}
	return summary, nil
}

func (l *Log) Summary() Summary {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
}

func (l *Log) AuthenticatedHead() (Summary, [32]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	summary := Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
	return summary, JournalAnchorAuth(l.key, summary.Records, summary.Bytes, summary.LastHash)
}

func (l *Log) AuthenticatedCheckpoint(payload []byte) (Summary, [32]byte, [32]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	summary := Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
	payloadHash := sha256.Sum256(payload)
	authentication := CheckpointAuth(l.key, summary.Records, summary.Bytes, summary.LastHash, payloadHash)
	return summary, payloadHash, authentication
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	clear(l.key)
	l.key = nil
	return err
}

func encodeFrame(key []byte, sequence uint64, previous [32]byte, payload []byte) ([]byte, [32]byte) {
	frame := make([]byte, frameHeaderSize+len(payload)+frameMACSize)
	copy(frame[:4], frameMagic)
	frame[4] = frameVersion
	binary.BigEndian.PutUint64(frame[8:16], sequence)
	binary.BigEndian.PutUint32(frame[16:20], uint32(len(payload)))
	copy(frame[20:52], previous[:])
	copy(frame[frameHeaderSize:], payload)
	mac := hmac.New(sha256.New, key)
	mac.Write(frame[:frameHeaderSize+len(payload)])
	copy(frame[frameHeaderSize+len(payload):], mac.Sum(nil))
	return frame, sha256.Sum256(frame)
}

func scan(file *os.File, key []byte, visit func(Record) error) (Summary, bool, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Summary{}, false, err
	}
	var summary Summary
	for {
		header := make([]byte, frameHeaderSize)
		n, err := io.ReadFull(file, header)
		if errors.Is(err, io.EOF) && n == 0 {
			return summary, false, nil
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return summary, true, nil
		}
		if err != nil {
			return Summary{}, false, err
		}
		if string(header[:4]) != frameMagic || header[4] != frameVersion {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid header", ErrCorrupt, summary.Bytes)
		}
		sequence := binary.BigEndian.Uint64(header[8:16])
		if sequence != summary.Records+1 {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid sequence", ErrCorrupt, summary.Bytes)
		}
		if !hmac.Equal(header[20:52], summary.LastHash[:]) {
			return Summary{}, false, fmt.Errorf("%w at offset %d: broken hash chain", ErrCorrupt, summary.Bytes)
		}
		payloadLength := binary.BigEndian.Uint32(header[16:20])
		if payloadLength > maxPayloadSize {
			return Summary{}, false, fmt.Errorf("%w at offset %d: payload too large", ErrCorrupt, summary.Bytes)
		}
		tail := make([]byte, int(payloadLength)+frameMACSize)
		if _, err := io.ReadFull(file, tail); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return summary, true, nil
			}
			return Summary{}, false, err
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(header)
		mac.Write(tail[:payloadLength])
		if !hmac.Equal(mac.Sum(nil), tail[payloadLength:]) {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid HMAC", ErrCorrupt, summary.Bytes)
		}
		var event Event
		if err := json.Unmarshal(tail[:payloadLength], &event); err != nil {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid event", ErrCorrupt, summary.Bytes)
		}
		if err := event.Validate(); err != nil {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid event: %v", ErrCorrupt, summary.Bytes, err)
		}
		frame := append(header, tail...)
		hash := sha256.Sum256(frame)
		summary.Records = sequence
		summary.Bytes += int64(len(frame))
		summary.LastHash = hash
		if visit != nil {
			if err := visit(Record{Sequence: sequence, Offset: summary.Bytes, Event: event, Hash: hash}); err != nil {
				return Summary{}, false, err
			}
		}
	}
}

func writeFull(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
