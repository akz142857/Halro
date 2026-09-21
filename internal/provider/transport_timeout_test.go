package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
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
