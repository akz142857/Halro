package metadatajournal

import (
	"context"
	"errors"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// RootCommitment is a complete independently authenticated content root, not a
// signature on an ordinary DB. Domain owners verify it with the content key.
type RootCommitment struct {
	Scope    [32]byte `json:"scope"`
	Cut      [32]byte `json:"cut"`
	Digest   [32]byte `json:"digest"`
	ID       [16]byte `json:"id"`
	Revision uint64   `json:"revision"`
	Rows     uint64   `json:"rows"`
}

// TrimStateBinding preserves the cut state's OLD physical frame provenance.
// New anchor offsets differ; copying only sequence would lose the content cut.
type TrimStateBinding struct {
	Version           uint8          `json:"version"`
	Root              RootCommitment `json:"root"`
	Sequence          uint64         `json:"sequence"`
	SourceRevision    uint64         `json:"source_revision"`
	SourceFrameOffset int64          `json:"source_frame_offset"`
	SourceEndOffset   int64          `json:"source_end_offset"`
	SourceHash        [32]byte       `json:"source_hash"`
}

func (s TrimStateBinding) valid(b BaselineBinding, revision, through uint64, hash []byte) bool {
	return s.Version == 1 && s.Sequence == through && through > 0 && s.SourceRevision > 0 && revision > 1 && s.SourceRevision == revision-1 && s.SourceFrameOffset >= 0 && s.SourceEndOffset > s.SourceFrameOffset && s.SourceHash != ([32]byte{}) && equalFrameDigest(hash, s.SourceHash) && s.Root.Scope == b.Scope && s.Root.Cut != ([32]byte{}) && s.Root.Digest != ([32]byte{}) && s.Root.ID == b.RootID && s.Root.Revision > 0 && s.Root.Rows > 0
}

// TrimBoundContext owns native exclusion, complete source validation and the
// replacement's actual creation receipt. It requires matching independently
// selected source head and cut-state facts. Domain/disk/role admission belongs
// to the caller; this API grants no HA authority.
func (l *Log) TrimBoundContext(ctx context.Context, expected Head, state TrimStateBinding) (Head, error) {
	return l.trimBoundContext(ctx, expected, state, nil)
}
func (l *Log) trimBoundContext(ctx context.Context, expected Head, state TrimStateBinding, phase func(string)) (result Head, resultErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !l.mu.TryLock() {
		return result, errors.New("metadata journal is busy")
	}
	defer l.mu.Unlock()
	if l.readOnly {
		return result, errors.New("read-only metadata journal refuses trim")
	}
	if l.freezeCancel != nil {
		return result, ErrNativeFrozen
	}
	if !l.selectedStartupStructuralReadyLocked() {
		return result, ErrStartupAuditPending
	}
	if err := l.availabilityError(); err != nil {
		return result, err
	}
	actual := Head{Epoch: l.epoch, Sequence: l.sequence, Offset: l.offset, Hash: l.lastHash, TrimmedThrough: l.trimmedThrough, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}
	if l.availabilityError() != nil || l.terminalErr != nil || l.formatVersion != boundFrameVersion || l.replica || l.afterDurable != nil || actual != expected || state.Sequence <= expected.TrimmedThrough || state.Sequence > expected.Sequence || expected.FileRevision == ^uint64(0) || !state.valid(expected.Baseline, expected.FileRevision+1, state.Sequence, state.SourceHash[:]) {
		return result, ErrCorrupt
	}
	if err := l.checkPreparedFile(); err != nil {
		return result, err
	}
	var published *ownedfile.Owner
	finished := false
	defer func() {
		if published != nil && !finished {
			var closeErr error
			if l.file != nil {
				closeErr = l.closeCurrent()
			}
			l.created = published
			l.terminalErr = errors.Join(ErrCorrupt, l.terminalErr, resultErr, closeErr)
			if l.retirementErr == nil {
				clear(l.key)
			}
		}
	}()
	result, resultErr = trimWithState(ctx, l.path, l.key, state.Sequence, &expected, &state, &published, l.maxBytes, phase)
	if errors.Is(resultErr, ownedfile.ErrRetirement) {
		l.retirementErr = errors.Join(l.retirementErr, resultErr)
		l.closed = true
	}
	if published == nil {
		return result, resultErr
	}
	// Even a post-rename failure retains the ACTUAL new inode's cleanup receipt.
	// The superseded descriptor must close before temporary rewrite fees retire.
	closeErr := l.closeCurrent()
	l.created = published
	if resultErr != nil || closeErr != nil {
		l.terminalErr = errors.Join(ErrCorrupt, resultErr, closeErr)
		if l.retirementErr == nil {
			clear(l.key)
		}
		return Head{}, l.terminalErr
	}
	file, err := os.OpenFile(l.path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		l.terminalErr = err
		clear(l.key)
		return Head{}, err
	}
	if err := published.VerifyFile(file); err != nil {
		l.file = file
		l.terminalErr = errors.Join(err, l.closeCurrent())
		if l.retirementErr == nil {
			clear(l.key)
		}
		return Head{}, l.terminalErr
	}
	l.file = file
	l.durability = durabilityFor(file)
	if l.durability == nil {
		l.terminalErr = errors.New("metadata durability writer is absent")
		return Head{}, l.terminalErr
	}
	l.epoch, l.sequence, l.offset, l.lastHash = result.Epoch, result.Sequence, result.Offset, result.Hash
	l.trimmedThrough, l.fileRevision = result.TrimmedThrough, result.FileRevision
	if _, err := file.Seek(l.offset, 0); err != nil {
		l.terminalErr = err
		return Head{}, err
	}
	if err := errors.Join(ctx.Err(), l.checkPreparedFile()); err != nil {
		l.terminalErr = err
		return Head{}, err
	}
	finished = true
	return result, nil
}

// AdoptClosedReplacement updates an existing creator's receipt from a real
// closed replacement creator. A borrowed reopen alone grants no receipt.
// Domain owners separately qualify same Manager/root/source/role and Close.
func (l *Log) AdoptClosedReplacement(next *Log) (resultErr error) {
	if l == nil || next == nil || l == next || !l.mu.TryLock() {
		return ErrCorrupt
	}
	defer l.mu.Unlock()
	if !next.mu.TryLock() {
		return ErrCorrupt
	}
	defer next.mu.Unlock()
	if !l.closed || !next.closed || l.closeErr != nil || next.closeErr != nil || l.retirementErr != nil || next.retirementErr != nil || l.file != nil || next.file != nil || l.created == nil || l.path != next.path || l.formatVersion != boundFrameVersion || next.formatVersion != boundFrameVersion || l.baseline != next.baseline || next.fileRevision < l.fileRevision || next.sequence < l.sequence || next.terminalErr != nil {
		return ErrCorrupt
	}
	if next.created == nil {
		if next.fileRevision != l.fileRevision {
			return ErrCorrupt
		}
		return nil
	}
	file, err := os.OpenFile(l.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() {
		if file != nil {
			if err := closeDescriptor(file); err != nil {
				l.retirementErr = errors.Join(l.retirementErr, err)
				l.file = file
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	if err := next.created.VerifyFile(file); err != nil {
		return err
	}
	if err := closeDescriptor(file); err != nil {
		l.file = file
		l.retirementErr = errors.Join(l.retirementErr, err)
		file = nil
		return l.retirementErr
	}
	file = nil
	l.created = next.created
	l.epoch, l.sequence, l.offset, l.lastHash = next.epoch, next.sequence, next.offset, next.lastHash
	l.trimmedThrough, l.fileRevision = next.trimmedThrough, next.fileRevision
	return nil
}
