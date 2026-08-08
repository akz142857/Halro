package gatewayapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/openaiapi"
)

// countingBody reports whether anything pulled on the request body, which is
// the cost the guard exists to avoid paying for an anonymous caller.
type countingBody struct {
	reads   int
	payload *strings.Reader
}

func newCountingBody(size int) *countingBody {
	return &countingBody{payload: strings.NewReader(strings.Repeat("x", size))}
}

func (c *countingBody) Read(destination []byte) (int, error) {
	c.reads++
	return c.payload.Read(destination)
}

func (c *countingBody) Close() error { return nil }

func rejectingHandler(t *testing.T) (*Handler, *bool) {
	t.Helper()
	reached := false
	handler, err := NewWithOptions(&fakeService{}, Options{
		MaxRequestBytes: 4 << 20,
		AuthorizeKey:    func(string) error { return errors.New("no such key") },
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, &reached
}

// The endpoint reads up to MaxRequestBytes and decodes it before the service
// authenticates, so an anonymous caller decided how much work the gateway did —
// and the limiter that would have bounded it is keyed by project, which is to
// say it only exists once authentication has already succeeded.
func TestGuardRejectsAnUnknownKeyWithoutReadingTheBody(t *testing.T) {
	handler, reached := rejectingHandler(t)
	body := newCountingBody(1 << 20)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	request.Header.Set("Authorization", "Bearer gw_forged_key_that_is_long_enough")
	response := httptest.NewRecorder()

	handler.GuardOpenAI(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		*reached = true
	})).ServeHTTP(response, request)

	if *reached {
		t.Fatal("a forged key reached the endpoint")
	}
	if body.reads != 0 {
		t.Fatalf("the body of a rejected request was read %d time(s)", body.reads)
	}
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	var envelope openaiapi.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("refusal is not a parsable envelope: %v (%s)", err, response.Body)
	}
	if envelope.Error.Code != "invalid_api_key" {
		t.Fatalf("code = %q, want invalid_api_key", envelope.Error.Code)
	}
	if challenge := response.Header().Get("WWW-Authenticate"); challenge == "" {
		t.Fatal("401 did not carry a WWW-Authenticate challenge")
	}
}

// The guard stands in front of the authoritative check, so it must never be the
// reason a legitimate request fails.
func TestGuardPassesAKeyItCanAuthenticate(t *testing.T) {
	handler, err := NewWithOptions(&fakeService{}, Options{
		MaxRequestBytes: 4 << 20,
		AuthorizeKey:    func(string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer gw_valid_key_that_is_long_enough")
	handler.GuardOpenAI(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	})).ServeHTTP(httptest.NewRecorder(), request)
	if !reached {
		t.Fatal("a key the guard could authenticate was turned away")
	}
}

// A request with no key at all falls through, so the endpoint keeps ownership of
// that response rather than having two places answer the same question.
func TestGuardLeavesAMissingKeyToTheEndpoint(t *testing.T) {
	handler, reached := rejectingHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	handler.GuardOpenAI(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		*reached = true
	})).ServeHTTP(httptest.NewRecorder(), request)
	if !*reached {
		t.Fatal("a request with no key was answered by the guard instead of the endpoint")
	}
}

// Anthropic callers authenticate with x-api-key, and their SDK parses a
// different envelope than the OpenAI one.
func TestAnthropicGuardRejectsInItsOwnEnvelope(t *testing.T) {
	handler, reached := rejectingHandler(t)
	body := newCountingBody(1 << 20)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	request.Header.Set("x-api-key", "gw_forged_key_that_is_long_enough")
	response := httptest.NewRecorder()

	handler.GuardAnthropic(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		*reached = true
	})).ServeHTTP(response, request)

	if *reached {
		t.Fatal("a forged x-api-key reached the endpoint")
	}
	if body.reads != 0 {
		t.Fatalf("the body of a rejected request was read %d time(s)", body.reads)
	}
	var envelope anthropicapi.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("refusal is not a parsable Anthropic envelope: %v (%s)", err, response.Body)
	}
	if envelope.Type != "error" || envelope.Error.Type != "authentication_error" {
		t.Fatalf("envelope = %#v", envelope)
	}
}

// Without a configured checker the guard has nothing to assert and must stay
// out of the way entirely.
func TestGuardIsInertWithoutAnAuthorizer(t *testing.T) {
	handler, err := NewWithOptions(&fakeService{}, Options{MaxRequestBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer gw_anything")
	handler.GuardOpenAI(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	})).ServeHTTP(httptest.NewRecorder(), request)
	if !reached {
		t.Fatal("the guard blocked a request with no authorizer configured")
	}
}

var _ io.ReadCloser = (*countingBody)(nil)
