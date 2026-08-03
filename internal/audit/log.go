package audit

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
	"strings"
	"sync"
	"time"
)

const (
	frameMagic       = "HAUD"
	frameVersion     = 1
	frameHeaderSize  = 52
	frameMACSize     = sha256.Size
	maxPayloadSize   = 64 << 10
	auditHMACKeySize = 32
)

var ErrCorrupt = errors.New("audit log is corrupt")

type Event struct {
	EventID       string    `json:"event_id"`
	OccurredAt    time.Time `json:"occurred_at"`
	ActorType     string    `json:"actor_type"`
	ActorID       string    `json:"actor_id,omitempty"`
	Action        string    `json:"action"`
	TargetType    string    `json:"target_type,omitempty"`
	TargetID      string    `json:"target_id,omitempty"`
	Outcome       string    `json:"outcome"`
	ReasonCode    string    `json:"reason_code,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
}

func (e Event) Validate() error {
	var problems []error
	for name, value := range map[string]string{
		"event ID": e.EventID, "actor type": e.ActorType,
		"action": e.Action, "outcome": e.Outcome,
	} {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, fmt.Errorf("%s is required", name))
		}
	}
	if e.OccurredAt.IsZero() {
		problems = append(problems, errors.New("occurred at is required"))
	}
	if e.OccurredAt.Location() != time.UTC {
		problems = append(problems, errors.New("occurred at must be UTC"))
	}
	if len(e.CorrelationID) > 256 {
		problems = append(problems, errors.New("correlation ID exceeds maximum size"))
	}
	return errors.Join(problems...)
}

type Record struct {
	Sequence uint64   `json:"sequence"`
	Event    Event    `json:"event"`
	Hash     [32]byte `json:"hash"`
}

type Summary struct {
	Records  uint64   `json:"records"`
	LastHash [32]byte `json:"last_hash"`
	Bytes    int64    `json:"bytes"`
}

type Log struct {
	mu               sync.Mutex
	file             *os.File
	key              []byte
	sequence         uint64
	offset           int64
	lastHash         [32]byte
	recordsByEventID map[string]Record
}

func Open(path string, key []byte) (*Log, error) {
	if len(key) != auditHMACKeySize {
		return nil, fmt.Errorf("audit HMAC key must be %d bytes", auditHMACKeySize)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	recordsByEventID := make(map[string]Record)
	summary, partial, err := scan(file, key, func(record Record) error { recordsByEventID[record.Event.EventID] = record; return nil })
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
	keyCopy := append([]byte(nil), key...)
	return &Log{
		file: file, key: keyCopy, sequence: summary.Records,
		offset: summary.Bytes, lastHash: summary.LastHash, recordsByEventID: recordsByEventID,
	}, nil
}

func Verify(path string, key []byte) (Summary, error) {
	if len(key) != auditHMACKeySize {
		return Summary{}, fmt.Errorf("audit HMAC key must be %d bytes", auditHMACKeySize)
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
			return nil, errors.New("audit event exceeds maximum size")
		}
		payloads[index] = payload
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil, errors.New("audit log is closed")
	}
	records := make([]Record, len(events))
	sequence, offset, previous := l.sequence, l.offset, l.lastHash
	for index, payload := range payloads {
		if existing, ok := l.recordsByEventID[events[index].EventID]; ok {
			records[index] = existing
			continue
		}
		sequence++
		frame, hash := encodeFrame(l.key, sequence, previous, payload)
		if err := writeFull(l.file, frame); err != nil {
			return nil, fmt.Errorf("append audit log: %w", err)
		}
		offset += int64(len(frame))
		previous = hash
		records[index] = Record{Sequence: sequence, Event: events[index], Hash: hash}
	}
	if err := l.file.Sync(); err != nil {
		return nil, fmt.Errorf("sync audit log: %w", err)
	}
	l.sequence, l.offset, l.lastHash = sequence, offset, previous
	for index, event := range events {
		l.recordsByEventID[event.EventID] = records[index]
	}
	return records, nil
}

func (l *Log) Replay(visit func(Record) error) (Summary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return Summary{}, errors.New("audit log is closed")
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
			if err := visit(Record{Sequence: sequence, Event: event, Hash: hash}); err != nil {
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
