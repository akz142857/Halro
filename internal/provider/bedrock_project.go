package provider

import "net/http"

// The headers that select a Bedrock Project on the Mantle access surface. AWS
// documents Workspaces (Anthropic-compatible) and Projects (OpenAI-compatible)
// as one underlying project resource named differently per protocol, so which
// header carries it is decided by the wire protocol, not by the operator.
//
// The Messages spelling is `anthropic-workspace-id`, the same name Claude
// Platform on AWS uses. The header name is not what separates the two products —
// this was the error that shipped: Halro sent `anthropic-workspace`, which no
// service reads, and deleted the documented name, so a connection that named a
// project was silently billed to the account default. What separates them is the
// host (bedrock-mantle vs aws-external-anthropic) and the identifier: Bedrock
// workspaces are the Projects API's `proj_` resource, Claude Platform's are
// `wrkspc_`. ValidateBedrockProjectID is where that boundary is enforced, and it
// is the only place it can be.
//
// Measured against a real account on 2026-08-29, us-east-2: the same request
// carrying the same nonexistent project id answers 404 under this name and 200
// under the old one. A name the service reads has to reject an unknown project;
// a name it does not read cannot. The 200 is the bug — served against the
// account default, carrying a project that does not exist.
// See docs/verification/provider-real-matrix.md. Still unmeasured: that a valid
// id lands on that project rather than merely passing validation, which is a
// CloudWatch reading, and that Halro's own path sends what curl sent.
const (
	HeaderBedrockOpenAIProject      = "OpenAI-Project"
	HeaderBedrockAnthropicWorkspace = "anthropic-workspace-id"
)

// bedrockResourceHeaders is every project-selecting header this gateway knows
// about. All of them are cleared before the correct one is set, so a header
// carried in from anywhere else cannot survive into a provider request.
//
// `anthropic-workspace` is in the list only to be deleted: it is the name Halro
// used to send, so a caller or an intermediary that learned it must not have it
// reach the provider now that it selects nothing.
var bedrockResourceHeaders = []string{
	HeaderBedrockOpenAIProject,
	HeaderBedrockAnthropicWorkspace,
	"anthropic-workspace",
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
