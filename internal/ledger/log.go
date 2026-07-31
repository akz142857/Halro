package ledger

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	frameMagic      = "HLDG"
	frameVersion    = 1
	frameHeaderSize = 24
	maxPayloadSize  = 1 << 20
)

var ErrCorrupt = errors.New("ledger is corrupt")

type Log struct {
	mu             sync.Mutex
	lifecycle      sync.RWMutex
	file           *os.File
	durability     DurabilityWriter
	path           string
	sequence       uint64
	offset         int64
	status         *Status
	options        Options
	appendQueue    chan appendRequest
	closeSignal    chan struct{}
	writerDone     chan struct{}
	closed         bool
	batches        atomic.Uint64
	writtenRecords atomic.Uint64
	appendErrors   atomic.Uint64
}

type Options struct {
	QueueCapacity int
	MaxBatch      int
	FlushInterval time.Duration
	// WrapDurability is an internal test/integration seam for deterministic
	// write and fsync fault injection. Production callers leave it nil.
	WrapDurability func(*os.File) DurabilityWriter
}

type DurabilityWriter interface {
	io.Writer
	Sync() error
}

type AppendStats struct {
	Batches       uint64
	Records       uint64
	Errors        uint64
	QueueDepth    int
	QueueCapacity int
}

type appendRequest struct {
	event   Event
	payload []byte
	result  chan appendResult
}

type appendResult struct {
	watermark Watermark
	err       error
}

func Open(path string, status *Status) (*Log, error) {
	return OpenWithOptions(path, status, Options{})
}

// Inspect verifies the existing committed WAL prefix without creating,
// truncating, syncing, or otherwise repairing the file.
func Inspect(path string) (Watermark, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return Watermark{}, false, err
	}
	defer file.Close()
	return scan(file, 0, 0, nil)
}

func OpenWithOptions(path string, status *Status, options Options) (*Log, error) {
	if status == nil {
		status = NewStatus()
	}
	if options.QueueCapacity <= 0 {
		options.QueueCapacity = 1024
	}
	if options.MaxBatch <= 0 {
		options.MaxBatch = 64
	}
	if options.MaxBatch > options.QueueCapacity {
		options.MaxBatch = options.QueueCapacity
	}
	if options.FlushInterval < 0 {
		return nil, errors.New("ledger flush interval cannot be negative")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create ledger directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		status.MarkUnavailable()
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	last, partial, err := scan(file, 0, 0, nil)
	if err != nil {
		file.Close()
		status.RequireRecovery()
		return nil, err
	}
	if partial {
		if err := file.Truncate(last.Offset); err != nil {
			file.Close()
			status.RequireRecovery()
			return nil, fmt.Errorf("truncate partial ledger tail: %w", err)
		}
		if err := file.Sync(); err != nil {
			file.Close()
			status.RequireRecovery()
			return nil, fmt.Errorf("sync repaired ledger tail: %w", err)
		}
	}
	if _, err := file.Seek(last.Offset, io.SeekStart); err != nil {
		file.Close()
		return nil, fmt.Errorf("seek ledger end: %w", err)
	}
	log := &Log{
		file:        file,
		durability:  file,
		path:        path,
		sequence:    last.Sequence,
		offset:      last.Offset,
		status:      status,
		options:     options,
		appendQueue: make(chan appendRequest, options.QueueCapacity),
		closeSignal: make(chan struct{}),
		writerDone:  make(chan struct{}),
	}
	if options.WrapDurability != nil {
		log.durability = options.WrapDurability(file)
		if log.durability == nil {
			file.Close()
			return nil, errors.New("ledger durability wrapper returned nil")
		}
	}
	go log.writerLoop()
	return log, nil
}

func (l *Log) Append(ctx context.Context, event Event) (Watermark, error) {
	if err := event.Validate(); err != nil {
		return Watermark{}, err
	}
	if err := ctx.Err(); err != nil {
		return Watermark{}, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return Watermark{}, fmt.Errorf("encode ledger event: %w", err)
	}
	if len(payload) > maxPayloadSize {
		return Watermark{}, fmt.Errorf("ledger payload exceeds %d bytes", maxPayloadSize)
	}

	if l.status.Load() != AccountingHealthy {
		return Watermark{}, errors.New("accounting is not healthy")
	}

	request := appendRequest{
		event: event, payload: payload, result: make(chan appendResult, 1),
	}
	l.lifecycle.RLock()
	if l.closed {
		l.lifecycle.RUnlock()
		return Watermark{}, errors.New("ledger is closed")
	}
	select {
	case l.appendQueue <- request:
		l.lifecycle.RUnlock()
	case <-ctx.Done():
		l.lifecycle.RUnlock()
		return Watermark{}, ctx.Err()
	case <-l.closeSignal:
		l.lifecycle.RUnlock()
		return Watermark{}, errors.New("ledger is closed")
	}
	// Once accepted, wait for the durability result even if the caller is
	// canceled: returning early would make an eventually committed event look
	// uncommitted to its caller.
	result := <-request.result
	return result.watermark, result.err
}

func (l *Log) Replay(from Watermark, visit func(Record) error) (Watermark, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return Watermark{}, errors.New("ledger is closed")
	}
	if from.Generation != 0 && from.Generation != 1 {
		return Watermark{}, fmt.Errorf("unsupported ledger generation %d", from.Generation)
	}
	last, partial, err := scan(l.file, from.Offset, from.Sequence, visit)
	if err != nil {
		l.status.RequireRecovery()
		return Watermark{}, err
	}
	if partial {
		return Watermark{}, errors.New("ledger has a partial tail while open")
	}
	return last, nil
}

// Snapshot copies exactly the committed prefix while holding the writer lock.
// Appends may queue concurrently, but no frame after the returned watermark can
// enter the snapshot file.
func (l *Log) Snapshot(path string) (Watermark, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Watermark{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return Watermark{}, errors.New("ledger is closed")
	}
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Watermark{}, fmt.Errorf("create ledger snapshot: %w", err)
	}
	complete := false
	defer func() {
		output.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	if _, err := io.CopyN(output, io.NewSectionReader(l.file, 0, l.offset), l.offset); err != nil {
		return Watermark{}, fmt.Errorf("copy ledger snapshot: %w", err)
	}
	if err := output.Sync(); err != nil {
		return Watermark{}, fmt.Errorf("sync ledger snapshot: %w", err)
	}
	if err := output.Close(); err != nil {
		return Watermark{}, fmt.Errorf("close ledger snapshot: %w", err)
	}
	complete = true
	return Watermark{Generation: 1, Offset: l.offset, Sequence: l.sequence}, nil
}

func (l *Log) Close() error {
	l.lifecycle.Lock()
	if !l.closed {
		l.closed = true
		close(l.closeSignal)
	}
	l.lifecycle.Unlock()
	<-l.writerDone
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *Log) Stats() AppendStats {
	return AppendStats{
		Batches: l.batches.Load(), Records: l.writtenRecords.Load(),
		Errors: l.appendErrors.Load(), QueueDepth: len(l.appendQueue),
		QueueCapacity: cap(l.appendQueue),
	}
}

func (l *Log) writerLoop() {
	defer close(l.writerDone)
	for {
		select {
		case first := <-l.appendQueue:
			batch := l.collectBatch(first)
			l.writeBatch(batch)
		case <-l.closeSignal:
			for {
				select {
				case request := <-l.appendQueue:
					l.writeBatch(l.collectBatch(request))
				default:
					return
				}
			}
		}
	}
}

func (l *Log) collectBatch(first appendRequest) []appendRequest {
	batch := make([]appendRequest, 1, l.options.MaxBatch)
	batch[0] = first
	if l.options.FlushInterval > 0 {
		timer := time.NewTimer(l.options.FlushInterval)
		defer timer.Stop()
		for len(batch) < l.options.MaxBatch {
			select {
			case request := <-l.appendQueue:
				batch = append(batch, request)
			case <-timer.C:
				return batch
			case <-l.closeSignal:
				return batch
			}
		}
		return batch
	}
	for len(batch) < l.options.MaxBatch {
		select {
		case request := <-l.appendQueue:
			batch = append(batch, request)
		default:
			return batch
		}
	}
	return batch
}

func (l *Log) writeBatch(batch []appendRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		respondBatch(batch, nil, errors.New("ledger is closed"))
		return
	}
	if l.status.Load() != AccountingHealthy {
		respondBatch(batch, nil, errors.New("accounting is not healthy"))
		return
	}
	sequence := l.sequence
	offset := l.offset
	watermarks := make([]Watermark, len(batch))
	var encoded bytes.Buffer
	for index, request := range batch {
		sequence++
		frame := encodeFrame(sequence, request.event.Kind, request.payload)
		if _, err := encoded.Write(frame); err != nil {
			respondBatch(batch, nil, fmt.Errorf("encode ledger batch: %w", err))
			return
		}
		offset += int64(len(frame))
		watermarks[index] = Watermark{Generation: 1, Offset: offset, Sequence: sequence}
	}
	if err := writeFull(l.durability, encoded.Bytes()); err != nil {
		l.status.MarkUnavailable()
		l.appendErrors.Add(uint64(len(batch)))
		respondBatch(batch, nil, fmt.Errorf("append ledger: %w", err))
		return
	}
	if err := l.durability.Sync(); err != nil {
		l.status.MarkUnavailable()
		l.appendErrors.Add(uint64(len(batch)))
		respondBatch(batch, nil, fmt.Errorf("sync ledger: %w", err))
		return
	}
	l.sequence = sequence
	l.offset = offset
	l.batches.Add(1)
	l.writtenRecords.Add(uint64(len(batch)))
	respondBatch(batch, watermarks, nil)
}

func respondBatch(batch []appendRequest, watermarks []Watermark, err error) {
	for index, request := range batch {
		var watermark Watermark
		if index < len(watermarks) {
			watermark = watermarks[index]
		}
		request.result <- appendResult{watermark: watermark, err: err}
	}
}

func encodeFrame(sequence uint64, kind EventKind, payload []byte) []byte {
	frame := make([]byte, frameHeaderSize+len(payload))
	copy(frame[:4], frameMagic)
	frame[4] = frameVersion
	frame[5] = byte(kind)
	binary.BigEndian.PutUint64(frame[8:16], sequence)
	binary.BigEndian.PutUint32(frame[16:20], uint32(len(payload)))
	copy(frame[frameHeaderSize:], payload)
	checksum := crc32.NewIEEE()
	checksum.Write(frame[4:20])
	checksum.Write(payload)
	binary.BigEndian.PutUint32(frame[20:24], checksum.Sum32())
	return frame
}

func scan(file io.ReadSeeker, fromOffset int64, initialSequence uint64, visit func(Record) error) (Watermark, bool, error) {
	if fromOffset < 0 {
		return Watermark{}, false, errors.New("ledger offset cannot be negative")
	}
	if _, err := file.Seek(fromOffset, io.SeekStart); err != nil {
		return Watermark{}, false, err
	}
	offset := fromOffset
	lastSequence := initialSequence
	for {
		header := make([]byte, frameHeaderSize)
		n, err := io.ReadFull(file, header)
		if errors.Is(err, io.EOF) && n == 0 {
			return Watermark{Generation: 1, Offset: offset, Sequence: lastSequence}, false, nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return Watermark{Generation: 1, Offset: offset, Sequence: lastSequence}, true, nil
		}
		if err != nil {
			return Watermark{}, false, err
		}
		if string(header[:4]) != frameMagic || header[4] != frameVersion {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: invalid frame header", ErrCorrupt, offset)
		}
		kind := EventKind(header[5])
		if !kind.Valid() {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: invalid event kind", ErrCorrupt, offset)
		}
		sequence := binary.BigEndian.Uint64(header[8:16])
		if sequence <= lastSequence {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: non-monotonic sequence", ErrCorrupt, offset)
		}
		payloadLength := binary.BigEndian.Uint32(header[16:20])
		if payloadLength > maxPayloadSize {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: payload too large", ErrCorrupt, offset)
		}
		payload := make([]byte, payloadLength)
		if _, err := io.ReadFull(file, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return Watermark{Generation: 1, Offset: offset, Sequence: lastSequence}, true, nil
			}
			return Watermark{}, false, err
		}
		checksum := crc32.NewIEEE()
		checksum.Write(header[4:20])
		checksum.Write(payload)
		if checksum.Sum32() != binary.BigEndian.Uint32(header[20:24]) {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: checksum mismatch", ErrCorrupt, offset)
		}
		var event Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: invalid event: %v", ErrCorrupt, offset, err)
		}
		if event.Kind != kind {
			return Watermark{}, false, fmt.Errorf("%w at offset %d: event kind mismatch", ErrCorrupt, offset)
		}
		nextOffset := offset + int64(frameHeaderSize) + int64(payloadLength)
		if visit != nil {
			if err := visit(Record{Sequence: sequence, Offset: nextOffset, Event: event}); err != nil {
				return Watermark{}, false, err
			}
		}
		offset = nextOffset
		lastSequence = sequence
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
