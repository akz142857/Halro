package replication

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var ErrPeerSessionQueueFull = errors.New("replication peer session queue is full")
var ErrPeerSessionClosed = errors.New("replication peer session is closed")

type PeerSessionOptions struct {
	HandshakeTimeout        time.Duration
	ReadTimeout             time.Duration
	WriteTimeout            time.Duration
	QueueCapacity           int
	MaxQueuedBytes          int64
	MaxReadBytesPerSecond   int64
	MaxReadRecordsPerSecond int
}

func (o PeerSessionOptions) normalized() (PeerSessionOptions, error) {
	if o.HandshakeTimeout == 0 {
		o.HandshakeTimeout = 10 * time.Second
	}
	if o.WriteTimeout == 0 {
		o.WriteTimeout = 10 * time.Second
	}
	if o.ReadTimeout == 0 {
		o.ReadTimeout = 30 * time.Second
	}
	if o.QueueCapacity == 0 {
		o.QueueCapacity = 64
	}
	if o.MaxQueuedBytes == 0 {
		o.MaxQueuedBytes = 64 << 20
	}
	if o.MaxReadBytesPerSecond == 0 {
		o.MaxReadBytesPerSecond = 64 << 20
	}
	if o.MaxReadRecordsPerSecond == 0 {
		o.MaxReadRecordsPerSecond = 4096
	}
	if o.HandshakeTimeout <= 0 || o.ReadTimeout <= 0 || o.WriteTimeout <= 0 || o.QueueCapacity < 1 || o.QueueCapacity > 1024 ||
		o.MaxQueuedBytes < streamPrefixBytes || o.MaxQueuedBytes > 1<<30 ||
		o.MaxReadBytesPerSecond < streamPrefixBytes || o.MaxReadBytesPerSecond > 1<<30 ||
		o.MaxReadRecordsPerSecond < 1 || o.MaxReadRecordsPerSecond > 1_000_000 {
		return PeerSessionOptions{}, errors.New("replication peer session timeouts or queue capacity are invalid")
	}
	return o, nil
}

type PeerRecordHandler func(context.Context, Hello, StreamRecord) error

type outboundRecord struct {
	encoded []byte
	done    chan error
}

// PeerSession owns one authenticated connection. Run creates exactly one
// reader and one writer; Send only copies into the bounded writer queue and
// never performs socket I/O in a store durability callback.
type PeerSession struct {
	conn           *tls.Conn
	local          Hello
	peer           Hello
	incoming       StreamDirection
	outgoing       StreamDirection
	data           bool
	readWait       time.Duration
	writeWait      time.Duration
	queue          chan outboundRecord
	maxQueued      int64
	queued         atomic.Int64
	maxReadBytes   int64
	maxReadRecords int
	closeOnce      sync.Once
	closed         atomic.Bool
	closedSignal   chan struct{}
	runMu          sync.Mutex
	running        bool
}

func DialPeerSession(ctx context.Context, raw net.Conn, tlsConfig *tls.Config, local Hello, key []byte, expectedNode, expectedPin string, nonces *NonceCache, options PeerSessionOptions) (*PeerSession, error) {
	if raw == nil || tlsConfig == nil {
		return nil, errors.New("replication client session requires a connection and TLS config")
	}
	options, err := options.normalized()
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	local, err = helloWithFreshNonce(local)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	connection := tls.Client(raw, tlsConfig.Clone())
	handshakeCtx, cancel := context.WithTimeout(ctx, options.HandshakeTimeout)
	defer cancel()
	peer, err := ClientHandshake(handshakeCtx, connection, local, key, expectedNode, expectedPin, nonces)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	return newPeerSession(connection, local, peer, options)
}

func AcceptPeerSession(ctx context.Context, raw net.Conn, tlsConfig *tls.Config, local Hello, key []byte, peerPins map[string]string, nonces *NonceCache, options PeerSessionOptions) (*PeerSession, error) {
	if raw == nil || tlsConfig == nil {
		return nil, errors.New("replication server session requires a connection and TLS config")
	}
	options, err := options.normalized()
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	local, err = helloWithFreshNonce(local)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	connection := tls.Server(raw, tlsConfig.Clone())
	handshakeCtx, cancel := context.WithTimeout(ctx, options.HandshakeTimeout)
	defer cancel()
	peer, err := ServerHandshake(handshakeCtx, connection, local, key, peerPins, nonces)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	return newPeerSession(connection, local, peer, options)
}

func helloWithFreshNonce(hello Hello) (Hello, error) {
	if _, err := rand.Read(hello.Nonce[:]); err != nil {
		return Hello{}, fmt.Errorf("generate replication hello nonce: %w", err)
	}
	if hello.Nonce == ([NonceBytes]byte{}) {
		return Hello{}, errors.New("generated replication hello nonce is zero")
	}
	return hello, nil
}

func newPeerSession(connection *tls.Conn, local, peer Hello, options PeerSessionOptions) (*PeerSession, error) {
	var err error
	options, err = options.normalized()
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	var incoming, outgoing StreamDirection
	data := true
	switch {
	case local.Role == RolePrimary && peer.Role == RoleReplica:
		incoming, outgoing = StreamReplicaToPrimary, StreamPrimaryToReplica
	case local.Role == RoleReplica && peer.Role == RolePrimary:
		incoming, outgoing = StreamPrimaryToReplica, StreamReplicaToPrimary
	default:
		data = false
	}
	return &PeerSession{
		conn: connection, local: local, peer: peer, incoming: incoming, outgoing: outgoing, data: data,
		readWait: options.ReadTimeout, writeWait: options.WriteTimeout,
		queue: make(chan outboundRecord, options.QueueCapacity), maxQueued: options.MaxQueuedBytes,
		maxReadBytes: options.MaxReadBytesPerSecond, maxReadRecords: options.MaxReadRecordsPerSecond,
		closedSignal: make(chan struct{}),
	}, nil
}

func (s *PeerSession) Peer() Hello          { return s.peer }
func (s *PeerSession) DataAuthorized() bool { return s.data }

func (s *PeerSession) Send(encoded []byte) error {
	return s.enqueue(encoded, false)
}

// SendSync is reserved for the tiny promotion control records whose durable
// state transition must not trigger process shutdown before the response has
// actually crossed the socket. Data frames and ACKs continue to use Send so a
// storage fsync callback never waits on network I/O.
func (s *PeerSession) SendSync(encoded []byte) error {
	return s.enqueue(encoded, true)
}

func (s *PeerSession) sendFromHandler(encoded []byte) error {
	if len(encoded) >= streamPrefixBytes {
		var magic [8]byte
		copy(magic[:], encoded[4:streamPrefixBytes])
		kind, _, _, err := streamRecordShape(magic, s.outgoing)
		if err == nil && (kind == StreamRecordPromotionProposal || kind == StreamRecordPromotionPromise) {
			return s.SendSync(encoded)
		}
	}
	return s.Send(encoded)
}

func (s *PeerSession) enqueue(encoded []byte, wait bool) error {
	if s.closed.Load() {
		return ErrPeerSessionClosed
	}
	if len(encoded) == 0 {
		return errors.New("replication peer session record is empty")
	}
	// Validate type, direction and bound before retaining a copy in the queue.
	if err := WriteStreamRecord(discardWriter{}, s.outgoing, encoded); err != nil {
		if !s.data {
			return fmt.Errorf("authenticated control-only session cannot exchange this record: %w", err)
		}
		return err
	}
	size := int64(len(encoded))
	for {
		used := s.queued.Load()
		if size > s.maxQueued-used {
			return ErrPeerSessionQueueFull
		}
		if s.queued.CompareAndSwap(used, used+size) {
			break
		}
	}
	copyOfRecord := append([]byte(nil), encoded...)
	record := outboundRecord{encoded: copyOfRecord}
	if wait {
		record.done = make(chan error, 1)
	}
	select {
	case s.queue <- record:
	default:
		s.queued.Add(-size)
		return ErrPeerSessionQueueFull
	}
	if !wait {
		return nil
	}
	select {
	case err := <-record.done:
		return err
	case <-s.closedSignal:
		return ErrPeerSessionClosed
	}
}

type discardWriter struct{}

func (discardWriter) Write(value []byte) (int, error) { return len(value), nil }

func (s *PeerSession) Run(ctx context.Context, handler PeerRecordHandler) error {
	if handler == nil {
		return errors.New("replication peer session handler is required")
	}
	s.runMu.Lock()
	if s.running {
		s.runMu.Unlock()
		return errors.New("replication peer session is already running")
	}
	s.running = true
	s.runMu.Unlock()

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopDeadline := context.AfterFunc(sessionCtx, func() { _ = s.conn.SetDeadline(time.Now()) })
	defer stopDeadline()
	errorsOut := make(chan error, 2)
	go func() { errorsOut <- s.readLoop(sessionCtx, handler) }()
	go func() { errorsOut <- s.writeLoop(sessionCtx) }()
	first := <-errorsOut
	cancel()
	_ = s.Close()
	second := <-errorsOut
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if first == nil {
		return second
	}
	return first
}

// ExchangeControl performs one request/response before Run owns the socket.
// It is used by an offline promotion command: the candidate has no long-lived
// runtime or queue, but still uses the identical authenticated member session.
func (s *PeerSession) ExchangeControl(ctx context.Context, encoded []byte) (StreamRecord, error) {
	s.runMu.Lock()
	if s.running {
		s.runMu.Unlock()
		return StreamRecord{}, errors.New("replication peer session is already running")
	}
	s.running = true
	s.runMu.Unlock()
	defer s.Close()
	cleanup := bindConnectionContext(ctx, s.conn)
	defer cleanup()
	if s.writeWait > 0 {
		if err := s.conn.SetWriteDeadline(time.Now().Add(s.writeWait)); err != nil {
			return StreamRecord{}, err
		}
	}
	if err := WriteStreamRecord(s.conn, s.outgoing, encoded); err != nil {
		return StreamRecord{}, err
	}
	if err := s.conn.SetReadDeadline(time.Now().Add(s.readWait)); err != nil {
		return StreamRecord{}, err
	}
	return ReadStreamRecord(s.conn, s.incoming)
}

func (s *PeerSession) readLoop(ctx context.Context, handler PeerRecordHandler) error {
	windowStart := time.Now()
	var windowBytes int64
	windowRecords := 0
	for {
		if err := s.conn.SetReadDeadline(time.Now().Add(s.readWait)); err != nil {
			return err
		}
		record, err := ReadStreamRecord(s.conn, s.incoming)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		now := time.Now()
		if now.Sub(windowStart) >= time.Second {
			windowStart, windowBytes, windowRecords = now, 0, 0
		}
		windowBytes += int64(len(record.Encoded))
		windowRecords++
		if windowBytes > s.maxReadBytes || windowRecords > s.maxReadRecords {
			return errors.New("replication peer session read rate limit exceeded")
		}
		if err := handler(ctx, s.peer, record); err != nil {
			return err
		}
	}
}

func (s *PeerSession) writeLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case record := <-s.queue:
			size := int64(len(record.encoded))
			if s.writeWait > 0 {
				if err := s.conn.SetWriteDeadline(time.Now().Add(s.writeWait)); err != nil {
					s.queued.Add(-size)
					if record.done != nil {
						record.done <- err
					}
					return err
				}
			}
			if err := WriteStreamRecord(s.conn, s.outgoing, record.encoded); err != nil {
				s.queued.Add(-size)
				if record.done != nil {
					record.done <- err
				}
				return err
			}
			s.queued.Add(-size)
			if err := s.conn.SetWriteDeadline(time.Time{}); err != nil {
				if record.done != nil {
					record.done <- err
				}
				return err
			}
			if record.done != nil {
				record.done <- nil
			}
		}
	}
}

func (s *PeerSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.closedSignal)
		err = s.conn.Close()
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			// The authenticated stream is already being retired and the socket is
			// closed even when TLS cannot deliver close_notify before its deadline.
			// Treating that advisory alert as a manager shutdown failure makes a
			// healthy peer race look like lost replicated data.
			err = nil
		}
	})
	return err
}
