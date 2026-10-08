package replication

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replicationlimits"
)

func readBudgetFixture(t *testing.T) *PeerQueueBudget {
	t.Helper()
	_, queue := queueBudgetFixture(t, 1, 4096+replicationlimits.MinReadRAMBytes)
	queue.readRAM = replicationlimits.MinReadRAMBytes
	return queue
}

// A real authenticated Replica session sharing the aggregate read/queue owner.
func budgetedReadPair(t *testing.T, b *PeerQueueBudget, wrap func(net.Conn) net.Conn, options PeerSessionOptions) (*PeerSession, *PeerSession, context.Context) {
	t.Helper()
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
	if wrap != nil {
		left = wrap(left)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	key := []byte("0123456789abcdef0123456789abcdef")
	type result struct {
		s   *PeerSession
		err error
	}
	accepted := make(chan result, 1)
	go func() {
		s, err := AcceptPeerSession(ctx, right, serverTLS, testHello("halro-1", RolePrimary, 0), key, map[string]string{"halro-0": digestText(pin)}, NewNonceCache(16), PeerSessionOptions{})
		accepted <- result{s, err}
	}()
	options.QueueBudget = b
	options.QueueCapacity = 2
	options.MaxQueuedBytes = 1024
	client, err := DialPeerSession(ctx, left, clientTLS, testHello("halro-0", RoleReplica, 0), key, "halro-1", digestText(pin), NewNonceCache(16), options)
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	if server.err != nil {
		t.Fatal(server.err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.s.Close() })
	return client, server.s, ctx
}

func TestPeerReadBudgetRejectsSmallOrOverflowedEnvelope(t *testing.T) {
	b := readBudgetFixture(t)
	for _, ram := range []int64{-1, 0, replicationlimits.MinReadRAMBytes - 1, math.MaxInt64} {
		if _, err := NewPeerSessionBudget(b.manager, 4096, ram, 1); err == nil {
			t.Fatal("invalid read RAM accepted", ram)
		}
	}
	budget, err := NewPeerSessionBudget(b.manager, 4096, replicationlimits.MinReadRAMBytes, 1)
	if err != nil {
		t.Fatal(err)
	}
	if budget.ram != 4096+replicationlimits.MinReadRAMBytes {
		t.Fatal("inbound omitted", budget.ram)
	}
	if _, err := (PeerSessionOptions{QueueBudget: budget}).normalized(); err == nil {
		t.Fatal("inbound RAM incorrectly credited to queue capacity")
	}
	// Escaping wire ownership must not refund the session while the caller owns it.
	s := queuedTestSession(t, budget)
	if _, err := s.ExchangeControl(context.Background(), nil); err == nil {
		t.Fatal("unowned result allowed")
	}
	_ = s.Close()
	assertQueueFee(t, b.manager, budget, 0)
}

func TestPeerReadBudgetRetainsSlotThroughBlockedHandlerAfterClose(t *testing.T) {
	b := readBudgetFixture(t)
	client, server, ctx := budgetedReadPair(t, b, nil, PeerSessionOptions{})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx, func(_ context.Context, _ Hello, r StreamRecord) error {
			if len(r.Encoded) == 0 {
				t.Error("missing borrowed record")
			}
			assertQueueFee(t, b.manager, b, 1)
			close(entered)
			<-release
			return nil
		})
	}()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx, func(context.Context, Hello, StreamRecord) error { return nil }) }()
	if err := server.Send(queueTestFrame(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handler did not run")
	}
	_ = client.Close()
	assertQueueFee(t, b.manager, b, 1)
	if _, err := b.acquire(); !errors.Is(err, ErrPeerQueueBudgetFull) {
		t.Fatal("blocked handler slot reused", err)
	}
	select {
	case err := <-done:
		t.Fatal("Run returned before handler", err)
	default:
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("reader did not retire")
	}
	assertQueueFee(t, b.manager, b, 0)
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("server did not retire")
	}
}

type gatedReadConn struct {
	net.Conn
	enabled          atomic.Bool
	once             sync.Once
	entered, release chan struct{}
}

func (c *gatedReadConn) Read(p []byte) (int, error) {
	if c.enabled.Load() {
		c.once.Do(func() { close(c.entered) })
		<-c.release
		return 0, errors.New("controlled reader stopped")
	}
	return c.Conn.Read(p)
}
func TestPeerReadBudgetRetainsSlotUntilActualReadReturns(t *testing.T) {
	b := readBudgetFixture(t)
	raw := &gatedReadConn{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(raw.release) }) })
	client, server, ctx := budgetedReadPair(t, b, func(c net.Conn) net.Conn { raw.Conn = c; return raw }, PeerSessionOptions{})
	raw.enabled.Store(true)
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx, func(context.Context, Hello, StreamRecord) error { return nil }) }()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx, func(context.Context, Hello, StreamRecord) error { return nil }) }()
	select {
	case <-raw.entered:
	case <-ctx.Done():
		t.Fatal("read not entered")
	}
	_ = client.Close()
	assertQueueFee(t, b.manager, b, 1)
	if _, err := b.acquire(); !errors.Is(err, ErrPeerQueueBudgetFull) {
		t.Fatal("closing reader slot reused", err)
	}
	once.Do(func() { close(raw.release) })
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("read did not retire")
	}
	assertQueueFee(t, b.manager, b, 0)
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("server did not retire")
	}
}

func TestPeerReadBudgetRefundsMalformedRateAndHandlerFailures(t *testing.T) {
	for _, mode := range []string{"malformed", "truncated", "rate", "handler", "maximum", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			b := readBudgetFixture(t)
			options := PeerSessionOptions{}
			if mode == "rate" {
				options.MaxReadBytesPerSecond = streamPrefixBytes
			}
			client, server, parent := budgetedReadPair(t, b, nil, options)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			var calls atomic.Int64
			done := make(chan error, 1)
			go func() {
				done <- client.Run(ctx, func(_ context.Context, _ Hello, r StreamRecord) error {
					calls.Add(1)
					assertQueueFee(t, b.manager, b, 1)
					if mode == "maximum" && len(r.Encoded) <= MaxPayloadBytes {
						t.Error("did not read maximum payload")
					}
					return errors.New("controlled handler failure")
				})
			}()
			assertQueueFee(t, b.manager, b, 1) // already charged before the first prefix Read
			if mode == "cancel" {
				cancel()
			} else {
				encoded := queueTestFrame(t)
				if mode == "malformed" {
					encoded = make([]byte, streamPrefixBytes)
					binary.BigEndian.PutUint32(encoded, uint32(len(encoded)-4))
				}
				if mode == "truncated" {
					encoded = encoded[:streamPrefixBytes]
				}
				if mode == "maximum" {
					frame := testConfirmationFrame(1, KindData)
					frame.Payload = make([]byte, MaxPayloadBytes)
					var err error
					encoded, err = frame.MarshalBinary()
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := server.conn.Write(encoded); err != nil && mode != "malformed" {
					t.Fatal(err)
				}
			}
			_ = server.Close()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("failure swallowed")
				}
			case <-parent.Done():
				t.Fatal("failure did not retire")
			}
			assertQueueFee(t, b.manager, b, 0)
			want := int64(0)
			if mode == "handler" || mode == "maximum" {
				want = 1
			}
			if calls.Load() != want {
				t.Fatal("invalid admission/handler count", calls.Load(), want)
			}
		})
	}
}
