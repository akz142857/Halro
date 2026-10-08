package replication

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func queueBudgetFixture(t *testing.T, slots int, ram int64) (*resourcebudget.Manager, *PeerQueueBudget) {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: int64(slots)*(ram+resourcebudget.ClaimRAMBytes) + 8192}, RecoveryReserve: resourcebudget.Cost{RAM: int64(slots) * (ram + resourcebudget.ClaimRAMBytes)}, MaxClaims: slots + 4, RecoveryClaims: slots, MaxOwners: 4, MaxPressureAge: time.Minute}
	limits.Classes[resourcebudget.Recovery].RAM = int64(slots) * ram
	limits.Classes[resourcebudget.Control].RAM = int64(limits.MaxClaims) * resourcebudget.ClaimRAMBytes
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := NewPeerQueueBudget(m, ram, slots)
	if err != nil {
		t.Fatal(err)
	}
	return m, budget // Unknown still allows dedicated recovery.
}
func queuedTestSession(t *testing.T, budget *PeerQueueBudget) *PeerSession {
	t.Helper()
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })
	s, err := newPeerSession(tls.Client(left, &tls.Config{}), testHello("halro-0", RolePrimary, 1), testHello("halro-1", RoleReplica, 2), PeerSessionOptions{QueueCapacity: 2, MaxQueuedBytes: 1024, QueueBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func queueTestFrame(t *testing.T) []byte {
	t.Helper()
	b, err := testConfirmationFrame(1, KindLeadershipEstablished).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func assertQueueFee(t *testing.T, m *resourcebudget.Manager, b *PeerQueueBudget, count int) {
	t.Helper()
	stats := m.Snapshot()
	if stats.RecoveryClaims != count || stats.Classes[resourcebudget.Recovery].RAM != int64(count)*b.ram || len(b.slots) != count || stats.Used.RAM != int64(count)*(b.ram+resourcebudget.ClaimRAMBytes) {
		t.Fatal("queue ownership/fee mismatch", count, stats, len(b.slots))
	}
}
func TestPeerQueueBudgetBoundsLiveAndRetiringOwnersAndRefusesBeforeAllocation(t *testing.T) {
	m, b := queueBudgetFixture(t, 1, 4096)
	s := queuedTestSession(t, b)
	assertQueueFee(t, m, b, 1)
	for i := 0; i < 10; i++ {
		left, right := net.Pipe()
		rejected, err := newPeerSession(tls.Client(left, &tls.Config{}), s.local, s.peer, PeerSessionOptions{QueueBudget: b, QueueCapacity: 2, MaxQueuedBytes: 1024})
		_ = right.Close()
		if !errors.Is(err, ErrPeerQueueBudgetFull) || rejected != nil {
			t.Fatal("unbounded session or allocated queue", err)
		}
	}
	if err := s.Send(queueTestFrame(t)); err != nil {
		t.Fatal(err)
	}
	assertQueueFee(t, m, b, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertQueueFee(t, m, b, 0)
	replacement := queuedTestSession(t, b)
	_ = replacement.Close()
	assertQueueFee(t, m, b, 0)
	// Exhaust shared RAM while a local slot is free. Slot admission rolls back.
	blocker, err := m.TryAcquireRecovery(resourcebudget.Cost{RAM: b.ram})
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	rejected, err := newPeerSession(tls.Client(left, &tls.Config{}), s.local, s.peer, PeerSessionOptions{QueueBudget: b, QueueCapacity: 2, MaxQueuedBytes: 1024})
	_ = right.Close()
	if rejected != nil || !errors.Is(err, resourcebudget.ErrExhausted) || len(b.slots) != 0 {
		t.Fatal("failed RAM admission retained slot", err)
	}
	_ = blocker.Release()
	assertQueueFee(t, m, b, 0)
	if _, err := (PeerSessionOptions{QueueBudget: b}).normalized(); err == nil {
		t.Fatal("uncalibrated default queue accepted")
	}
}
func TestPeerQueueCloseClearsPendingCopiesSyncWaitersAndConcurrentSend(t *testing.T) {
	m, b := queueBudgetFixture(t, 1, 4096)
	s := queuedTestSession(t, b)
	frame := queueTestFrame(t)
	syncDone := make(chan error, 1)
	go func() { syncDone <- s.SendSync(frame) }()
	deadline := time.Now().Add(time.Second)
	for s.queued.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("sync record not queued")
		}
		time.Sleep(time.Millisecond)
	}
	assertQueueFee(t, m, b, 1)
	if err := s.Send(frame); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(frame); !errors.Is(err, ErrPeerSessionQueueFull) {
		t.Fatal(err)
	}
	var senders sync.WaitGroup
	for range 32 {
		senders.Add(1)
		go func() {
			defer senders.Done()
			for range 100 {
				err := s.Send(frame)
				if err != nil && !errors.Is(err, ErrPeerSessionClosed) && !errors.Is(err, ErrPeerSessionQueueFull) {
					t.Error(err)
				}
			}
		}()
	}
	_ = s.Close()
	senders.Wait()
	select {
	case err := <-syncDone:
		if !errors.Is(err, ErrPeerSessionClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release SendSync waiter")
	}
	assertQueueFee(t, m, b, 0)
	if s.queued.Load() != 0 || s.queue != nil || s.queueLease != nil {
		t.Fatal("Close retained queue bytes/owner")
	}
	if err := s.Run(context.Background(), func(context.Context, Hello, StreamRecord) error { return nil }); !errors.Is(err, ErrPeerSessionClosed) {
		t.Fatal("closed session restarted writer", err)
	}
}

type gatedQueueConn struct {
	net.Conn
	enabled          atomic.Bool
	once             sync.Once
	entered, release chan struct{}
}

func (c *gatedQueueConn) Write(p []byte) (int, error) {
	if !c.enabled.Load() {
		return c.Conn.Write(p)
	}
	c.once.Do(func() { close(c.entered) })
	<-c.release // controlled uncancellable writer; Close deliberately cannot join it
	return 0, errors.New("controlled writer stopped")
}
func TestPeerQueueCloseRetainsEnvelopeAndSlotUntilActualWriterReturns(t *testing.T) {
	m, b := queueBudgetFixture(t, 1, 4096)
	files, leaf := writeTestCertificateChain(t)
	serverTLS, err := LoadServerTLSConfig(files)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := LoadClientTLSConfig(files, "halro-1.internal")
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	raw := &gatedQueueConn{Conn: left, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(raw.release) }) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	key := []byte("0123456789abcdef0123456789abcdef")
	type result struct {
		s   *PeerSession
		err error
	}
	accepted := make(chan result, 1)
	go func() {
		s, err := AcceptPeerSession(ctx, right, serverTLS, testHello("halro-1", RoleReplica, 0), key, map[string]string{"halro-0": digestText(pin)}, NewNonceCache(16), PeerSessionOptions{})
		accepted <- result{s, err}
	}()
	client, err := DialPeerSession(ctx, raw, clientTLS, testHello("halro-0", RolePrimary, 0), key, "halro-1", digestText(pin), NewNonceCache(16), PeerSessionOptions{QueueBudget: b, QueueCapacity: 2, MaxQueuedBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	if server.err != nil {
		t.Fatal(server.err)
	}
	defer server.s.Close()
	raw.enabled.Store(true)
	runDone := make(chan error, 1)
	go func() { runDone <- client.Run(ctx, func(context.Context, Hello, StreamRecord) error { return nil }) }()
	frame := queueTestFrame(t)
	syncDone := make(chan error, 1)
	go func() { syncDone <- client.SendSync(frame) }()
	select {
	case <-raw.entered:
	case <-ctx.Done():
		t.Fatal("writer never entered controlled IO")
	}
	if err := client.Send(frame); err != nil {
		t.Fatal(err)
	} // one active writer and one tail
	_ = client.Close()
	select {
	case err := <-syncDone:
		if !errors.Is(err, ErrPeerSessionClosed) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Close did not release in-flight SendSync waiter")
	}
	assertQueueFee(t, m, b, 1)
	if client.queued.Load() != int64(len(frame)) {
		t.Fatal("Close discarded active writer's byte ownership")
	}
	client.runMu.Lock()
	tail := len(client.queue)
	client.runMu.Unlock()
	if tail != 0 {
		t.Fatal("Close retained pending tail")
	}
	if _, err := b.acquire(); !errors.Is(err, ErrPeerQueueBudgetFull) {
		t.Fatal("retiring writer's slot reused", err)
	}
	select {
	case err := <-runDone:
		t.Fatal("Run claimed drain before actual writer exit", err)
	default:
	}
	releaseOnce.Do(func() { close(raw.release) })
	select {
	case <-runDone:
	case <-ctx.Done():
		t.Fatal("writer owner did not drain")
	}
	assertQueueFee(t, m, b, 0)
	if client.queued.Load() != 0 {
		t.Fatal("in-flight byte fee survived writer exit")
	}
}

type queueClosingConn struct {
	net.Conn
	entered, release chan struct{}
}

func (c *queueClosingConn) Close() error { close(c.entered); <-c.release; return c.Conn.Close() }
func TestPeerQueueRetirementRetainsSlotUntilSocketCloseCompletes(t *testing.T) {
	m, b := queueBudgetFixture(t, 1, 4096)
	left, right := net.Pipe()
	defer right.Close()
	raw := &queueClosingConn{Conn: left, entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(raw.release) }) })
	s, err := newPeerSession(tls.Client(raw, &tls.Config{}), testHello("halro-0", RolePrimary, 1), testHello("halro-1", RoleReplica, 2), PeerSessionOptions{QueueBudget: b, QueueCapacity: 2, MaxQueuedBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(queueTestFrame(t)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case <-raw.entered:
	case <-time.After(time.Second):
		t.Fatal("socket retirement did not begin")
	}
	assertQueueFee(t, m, b, 1)
	if s.queued.Load() != 0 {
		t.Fatal("unsent tail survived retirement")
	}
	if _, err := b.acquire(); !errors.Is(err, ErrPeerQueueBudgetFull) {
		t.Fatal("closing socket escaped session bound", err)
	}
	once.Do(func() { close(raw.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertQueueFee(t, m, b, 0)
}
