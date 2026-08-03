package kms

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"time"
)

type RetryPolicy struct {
	CallTimeout     time.Duration
	StartupDeadline time.Duration
	InitialBackoff  time.Duration
	MaxBackoff      time.Duration
	MaxAttempts     int
}

func (p RetryPolicy) Validate() error {
	if p.CallTimeout <= 0 || p.StartupDeadline <= p.CallTimeout || p.InitialBackoff <= 0 ||
		p.MaxBackoff < p.InitialBackoff || p.MaxBackoff >= p.StartupDeadline || p.MaxAttempts < 1 || p.MaxAttempts > 64 {
		return errors.New("invalid KMS retry policy")
	}
	return nil
}

type Executor struct {
	wrapper Wrapper
	policy  RetryPolicy
	jitter  func(time.Duration) time.Duration
	sleep   func(context.Context, time.Duration) error
}

func NewExecutor(wrapper Wrapper, policy RetryPolicy) (*Executor, error) {
	return newExecutor(wrapper, policy, fullJitter, sleepContext)
}

func newExecutor(
	wrapper Wrapper,
	policy RetryPolicy,
	jitter func(time.Duration) time.Duration,
	sleep func(context.Context, time.Duration) error,
) (*Executor, error) {
	if wrapper == nil || ValidateProvider(wrapper.Provider()) != nil {
		return nil, errors.New("valid KMS wrapper is required")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if jitter == nil || sleep == nil {
		return nil, errors.New("KMS retry timing functions are required")
	}
	return &Executor{wrapper: wrapper, policy: policy, jitter: jitter, sleep: sleep}, nil
}

func (e *Executor) Provider() string { return e.wrapper.Provider() }

func (e *Executor) Wrap(ctx context.Context, request WrapRequest) (WrapResult, error) {
	var result WrapResult
	err := e.execute(ctx, OperationWrap, func(callCtx context.Context) error {
		candidate, err := e.wrapper.Wrap(callCtx, request.Clone())
		if err != nil {
			return err
		}
		if err := candidate.Validate(); err != nil {
			return NewError(ErrorTransient, e.Provider(), OperationWrap, 0, err)
		}
		result = candidate.Clone()
		return nil
	})
	return result, err
}

func (e *Executor) Unwrap(ctx context.Context, request UnwrapRequest) (UnwrapResult, error) {
	var result UnwrapResult
	err := e.execute(ctx, OperationUnwrap, func(callCtx context.Context) error {
		candidate, err := e.wrapper.Unwrap(callCtx, request.Clone())
		if err != nil {
			return err
		}
		if err := candidate.Validate(); err != nil {
			clear(candidate.Plaintext)
			return NewError(ErrorPayloadInvalid, e.Provider(), OperationUnwrap, 0, err)
		}
		result = candidate.Clone()
		clear(candidate.Plaintext)
		return nil
	})
	return result, err
}

func (e *Executor) execute(ctx context.Context, operation Operation, call func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return NewError(ErrorTransient, e.Provider(), operation, 0, err)
	}
	startupCtx, cancelStartup := context.WithTimeout(ctx, e.policy.StartupDeadline)
	defer cancelStartup()
	backoff := e.policy.InitialBackoff
	for attempt := 1; attempt <= e.policy.MaxAttempts; attempt++ {
		callCtx, cancelCall := context.WithTimeout(startupCtx, e.policy.CallTimeout)
		err := call(callCtx)
		cancelCall()
		if err == nil {
			return nil
		}
		if !Retryable(Classify(err)) || attempt == e.policy.MaxAttempts {
			return err
		}
		if startupErr := startupCtx.Err(); startupErr != nil {
			return NewError(ErrorTransient, e.Provider(), operation, 0, startupErr)
		}
		delay := e.jitter(backoff)
		var classified *Error
		if errors.As(err, &classified) && classified.RetryAfter > 0 {
			retryAfter := classified.RetryAfter
			if retryAfter > e.policy.StartupDeadline {
				retryAfter = e.policy.StartupDeadline
			}
			if delay > e.policy.StartupDeadline-retryAfter {
				delay = e.policy.StartupDeadline
			} else {
				delay += retryAfter
			}
		}
		if err := e.sleep(startupCtx, delay); err != nil {
			return NewError(ErrorTransient, e.Provider(), operation, 0, err)
		}
		if backoff < e.policy.MaxBackoff {
			if backoff > e.policy.MaxBackoff/2 {
				backoff = e.policy.MaxBackoff
			} else {
				backoff *= 2
			}
			if backoff > e.policy.MaxBackoff {
				backoff = e.policy.MaxBackoff
			}
		}
	}
	return NewError(ErrorTransient, e.Provider(), operation, 0, errors.New("KMS retry attempts exhausted"))
}

func fullJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(max)+1))
	if err != nil {
		return max
	}
	return time.Duration(value.Int64())
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
