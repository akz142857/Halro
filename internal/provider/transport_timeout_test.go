package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/safetransport"
)

// The real error, produced by the real transport, because the defect was
// entirely about which error value arrives: http2's own timeout reports
// Timeout() and wraps no sentinel, so errors.Is(err, context.DeadlineExceeded)
// is false and the classifier fell through to "the connection could not be
// established". An operator then gets told to check DNS, TLS and the egress
// proxy for an upstream that was reachable the whole time and simply took
// longer than ResponseHeaderTimeout to start answering.
func TestAResponseHeaderTimeoutIsClassifiedAsATimeout(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-blocked:
		case <-request.Context().Done():
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	client := server.Client()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("test server client transport is %T, want *http.Transport", client.Transport)
	}
	transport.ResponseHeaderTimeout = 100 * time.Millisecond

	_, err := client.Post(server.URL, "application/json", strings.NewReader(`{}`))
	if err == nil {
		t.Fatal("the blocked handler answered")
	}
	// The test is only evidence if it produced the error the defect was about.
	if !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Fatalf("err = %v, want the transport's response-header timeout", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v wraps context.DeadlineExceeded, so it would have classified correctly before the fix", err)
	}

	failure := ClassifyTransportFailure(err)
	if failure.Class != ErrorTimeout {
		t.Fatalf("Class = %q, want %q: the upstream was reachable and simply did not answer in time", failure.Class, ErrorTimeout)
	}
	// Both unchanged by the reclassification, and both still the conservative
	// answer: the request went out, so the upstream may have run it and billed
	// for it, and asking again would be the second bill.
	if !failure.Ambiguous {
		t.Error("a request that was sent and never answered is not ambiguous")
	}
	if failure.Retryable {
		t.Error("a possibly-executed request was made retryable")
	}
}

// The other half of the rule. A timeout that happened before anything was sent
// is the connection never being made, and that is what the operator needs told
// — "check DNS, TLS, the proxy and the allowlist" is right about a dial and
// wrong about a slow answer.
func TestATimeoutBeforeAnythingWasSentStaysAConnectionFailure(t *testing.T) {
	for name, err := range map[string]error{
		"dial":       &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded},
		"resolution": &net.DNSError{Err: "i/o timeout", Name: "provider.example", IsTimeout: true},
	} {
		t.Run(name, func(t *testing.T) {
			if !timedOut(err) {
				t.Fatalf("%v does not report itself as a timeout, so this case proves nothing", err)
			}
			if !Unsent(err) {
				t.Fatalf("%v is not recognised as unsent, so this case proves nothing", err)
			}
			if class := TransportClass(err); class != ErrorConnect {
				t.Fatalf("Class = %q, want %q", class, ErrorConnect)
			}
		})
	}
}

type stalledResolver struct{ addr netip.Addr }

func (r stalledResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{r.addr}, nil
}

// stalledDialer answers the address the policy validated with a connection to a
// local listener that never speaks, so the real pinnedDialTLSContext runs
// against a handshake that cannot complete. The policy refuses loopback even
// with AllowPrivate, so the address it validates and the address dialled have
// to be separated for this path to be reachable at all.
type stalledDialer struct{ target string }

func (d stalledDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, d.target)
}

// A stalled TLS handshake produces one of two errors, and which one is a race
// between two deadlines set to the same instant: pinnedDialTLSContext bounds the
// handshake with a context and also puts that same deadline on the conn. If the
// context wins, crypto/tls returns context.DeadlineExceeded; if the conn wins, a
// read returns *net.OpError{Op: "read"} carrying os.ErrDeadlineExceeded.
//
// Before this change the two disagreed — the first was already a timeout, the
// second a connection failure — so the same stall reported two different classes
// depending on which deadline fired first. This asserts they now agree, and it
// asserts the invariant rather than the coin flip, because a test that pinned
// one shape passed locally and failed in CI for no reason but timing.
func TestAStalledTLSHandshakeIsATimeoutWhicheverDeadlineWins(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held struct {
		sync.Mutex
		conns []net.Conn
	}
	t.Cleanup(func() {
		_ = listener.Close()
		held.Lock()
		defer held.Unlock()
		for _, conn := range held.conns {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Held, never spoken to: the handshake waits for a ServerHello that
			// is never sent.
			held.Lock()
			held.conns = append(held.conns, conn)
			held.Unlock()
		}
	}()

	client, err := safetransport.NewClient(safetransport.Options{
		Policy: safetransport.Policy{
			RequireHTTPS: true, AllowedHosts: []string{"stalled.example"},
		},
		Resolver:              stalledResolver{addr: netip.MustParseAddr("203.0.113.10")},
		Dialer:                stalledDialer{target: listener.Addr().String()},
		ConnectTimeout:        300 * time.Millisecond,
		ResponseHeaderTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, requestErr := client.Get("https://stalled.example/")
	if requestErr == nil {
		t.Fatal("the stalled handshake completed")
	}
	// Named so a failure says which shape it saw, since that is the one thing
	// this test cannot control.
	shape := "conn deadline (*net.OpError)"
	if errors.Is(requestErr, context.DeadlineExceeded) {
		shape = "handshake context"
	}
	t.Logf("shape = %s: %v", shape, requestErr)
	// Not a tautology: whichever shape arrived has to actually be a timeout, or
	// the stall was something else and this proves nothing about it.
	if !errors.Is(requestErr, context.DeadlineExceeded) && !timedOut(requestErr) {
		t.Fatalf("the stall was not a timeout in either shape: %v", requestErr)
	}
	if class := TransportClass(requestErr); class != ErrorTimeout {
		t.Fatalf("Class = %q for the %s shape, want %q: both shapes of one stall must agree", class, shape, ErrorTimeout)
	}
}

// The same invariant without the race, so the behaviour is pinned even on a
// machine where one deadline always wins.
func TestBothShapesOfAStalledHandshakeClassifyAlike(t *testing.T) {
	for name, err := range map[string]error{
		"handshake context": context.DeadlineExceeded,
		"conn deadline":     &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			if class := TransportClass(err); class != ErrorTimeout {
				t.Fatalf("Class = %q, want %q", class, ErrorTimeout)
			}
		})
	}
}
