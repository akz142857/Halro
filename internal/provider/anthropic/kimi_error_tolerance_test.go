package anthropic

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/provider"
)

// What this test is, stated up front so it is not mistaken for something it is
// not: it verifies **Halro's tolerance**, not Kimi's error shapes.
//
// Kimi's Anthropic-shaped route was measured on 2026-09-01 answering 400 in
// Anthropic's shape (`{"type":"error","error":{...},"request_id":...}`) and both
// 401 and 429 in OpenAI's (`{"error":{...}}`) — the same endpoint changing shape
// by status code. 503 is still unknown and cannot be settled deliberately: it
// needs the upstream to actually be unavailable, which nothing here can arrange.
// Kimi's own documentation adds a third possibility on top — it says a 504
// arrives as an **HTML** gateway page after 900 seconds, which is not JSON at
// all.
//
// So rather than guess a shape and call it evidence, this pins the property that
// has to hold whatever the shape turns out to be: the classifier reads the
// status code, never fails on a body it cannot parse, and never carries provider
// response bytes into the error it returns. That last one is an invariant rather
// than a nicety — response bodies must not reach logs, errors, metrics or audit
// records.
func TestAnthropicErrorDecodingToleratesShapesItHasNotSeen(t *testing.T) {
	const htmlTimeout = "<html><head><title>504 Gateway Time-out</title></head><body>upstream timed out</body></html>"
	for _, test := range []struct {
		name          string
		status        int
		body          string
		wantClass     provider.ErrorClass
		wantRetryable bool
		wantAmbiguous bool
		wantMessage   string
		// wantCode is the narrowed provider identifier. Empty means the upstream
		// sent nothing an identifier could be read from — including sending
		// something that is not one.
		wantCode string
		// wantNoRefusal pins that a refusal verdict was not manufactured from a
		// value the error refused to carry.
		wantNoRefusal bool
		// leaked is a fragment of the body that must not appear in the error, in
		// its identifier fields, or in anything else that reaches a log.
		leaked string
	}{
		{
			name: "an HTML gateway page, which Kimi documents for 504",
			// The status Kimi names for it. 504 falls to the generic 5xx branch:
			// the origin may still be running and billing, so retrying would
			// duplicate the generation and settling it free would hide the charge.
			status: http.StatusGatewayTimeout, body: htmlTimeout,
			wantClass: provider.ErrorProvider5xx, wantAmbiguous: true,
			wantMessage: http.StatusText(http.StatusGatewayTimeout),
			leaked:      "upstream timed out",
		},
		{
			name:   "a JSON body in neither of the two shapes",
			status: http.StatusServiceUnavailable, body: `{"detail":"capacity","trace":"abc"}`,
			wantClass: provider.ErrorProvider5xx, wantRetryable: true,
			wantMessage: http.StatusText(http.StatusServiceUnavailable),
			leaked:      "abc",
		},
		{
			// An upstream answering with something that is not an identifier in
			// the field an identifier is read from. Anthropic's schema says this
			// is a short type name; nothing makes an upstream honour it, and this
			// adapter now serves three profiles whose upstreams demonstrably do
			// not follow the schema. The verdict must not be manufactured from it
			// either: a 400 whose type is prose is not the upstream naming a
			// refusal.
			name:   "a 400 whose error type is a wall of prose",
			status: http.StatusBadRequest,
			body: `{"type":"error","error":{"type":"the request you sent was not acceptable to this service for reasons we will now explain at length",` +
				`"message":"bad request"}}`,
			wantClass: provider.ErrorBadRequest, wantMessage: "bad request",
			wantCode: "", wantNoRefusal: true,
			leaked: "at length",
		},
		{
			name:   "an empty body",
			status: http.StatusTooManyRequests, body: "",
			wantClass: provider.ErrorRateLimit, wantRetryable: true,
			wantMessage: http.StatusText(http.StatusTooManyRequests),
		},
		{
			// Measured by tripping the organisation limit on that exact route on
			// 2026-09-01. It is the OpenAI envelope again, not Anthropic's — so
			// this endpoint answers 400 in one shape and 401 and 429 in the other.
			name:      "the OpenAI error envelope Kimi answers 429 with",
			status:    http.StatusTooManyRequests,
			body:      `{"error":{"message":"Organization Rate limit exceeded, please try again after 1 seconds","type":"rate_limit_reached_error"}}`,
			wantClass: provider.ErrorRateLimit, wantRetryable: true,
			wantMessage: "Organization Rate limit exceeded, please try again after 1 seconds",
			wantCode:    "rate_limit_reached_error",
		},
		{
			// Measured on Kimi's Anthropic route: the OpenAI envelope on a 401.
			// It parses here because both shapes carry a top-level `error` object
			// with a message, which is why the message survives.
			name:        "the OpenAI error envelope Kimi answers 401 with",
			status:      http.StatusUnauthorized,
			body:        `{"error":{"message":"Incorrect API key provided","type":"incorrect_api_key_error"}}`,
			wantClass:   provider.ErrorAuthentication,
			wantMessage: "Incorrect API key provided",
			wantCode:    "incorrect_api_key_error",
		},
		{
			// Measured on the same route: Anthropic's own envelope on a 400.
			name:        "the Anthropic error envelope Kimi answers 400 with",
			status:      http.StatusBadRequest,
			body:        `{"type":"error","error":{"type":"invalid_request_error","message":"tool_choice 'specified' is incompatible with thinking enabled"},"request_id":"3f84e9cd"}`,
			wantClass:   provider.ErrorBadRequest,
			wantMessage: "tool_choice 'specified' is incompatible with thinking enabled",
			wantCode:    "invalid_request_error",
		},
	} {
		response := &http.Response{
			StatusCode: test.status,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(test.body)),
		}
		err := decodeHTTPError(response)
		providerErr, ok := err.(*provider.Error)
		if !ok {
			t.Fatalf("%s: decodeHTTPError returned %T, want *provider.Error", test.name, err)
		}
		if providerErr.Class != test.wantClass {
			t.Errorf("%s: class %v, want %v", test.name, providerErr.Class, test.wantClass)
		}
		if providerErr.Retryable != test.wantRetryable {
			t.Errorf("%s: retryable %v, want %v", test.name, providerErr.Retryable, test.wantRetryable)
		}
		if providerErr.Ambiguous != test.wantAmbiguous {
			t.Errorf("%s: ambiguous %v, want %v", test.name, providerErr.Ambiguous, test.wantAmbiguous)
		}
		if providerErr.Message != test.wantMessage {
			t.Errorf("%s: message %q, want %q", test.name, providerErr.Message, test.wantMessage)
		}
		if providerErr.StatusCode != test.status {
			t.Errorf("%s: status %d, want %d", test.name, providerErr.StatusCode, test.status)
		}
		if providerErr.ProviderCode != test.wantCode {
			t.Errorf("%s: provider_code %q, want %q", test.name, providerErr.ProviderCode, test.wantCode)
		}
		if test.wantNoRefusal && providerErr.Refusal != "" {
			t.Errorf("%s: a refusal verdict was taken from a value that is not an identifier: %q", test.name, providerErr.Refusal)
		}
		if test.leaked != "" && strings.Contains(err.Error(), test.leaked) {
			t.Errorf("%s: the error carries provider response bytes: %q", test.name, err.Error())
		}
		// err.Error() returns Message and nothing else, so the check above cannot
		// see the two fields that actually reach a log line. Asserting them by
		// name is the difference between this test proving the invariant and
		// merely appearing to: both cases carrying a `leaked` fragment decode to
		// an empty ProviderCode, so the assertion above would pass unchanged if
		// the whole HTML page were carried in it.
		if test.leaked != "" {
			if strings.Contains(providerErr.ProviderCode, test.leaked) {
				t.Errorf("%s: provider_code carries provider response bytes: %q", test.name, providerErr.ProviderCode)
			}
			if strings.Contains(providerErr.ProviderRequestID, test.leaked) {
				t.Errorf("%s: provider_request_id carries provider response bytes: %q", test.name, providerErr.ProviderRequestID)
			}
		}
	}
}
