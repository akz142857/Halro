package metadatajournal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// BoundReplicationAuthority is supplied by the independently qualified group
// baseline owner, never copied from a candidate journal's header. It authorizes
// physical frames only; it does not select a Metadata projection or HA role.
// File replacement/trim requires a new independent file revision permission.
type BoundReplicationAuthority struct {
	Baseline     BaselineBinding
	FileRevision uint64
	MaxFileBytes int64
}

func (a BoundReplicationAuthority) Validate() error {
	if !a.Baseline.valid() || a.FileRevision == 0 || a.MaxFileBytes <= 0 {
		return ErrIncompatibleFormat
	}
	return nil
}
func (a BoundReplicationAuthority) matches(head Head) bool {
	return head.FormatVersion == boundFrameVersion && head.Epoch == a.Baseline.Epoch && head.Baseline == a.Baseline && head.FileRevision == a.FileRevision && head.Offset <= a.MaxFileBytes
}
func (a BoundReplicationAuthority) checkControl(record Record) error {
	if record.FormatVersion != boundFrameVersion || record.Epoch != a.Baseline.Epoch {
		return ErrIncompatibleFormat
	}
	var binding *BaselineBinding
	var revision uint64
	switch record.Kind {
	case KindEpoch:
		binding, revision = record.Header.Baseline, record.Header.FileRevision
	case KindTrim:
		binding, revision = record.Trim.Baseline, record.Trim.FileRevision
	default:
		return nil
	}
	if binding == nil || *binding != a.Baseline || revision != a.FileRevision {
		return ErrIncompatibleFormat
	}
	return nil
}

// openBoundReplicationFile authenticates a fixed complete prefix with one
// bounded native decoder. Only a repair caller may accept a torn suffix.
func withBoundReplicationFile(ctx context.Context, path string, key []byte, a BoundReplicationAuthority, writable bool, visit func(Record) error, use func(*os.File, Head, bool) error) (result Head, resultErr error) {
	if err := a.Validate(); err != nil {
		return Head{}, err
	}
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	flags := os.O_RDONLY
	if writable {
		flags = os.O_RDWR
	}
	file, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return Head{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeDescriptor(file))
		if resultErr != nil {
			result = Head{}
		}
	}()
	before, err := file.Stat()
	named, nameErr := os.Lstat(path)
	if err != nil || nameErr != nil || !before.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(before, named) || before.Size() <= 0 || before.Size() > a.MaxFileBytes {
		return Head{}, errors.Join(ErrCorrupt, err, nameErr)
	}
	head, partial, err := scanContext(ctx, io.NewSectionReader(file, 0, before.Size()), key, func(record Record) error {
		if err := a.checkControl(record); err != nil {
			return err
		}
		if visit != nil {
			return visit(record)
		}
		return ctx.Err()
	})
	if err != nil || !a.matches(head) {
		return Head{}, errors.Join(ErrIncompatibleFormat, err)
	}
	after, err := file.Stat()
	named, nameErr = os.Lstat(path)
	if err != nil || nameErr != nil || !os.SameFile(before, after) || !os.SameFile(before, named) || after.Size() != before.Size() || named.Size() != before.Size() || !before.ModTime().Equal(after.ModTime()) || !before.ModTime().Equal(named.ModTime()) {
		return Head{}, errors.Join(ownedfile.ErrReplaced, err, nameErr)
	}
	if err := ctx.Err(); err != nil {
		return Head{}, err
	}
	if use != nil {
		if err := use(file, head, partial); err != nil {
			return Head{}, err
		}
	} else if partial {
		return Head{}, fmt.Errorf("%w: partial bound replication source", ErrCorrupt)
	}
	return head, nil
}

func BoundReplicationCursor(ctx context.Context, path string, key []byte, a BoundReplicationAuthority) (uint64, uint64, error) {
	head, err := withBoundReplicationFile(ctx, path, key, a, false, nil, nil)
	if err != nil {
		return 0, 0, err
	}
	return head.Epoch, head.Sequence, nil
}
func BoundReplicationHeadAt(ctx context.Context, path string, key []byte, a BoundReplicationAuthority, epoch, sequence uint64) ([32]byte, error) {
	if epoch != a.Baseline.Epoch {
		return [32]byte{}, ErrIncompatibleFormat
	}
	var digest [32]byte
	head, err := withBoundReplicationFile(ctx, path, key, a, false, func(r Record) error {
		if r.Kind == KindEpoch && sequence == 0 || r.Kind == KindOperations && r.Sequence == sequence {
			digest = r.Hash
		}
		if r.Kind == KindTrim && r.Trim.TrimmedThrough == sequence {
			copy(digest[:], r.Trim.ChainHeadAtTrim)
		}
		return nil
	}, nil)
	if err != nil {
		return [32]byte{}, err
	}
	if sequence < head.TrimmedThrough || sequence > head.Sequence || digest == ([32]byte{}) {
		return [32]byte{}, ErrCorrupt
	}
	return digest, nil
}

// RepairBoundReplicaTail requires the complete cursor obtained from the
// authenticated ordering scan. It removes only torn bytes at that exact head;
// a complete later frame is refused. Maintenance exclusion belongs to caller.
func RepairBoundReplicaTail(ctx context.Context, path string, key []byte, a BoundReplicationAuthority, epoch, sequence uint64) error {
	_, err := withBoundReplicationFile(ctx, path, key, a, true, nil, func(f *os.File, h Head, partial bool) error {
		if h.Epoch != epoch || h.Sequence != sequence {
			return ErrCorrupt
		}
		if !partial {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.Truncate(h.Offset); err != nil {
			return err
		}
		return f.Sync()
	})
	return err
}

// TruncateBoundReplicationTail requires an independently ordered cursor.
// It authenticates the whole source before removing complete unauthorized tail.
func TruncateBoundReplicationTail(ctx context.Context, path string, key []byte, a BoundReplicationAuthority, epoch, sequence uint64) error {
	var end int64 = -1
	_, err := withBoundReplicationFile(ctx, path, key, a, true, func(r Record) error {
		if epoch != r.Epoch {
			return ErrCorrupt
		}
		if r.Kind == KindEpoch && sequence == 0 || r.Kind == KindOperations && r.Sequence == sequence || r.Kind == KindTrim && r.Trim.TrimmedThrough == sequence {
			payload, err := encodePayload(r.Kind, r.Ops, r.Header, r.Trim)
			if err != nil {
				return err
			}
			end = r.Offset + int64(frameHeaderSize+len(payload)+frameMACSize)
		}
		return nil
	}, func(f *os.File, h Head, partial bool) error {
		if partial || epoch != h.Epoch || sequence < h.TrimmedThrough || sequence > h.Sequence || end < 0 {
			return ErrCorrupt
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.Truncate(end); err != nil {
			return err
		}
		return f.Sync()
	})
	return err
}

func OpenBoundReplicaContext(ctx context.Context, path string, key []byte, a BoundReplicationAuthority) (*Log, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	l, err := openExpected(ctx, path, key, true, &a.Baseline, a.FileRevision, a.MaxFileBytes, nil)
	if err != nil {
		return nil, err
	}
	l.boundReplication = &a
	return l, nil
}

// SetBoundReplicationAdmissionAndAfterDurable installs the normal actual
// admission/fsync/ordering/confirmation protocol after independent qualification.
// Prepared Standalone append and selected startup authority remain separate.
func (l *Log) SetBoundReplicationAdmissionAndAfterDurable(a BoundReplicationAuthority, before func(DurableBatch) (AppendAdmission, error), after func(DurableBatch) (uint64, error), wait func(context.Context, uint64) error) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if before == nil || after == nil || wait == nil {
		return errors.New("bound metadata admission, ordering and confirmation are required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.availabilityError(); err != nil {
		return err
	}
	if l.freezeCancel != nil {
		return ErrNativeFrozen
	}
	h := Head{Epoch: l.epoch, Offset: l.offset, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}
	if l.replica || l.readOnly || l.startupAudit != nil || l.terminalErr != nil || !a.matches(h) || l.maxBytes != a.MaxFileBytes || l.afterDurable != nil || l.beforeDurable != nil {
		return ErrIncompatibleFormat
	}
	if err := l.checkPreparedFile(); err != nil {
		return err
	}
	l.boundReplication = &a
	l.beforeDurable, l.afterDurable, l.waitConfirmed = before, after, wait
	return nil
}

// ReplicationVersion advertises the actual qualified native capability.
func (l *Log) ReplicationVersion() (uint16, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.availabilityError(); err != nil {
		return 0, err
	}
	if l.formatVersion == frameVersion {
		return uint16(frameVersion), nil
	}
	if l.boundReplication == nil || !l.boundReplication.matches(Head{Epoch: l.epoch, Offset: l.offset, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}) {
		return 0, ErrIncompatibleFormat
	}
	if err := l.checkPreparedFile(); err != nil {
		return 0, err
	}
	return uint16(boundFrameVersion), nil
}

// BoundReplicationPrefixHead returns the complete independently authorized native
// frame-end receipt. Complete-file authentication includes an excluded tail;
// epoch/header presence alone never supplies a permitted semantic cut.
func BoundReplicationPrefixHead(ctx context.Context, path string, key []byte, a BoundReplicationAuthority, epoch, sequence uint64) (result Head, resultErr error) {
	if epoch != a.Baseline.Epoch {
		return Head{}, ErrIncompatibleFormat
	}
	var previous Record
	var have, found bool
	observe := func(end int64) {
		if have && previous.Sequence == sequence {
			result = Head{Epoch: epoch, Sequence: sequence, Offset: end, Hash: previous.Hash, FormatVersion: 2, FileRevision: a.FileRevision, Baseline: a.Baseline}
			found = true
		}
	}
	full, err := withBoundReplicationFile(ctx, path, key, a, false, func(record Record) error {
		observe(record.Offset)
		if record.Kind == KindTrim {
			return ErrIncompatibleFormat
		} // migration v1 retains its original header.
		previous = Record{Sequence: record.Sequence, Hash: record.Hash}
		have = true
		return ctx.Err()
	}, nil)
	if err != nil {
		return Head{}, err
	}
	observe(full.Offset)
	if !found {
		return Head{}, ErrCorrupt
	}
	return result, nil
}
