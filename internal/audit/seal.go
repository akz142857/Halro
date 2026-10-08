package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"github.com/akz142857/Halro/internal/ownedfile"
	"io"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

const (
	AuthenticatedAuditSealIndexID uint16 = 1
	AuthenticatedAuditSealCodec   uint16 = 1
	AuditSealScanRAMBytes         int64  = 512 << 10
)

var ErrSealSource = errors.New("audit seal source cut is not eligible")

// SealReference binds a fully reauthenticated Audit prefix to the final native
// frame that commits that prefix. expected must come from independent startup
// authority; a cached Summary alone does not prove the bytes still exist.
// The frame chain is scanned from genesis using fixed per-frame memory.
func (l *Log) SealReference(ctx context.Context, budget *resourcebudget.Manager, expected Summary) (result AuditSealReference, resultErr error) {
	if l == nil || budget == nil {
		return result, ErrSealSource
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	permit, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: AuditSealScanRAMBytes})
	if err != nil {
		return result, err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, permit.Release())
		}
		if resultErr != nil {
			result = AuditSealReference{}
		}
	}()

	l.mu.Lock()
	if l.startupAudit != nil && !l.startupAudit.Snapshot().Complete {
		l.mu.Unlock()
		return result, ErrStartupAuditPending
	}
	if l.file == nil || l.closing || l.terminalErr != nil || expected.Records > l.sequence || expected.Bytes < 0 || expected.Bytes > l.offset || (expected.Records == 0 && (expected.Bytes != 0 || expected.LastHash != ([32]byte{}))) || (expected.Records > 0 && (expected.Bytes == 0 || expected.LastHash == ([32]byte{}))) {
		l.mu.Unlock()
		return result, errors.Join(ErrSealSource, l.terminalErr)
	}
	current := Summary{Records: l.sequence, LastHash: l.lastHash, Bytes: l.offset}
	actual, err := l.file.Stat()
	if err != nil {
		l.mu.Unlock()
		return result, errors.Join(ErrSealSource, err)
	}
	named, err := os.Lstat(l.path)
	if err != nil || !actual.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(actual, named) || actual.Size() != current.Bytes || named.Size() != current.Bytes {
		l.mu.Unlock()
		return result, errors.Join(ErrSealSource, err)
	}
	file, err := os.OpenFile(l.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		l.mu.Unlock()
		return result, errors.Join(ErrSealSource, err)
	}
	closeFile := l.closeReplayFile
	if closeFile == nil {
		closeFile = (*os.File).Close
	}
	key := append([]byte(nil), l.key...)
	l.replayWG.Add(1)
	l.mu.Unlock()
	defer l.replayWG.Done()
	defer func() { resultErr = errors.Join(resultErr, l.retireReader(file, closeFile)) }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(actual, opened) || opened.Size() != current.Bytes {
		return result, errors.Join(ErrSealSource, err)
	}
	defer clear(key)

	var firstHash [32]byte
	var last Record
	var lastFrameStart int64
	var lastFrameLength uint32
	reader := io.NewSectionReader(file, 0, expected.Bytes)
	verified, partial, err := scanFromContextWithFrame(ctx, reader, key, Summary{}, func(record Record, frameStart int64, frameLength uint32) error {
		if record.Sequence == 1 {
			firstHash = record.Hash
		}
		last = record
		lastFrameStart, lastFrameLength = frameStart, frameLength
		return ctx.Err()
	})
	if err != nil || partial || verified != expected {
		return result, errors.Join(ErrSealSource, ErrCorrupt, err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	finalInfo, err := file.Stat()
	finalName, nameErr := os.Lstat(l.path)
	if err != nil || nameErr != nil || !os.SameFile(opened, finalInfo) || !os.SameFile(opened, finalName) || finalInfo.Size() < expected.Bytes || finalName.Size() < expected.Bytes {
		return result, errors.Join(ErrSealSource, err, nameErr)
	}
	schema := authenticatedstate.SealRootSchema{Component: authenticatedstate.SealComponentAudit, IndexID: AuthenticatedAuditSealIndexID, Codec: AuthenticatedAuditSealCodec}
	root := authenticatedstate.SealRoot{Component: schema.Component, IndexID: schema.IndexID, Codec: schema.Codec, Rows: expected.Records}
	if expected.Records == 0 {
		root.Empty = true
		root.Digest = sha256.Sum256([]byte("halro:audit:empty-prefix:v1\x00"))
	} else {
		if last.Sequence != expected.Records || last.Offset != expected.Bytes || lastFrameLength == 0 || lastFrameStart < 0 || last.Hash != expected.LastHash || firstHash == ([32]byte{}) {
			return result, ErrSealSource
		}
		lineage := hmac.New(sha256.New, key)
		lineage.Write([]byte("halro:audit:chain-lineage:v1\x00"))
		lineage.Write(firstHash[:])
		copy(root.FileID[:], lineage.Sum(nil))
		root.Offset = uint64(lastFrameStart)
		root.Length = lastFrameLength
		root.Digest = last.Hash
	}
	return AuditSealReference{
		Schema: schema,
		Root:   root,
		Position: authenticatedstate.SealPosition{
			Component: authenticatedstate.SealComponentAudit,
			Sequence:  expected.Records,
			Offset:    uint64(expected.Bytes),
			Hash:      expected.LastHash,
		},
	}, nil
}

// AuditSealReference is a fixed native position and final-frame commitment.
// Codec 1 defines FileID as a key-derived chain-lineage identifier, not a path.
type AuditSealReference struct {
	Schema   authenticatedstate.SealRootSchema
	Root     authenticatedstate.SealRoot
	Position authenticatedstate.SealPosition
}
