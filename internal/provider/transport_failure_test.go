package provider

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/akz142857/Halro/internal/safetransport"
)

func TestClassifyTransportFailureUsesClosedRetrySet(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
		ambiguous bool
	}{
		{name: "policy refusal", err: safetransport.ErrRefusedBeforeSend},
		{name: "temporary dns", err: &net.DNSError{Err: "temporary", Name: "provider.example", IsTemporary: true}, retryable: true},
		{name: "dial", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, retryable: true},
		{name: "caller cancel", err: context.Canceled, ambiguous: true},
		{name: "post-connect deadline", err: context.DeadlineExceeded, ambiguous: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := ClassifyTransportFailure(test.err)
			if failure.Retryable != test.retryable || failure.Ambiguous != test.ambiguous {
				t.Fatalf("classification=%+v", failure)
			}
		})
	}
}

func TestClassifyTransportFailureHandlesProxyStatuses(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504} {
		err := errors.Join(&safetransport.ProxyError{
			ProxyID: "proxy-a", Stage: safetransport.ProxyStageStatus, StatusCode: status,
		}, safetransport.ErrRefusedBeforeSend)
		failure := ClassifyTransportFailure(err)
		if !failure.Retryable || failure.Ambiguous {
			t.Errorf("status %d classified %+v", status, failure)
		}
	}
	for _, status := range []int{400, 403, 407, 501, 599} {
		err := errors.Join(&safetransport.ProxyError{
			ProxyID: "proxy-a", Stage: safetransport.ProxyStageStatus, StatusCode: status,
		}, safetransport.ErrRefusedBeforeSend)
		failure := ClassifyTransportFailure(err)
		if failure.Retryable || failure.Ambiguous {
			t.Errorf("status %d classified %+v", status, failure)
		}
	}
}
