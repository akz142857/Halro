package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

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
