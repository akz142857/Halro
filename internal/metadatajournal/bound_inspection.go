package metadatajournal

import (
	"context"
	"errors"
)

// OpenBoundExactContext authenticates an independently selected complete head.
// Unlike OpenBoundContext it never repairs a partial tail or accepts a different
// durable boundary. Callers retain independent maintenance/disk admission.
func OpenBoundExactContext(ctx context.Context, path string, key []byte, expected Head, maxBytes int64) (*Log, error) {
	if !expected.Baseline.valid() || expected.Epoch != expected.Baseline.Epoch || expected.FormatVersion != boundFrameVersion || expected.FileRevision == 0 || expected.Offset <= 0 || expected.Offset > maxBytes || expected.Sequence < expected.TrimmedThrough || expected.Hash == ([32]byte{}) {
		return nil, ErrCorrupt
	}
	return openExpected(ctx, path, key, false, &expected.Baseline, expected.FileRevision, maxBytes, &expected)
}

// InspectBoundContext authenticates the entire frozen descriptor before any
// visitor, then visits all frames including the initial control. It retains the
// native append lock through both scans and final inode/size/head verification.
// Memory is bounded by one frame; work remains O(history bytes).
func (l *Log) InspectBoundContext(ctx context.Context, visit func(Record) error) (Head, error) {
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.startupAudit != nil && !l.selectedStartupStructuralReadyLocked() {
		return Head{}, ErrStartupAuditPending
	}
	if err := l.availabilityError(); err != nil {
		return Head{}, err
	}
	if l.availabilityError() != nil || l.formatVersion != boundFrameVersion {
		return Head{}, ErrCorrupt
	}
	expected := Head{Epoch: l.epoch, Sequence: l.sequence, Offset: l.offset, Hash: l.lastHash, TrimmedThrough: l.trimmedThrough, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}
	validate := func(actual Head, partial bool, err error) error {
		if err != nil {
			return err
		}
		if partial || actual != expected {
			return ErrCorrupt
		}
		return errors.Join(ctx.Err(), l.checkPreparedFile())
	}
	if err := l.checkPreparedFile(); err != nil {
		return Head{}, err
	}
	head, partial, err := scanContext(ctx, l.file, l.key, nil)
	if err := validate(head, partial, err); err != nil {
		return Head{}, err
	}
	head, partial, err = scanContext(ctx, l.file, l.key, visit)
	if err := validate(head, partial, err); err != nil {
		return Head{}, err
	}
	return head, nil
}
