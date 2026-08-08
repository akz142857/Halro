package gatewayapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
)

// The decoders say exactly what is wrong and where. Collapsing that into
// "invalid request body" left the caller to bisect their payload field by
// field, which also hollowed out the compatibility promise: unsupported fields
// are rejected rather than dropped, but the rejection did not say which field.
func TestDecodeProblemCarriesTheDecoderVerdictAndField(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		err     error
		message string
		param   string
	}{
		{
			"indexed field",
			errors.New("messages[3].role is invalid"),
			"messages[3].role is invalid", "messages[3].role",
		},
		{
			"value attached with equals",
			errors.New("tools[2].strict=true cannot be represented losslessly"),
			"tools[2].strict=true cannot be represented losslessly", "tools[2].strict",
		},
		{
			"nested field",
			errors.New("text.format json_schema requires name and schema"),
			"text.format json_schema requires name and schema", "text.format",
		},
		{
			"endpoint-level rejection",
			errors.New("store=true is unavailable on the stateless endpoint"),
			"store=true is unavailable on the stateless endpoint", "store",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			message, param := decodeProblem(testCase.err)
			if message != testCase.message {
				t.Fatalf("message = %q, want %q", message, testCase.message)
			}
			if param == nil {
				t.Fatalf("no param recovered from %q, want %q", testCase.err, testCase.param)
			}
			if *param != testCase.param {
				t.Fatalf("param = %q, want %q", *param, testCase.param)
			}
		})
	}
}

// A guess is worse than no answer: pointing an SDK at a field that does not
// exist sends the caller looking in the wrong place. The opening word of a
// sentence is the trap — "invalid character 'x' ..." must not announce a field
// called "invalid".
func TestDecodeProblemOffersNoParamWhenItCannotIdentifyOne(t *testing.T) {
	for _, message := range []string{
		"invalid character 'x' looking for beginning of value",
		"unexpected EOF",
		"JSON body is required",
		"request body is empty",
	} {
		if _, param := decodeProblem(errors.New(message)); param != nil {
			t.Fatalf("invented param %q for %q", *param, message)
		}
	}
}

// One decoder problem quotes the content block type the caller sent, so the
// caller must not be able to size the response by sending a long one.
func TestDecodeProblemBoundsWhatItEchoesBack(t *testing.T) {
	message, _ := decodeProblem(errors.New(strings.Repeat("A", 4096)))
	if len(message) > maxDecodeProblemBytes+len("…") {
		t.Fatalf("echoed %d bytes, want at most %d", len(message), maxDecodeProblemBytes)
	}
}

func TestDecodeProblemFallsBackWhenTheErrorSaysNothing(t *testing.T) {
	message, param := decodeProblem(errors.New("   "))
	if message != "invalid request body" || param != nil {
		t.Fatalf("message = %q param = %v, want the generic fallback", message, param)
	}
}

func TestIsFieldPathAcceptsOnlyPathCharacters(t *testing.T) {
	for _, candidate := range []string{"Halro", "'x'", "3", "a b", "föö"} {
		if isFieldPath(candidate) {
			t.Fatalf("%q was accepted as a field path", candidate)
		}
	}
	for _, candidate := range []string{"store", "text.format", "messages[3].role", "tools[2].strict"} {
		if !isFieldPath(candidate) {
			t.Fatalf("%q was rejected as a field path", candidate)
		}
	}
}

// An SDK calling models.list() against a gateway that does not implement it was
// getting the router's plain-text 404, which its error type cannot parse — so
// the caller saw an opaque transport failure rather than the reason, and an
// operator saw an empty model list with no explanation.
func TestUnimplementedEndpointAnswersInTheEnvelopeSDKsParse(t *testing.T) {
	handler, err := New(&fakeService{}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	response := httptest.NewRecorder()
	handler.NotFound(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	var envelope openaiapi.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not a parsable error envelope: %v (%s)", err, response.Body)
	}
	if envelope.Error.Code != "endpoint_not_implemented" {
		t.Fatalf("code = %q, want endpoint_not_implemented", envelope.Error.Code)
	}
	// The generic message names no remedy; models.list() deserves the specific one.
	if !strings.Contains(envelope.Error.Message, "public alias") {
		t.Fatalf("message does not say what to use instead: %q", envelope.Error.Message)
	}
}

func TestUnroutedPathStillExplainsItself(t *testing.T) {
	handler, err := New(&fakeService{}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.NotFound(response, httptest.NewRequest(http.MethodGet, "/v1/nonsense", nil))
	var envelope openaiapi.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not a parsable error envelope: %v (%s)", err, response.Body)
	}
	if !strings.Contains(envelope.Error.Message, "not implemented") {
		t.Fatalf("message = %q", envelope.Error.Message)
	}
}

func TestWrongMethodAnswersInTheEnvelopeToo(t *testing.T) {
	handler, err := New(&fakeService{}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.MethodNotAllowed(response, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
	var envelope openaiapi.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not a parsable error envelope: %v (%s)", err, response.Body)
	}
	if !strings.Contains(envelope.Error.Message, http.MethodGet) {
		t.Fatalf("message does not name the rejected method: %q", envelope.Error.Message)
	}
}
