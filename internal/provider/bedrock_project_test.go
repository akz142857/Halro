package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

func TestApplyBedrockProjectSetsOnlyTheProtocolsOwnHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://bedrock-mantle.us-east-1.api.aws/v1/responses", nil)
	ApplyBedrockProject(request, HeaderBedrockOpenAIProject, "proj_abc123")
	if request.Header.Get(HeaderBedrockOpenAIProject) != "proj_abc123" {
		t.Fatalf("the project was not addressed: %v", request.Header)
	}
	if request.Header.Get(HeaderBedrockAnthropicWorkspace) != "" {
		t.Fatal("the Anthropic-protocol header was set on an OpenAI-protocol request")
	}
}

// The account default is addressed by sending nothing, so an empty project must
// leave no header behind — including one an earlier caller set.
func TestApplyBedrockProjectClearsEveryResourceHeaderItKnows(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://bedrock-mantle.us-east-1.api.aws/anthropic/v1/messages", nil)
	request.Header.Set(HeaderBedrockOpenAIProject, "proj_carried_in")
	request.Header.Set(HeaderBedrockAnthropicWorkspace, "proj_carried_in")
	request.Header.Set("anthropic-workspace-id", "wrkspc_carried_in")
	request.Header.Set("OpenAI-Organization", "org_carried_in")

	ApplyBedrockProject(request, HeaderBedrockAnthropicWorkspace, "")

	for _, name := range []string{
		HeaderBedrockOpenAIProject, HeaderBedrockAnthropicWorkspace,
		"anthropic-workspace-id", "OpenAI-Organization",
	} {
		if value := request.Header.Get(name); value != "" {
			t.Fatalf("%s survived into a default-project request: %q", name, value)
		}
	}
}

// anthropic-workspace-id belongs to Claude Platform on AWS. Selecting a Bedrock
// project must never produce it, whichever protocol asked.
func TestApplyBedrockProjectNeverSendsTheClaudePlatformHeader(t *testing.T) {
	for _, header := range []string{HeaderBedrockOpenAIProject, HeaderBedrockAnthropicWorkspace} {
		request := httptest.NewRequest(http.MethodPost, "https://bedrock-mantle.us-east-1.api.aws/v1/responses", nil)
		request.Header.Set("anthropic-workspace-id", "wrkspc_01AbCdEf23GhIj")
		ApplyBedrockProject(request, header, "proj_abc123")
		if request.Header.Get("anthropic-workspace-id") != "" {
			t.Fatalf("addressing via %s left the Claude Platform header in place", header)
		}
	}
}

// The project header is applied before the credential, and the authorizer still
// clears the credential headers it owns. Together that means a stored project
// value cannot displace or forge authentication.
func TestBedrockProjectCannotDisplaceTheCredentialHeader(t *testing.T) {
	authorizer, err := NewStaticHeaderAuthorizer(domain.CredentialBedrockAPIKey, "x-api-key", "", []byte("bedrock-key"), "Authorization", "api-key")
	if err != nil {
		t.Fatal(err)
	}
	defer authorizer.Close()

	request := httptest.NewRequest(http.MethodPost, "https://bedrock-mantle.us-east-1.api.aws/anthropic/v1/messages", nil)
	request.Header.Set("Authorization", "Bearer forged")
	ApplyBedrockProject(request, HeaderBedrockAnthropicWorkspace, "proj_abc123")
	if err := authorizer.Authorize(request, nil); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Authorization") != "" {
		t.Fatal("a forged bearer header survived authorization")
	}
	if request.Header.Get("x-api-key") != "bedrock-key" {
		t.Fatalf("the credential was not applied: %v", request.Header)
	}
	if request.Header.Get(HeaderBedrockAnthropicWorkspace) != "proj_abc123" {
		t.Fatal("authorization dropped the project addressing")
	}
}
