package governance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/ownedfile"
	"io"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const (
	AuthenticatedGovernanceSealIndexID uint16 = 1
	AuthenticatedGovernanceSealCodec   uint16 = 1
	GovernanceSealScanRAMBytes         int64  = 512 << 10
)

var ErrGovernanceSealSource = errors.New("governance seal source cut is not eligible")

// SealReference binds a fully reauthenticated Governance journal prefix to its
// final native frame. expected must be independent startup authority; a cached
// Summary or checkpoint alone does not prove that the source bytes remain.
// The complete prefix is scanned with fixed per-frame memory.
func (l *Log) SealReference(ctx context.Context, budget *resourcebudget.Manager, expected Summary) (result GovernanceSealReference, resultErr error) {
	if l == nil || budget == nil {
		return result, ErrGovernanceSealSource
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	permit, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: GovernanceSealScanRAMBytes})
	if err != nil {
		return result, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, permit.Release())
		}
		if resultErr != nil {
			result = GovernanceSealReference{}
		}
	}()

	l.mu.Lock()
	if l.startupAudit != nil && !l.startupAudit.Snapshot().Complete {
		l.mu.Unlock()
		return result, ErrStartupAuditPending
	}
	if l.file == nil || l.closing || l.terminalErr != nil || expected.Records > l.sequence || expected.Bytes < 0 || expected.Bytes > l.offset ||
		(expected.Records == 0 && (expected.Bytes != 0 || expected.LastHash != ([32]byte{}))) ||
		(expected.Records > 0 && (expected.Bytes == 0 || expected.LastHash == ([32]byte{}))) {
		l.mu.Unlock()
		return result, errors.Join(ErrGovernanceSealSource, l.terminalErr)
	}
	current := Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
	actual, err := l.file.Stat()
	if err != nil {
		l.mu.Unlock()
		return result, errors.Join(ErrGovernanceSealSource, err)
	}
	named, err := os.Lstat(l.path)
	if err != nil || !actual.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(actual, named) || actual.Size() != current.Bytes || named.Size() != current.Bytes {
		l.mu.Unlock()
		return result, errors.Join(ErrGovernanceSealSource, err)
	}
	file, err := os.OpenFile(l.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		l.mu.Unlock()
		return result, errors.Join(ErrGovernanceSealSource, err)
	}
	closeFile := l.closeReaderFile
	if closeFile == nil {
		closeFile = (*os.File).Close
	}
	key := append([]byte(nil), l.key...)
	l.readerWG.Add(1)
	l.mu.Unlock()
	defer l.readerWG.Done()
	defer func() { resultErr = errors.Join(resultErr, l.retireReader(file, closeFile)) }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(actual, opened) || opened.Size() != current.Bytes {
		return result, errors.Join(ErrGovernanceSealSource, err)
	}
	defer clear(key)

	var firstHash [32]byte
	var last Record
	var lastFrameStart int64
	var lastFrameLength uint32
	reader := io.NewSectionReader(file, 0, expected.Bytes)
	verified, partial, err := scanSealPrefix(ctx, reader, key, func(record Record, start int64, length uint32) error {
		if record.Sequence == 1 {
			firstHash = record.Hash
		}
		last = record
		lastFrameStart, lastFrameLength = start, length
		return ctx.Err()
	})
	if err != nil || partial || verified != expected {
		return result, errors.Join(ErrGovernanceSealSource, fmt.Errorf("%w: authenticated prefix mismatch (verified=%+v partial=%t expected=%+v): %v", ErrCorrupt, verified, partial, expected, err))
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	finalInfo, err := file.Stat()
	finalName, nameErr := os.Lstat(l.path)
	if err != nil || nameErr != nil || !os.SameFile(opened, finalInfo) || !os.SameFile(opened, finalName) || finalInfo.Size() < expected.Bytes || finalName.Size() < expected.Bytes {
		return result, errors.Join(ErrGovernanceSealSource, err, nameErr)
	}
	return governanceSealReferenceFromVerified(expected, key, firstHash, last, lastFrameStart, lastFrameLength)
}

// Used only after complete native prefix authentication, never to qualify
// caller-supplied hashes or infer a role permission.
func governanceSealReferenceFromVerified(expected Summary, key []byte, firstHash [32]byte, last Record, lastFrameStart int64, lastFrameLength uint32) (GovernanceSealReference, error) {
	schema := authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootNative, Component: authenticatedstate.SealComponentGovernance, IndexID: AuthenticatedGovernanceSealIndexID, Codec: AuthenticatedGovernanceSealCodec}
	root := authenticatedstate.SealRoot{Component: schema.Component, IndexID: schema.IndexID, Codec: schema.Codec, Rows: expected.Records}
	if expected.Records == 0 {
		root.Empty = true
		root.Digest = sha256.Sum256([]byte("halro:governance:empty-prefix:v1\x00"))
	} else {
		if last.Sequence != expected.Records || last.Offset != expected.Bytes || lastFrameLength == 0 || lastFrameStart < 0 || last.Hash != expected.LastHash || firstHash == ([32]byte{}) {
			return GovernanceSealReference{}, ErrGovernanceSealSource
		}
		lineage := hmac.New(sha256.New, key)
		lineage.Write([]byte("halro:governance:chain-lineage:v1\x00"))
		lineage.Write(firstHash[:])
		copy(root.FileID[:], lineage.Sum(nil))
		root.Offset = uint64(lastFrameStart)
		root.Length = lastFrameLength
		root.Digest = last.Hash
	}
	return GovernanceSealReference{Schema: schema, Root: root, Position: authenticatedstate.SealPosition{
		Component: authenticatedstate.SealComponentGovernance,
		Sequence:  expected.Records,
		Offset:    uint64(expected.Bytes),
		Hash:      expected.LastHash,
	}}, nil
}

type GovernanceSealReference struct {
	Schema   authenticatedstate.SealRootSchema
	Root     authenticatedstate.SealRoot
	Position authenticatedstate.SealPosition
}

type governanceSealFrameVisitor func(record Record, start int64, length uint32) error

// scanSealPrefix authenticates the exact bytes in one fixed SectionReader.
// It never builds a second full-frame copy while computing the native hash.
func scanSealPrefix(ctx context.Context, source io.Reader, key []byte, visit governanceSealFrameVisitor) (Summary, bool, error) {
	var summary Summary
	for {
		if err := ctx.Err(); err != nil {
			return Summary{}, false, err
		}
		start := summary.Bytes
		header := make([]byte, frameHeaderSize)
		n, err := io.ReadFull(source, header)
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
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid header", ErrCorrupt, start)
		}
		sequence := binary.BigEndian.Uint64(header[8:16])
		if sequence != summary.Records+1 || !hmac.Equal(header[20:52], summary.LastHash[:]) {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid sequence or hash chain", ErrCorrupt, start)
		}
		payloadLength := binary.BigEndian.Uint32(header[16:20])
		if payloadLength > maxPayloadSize {
			return Summary{}, false, fmt.Errorf("%w at offset %d: payload too large", ErrCorrupt, start)
		}
		tail := make([]byte, int(payloadLength)+frameMACSize)
		if _, err := io.ReadFull(source, tail); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return summary, true, nil
			}
			return Summary{}, false, err
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write(header)
		_, _ = mac.Write(tail[:payloadLength])
		if !hmac.Equal(mac.Sum(nil), tail[payloadLength:]) {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid HMAC", ErrCorrupt, start)
		}
		var event Event
		if err := json.Unmarshal(tail[:payloadLength], &event); err != nil {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid event", ErrCorrupt, start)
		}
		if err := event.Validate(); err != nil {
			return Summary{}, false, fmt.Errorf("%w at offset %d: invalid event: %v", ErrCorrupt, start, err)
		}
		digest := sha256.New()
		_, _ = digest.Write(header)
		_, _ = digest.Write(tail)
		var frameHash [32]byte
		copy(frameHash[:], digest.Sum(nil))
		frameLength := frameHeaderSize + len(tail)
		summary.Records = sequence
		summary.Bytes += int64(frameLength)
		summary.LastHash = frameHash
		if visit != nil {
			if err := visit(Record{Sequence: sequence, Offset: summary.Bytes, Event: event, Hash: frameHash}, start, uint32(frameLength)); err != nil {
				return Summary{}, false, err
			}
		}
	}
}
