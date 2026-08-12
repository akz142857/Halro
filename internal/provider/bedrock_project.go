package provider

import "net/http"

// The headers that select a Bedrock Project on the Mantle access surface. AWS
// documents Workspaces (Anthropic-compatible) and Projects (OpenAI-compatible)
// as one underlying project resource named differently per protocol, so which
// header carries it is decided by the wire protocol, not by the operator.
const (
	HeaderBedrockOpenAIProject      = "OpenAI-Project"
	HeaderBedrockAnthropicWorkspace = "anthropic-workspace"
)

// bedrockResourceHeaders is every project-selecting header this gateway knows
// about, including the one that belongs to a different AWS service. All of them
// are cleared before the correct one is set, so a header carried in from
// anywhere else cannot survive into a provider request.
//
// anthropic-workspace-id is Claude Platform on AWS. It is listed here to be
// deleted, never to be sent.
var bedrockResourceHeaders = []string{
	HeaderBedrockOpenAIProject,
	HeaderBedrockAnthropicWorkspace,
	"anthropic-workspace-id",
	"OpenAI-Organization",
}

// ApplyBedrockProject sets the project header this protocol uses, or sends none
// when the provider addresses the account's default project.
//
// Deliberately not part of the credential authorizer and deliberately not a
// free-form header map. A project is resource addressing, not authentication;
// routing it through the authorizer would put operator-supplied text on the
// same path as the secret, and a map would let a stored value name
// Authorization and defeat the authorizer's own header clearing. The header
// name is chosen by the caller from the two constants above, and nothing else
// can be set through this function.
//
// Call before authorizing, so a signing credential scheme sees the headers it
// has to cover.
func ApplyBedrockProject(request *http.Request, header, projectID string) {
	if request == nil {
		return
	}
	for _, name := range bedrockResourceHeaders {
		request.Header.Del(name)
	}
	if projectID == "" {
		return
	}
	request.Header.Set(header, projectID)
}
