package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/safetransport"
)

func TestTransportClassSeparatesTheCallerFromTheNetwork(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"caller canceled", context.Canceled, ErrorCanceled},
		{"wrapped cancel from the HTTP client", fmt.Errorf("Post %q: %w", "https://provider.example/v1/chat/completions", context.Canceled), ErrorCanceled},
		{"deadline", context.DeadlineExceeded, ErrorTimeout},
		{"dial refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, ErrorConnect},
		{"opaque fault", errors.New("stream ended early"), ErrorConnect},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := TransportClass(testCase.err); got != testCase.want {
				t.Fatalf("TransportClass(%v) = %q, want %q", testCase.err, got, testCase.want)
			}
		})
	}
}

func TestUnsentSeparatesSetupFailuresFromPossiblyExecutedOnes(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"name resolution", &net.DNSError{Err: "no such host", Name: "provider.invalid"}, true},
		{"dial refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		// Past the dial the provider may already have run the request, so these
		// stay ambiguous even though they look like transport faults.
		{"reset while reading", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, false},
		{"write interrupted", &net.OpError{Op: "write", Err: errors.New("broken pipe")}, false},
		{"waiting for response", context.DeadlineExceeded, false},
		{"opaque fault", errors.New("stream ended early"), false},
		{
			"through the wrapper adapters return",
			&Error{Class: ErrorConnect, Cause: &net.OpError{Op: "dial", Err: errors.New("connection refused")}},
			true,
		},
		{
			"wrapper around an executed attempt",
			&Error{Class: ErrorMalformed, Cause: &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}},
			false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Unsent(testCase.err); got != testCase.want {
				t.Fatalf("Unsent(%v) = %v, want %v", testCase.err, got, testCase.want)
			}
		})
	}
}

// Unsent reads the shapes the standard library actually produces, so a
// synthetic table alone would keep passing if those shapes ever changed. This
// drives a real refused connection through net/http the way an adapter does.
func TestUnsentRecognisesARefusedConnectionFromTheStandardLibrary(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + address)
	if err == nil {
		response.Body.Close()
		t.Skip("a request to the released port unexpectedly connected")
	}
	if !Unsent(err) {
		t.Fatalf("a refused connection was not recognised as unsent: %#v", err)
	}
	if !Unsent(&Error{Class: ErrorConnect, Retryable: true, Cause: err}) {
		t.Fatal("Unsent did not see through the provider error wrapper")
	}
}

// SafeTransport refusing to dial is the one case where "nothing was sent" is a
// fact this process owns rather than an inference from an error the network
// produced. It arrives as a plain error, so without this it was classified as
// possibly-executed and settled at the full reservation.
func TestSafeTransportRefusalCountsAsUnsent(t *testing.T) {
	refusal := fmt.Errorf("outbound host %q: %w: %w", "api.example.com",
		safetransport.ErrRefusedBeforeSend, errors.New("address 169.254.169.254 is not allowed"))
	if !Unsent(refusal) {
		t.Fatal("a refusal made before any connection existed was treated as possibly sent")
	}
	// Adapters classify the wrapped error, so the marker has to survive the wrap.
	if !Unsent(&Error{Class: ErrorConnect, Retryable: true, Cause: refusal}) {
		t.Fatal("the marker did not survive an adapter's provider.Error wrapper")
	}
}
