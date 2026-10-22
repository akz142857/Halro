package replication

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrPeerCatchingUp refuses a record for a Replica whose session has not yet
// carried every earlier ordering index. The record is not lost: the session's
// catch-up reads it from the ordering journal once it reaches it.
var ErrPeerCatchingUp = errors.New("replication peer is catching up")

// FrameSource lets a Primary session read the exact bytes of any frame in its
// authenticated ordering prefix.
type FrameSource interface {
	OrderingHead() uint64
	EncodedFrame(index uint64) ([]byte, error)
}

// peerStream keeps one Primary-to-Replica session in ordering-index order. A
// Replica accepts a frame it already holds but refuses one that skips an
// index, so whichever path enqueues next — a live commit or catch-up from the
// journal — must enqueue exactly the next index, and both decide that under
// mu. Live commits never wait: one that is ahead of next, or meets a full
// queue, is refused and left to catch-up, which waits for the writer to make
// room. Catch-up reads the journal outside mu and outside the coordinator, so
// neither a slow read nor a full queue holds up a commit.
type peerStream struct {
	session *PeerSession
	frames  FrameSource
	wake    chan struct{}

	mu   sync.Mutex
	next uint64
}

func newPeerStream(session *PeerSession, frames FrameSource, durableIndex uint64) *peerStream {
	return &peerStream{session: session, frames: frames, wake: make(chan struct{}, 1), next: durableIndex + 1}
}

func (p *peerStream) offerFrame(index uint64, encoded []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < p.next {
		// This session already carried it, ahead of anything enqueued now.
		return nil
	}
	if index > p.next {
		p.signal()
		return ErrPeerCatchingUp
	}
	if err := p.session.Send(encoded); err != nil {
		if errors.Is(err, ErrPeerSessionQueueFull) {
			// The peer has fallen behind this index; catch-up carries it.
			p.signal()
			return fmt.Errorf("%w: %w", ErrPeerCatchingUp, err)
		}
		return err
	}
	p.next++
	return nil
}

// offerCommitNotice sends a notice only behind the frame it confirms: a
// Replica refuses a notice for an index it has not durably received.
func (p *peerStream) offerCommitNotice(confirmedIndex uint64, encoded []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if confirmedIndex >= p.next {
		p.signal()
		return ErrPeerCatchingUp
	}
	return p.session.Send(encoded)
}

func (p *peerStream) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// run carries the session from the Replica's durable index to the ordering
// head, and again whenever a live commit is refused, until ctx ends or the
// session closes.
func (p *peerStream) run(ctx context.Context) error {
	for {
		if err := p.catchUp(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-p.session.closedSignal:
			return nil
		case <-p.wake:
		}
	}
}

func (p *peerStream) catchUp(ctx context.Context) error {
	for {
		p.mu.Lock()
		index := p.next
		p.mu.Unlock()
		if index > p.frames.OrderingHead() {
			return nil
		}
		encoded, err := p.frames.EncodedFrame(index)
		if err != nil {
			return fmt.Errorf("catch up ordering index %d: %w", index, err)
		}
		if err := p.enqueue(ctx, index, encoded); err != nil {
			return err
		}
	}
}

// enqueue places index in the queue unless a live commit already did,
// waiting for the writer whenever the queue is full.
func (p *peerStream) enqueue(ctx context.Context, index uint64, encoded []byte) error {
	for {
		p.mu.Lock()
		if p.next != index {
			p.mu.Unlock()
			return nil
		}
		err := p.session.Send(encoded)
		if err == nil {
			p.next++
		}
		p.mu.Unlock()
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrPeerSessionQueueFull) {
			return fmt.Errorf("catch up ordering index %d: %w", index, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-p.session.closedSignal:
			return nil
		case <-p.session.drained:
		}
	}
}
