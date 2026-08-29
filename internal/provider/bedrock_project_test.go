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
	request.Header.Set(HeaderBedrockAnthropicWorkspace, "wrkspc_carried_in")
	request.Header.Set("anthropic-workspace", "proj_carried_in")
	request.Header.Set("OpenAI-Organization", "org_carried_in")

	ApplyBedrockProject(request, HeaderBedrockAnthropicWorkspace, "")

	for _, name := range []string{
		HeaderBedrockOpenAIProject, HeaderBedrockAnthropicWorkspace,
		"anthropic-workspace", "OpenAI-Organization",
	} {
		if value := request.Header.Get(name); value != "" {
			t.Fatalf("%s survived into a default-project request: %q", name, value)
		}
	}
}

// A value someone else put on the request never survives, whichever protocol
// asked. Bedrock and Claude Platform on AWS spell this header the same way, so
// a carried-in wrkspc_ id is exactly the shape that would otherwise ride along
// — cleared here, and refused at the write path by ValidateBedrockProjectID.
func TestApplyBedrockProjectReplacesACarriedInWorkspaceValue(t *testing.T) {
	for _, test := range []struct {
		header string
		want   string
	}{
		{HeaderBedrockOpenAIProject, ""},
		{HeaderBedrockAnthropicWorkspace, "proj_abc123"},
	} {
		request := httptest.NewRequest(http.MethodPost, "https://bedrock-mantle.us-east-1.api.aws/v1/responses", nil)
		request.Header.Set(HeaderBedrockAnthropicWorkspace, "wrkspc_01AbCdEf23GhIj")
		ApplyBedrockProject(request, test.header, "proj_abc123")
		if got := request.Header.Get(HeaderBedrockAnthropicWorkspace); got != test.want {
			t.Fatalf("addressing via %s left %s=%q, want %q", test.header, HeaderBedrockAnthropicWorkspace, got, test.want)
		}
	}
}

// The name Halro used to send selects nothing. It must not reach the provider
// from a caller or an intermediary that learned it.
func TestApplyBedrockProjectClearsTheSupersededSpelling(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://bedrock-mantle.us-east-1.api.aws/anthropic/v1/messages", nil)
	request.Header.Set("anthropic-workspace", "proj_carried_in")
	ApplyBedrockProject(request, HeaderBedrockAnthropicWorkspace, "proj_abc123")
	if value := request.Header.Get("anthropic-workspace"); value != "" {
		t.Fatalf("the superseded spelling survived: %q", value)
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
