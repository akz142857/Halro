package kms

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type retryTestWrapper struct {
	mu        sync.Mutex
	wrapCalls int
	errors    []error
}

func (w *retryTestWrapper) Provider() string { return "retry-test" }

func (w *retryTestWrapper) Wrap(_ context.Context, request WrapRequest) (WrapResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.wrapCalls++
	request.Plaintext[0] ^= 0xff
	if w.wrapCalls <= len(w.errors) && w.errors[w.wrapCalls-1] != nil {
		return WrapResult{}, w.errors[w.wrapCalls-1]
	}
	return WrapResult{Ciphertext: []byte("ciphertext")}, nil
}

func (w *retryTestWrapper) Unwrap(context.Context, UnwrapRequest) (UnwrapResult, error) {
	return UnwrapResult{Plaintext: bytes.Repeat([]byte{1}, ProtectedPayloadSize)}, nil
}

func TestExecutorRetriesOnlyRetryableErrorsWithFullJitterBoundary(t *testing.T) {
	transient := NewError(ErrorTransient, "retry-test", OperationWrap, 0, errors.New("temporary"))
	throttled := NewError(ErrorThrottled, "retry-test", OperationWrap, 250*time.Millisecond, errors.New("throttled"))
	wrapper := &retryTestWrapper{errors: []error{transient, throttled}}
	var sleeps []time.Duration
	executor, err := newExecutor(wrapper, testRetryPolicy(), func(max time.Duration) time.Duration { return max / 2 }, func(_ context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{7}, ProtectedPayloadSize)
	result, err := executor.Wrap(context.Background(), WrapRequest{KeyReference: "key", Algorithm: "algorithm", Plaintext: payload})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Ciphertext) != "ciphertext" || wrapper.wrapCalls != 3 {
		t.Fatalf("result=%#v calls=%d", result, wrapper.wrapCalls)
	}
	if !bytes.Equal(payload, bytes.Repeat([]byte{7}, ProtectedPayloadSize)) {
		t.Fatal("retry wrapper mutated caller payload")
	}
	if len(sleeps) != 2 || sleeps[0] != 50*time.Millisecond || sleeps[1] != 350*time.Millisecond {
		t.Fatalf("full-jitter/retry-after sleeps=%v", sleeps)
	}
}

func TestExecutorFailsFastForPermanentErrors(t *testing.T) {
	permission := NewError(ErrorPermissionDenied, "retry-test", OperationWrap, 0, errors.New("denied"))
	wrapper := &retryTestWrapper{errors: []error{permission}}
	executor, err := newExecutor(wrapper, testRetryPolicy(), func(time.Duration) time.Duration { return 0 }, func(context.Context, time.Duration) error {
		t.Fatal("permanent error attempted to sleep")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Wrap(context.Background(), WrapRequest{KeyReference: "key", Algorithm: "algorithm", Plaintext: bytes.Repeat([]byte{1}, ProtectedPayloadSize)})
	if Classify(err) != ErrorPermissionDenied || wrapper.wrapCalls != 1 {
		t.Fatalf("err=%v class=%q calls=%d", err, Classify(err), wrapper.wrapCalls)
	}
}

func TestExecutorEnforcesCallTimeoutAndTotalDeadline(t *testing.T) {
	wrapper := &blockingRetryWrapper{}
	policy := testRetryPolicy()
	policy.CallTimeout = 10 * time.Millisecond
	policy.StartupDeadline = 35 * time.Millisecond
	policy.InitialBackoff = 5 * time.Millisecond
	policy.MaxBackoff = 15 * time.Millisecond
	policy.MaxAttempts = 20
	executor, err := NewExecutor(wrapper, policy)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = executor.Wrap(context.Background(), WrapRequest{KeyReference: "key", Algorithm: "algorithm", Plaintext: bytes.Repeat([]byte{1}, ProtectedPayloadSize)})
	if Classify(err) != ErrorTransient || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("deadline err=%v class=%q elapsed=%s", err, Classify(err), time.Since(started))
	}
	if wrapper.calls < 1 || wrapper.calls > 4 {
		t.Fatalf("bounded calls=%d", wrapper.calls)
	}
}

type blockingRetryWrapper struct {
	calls int
}

func (w *blockingRetryWrapper) Provider() string { return "retry-test" }
func (w *blockingRetryWrapper) Wrap(ctx context.Context, _ WrapRequest) (WrapResult, error) {
	w.calls++
	<-ctx.Done()
	return WrapResult{}, NewError(ErrorTransient, w.Provider(), OperationWrap, 0, ctx.Err())
}
func (w *blockingRetryWrapper) Unwrap(context.Context, UnwrapRequest) (UnwrapResult, error) {
	return UnwrapResult{}, errors.New("not implemented")
}

func testRetryPolicy() RetryPolicy {
	return RetryPolicy{
		CallTimeout: time.Second, StartupDeadline: 5 * time.Second,
		InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second, MaxAttempts: 5,
	}
}
