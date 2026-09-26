package replication

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

var ErrPeerNotConnected = errors.New("replication peer is not connected")
var ErrStalePeerSession = errors.New("replication peer session generation is stale")
var errPeerDataNotAuthorized = errors.New("authenticated peer hello did not authorize a data session")

type PeerEndpoint struct {
	NodeID     string
	Address    string
	ServerName string
	SPKISHA256 string
}

type ConnectionRecordHandler func(context.Context, Hello, StreamRecord, func([]byte) error) error
type AuthenticatedHelloHandler func(context.Context, Hello) (bool, error)

type ConnectionManagerOptions struct {
	LocalNode  string
	TLS        TLSFiles
	ClusterKey []byte
	Peers      []PeerEndpoint
	LocalHello func() (Hello, error)
	// OnAuthenticatedHello durably adjudicates incarnation, term and role before
	// a connection can become visible for data delivery. It is called for every
	// authenticated role pairing; the bool authorizes data only after that
	// adjudication has completed.
	OnAuthenticatedHello  AuthenticatedHelloHandler
	Handle                ConnectionRecordHandler
	OnDataSessionReady    func(peer string) error
	OnSessionError        func(peer string, err error)
	Session               PeerSessionOptions
	DialTimeout           time.Duration
	MinRetryBackoff       time.Duration
	MaxRetryBackoff       time.Duration
	StableSessionDuration time.Duration
	MaxHandshakes         int
}

type ConnectionManager struct {
	options    ConnectionManagerOptions
	serverTLS  *tls.Config
	clientBase *tls.Config
	key        [sha256.Size]byte
	peers      map[string]PeerEndpoint
	pins       map[string]string
	nonces     *NonceCache
	registry   *SessionRegistry
	serveMu    sync.Mutex
	serving    bool
	closed     bool
}

func NewConnectionManager(options ConnectionManagerOptions) (*ConnectionManager, error) {
	if len(options.ClusterKey) != sha256.Size || options.LocalHello == nil || options.OnAuthenticatedHello == nil || options.Handle == nil {
		return nil, errors.New("replication connection manager requires cluster key, hello adjudicator, hello source and record handler")
	}
	if len(options.Peers) < 1 || len(options.Peers) > 2 {
		return nil, errors.New("replication connection manager requires one or two peers")
	}
	var err error
	options.Session, err = options.Session.normalized()
	if err != nil {
		return nil, err
	}
	if options.DialTimeout == 0 {
		options.DialTimeout = 5 * time.Second
	}
	if options.MinRetryBackoff == 0 {
		options.MinRetryBackoff = 100 * time.Millisecond
	}
	if options.MaxRetryBackoff == 0 {
		options.MaxRetryBackoff = 5 * time.Second
	}
	if options.StableSessionDuration == 0 {
		options.StableSessionDuration = 30 * time.Second
	}
	if options.MaxHandshakes == 0 {
		options.MaxHandshakes = 4
	}
	if options.DialTimeout <= 0 || options.MinRetryBackoff <= 0 || options.MaxRetryBackoff < options.MinRetryBackoff || options.StableSessionDuration <= 0 || options.MaxHandshakes < 1 || options.MaxHandshakes > 16 {
		return nil, errors.New("replication connection manager limits are invalid")
	}
	serverTLS, err := LoadServerTLSConfig(options.TLS)
	if err != nil {
		return nil, err
	}
	clientBase, err := loadTLSConfig(options.TLS)
	if err != nil {
		return nil, err
	}
	peers := make(map[string]PeerEndpoint, len(options.Peers))
	pins := make(map[string]string, len(options.Peers))
	names := make([]string, 0, len(options.Peers))
	for _, peer := range options.Peers {
		if peer.NodeID == "" || peer.NodeID == options.LocalNode || peer.Address == "" {
			return nil, errors.New("replication connection manager peer is invalid")
		}
		if _, exists := peers[peer.NodeID]; exists {
			return nil, errors.New("replication connection manager peer is duplicated")
		}
		if _, err := parseDigestText(peer.SPKISHA256); err != nil {
			return nil, fmt.Errorf("replication peer %s SPKI pin: %w", peer.NodeID, err)
		}
		if peer.ServerName == "" {
			host, _, err := net.SplitHostPort(peer.Address)
			if err != nil || host == "" {
				return nil, fmt.Errorf("replication peer %s address cannot supply a TLS server name", peer.NodeID)
			}
			peer.ServerName = host
		}
		peers[peer.NodeID] = peer
		pins[peer.NodeID] = peer.SPKISHA256
		names = append(names, peer.NodeID)
	}
	registry, err := NewSessionRegistry(options.LocalNode, names)
	if err != nil {
		return nil, err
	}
	manager := &ConnectionManager{
		options: options, serverTLS: serverTLS, clientBase: clientBase,
		peers: peers, pins: pins, nonces: NewNonceCache(256), registry: registry,
	}
	copy(manager.key[:], options.ClusterKey)
	manager.options.ClusterKey = nil
	return manager, nil
}

// Serve owns the supplied already-bound listener until ctx is cancelled. A
// caller can bind every required listener first and only then publish process
// readiness; production may pass a normal TCP listener, while tests can use a
// controlled listener without changing session code.
func (m *ConnectionManager) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("replication connection manager listener is required")
	}
	m.serveMu.Lock()
	if m.serving || m.closed {
		m.serveMu.Unlock()
		_ = listener.Close()
		return errors.New("replication connection manager cannot be served twice")
	}
	m.serving = true
	m.serveMu.Unlock()

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fatal := make(chan error, 1)
	var wait sync.WaitGroup
	handshakes := make(chan struct{}, m.options.MaxHandshakes)
	wait.Add(1)
	go func() {
		defer wait.Done()
		if err := m.acceptLoop(serveCtx, listener, handshakes, &wait); err != nil {
			select {
			case fatal <- err:
			default:
			}
		}
	}()
	for _, peer := range m.peers {
		if !m.registry.PreferOutbound(peer.NodeID) {
			continue
		}
		peer := peer
		wait.Add(1)
		go func() {
			defer wait.Done()
			m.dialLoop(serveCtx, peer)
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-fatal:
	}
	cancel()
	_ = listener.Close()
	closeErr := m.registry.Close()
	wait.Wait()
	m.serveMu.Lock()
	m.closed = true
	clear(m.key[:])
	m.serveMu.Unlock()
	return errors.Join(runErr, closeErr)
}

func (m *ConnectionManager) Send(peer string, encoded []byte) error {
	managed, _, ok := m.registry.Current(peer)
	if !ok {
		return ErrPeerNotConnected
	}
	session, ok := managed.(*PeerSession)
	if !ok {
		return errors.New("replication registry contains an unexpected session type")
	}
	return session.Send(encoded)
}

func (m *ConnectionManager) acceptLoop(ctx context.Context, listener net.Listener, handshakes chan struct{}, wait *sync.WaitGroup) error {
	for {
		raw, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept replication connection: %w", err)
		}
		select {
		case handshakes <- struct{}{}:
			wait.Add(1)
			go func() {
				defer wait.Done()
				m.acceptOne(ctx, raw, func() { <-handshakes })
			}()
		default:
			_ = raw.Close()
			m.report("", errors.New("replication handshake concurrency limit reached"))
		}
	}
}

func (m *ConnectionManager) acceptOne(ctx context.Context, raw net.Conn, releaseHandshake func()) {
	released := false
	defer func() {
		if !released {
			releaseHandshake()
		}
	}()
	local, err := m.options.LocalHello()
	if err != nil {
		_ = raw.Close()
		m.report("", err)
		return
	}
	session, err := AcceptPeerSession(ctx, raw, m.serverTLS, local, m.key[:], m.pins, m.nonces, m.options.Session)
	releaseHandshake()
	released = true
	if err != nil {
		m.report("", err)
		return
	}
	if err := m.authorizeSession(ctx, session); err != nil {
		if !errors.Is(err, errPeerDataNotAuthorized) {
			m.report(session.Peer().NodeID, err)
		}
		return
	}
	_, err = m.runRegistered(ctx, session.Peer().NodeID, false, session)
	if err != nil && ctx.Err() == nil && !errors.Is(err, ErrNonPreferredPeerConnection) {
		m.report(session.Peer().NodeID, err)
	}
}

func (m *ConnectionManager) dialLoop(ctx context.Context, peer PeerEndpoint) {
	attempt := 0
	for ctx.Err() == nil {
		if _, _, connected := m.registry.Current(peer.NodeID); connected {
			if !waitForRetry(ctx, m.retryDelay(0)) {
				return
			}
			continue
		}
		dialer := net.Dialer{Timeout: m.options.DialTimeout}
		raw, err := dialer.DialContext(ctx, "tcp", peer.Address)
		if err == nil {
			local, helloErr := m.options.LocalHello()
			if helloErr != nil {
				_ = raw.Close()
				err = helloErr
			} else {
				clientTLS := m.clientBase.Clone()
				clientTLS.ServerName = peer.ServerName
				var session *PeerSession
				session, err = DialPeerSession(ctx, raw, clientTLS, local, m.key[:], peer.NodeID, peer.SPKISHA256, m.nonces, m.options.Session)
				if err == nil {
					err = m.authorizeSession(ctx, session)
					if err == nil {
						var lifetime time.Duration
						lifetime, err = m.runRegistered(ctx, peer.NodeID, true, session)
						if lifetime >= m.options.StableSessionDuration {
							attempt = 0
						}
					}
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, errPeerDataNotAuthorized) && !errors.Is(err, ErrNonPreferredPeerConnection) {
			m.report(peer.NodeID, err)
		}
		attempt++
		if !waitForRetry(ctx, m.retryDelay(attempt)) {
			return
		}
	}
}

func (m *ConnectionManager) authorizeSession(ctx context.Context, session *PeerSession) error {
	allowData, err := m.options.OnAuthenticatedHello(ctx, session.Peer())
	if err != nil {
		_ = session.Close()
		return err
	}
	if !allowData {
		_ = session.Close()
		return errPeerDataNotAuthorized
	}
	if !session.DataAuthorized() {
		_ = session.Close()
		return errors.New("hello adjudicator authorized data for incompatible peer roles")
	}
	return nil
}

func (m *ConnectionManager) runRegistered(ctx context.Context, peer string, outbound bool, session *PeerSession) (time.Duration, error) {
	started := time.Now()
	generation, err := m.registry.Register(peer, outbound, session)
	if err != nil {
		return time.Since(started), err
	}
	if m.options.OnDataSessionReady != nil {
		if err := m.options.OnDataSessionReady(peer); err != nil {
			m.registry.Remove(peer, generation)
			_ = session.Close()
			return time.Since(started), fmt.Errorf("prepare replication data session for %s: %w", peer, err)
		}
	}
	err = session.Run(ctx, func(handlerCtx context.Context, hello Hello, record StreamRecord) error {
		return m.registry.Deliver(peer, generation, func() error {
			return m.options.Handle(handlerCtx, hello, record, session.Send)
		})
	})
	m.registry.Remove(peer, generation)
	return time.Since(started), err
}

func (m *ConnectionManager) retryDelay(attempt int) time.Duration {
	delay := m.options.MinRetryBackoff
	for count := 0; count < attempt && delay < m.options.MaxRetryBackoff/2; count++ {
		delay *= 2
	}
	if delay > m.options.MaxRetryBackoff {
		delay = m.options.MaxRetryBackoff
	}
	var random [1]byte
	if _, err := cryptorand.Read(random[:]); err == nil {
		// 80%-120% jitter keeps simultaneously restarted members from retaining
		// the same reconnect cadence without using a shared pseudo-random source.
		delay = delay * time.Duration(80+int(random[0])%41) / 100
	}
	return delay
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (m *ConnectionManager) report(peer string, err error) {
	if err != nil && m.options.OnSessionError != nil {
		m.options.OnSessionError(peer, err)
	}
}
