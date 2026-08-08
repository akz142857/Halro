package gatewayapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/openaiapi"
)

// countingLimiter records what the middleware charged so a test can assert on
// the address the limiter was asked about, not only on the response.
type countingLimiter struct {
	allow      bool
	retryAfter time.Duration
	charged    []netip.Addr
}

func (c *countingLimiter) Allow(source netip.Addr, _ time.Time) (bool, time.Duration) {
	c.charged = append(c.charged, source)
	if c.allow {
		return true, 0
	}
	return false, c.retryAfter
}

func limitedHandler(t *testing.T, limiter SourceLimiter, options ...func(*Options)) *Handler {
	t.Helper()
	settings := Options{MaxRequestBytes: 4 << 20, SourceLimiter: limiter}
	for _, apply := range options {
		apply(&settings)
	}
	handler, err := NewWithOptions(&fakeService{}, settings)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestLimitOpenAIShedsInAParsableEnvelope(t *testing.T) {
	limiter := &countingLimiter{retryAfter: 42 * time.Second}
	handler := limitedHandler(t, limiter)
	body := newCountingBody(1 << 20)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	request.RemoteAddr = "203.0.113.5:44321"
	response := httptest.NewRecorder()
	reached := false

	handler.LimitOpenAI(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	})).ServeHTTP(response, request)

	if reached {
		t.Fatal("a shed request still reached the endpoint")
	}
	if body.reads != 0 {
		t.Fatalf("the body of a shed request was read %d time(s) — shedding must be cheaper than serving", body.reads)
	}
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
	if got := response.Header().Get("Retry-After"); got != "42" {
		t.Fatalf("Retry-After = %q, want 42", got)
	}
	var envelope openaiapi.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("refusal is not a parsable envelope: %v (%s)", err, response.Body)
	}
	if envelope.Error.Code != "rate_limit_exceeded" || envelope.Error.Type != "rate_limit_error" {
		t.Fatalf("envelope = %+v, want a rate_limit_exceeded/rate_limit_error refusal", envelope.Error)
	}
	if len(limiter.charged) != 1 || limiter.charged[0].String() != "203.0.113.5" {
		t.Fatalf("charged = %v, want exactly the peer address", limiter.charged)
	}
}

func TestLimitAnthropicShedsInAnthropicsEnvelope(t *testing.T) {
	handler := limitedHandler(t, &countingLimiter{retryAfter: time.Second})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", newCountingBody(64))
	request.RemoteAddr = "203.0.113.6:44321"
	request.Header.Set("X-Request-Id", "req_abc")
	response := httptest.NewRecorder()

	handler.LimitAnthropic(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a shed request still reached the endpoint")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
	var envelope anthropicapi.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("refusal is not a parsable Anthropic envelope: %v (%s)", err, response.Body)
	}
	if envelope.Error.Type != "rate_limit_error" {
		t.Fatalf("error type = %q, want rate_limit_error", envelope.Error.Type)
	}
	if envelope.RequestID != "req_abc" {
		t.Fatalf("request id = %q, want the caller's id echoed back", envelope.RequestID)
	}
}

// The limiter has to run ahead of the guard. If the guard ran first, the work
// of authenticating an anonymous flood would itself be the unbounded cost.
func TestLimiterShedsBeforeTheGuardAuthenticates(t *testing.T) {
	authenticated := false
	handler := limitedHandler(t, &countingLimiter{}, func(options *Options) {
		options.AuthorizeKey = func(string) error {
			authenticated = true
			return errors.New("no such key")
		}
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", newCountingBody(64))
	request.RemoteAddr = "203.0.113.7:44321"
	request.Header.Set("Authorization", "Bearer gw_forged_key_that_is_long_enough")
	response := httptest.NewRecorder()

	handler.LimitOpenAI(handler.GuardOpenAI(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a shed request reached the endpoint")
	}))).ServeHTTP(response, request)

	if authenticated {
		t.Fatal("the key was authenticated for a request the limiter had already shed")
	}
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 rather than the guard's 401", response.Code)
	}
}

// A source that cannot be resolved must still be charged. Letting it through
// would make a malformed X-Forwarded-For the cheapest way around the bound.
func TestUnresolvableSourceIsChargedToTheZeroAddress(t *testing.T) {
	limiter := &countingLimiter{allow: true}
	handler := limitedHandler(t, limiter, func(options *Options) {
		options.TrustProxyHeaders = true
		options.TrustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", newCountingBody(64))
	request.RemoteAddr = "203.0.113.8:44321"
	request.Header.Set("X-Forwarded-For", "not-an-address")
	response := httptest.NewRecorder()
	reached := false

	handler.LimitOpenAI(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		reached = true
		writer.WriteHeader(http.StatusOK)
	})).ServeHTTP(response, request)

	if len(limiter.charged) != 1 {
		t.Fatalf("the limiter was consulted %d time(s), want exactly once", len(limiter.charged))
	}
	if limiter.charged[0].IsValid() {
		t.Fatalf("charged = %v, want the zero address for an unresolvable source", limiter.charged[0])
	}
	// The endpoint behind still owns the 400 invalid_forwarded_for answer; the
	// limiter's job was only to make reaching it cost something.
	if !reached {
		t.Fatal("an admitted request did not reach the endpoint")
	}
}

func TestNoLimiterConfiguredLeavesTheChainUnchanged(t *testing.T) {
	handler := limitedHandler(t, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", newCountingBody(64))
	request.RemoteAddr = "203.0.113.9:44321"
	response := httptest.NewRecorder()
	reached := false

	handler.LimitOpenAI(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	})).ServeHTTP(response, request)

	if !reached {
		t.Fatal("a request was blocked although no limiter was configured")
	}
}

// Behind a trusted proxy the budget must follow the client, not the proxy —
// otherwise one proxy exhausts a single bucket for everyone behind it.
func TestLimiterChargesTheClientAddressBehindATrustedProxy(t *testing.T) {
	limiter := &countingLimiter{allow: true}
	handler := limitedHandler(t, limiter, func(options *Options) {
		options.TrustProxyHeaders = true
		options.TrustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", newCountingBody(64))
	request.RemoteAddr = "203.0.113.10:44321"
	request.Header.Set("X-Forwarded-For", "198.51.100.23")
	response := httptest.NewRecorder()

	handler.LimitOpenAI(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})).ServeHTTP(response, request)

	if len(limiter.charged) != 1 || limiter.charged[0].String() != "198.51.100.23" {
		t.Fatalf("charged = %v, want the client address the proxy forwarded", limiter.charged)
	}
}
