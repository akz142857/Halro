package fakekms

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/kms"
)

var _ kms.Wrapper = (*Wrapper)(nil)

func TestFakeKMSContractWrapUnwrapAndBinding(t *testing.T) {
	wrapper := newTestWrapper(t)
	payload := bytes.Repeat([]byte{7}, kms.ProtectedPayloadSize)
	binding := kms.BindingContext{"instance": "instance-1", "slot": "slot-primary", "purpose": "primary"}
	wrapped, err := wrapper.Wrap(context.Background(), kms.WrapRequest{
		KeyReference: "fake-key", Algorithm: "fake-aes-gcm", Plaintext: payload, BindingContext: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wrapped.Ciphertext, payload) {
		t.Fatal("fake ciphertext contains plaintext payload")
	}
	unwrapped, err := wrapper.Unwrap(context.Background(), kms.UnwrapRequest{
		KeyReference: "fake-key", Algorithm: "fake-aes-gcm", Ciphertext: wrapped.Ciphertext, BindingContext: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(unwrapped.Plaintext)
	if !bytes.Equal(unwrapped.Plaintext, payload) {
		t.Fatal("fake KMS unwrap changed protected payload")
	}

	tampered := cloneBinding(binding)
	tampered["slot"] = "slot-recovery"
	if _, err := wrapper.Unwrap(context.Background(), kms.UnwrapRequest{
		KeyReference: "fake-key", Algorithm: "fake-aes-gcm", Ciphertext: wrapped.Ciphertext, BindingContext: tampered,
	}); kms.Classify(err) != kms.ErrorCiphertextInvalid {
		t.Fatalf("binding tamper class=%q err=%v", kms.Classify(err), err)
	}
	if len(wrapper.Calls()) != 3 {
		t.Fatalf("calls=%#v", wrapper.Calls())
	}
}

func TestFakeKMSFaultInjection(t *testing.T) {
	permission := kms.NewError(kms.ErrorPermissionDenied, Provider, kms.OperationWrap, 0, errors.New("native secret"))
	wrapper, err := New(bytes.Repeat([]byte{3}, 32), Fault{Operation: kms.OperationWrap, Call: 1, Err: permission})
	if err != nil {
		t.Fatal(err)
	}
	request := kms.WrapRequest{KeyReference: "fake-key", Algorithm: "fake-aes-gcm", Plaintext: bytes.Repeat([]byte{1}, kms.ProtectedPayloadSize)}
	if _, err := wrapper.Wrap(context.Background(), request); kms.Classify(err) != kms.ErrorPermissionDenied {
		t.Fatalf("first fault class=%q err=%v", kms.Classify(err), err)
	}
	if _, err := wrapper.Wrap(context.Background(), request); err != nil {
		t.Fatalf("second call should recover after scripted fault: %v", err)
	}
}

func TestFakeKMSTimeoutAndCancellation(t *testing.T) {
	wrapper, err := New(bytes.Repeat([]byte{4}, 32),
		Fault{Operation: kms.OperationWrap, Call: 1, Delay: time.Second},
		Fault{Operation: kms.OperationWrap, Call: 2, Delay: time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := kms.WrapRequest{KeyReference: "fake-key", Algorithm: "fake-aes-gcm", Plaintext: bytes.Repeat([]byte{1}, kms.ProtectedPayloadSize)}
	timeout, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := wrapper.Wrap(timeout, request); !errors.Is(err, context.DeadlineExceeded) || kms.Classify(err) != kms.ErrorTransient {
		t.Fatalf("timeout err=%v class=%q", err, kms.Classify(err))
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := wrapper.Wrap(cancelled, request); !errors.Is(err, context.Canceled) || kms.Classify(err) != kms.ErrorTransient {
		t.Fatalf("cancellation err=%v class=%q", err, kms.Classify(err))
	}
}

func TestFakeKMSRejectsInvalidContractInputsWithoutCrypto(t *testing.T) {
	wrapper := newTestWrapper(t)
	if _, err := wrapper.Wrap(context.Background(), kms.WrapRequest{KeyReference: "fake-key", Algorithm: "fake-aes-gcm", Plaintext: []byte("short")}); kms.Classify(err) != kms.ErrorConfigInvalid {
		t.Fatalf("invalid wrap class=%q err=%v", kms.Classify(err), err)
	}
	if _, err := wrapper.Unwrap(context.Background(), kms.UnwrapRequest{KeyReference: "fake-key", Algorithm: "fake-aes-gcm"}); kms.Classify(err) != kms.ErrorConfigInvalid {
		t.Fatalf("invalid unwrap class=%q err=%v", kms.Classify(err), err)
	}
}

func newTestWrapper(t *testing.T) *Wrapper {
	t.Helper()
	wrapper, err := New(bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return wrapper
}

func cloneBinding(binding kms.BindingContext) kms.BindingContext {
	cloned := make(kms.BindingContext, len(binding))
	for key, value := range binding {
		cloned[key] = value
	}
	return cloned
}
