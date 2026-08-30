package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// Probe is the same POST with max_tokens=1, so it is a billable inference call
// rather than a free metadata read. Documented here because the OpenAI-shaped
// Mantle profiles probe /v1/models instead, and an operator reading only the
// console cannot tell the two apart.
func TestBedrockMantleMessagesProbeIsABillableInferenceCall(t *testing.T) {
	var seen *http.Request
	var body struct {
		MaxTokens int `json:"max_tokens"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = request.Clone(request.Context())
		_ = json.NewDecoder(request.Body).Decode(&body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"anthropic.claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(server.Close)

	endpoint, _ := url.Parse(server.URL)
	authorizer, _ := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "x-api-key", "", []byte("bedrock-key"), "Authorization")
	adapter, err := New(Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: server.Client(),
		Capabilities:     provider.Capabilities{Chat: true},
		ProviderType:     string(domain.ProviderBedrock),
		CredentialScheme: domain.CredentialBedrockAPIKey,
		MessagesPath:     "anthropic/v1/messages",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)

	if err := adapter.Probe(context.Background(), "anthropic.claude-test"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if seen == nil || seen.Method != http.MethodPost || seen.URL.Path != "/anthropic/v1/messages" {
		t.Fatalf("probe did not use the Messages endpoint: %+v", seen)
	}
	if body.MaxTokens != 1 {
		t.Fatalf("probe did not bound its output to one token: %d", body.MaxTokens)
	}
}

func assertNoBedrockResourceHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, name := range []string{"anthropic-workspace", "anthropic-workspace-id", "OpenAI-Project", "OpenAI-Organization"} {
		if value := header.Get(name); value != "" {
			t.Fatalf("%s was sent while Halro only supports the account default project: %q", name, value)
		}
	}
}

// A Mantle provider that names a project sends anthropic-workspace-id — the
// Anthropic protocol's spelling of the same Bedrock Project resource the
// OpenAI-shaped profiles select with OpenAI-Project.
func TestBedrockMantleMessagesRendersTheProjectAsAWorkspaceHeader(t *testing.T) {
	for _, test := range []struct{ projectID, expected string }{
		{"proj_abc123", "proj_abc123"},
		{"", ""},
	} {
		var seen *http.Request
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			seen = request.Clone(request.Context())
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"anthropic.claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		endpoint, _ := url.Parse(server.URL)
		authorizer, _ := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "x-api-key", "", []byte("bedrock-key"), "Authorization")
		adapter, err := New(Options{
			Endpoint: endpoint, Authorizer: authorizer, Client: server.Client(),
			Capabilities:     provider.Capabilities{Chat: true},
			ProviderType:     string(domain.ProviderBedrock),
			CredentialScheme: domain.CredentialBedrockAPIKey,
			MessagesPath:     "anthropic/v1/messages",
			BedrockProjectID: test.projectID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := adapter.Probe(context.Background(), "anthropic.claude-test"); err != nil {
			t.Fatalf("project %q: %v", test.projectID, err)
		}
		if got := seen.Header.Get("anthropic-workspace-id"); got != test.expected {
			t.Fatalf("project %q rendered anthropic-workspace-id=%q", test.projectID, got)
		}
		if seen.Header.Get("OpenAI-Project") != "" || seen.Header.Get("anthropic-workspace") != "" {
			t.Fatalf("project %q leaked a foreign resource header: %v", test.projectID, seen.Header)
		}
		if seen.Header.Get("x-api-key") != "bedrock-key" {
			t.Fatalf("project %q disturbed the credential header: %v", test.projectID, seen.Header)
		}
		adapter.Close()
		server.Close()
	}
}

// The Messages profile's own status matrix. It is not covered by the OpenAI
// adapter's: this is a different classifier on a different envelope, and 401 or
// 403 here decides the same thing — whether an expired key or an unusable
// project quietly moves traffic to a standby deployment.
func TestBedrockMantleMessagesClassifiesTheStatusMatrix(t *testing.T) {
	for _, test := range []struct {
		status    int
		class     provider.ErrorClass
		retryable bool
		ambiguous bool
	}{
		{401, provider.ErrorAuthentication, false, false},
		{403, provider.ErrorAuthentication, false, false},
		{400, provider.ErrorBadRequest, false, false},
		{429, provider.ErrorRateLimit, true, false},
		// 529 is a stated overload refusal and retries; a 500 or an edge 502
		// leaves the generation's fate unknown, so it does neither.
		{529, provider.ErrorProvider5xx, true, false},
		{503, provider.ErrorProvider5xx, true, false},
		{500, provider.ErrorProvider5xx, false, true},
		{502, provider.ErrorProvider5xx, false, true},
		{504, provider.ErrorProvider5xx, false, true},
	} {
		adapter := newMantleMessagesAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("x-amzn-requestid", "aws-request-matrix")
			writer.WriteHeader(test.status)
			_, _ = writer.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"denied"}}`))
		}, "")
		_, err := adapter.MessagesNative(context.Background(), mantleMessagesCall())
		var classified *provider.Error
		if !errors.As(err, &classified) {
			t.Fatalf("status %d was not classified: %v", test.status, err)
		}
		if classified.Class != test.class || classified.Retryable != test.retryable ||
			classified.Ambiguous != test.ambiguous {
			t.Fatalf("status %d classified as %s retryable=%v ambiguous=%v",
				test.status, classified.Class, classified.Retryable, classified.Ambiguous)
		}
		if classified.ProviderRequestID != "aws-request-matrix" {
			t.Fatalf("status %d lost the AWS request id: %q", test.status, classified.ProviderRequestID)
		}
	}
}

// The same failure on the streaming path. A stream that never emitted must not
// be reported as ambiguous, or the gateway would settle it conservatively as if
// the provider might have done the work.
func TestBedrockMantleMessagesStreamAuthenticationFailureIsTerminal(t *testing.T) {
	for _, status := range []int{401, 403} {
		adapter := newMantleMessagesAdapter(t, func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(status)
			_, _ = writer.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"denied"}}`))
		}, "")
		emitted := 0
		_, err := adapter.MessagesNativeStream(context.Background(), mantleMessagesCall(), func(anthropicapi.RawStreamEvent) error {
			emitted++
			return nil
		})
		var classified *provider.Error
		if !errors.As(err, &classified) {
			t.Fatalf("status %d was not classified: %v", status, err)
		}
		if classified.Class != provider.ErrorAuthentication || classified.Retryable || classified.Ambiguous {
			t.Fatalf("status %d stream error: %#v", status, classified)
		}
		if emitted != 0 {
			t.Fatalf("status %d emitted %d events before failing", status, emitted)
		}
	}
}

// A project that does not exist, is archived, or the key has no rights to,
// answers 403. It must reach the same non-retryable authentication class as a
// bad key: the operator has to fix the provider record either way, and a
// fallback would hide that.
func TestBedrockMantleMessagesTreatsAnUnusableProjectAsAuthenticationFailure(t *testing.T) {
	adapter := newMantleMessagesAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("anthropic-workspace-id") != "proj_archived" {
			t.Errorf("the project was not addressed: %v", request.Header)
		}
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"workspace is archived"}}`))
	}, "proj_archived")
	_, err := adapter.MessagesNative(context.Background(), mantleMessagesCall())
	var classified *provider.Error
	if !errors.As(err, &classified) {
		t.Fatalf("unclassified project failure: %v", err)
	}
	if classified.Class != provider.ErrorAuthentication || classified.Retryable {
		t.Fatalf("project failure classified as %s retryable=%v", classified.Class, classified.Retryable)
	}
}

func newMantleMessagesAdapter(t *testing.T, handler http.HandlerFunc, projectID string) *Adapter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, _ := url.Parse(server.URL)
	authorizer, err := provider.NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "x-api-key", "", []byte("bedrock-key"), "Authorization")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: server.Client(),
		Capabilities:     provider.Capabilities{Chat: true, Streaming: true},
		ProviderType:     string(domain.ProviderBedrock),
		CredentialScheme: domain.CredentialBedrockAPIKey,
		MessagesPath:     "anthropic/v1/messages",
		BedrockProjectID: projectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

func mantleMessagesCall() provider.NativeMessageCall {
	payload := []byte(`{"model":"public","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	return provider.NativeMessageCall{RequestID: "req_1", ProviderModel: "anthropic.claude-test", Version: anthropicapi.SupportedVersion, Payload: payload}
}
