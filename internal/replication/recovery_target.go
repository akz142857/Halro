package replication

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
)

// OrderingPrefix describes one authenticated, durable global cut. Store
// positions are fixed-size summaries maintained during the existing scan and
// successful fsync append; capture does not re-walk the history. This inherits
// the live journal's authenticated-prefix trust, not a fresh physical audit of
// every old record or the native store bodies.
type OrderingPrefix struct {
	ClusterID   string
	Incarnation string
	Index       uint64
	Term        uint64
	MAC         [sha256.Size]byte
	Stores      [4]StoreCursor
}

func advanceOrderingCursors(cursors [4]StoreCursor, record OrderingRecord) ([4]StoreCursor, error) {
	switch record.Kind {
	case KindData:
		if record.Store < StoreLedger || record.Store > StoreMetadata {
			return cursors, errors.New("invalid ordering data store")
		}
		cursor := cursors[record.Store-StoreLedger]
		if cursor.Generation == 0 {
			if record.StoreSequenceFirst != 1 {
				return cursors, fmt.Errorf("ordering store %d begins at sequence %d", record.Store, record.StoreSequenceFirst)
			}
		} else if cursor.Generation != record.StoreGeneration || cursor.Sequence == ^uint64(0) || cursor.Sequence+1 != record.StoreSequenceFirst {
			return cursors, fmt.Errorf("ordering store %d range does not continue its durable cursor", record.Store)
		}
		cursors[record.Store-StoreLedger] = StoreCursor{Generation: record.StoreGeneration, Sequence: record.StoreSequenceLast}
	case KindLedgerRoll:
		roll, err := DecodeLedgerRollMetadata(record.Metadata)
		if err != nil {
			return cursors, err
		}
		cursor := cursors[StoreLedger-StoreLedger]
		if cursor.Generation != roll.Generation || cursor.Sequence != roll.LastSequence || roll.Generation == ^uint64(0) {
			return cursors, errors.New("ordering Ledger Roll does not continue its durable cursor")
		}
		cursors[StoreLedger-StoreLedger] = StoreCursor{Generation: roll.Generation + 1, Sequence: roll.LastSequence}
	case KindMetadataBaseline:
		boundary, err := DecodeMetadataBaselineMetadata(record.Metadata)
		if err != nil {
			return cursors, err
		}
		cursor := cursors[StoreMetadata-StoreLedger]
		if cursor.Generation != boundary.PreviousEpoch || cursor.Sequence != boundary.PreviousSequence {
			return cursors, errors.New("ordering Metadata baseline does not continue its durable cursor")
		}
		cursors[StoreMetadata-StoreLedger] = StoreCursor{Generation: boundary.Epoch}
	}
	return cursors, nil
}

func (j *OrderingJournal) capturePrefix() (OrderingPrefix, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.capturePrefixLocked()
}

func (j *OrderingJournal) capturePrefixLocked() (OrderingPrefix, error) {
	if j.closed || j.closing || j.file == nil {
		return OrderingPrefix{}, errors.New("ordering journal is closed")
	}
	if j.poisoned != nil || j.tailCursorErr != nil {
		return OrderingPrefix{}, fmt.Errorf("ordering prefix is unavailable: %w", errors.Join(j.poisoned, j.tailCursorErr))
	}
	if j.lastIndex > 0 {
		record, err := j.recordLocked(j.lastIndex)
		if err != nil {
			return OrderingPrefix{}, err
		}
		if record.MAC != j.head || record.Term != j.lastTerm {
			return OrderingPrefix{}, errors.New("ordering tail changed after authentication")
		}
	}
	return OrderingPrefix{ClusterID: j.header.ClusterID, Incarnation: j.header.Incarnation, Index: j.lastIndex, Term: j.lastTerm, MAC: j.head, Stores: j.tailCursors}, nil
}

// Verify and subscribe under the same lock so a term transition between these
// operations cannot leave a waiter listening only to the new term's channel.
func (j *OrderingJournal) verifyRecoveryPrefix(target OrderingPrefix) (<-chan struct{}, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	current, err := j.capturePrefixLocked()
	if err != nil {
		return nil, err
	}
	if target.ClusterID != current.ClusterID || target.Incarnation != current.Incarnation || target.Term != current.Term || target.Index == 0 || target.Index > current.Index {
		return nil, errors.New("recovery ordering prefix belongs to another leadership or history")
	}
	if target.Index == current.Index {
		if target.MAC != current.MAC || target.Stores != current.Stores {
			return nil, errors.New("recovery ordering prefix changed")
		}
		return j.invalidity, nil
	}
	record, err := j.recordLocked(target.Index)
	if err != nil || record.MAC != target.MAC || record.Term != target.Term {
		return nil, errors.Join(err, errors.New("recovery target ordering evidence changed"))
	}
	return j.invalidity, nil
}

// RecoveryTarget is opaque to callers. Evidence returns a copy for recording;
// caller edits cannot change the wait. It is valid only in the coordinator
// lifecycle that captured it, and cannot be resumed on another Primary.
type RecoveryTarget struct {
	owner    *PrimaryCoordinator
	revision uint64
	prefix   OrderingPrefix
}

func (t RecoveryTarget) Evidence() OrderingPrefix { return t.prefix }

// WaitRecoveryAvailability waits for a real source-backed peer window.
// An already confirmed index is not evidence
// that the transport can support recovery. Permanent failures and revoked
// coordinator lifecycles remain errors; this does not confer a recovery target.
func (c *PrimaryCoordinator) WaitRecoveryAvailability(ctx context.Context) error {
	return c.withRecoveryAvailability(ctx, nil)
}

func (c *PrimaryCoordinator) withRecoveryAvailability(ctx context.Context, visit func() error) error {
	c.mu.Lock()
	revision := c.recoveryRevision
	c.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if revision != c.recoveryRevision {
			c.mu.Unlock()
			return fmt.Errorf("%w: Primary recovery lifecycle revoked", ErrReplicationUnavailable)
		}
		err := c.recoveryAuthorityLocked()
		if err == nil {
			if visit != nil {
				err = visit()
			}
			c.mu.Unlock()
			return err
		}
		if !c.recoveryPeerWaitLocked() {
			c.mu.Unlock()
			return err
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (c *PrimaryCoordinator) recoveryPeerWaitLocked() bool {
	return !c.stopped && c.poisoned == nil && c.manualUnavailable == nil && c.source != nil && errors.Is(c.unavailable, ErrPeerNotConnected)
}

func (c *PrimaryCoordinator) recoveryAuthorityLocked() error {
	if c.stopped || c.poisoned != nil || c.manualUnavailable != nil || !c.available {
		return fmt.Errorf("%w: %v", ErrReplicationUnavailable, errors.Join(c.poisoned, c.manualUnavailable, c.unavailable))
	}
	return nil
}

// CaptureRecoveryTarget binds an already-applied Ledger position to the entire
// current durable ordering cut, including preceding/trailing non-Ledger facts.
// Native content/hash/offset and all projection readiness checks remain the
// maintenance runtime's responsibility. A mismatching position is an error,
// never a fabricated index or a successful empty-prefix wait.
func (c *PrimaryCoordinator) CaptureRecoveryTarget(ctx context.Context, ledgerPosition StoreCursor) (RecoveryTarget, error) {
	if err := ctx.Err(); err != nil {
		return RecoveryTarget{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captureRecoveryTargetLocked(ledgerPosition)
}

// CaptureRecoveryTargetWhenAvailable is for withdrawn, joined startup recovery.
// Peer availability and target capture share one lock; reconnect never renews
// a revoked lifecycle. Ordinary capture keeps its immediate refusal contract.
func (c *PrimaryCoordinator) CaptureRecoveryTargetWhenAvailable(ctx context.Context, ledgerPosition StoreCursor) (target RecoveryTarget, err error) {
	err = c.withRecoveryAvailability(ctx, func() error {
		var captureErr error
		target, captureErr = c.captureRecoveryTargetLocked(ledgerPosition)
		return captureErr
	})
	return target, err
}

func (c *PrimaryCoordinator) captureRecoveryTargetLocked(ledgerPosition StoreCursor) (RecoveryTarget, error) {
	if err := c.recoveryAuthorityLocked(); err != nil {
		return RecoveryTarget{}, err
	}
	prefix, err := c.journal.capturePrefix()
	if err != nil {
		return RecoveryTarget{}, err
	}
	if prefix.ClusterID != c.clusterID || prefix.Incarnation != c.incarnation || prefix.Term != c.term || prefix.Index == 0 || prefix.Stores[StoreLedger-StoreLedger] != ledgerPosition {
		return RecoveryTarget{}, errors.New("recovery Ledger cut or leadership identity does not match ordering prefix")
	}
	return RecoveryTarget{owner: c, revision: c.recoveryRevision, prefix: prefix}, nil
}

func (c *PrimaryCoordinator) WaitRecoveryTarget(ctx context.Context, target RecoveryTarget) error {
	return c.waitRecoveryTarget(ctx, target, false, false)
}

// WaitRecoveryTargetWithPeer keeps the same opaque target while a source-backed
// peer reconnects. It never recaptures authority after withdrawal or term change.
func (c *PrimaryCoordinator) WaitRecoveryTargetWithPeer(ctx context.Context, target RecoveryTarget) error {
	return c.waitRecoveryTarget(ctx, target, false, true)
}

// WaitRecoveryAppliedTarget joins every configured Replica at the captured
// authenticated Ordering prefix. Confirmation quorum alone does not prove
// that trailing Metadata or derived state has been applied on every member.
func (c *PrimaryCoordinator) WaitRecoveryAppliedTarget(ctx context.Context, target RecoveryTarget) error {
	return c.waitRecoveryTarget(ctx, target, true, false)
}

func (c *PrimaryCoordinator) waitRecoveryTarget(ctx context.Context, target RecoveryTarget, applied, waitPeer bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if target.owner != c || target.revision != c.recoveryRevision || target.prefix.ClusterID != c.clusterID || target.prefix.Incarnation != c.incarnation || target.prefix.Term != c.term || target.prefix.Index == 0 {
			c.mu.Unlock()
			return errors.New("recovery target belongs to another or revoked Primary lifecycle")
		}
		authorityErr := c.recoveryAuthorityLocked()
		if authorityErr != nil && (!waitPeer || !c.recoveryPeerWaitLocked()) {
			c.mu.Unlock()
			return authorityErr
		}
		invalidity, err := c.journal.verifyRecoveryPrefix(target.prefix)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		if authorityErr == nil && target.prefix.Index <= c.confirmed && (!applied || c.allPeersApplied(target.prefix.Index)) {
			c.mu.Unlock()
			return nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-invalidity:
		}
	}
}
