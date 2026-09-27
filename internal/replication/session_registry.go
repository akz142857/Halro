package replication

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

var ErrNonPreferredPeerConnection = errors.New("replication peer connection lost deterministic tie-break")

type managedPeerSession interface {
	Close() error
}

type peerSessionEntry struct {
	generation uint64
	session    managedPeerSession
	stale      bool
	inflight   uint64
	drained    chan struct{}
}

// SessionRegistry gives every peer one current session generation. The
// lexicographically lower node owns dialing; if both members dial at once, both
// endpoints select the same physical connection and close the other. A handler
// must check IsCurrent before applying a record so a read goroutine from a
// replaced connection cannot deliver a late ACK or frame. Delivery leases are
// held across the complete handler: replacement first makes the old generation
// stale, closes its socket and waits for any admitted handler to drain before
// publishing the replacement.
type SessionRegistry struct {
	registerMu sync.Mutex
	mu         sync.Mutex
	localNode  string
	configured map[string]struct{}
	entries    map[string]*peerSessionEntry
	next       uint64
	closed     bool
}

func NewSessionRegistry(localNode string, peers []string) (*SessionRegistry, error) {
	if localNode == "" || len(peers) < 1 || len(peers) > 2 {
		return nil, errors.New("replication session registry identity or peer count is invalid")
	}
	configured := make(map[string]struct{}, len(peers))
	for _, peer := range peers {
		if peer == "" || peer == localNode {
			return nil, errors.New("replication session registry peer is invalid")
		}
		if _, exists := configured[peer]; exists {
			return nil, errors.New("replication session registry peer is duplicated")
		}
		configured[peer] = struct{}{}
	}
	return &SessionRegistry{localNode: localNode, configured: configured, entries: make(map[string]*peerSessionEntry)}, nil
}

func (r *SessionRegistry) PreferOutbound(peer string) bool { return r.localNode < peer }

func (r *SessionRegistry) Register(peer string, outbound bool, session managedPeerSession) (uint64, error) {
	if session == nil {
		return 0, errors.New("replication peer session is required")
	}
	r.registerMu.Lock()
	defer r.registerMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = session.Close()
		return 0, errors.New("replication session registry is closed")
	}
	if _, ok := r.configured[peer]; !ok {
		r.mu.Unlock()
		_ = session.Close()
		return 0, errors.New("replication session peer is not configured")
	}
	if outbound != r.PreferOutbound(peer) {
		r.mu.Unlock()
		_ = session.Close()
		return 0, ErrNonPreferredPeerConnection
	}
	previous := r.entries[peer]
	if previous != nil {
		r.retireLocked(peer, previous)
	}
	r.mu.Unlock()
	if previous != nil {
		_ = previous.session.Close()
		<-previous.drained
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = session.Close()
		return 0, errors.New("replication session registry is closed")
	}
	r.next++
	entry := &peerSessionEntry{generation: r.next, session: session, drained: make(chan struct{})}
	r.entries[peer] = entry
	r.mu.Unlock()
	return entry.generation, nil
}

func (r *SessionRegistry) IsCurrent(peer string, generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[peer]
	return ok && entry.generation == generation && !entry.stale && !r.closed
}

func (r *SessionRegistry) Current(peer string) (managedPeerSession, uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[peer]
	if !ok || entry.stale || r.closed {
		return nil, 0, false
	}
	return entry.session, entry.generation, true
}

// Deliver admits one record from generation and keeps that generation current
// until handler returns. This closes the check-then-handle race: Register
// cannot publish a replacement while an old handler can still durably apply.
func (r *SessionRegistry) Deliver(peer string, generation uint64, handler func() error) error {
	if handler == nil {
		return errors.New("replication session delivery handler is required")
	}
	r.mu.Lock()
	entry, ok := r.entries[peer]
	if !ok || entry.generation != generation || entry.stale || r.closed {
		r.mu.Unlock()
		return ErrStalePeerSession
	}
	entry.inflight++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		entry.inflight--
		if entry.stale && entry.inflight == 0 {
			close(entry.drained)
		}
		r.mu.Unlock()
	}()
	return handler()
}

func (r *SessionRegistry) Remove(peer string, generation uint64) {
	r.mu.Lock()
	entry, ok := r.entries[peer]
	if ok && entry.generation == generation {
		r.retireLocked(peer, entry)
	}
	r.mu.Unlock()
}

func (r *SessionRegistry) retireLocked(peer string, entry *peerSessionEntry) {
	delete(r.entries, peer)
	entry.stale = true
	if entry.inflight == 0 {
		close(entry.drained)
	}
}

func (r *SessionRegistry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	peers := make([]string, 0, len(r.entries))
	for peer := range r.entries {
		peers = append(peers, peer)
	}
	sort.Strings(peers)
	entries := make([]*peerSessionEntry, 0, len(peers))
	for _, peer := range peers {
		entry := r.entries[peer]
		r.retireLocked(peer, entry)
		entries = append(entries, entry)
	}
	r.mu.Unlock()
	var closeErrors []error
	for index, entry := range entries {
		if err := entry.session.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close replication session for %s: %w", peers[index], err))
		}
		<-entry.drained
	}
	return errors.Join(closeErrors...)
}
