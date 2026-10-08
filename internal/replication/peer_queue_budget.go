package replication

import (
	"errors"
	"fmt"
	"math"

	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

var ErrPeerQueueBudgetFull = errors.New("replication session queue slots exhausted")

// PeerQueueBudget bounds aggregate queue owners, including retiring sessions.
// One fixed envelope covers a session's queue and its in-flight writer record;
// NewPeerSessionBudget also reserves one inbound temporary working set.
// TLS internals, handshake and retained domain projections remain separate.
type PeerQueueBudget struct {
	manager *resourcebudget.Manager
	ram     int64 // aggregate queue + optional inbound envelope
	readRAM int64
	slots   chan struct{}
}

func NewPeerQueueBudget(manager *resourcebudget.Manager, ram int64, sessions int) (*PeerQueueBudget, error) {
	if manager == nil || ram <= 0 || sessions < 1 || sessions > replicationlimits.MaxBudgetedPeerSessions {
		return nil, errors.New("peer queue budget requires shared RAM and 1-3 session slots")
	}
	return &PeerQueueBudget{manager: manager, ram: ram, slots: make(chan struct{}, sessions)}, nil
}

// NewPeerSessionBudget admits both envelopes atomically before session buffers
// exist. The inbound envelope covers one borrowed record and synchronous handler
// temporaries; it does not account for retained domain state or TLS buffers.
func NewPeerSessionBudget(manager *resourcebudget.Manager, queueRAM, readRAM int64, sessions int) (*PeerQueueBudget, error) {
	if readRAM < replicationlimits.MinReadRAMBytes || queueRAM <= 0 || queueRAM > math.MaxInt64-readRAM {
		return nil, errors.New("peer session read RAM requires calibrated envelope without overflow")
	}
	b, err := NewPeerQueueBudget(manager, queueRAM+readRAM, sessions)
	if err != nil {
		return nil, err
	}
	b.readRAM = readRAM
	return b, nil
}

type peerQueueLease struct {
	budget *PeerQueueBudget
	permit resourcebudget.Permit
}

func (b *PeerQueueBudget) validate(options PeerSessionOptions) error {
	if b == nil {
		return nil
	}
	if b.slots == nil || cap(b.slots) < 1 || cap(b.slots) > replicationlimits.MaxBudgetedPeerSessions {
		return errors.New("invalid peer queue slots")
	}
	if b.manager != nil && b.ram-b.readRAM < options.MaxQueuedBytes+int64(options.QueueCapacity+1)*replicationlimits.QueueRecordRAMBytes+replicationlimits.QueueFixedRAMBytes {
		return errors.New("peer queue RAM must cover byte capacity and queue/writer metadata; allocator amplification requires calibration")
	}
	return nil
}
func (b *PeerQueueBudget) acquire() (*peerQueueLease, error) {
	if b == nil {
		return nil, nil
	}
	select {
	case b.slots <- struct{}{}:
	default:
		return nil, ErrPeerQueueBudgetFull
	}
	lease := &peerQueueLease{budget: b}
	if b.manager != nil {
		permit, err := b.manager.TryAcquireRecovery(resourcebudget.Cost{RAM: b.ram})
		if err != nil {
			<-b.slots
			return nil, fmt.Errorf("peer queue RAM admission: %w", err)
		}
		lease.permit = permit
	}
	return lease, nil
}

// Called once, after queue, socket, reader/handler and writer have retired.
func (l *peerQueueLease) release() {
	if l.budget.manager != nil {
		_ = l.permit.Release()
	}
	l.permit = resourcebudget.Permit{}
	<-l.budget.slots
	l.budget = nil
}
