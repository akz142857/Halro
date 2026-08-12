package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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

// A Mantle provider that names a project sends anthropic-workspace — the
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
		if got := seen.Header.Get("anthropic-workspace"); got != test.expected {
			t.Fatalf("project %q rendered anthropic-workspace=%q", test.projectID, got)
		}
		if seen.Header.Get("OpenAI-Project") != "" || seen.Header.Get("anthropic-workspace-id") != "" {
			t.Fatalf("project %q leaked a foreign resource header: %v", test.projectID, seen.Header)
		}
		if seen.Header.Get("x-api-key") != "bedrock-key" {
			t.Fatalf("project %q disturbed the credential header: %v", test.projectID, seen.Header)
		}
		adapter.Close()
		server.Close()
	}
}
