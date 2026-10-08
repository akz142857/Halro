package metadatajournal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
)

// ValidateBoundOperations checks format2's operation encoding before source IO
// or domain preparation. It does not authenticate facts or their classification.
func ValidateBoundOperations(ops []Op) error { return validateBoundOps(ops) }

// AppendPreparedContext holds the append exclusion through complete projection
// preparation, native Sync and projection publication. The callback must apply
// the supplied immutable facts before calling durable once, and publish its
// root only afterward. A projection/resource refusal before durable leaves the
// journal unchanged. Failure after durable keeps the actual head and requires
// independent projection recovery. Caller supplies RAM/disk admission and role
// authority; this explicit API accepts only private format2 without HA hooks.
// The callback must not reenter this Log; its source exclusion remains held.
func (l *Log) AppendPreparedContext(ctx context.Context, ops []Op, prepare func(Record, func() error) error) (Record, error) {
	return l.appendPreparedContext(ctx, ops, prepare, nil, nil)
}

// AppendBoundPreparedContext uses an already independently installed Primary
// tenure. Candidate preparation precedes admission; its durable barrier records
// global ordering after native fsync and before the candidate publishes. The
// caller must finish publication before WaitConfirmed. Neither a candidate
// header nor this physical receipt grants business or common-seal authority.
func (l *Log) AppendBoundPreparedContext(ctx context.Context, a BoundReplicationAuthority, ops []Op, prepare func(Record, func() error) error) (AppendReceipt, error) {
	if err := a.Validate(); err != nil {
		return AppendReceipt{}, err
	}
	var index uint64
	record, err := l.appendPreparedContext(ctx, ops, prepare, &a, &index)
	if err != nil {
		return AppendReceipt{}, err
	}
	return AppendReceipt{Record: record, ReplicationIndex: index}, nil
}

func (l *Log) appendPreparedContext(ctx context.Context, ops []Op, prepare func(Record, func() error) error, authority *BoundReplicationAuthority, replicationIndex *uint64) (result Record, resultErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if prepare == nil {
		return result, errors.New("missing metadata projection preparation")
	}
	if err := ValidateBoundOperations(ops); err != nil {
		return result, err
	}
	payload, err := json.Marshal(ops)
	if err != nil || len(payload) > MaxPayloadSize {
		return result, errors.Join(errors.New("invalid prepared metadata payload"), err)
	}
	owned, err := decodeBoundOps(payload)
	if err != nil {
		return result, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.freezeCancel != nil {
		return result, ErrNativeFrozen
	}
	if err := l.availabilityError(); err != nil {
		return result, err
	}
	if l.availabilityError() != nil || l.formatVersion != boundFrameVersion || l.replica || l.readOnly || (authority == nil && (l.afterDurable != nil || l.beforeDurable != nil)) || l.terminalErr != nil || l.sequence == ^uint64(0) {
		return result, errors.New("metadata journal is not eligible for private prepared append")
	}
	if authority != nil {
		head := Head{Epoch: l.epoch, Offset: l.offset, FormatVersion: l.formatVersion, FileRevision: l.fileRevision, Baseline: l.baseline}
		if l.boundReplication == nil || *l.boundReplication != *authority || !authority.matches(head) || l.maxBytes != authority.MaxFileBytes || l.beforeDurable == nil || l.afterDurable == nil || l.waitConfirmed == nil {
			return result, ErrIncompatibleFormat
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := l.checkPreparedFile(); err != nil {
		return result, err
	}
	frame, hash := encodeFrameVersion(boundFrameVersion, l.key, KindOperations, l.epoch, l.sequence+1, l.lastHash, payload)
	if l.maxBytes <= 0 || l.offset > l.maxBytes || int64(len(frame)) > l.maxBytes-l.offset {
		return result, errors.New("bound metadata journal disk limit reached")
	}
	record := Record{FormatVersion: boundFrameVersion, Kind: KindOperations, Epoch: l.epoch, Sequence: l.sequence + 1, Ops: owned, Hash: hash, Offset: l.offset}
	var scope sync.Mutex
	closed := false
	closeScope := func() { scope.Lock(); closed = true; scope.Unlock() }
	called, committed := false, false
	returned := false
	var barrierErr error
	defer func() {
		closeScope()
		if committed && (!returned || resultErr != nil) {
			l.terminalErr = errors.Join(errors.New("prepared metadata projection publication uncertain"), resultErr)
		}
	}()
	durable := func() (durableErr error) {
		if !scope.TryLock() {
			return errors.New("prepared metadata barrier busy")
		}
		defer scope.Unlock()
		defer func() { barrierErr = errors.Join(barrierErr, durableErr) }()
		if closed {
			return errors.New("prepared metadata barrier closed")
		}
		if called {
			barrierErr = errors.New("prepared metadata durable barrier repeated")
			return barrierErr
		}
		called = true
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.availabilityError(); err != nil {
			return err
		}
		if err := l.checkPreparedFile(); err != nil {
			return err
		}
		if err := ValidateBoundOperations(record.Ops); err != nil {
			return err
		}
		actual, err := json.Marshal(record.Ops)
		if err != nil || !bytes.Equal(actual, payload) {
			return errors.New("prepared metadata facts changed")
		}
		batch := DurableBatch{Context: ctx, Epoch: record.Epoch, FirstSequence: record.Sequence, LastSequence: record.Sequence, Frames: append([]byte(nil), frame...)}
		var admission AppendAdmission
		if authority != nil {
			var err error
			admission, err = l.beforeDurable(batch)
			if err != nil {
				return err
			}
			batch.Admission = admission
			// A callback may withdraw the tenure or replace the named file. Recheck
			// before writing; a known pre-write refusal may return its reservation.
			if err := errors.Join(ctx.Err(), l.availabilityError(), l.checkPreparedFile(), func() error {
				if !bytes.Equal(batch.Frames, frame) {
					return errors.New("metadata admission changed immutable native frames")
				}
				return nil
			}()); err != nil {
				if admission != nil {
					if cleanup := admission.Release(); cleanup != nil {
						l.uncertainAdmissions = append(l.uncertainAdmissions, admission)
						l.terminalErr = errors.Join(err, cleanup)
						return l.terminalErr
					}
				}
				return err
			}
		}
		retainUncertain := func() {
			if admission != nil {
				l.uncertainAdmissions = append(l.uncertainAdmissions, admission)
				admission = nil
			}
		}
		physical, err := l.validatePhysicalPreparedLocked(ctx, int64(len(frame)))
		if err != nil {
			// No native bytes have been attempted. The original admission still
			// belongs to this append until its release is known to have joined.
			if admission != nil {
				if cleanup := admission.Release(); cleanup != nil {
					retainUncertain()
					l.terminalErr = errors.Join(err, cleanup)
					return l.terminalErr
				}
			}
			return err
		}
		if physical != nil {
			physical.accepted.Store(true)
		}
		if err := writeFull(l.durability, frame); err != nil {
			retainUncertain()
			l.terminalErr = err
			return err
		}
		if err := l.durability.Sync(); err != nil {
			retainUncertain()
			l.terminalErr = err
			return err
		}
		l.sequence, l.offset, l.lastHash = record.Sequence, l.offset+int64(len(frame)), hash
		committed = true
		if err := l.checkPreparedFile(); err != nil {
			retainUncertain()
			return err
		}
		if authority != nil {
			// The actual ordering owner adopts Admission on entry, including failures.
			// Do not also release it here after transferring that ownership.
			admission = nil
			index, err := l.afterDurable(batch)
			if err != nil || index == 0 {
				l.terminalErr = errors.Join(errors.New("prepared metadata ordering uncertain"), err)
				return l.terminalErr
			}
			*replicationIndex = index
		}
		if err := l.checkPreparedFile(); err != nil {
			return err
		}
		return errors.Join(ctx.Err(), l.availabilityError())
	}
	err = prepare(record, durable)
	returned = true
	closeScope()
	if err != nil {
		return result, err
	}
	if err := l.availabilityError(); err != nil {
		return result, err
	}
	if barrierErr != nil {
		return result, barrierErr
	}
	if !committed {
		return result, errors.New("prepared metadata callback did not commit native frame")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := ValidateBoundOperations(record.Ops); err != nil {
		return result, err
	}
	actual, err := json.Marshal(record.Ops)
	if err != nil || !bytes.Equal(actual, payload) {
		return result, errors.New("prepared metadata facts changed after durable append")
	}
	if err := l.checkPreparedFile(); err != nil {
		return result, err
	}
	if err := l.availabilityError(); err != nil {
		return result, err
	}
	return record, nil
}

// Check the actual borrowed FD and named inode, never reopen by pathname.
func (l *Log) checkPreparedFile() error {
	actual, e := l.file.Stat()
	named, ne := os.Lstat(l.path)
	if e != nil || ne != nil || !os.SameFile(actual, named) || actual.Size() != l.offset {
		return errors.Join(ErrCorrupt, e, ne)
	}
	return nil
}
