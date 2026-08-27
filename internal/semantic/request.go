package semantic

import (
	"bytes"
	"encoding/json"
	"errors"
)

type Requirements struct {
	Streaming     bool `json:"streaming,omitempty"`
	StreamUsage   bool `json:"stream_usage,omitempty"`
	Tools         bool `json:"tools,omitempty"`
	ParallelTools bool `json:"parallel_tools,omitempty"`
	Vision        bool `json:"vision,omitempty"`
	// FetchedImage is set by a request that names an image instead of carrying
	// it. It is a separate requirement because it is a separate claim about the
	// target: reading a picture and going to get one are different things, and a
	// target that does only the first must not be handed the second.
	FetchedImage bool `json:"fetched_image,omitempty"`
	// JSONObject and StructuredOutputs are set by the two output formats that
	// used to raise one requirement between them. A schema-less json_object
	// request and a schema-backed one ask a target for different things, and the
	// providers divide on exactly that line, so one requirement could not keep a
	// schema request off a target that serves only the schema-less mode.
	JSONObject         bool `json:"json_object,omitempty"`
	StructuredOutputs  bool `json:"structured_outputs,omitempty"`
	DeveloperRole      bool `json:"developer_role,omitempty"`
	Reasoning          bool `json:"reasoning,omitempty"`
	Seed               bool `json:"seed,omitempty"`
	MultipleCandidates bool `json:"multiple_candidates,omitempty"`
	EndUserReference   bool `json:"end_user_reference,omitempty"`
	// ProviderExecutedTools is set by a request that asks the upstream to run
	// tools of its own. Routing treats it like any other requirement, which is
	// what keeps a request that implies upstream egress off a connection whose
	// operator never accepted it.
	ProviderExecutedTools bool `json:"provider_executed_tools,omitempty"`
}

// ToolExecution says who runs a tool. It is not a formatting distinction: a
// caller-executed tool comes back as a call for the caller to answer, while a
// provider-executed one is run upstream, which means the provider originates
// network calls Halro never sees and SafeTransport never filters.
//
// That is why it is a member here rather than a naming convention: routing has
// to be able to keep a request that implies upstream egress off a connection
// whose operator never accepted it, and it can only do that if the request says
// so in a field.
type ToolExecution string

const (
	ToolExecutionCaller   ToolExecution = "caller"
	ToolExecutionProvider ToolExecution = "provider"
)

// ProviderToolWebSearch is the only provider-executed tool this model carries.
//
// The others were considered and left out, each for the same reason rather than
// for want of demand. A code interpreter runs in a container that survives the
// call and returns files stored on the provider's side; a file search reads a
// vector store the provider holds. Both are provider-side state, and a gateway
// whose consistency boundary is one process owning one data directory has
// nowhere to put a handle to somebody else's state. Web search has no such
// handle: it takes a query and returns text with citations.
const ProviderToolWebSearch = "web_search"

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	// Execution is empty for a caller-executed tool, which is what an absent
	// member has always meant here. A provider-executed tool carries the
	// upstream's own name for it in Name and no schema of its own — the caller
	// is not describing a function, they are naming one the provider already has.
	Execution ToolExecution `json:"execution,omitempty"`
}

// ProviderExecuted reports a tool the upstream runs itself.
func (tool Tool) ProviderExecuted() bool { return tool.Execution == ToolExecutionProvider }

type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceNamed    ToolChoiceMode = "named"
)

type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	Name string         `json:"name,omitempty"`
}

type OutputFormatKind string

const (
	OutputText       OutputFormatKind = "text"
	OutputJSONObject OutputFormatKind = "json_object"
	OutputJSONSchema OutputFormatKind = "json_schema"
)

type OutputFormat struct {
	Kind        OutputFormatKind `json:"kind"`
	Name        string           `json:"name,omitempty"`
	Description string           `json:"description,omitempty"`
	Schema      json.RawMessage  `json:"schema,omitempty"`
	Strict      bool             `json:"strict,omitempty"`
}

type GenerateRequest struct {
	Operation               Operation     `json:"operation"`
	Source                  Source        `json:"source"`
	Mode                    ExecutionMode `json:"mode"`
	RequestedModel          string        `json:"requested_model"`
	Messages                []Message     `json:"messages"`
	Tools                   []Tool        `json:"tools,omitempty"`
	ToolChoice              *ToolChoice   `json:"tool_choice,omitempty"`
	ParallelTools           *bool         `json:"parallel_tools,omitempty"`
	OutputFormat            *OutputFormat `json:"output_format,omitempty"`
	Temperature             *float64      `json:"temperature,omitempty"`
	TopP                    *float64      `json:"top_p,omitempty"`
	VisibleOutputTokenLimit *int64        `json:"visible_output_token_limit,omitempty"`
	CompletionTokenLimit    *int64        `json:"completion_token_limit,omitempty"`
	Candidates              *int          `json:"candidates,omitempty"`
	Stop                    []string      `json:"stop,omitempty"`
	Seed                    *int64        `json:"seed,omitempty"`
	ReasoningEffort         string        `json:"reasoning_effort,omitempty"`
	EndUserRef              string        `json:"end_user_ref,omitempty"`
	Stream                  bool          `json:"stream,omitempty"`
	IncludeUsage            bool          `json:"include_usage,omitempty"`
	Requirements            Requirements  `json:"requirements"`
}

func (request GenerateRequest) Validate() error {
	if request.Operation != OperationGenerate || request.Source.Validate() != nil || request.Mode != ModePortable || request.RequestedModel == "" || len(request.Messages) == 0 {
		return errors.New("semantic generate request identity is invalid")
	}
	for _, message := range request.Messages {
		if err := message.Validate(); err != nil {
			return err
		}
	}
	for _, tool := range request.Tools {
		if tool.Name == "" || (len(tool.Schema) > 0 && (!json.Valid(tool.Schema) || bytes.TrimSpace(tool.Schema)[0] != '{')) {
			return errors.New("semantic tool is invalid")
		}
		switch tool.Execution {
		case "", ToolExecutionCaller:
		case ToolExecutionProvider:
			// A schema or a description would be the caller describing a function
			// the provider already owns. Neither is carried, so neither is accepted:
			// silently ignoring them would let a caller believe they had constrained
			// something they had not.
			if tool.Name != ProviderToolWebSearch || len(tool.Schema) > 0 || tool.Description != "" {
				return errors.New("semantic provider-executed tool is invalid")
			}
		default:
			return errors.New("semantic tool execution is invalid")
		}
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Mode {
		case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
			if request.ToolChoice.Name != "" {
				return errors.New("semantic tool choice has an unexpected name")
			}
		case ToolChoiceNamed:
			if request.ToolChoice.Name == "" {
				return errors.New("semantic named tool choice is missing a name")
			}
		default:
			return errors.New("semantic tool choice is invalid")
		}
	}
	if request.OutputFormat != nil {
		switch request.OutputFormat.Kind {
		case OutputText, OutputJSONObject:
			if request.OutputFormat.Name != "" || request.OutputFormat.Description != "" || len(request.OutputFormat.Schema) > 0 || request.OutputFormat.Strict {
				return errors.New("semantic output format has unrelated fields")
			}
		case OutputJSONSchema:
			if request.OutputFormat.Name == "" || len(request.OutputFormat.Schema) == 0 || !json.Valid(request.OutputFormat.Schema) {
				return errors.New("semantic json schema output is invalid")
			}
		default:
			return errors.New("semantic output format is invalid")
		}
	}
	if request.VisibleOutputTokenLimit != nil && *request.VisibleOutputTokenLimit < 0 {
		return errors.New("semantic visible output token limit is invalid")
	}
	if request.CompletionTokenLimit != nil && *request.CompletionTokenLimit < 0 {
		return errors.New("semantic completion token limit is invalid")
	}
	if request.Requirements != request.DeriveRequirements() {
		return errors.New("semantic capability requirements do not match request content")
	}
	if request.Requirements.Streaming != request.Stream || request.Requirements.StreamUsage != request.IncludeUsage {
		return errors.New("semantic streaming requirements are inconsistent")
	}
	return nil
}

func (request GenerateRequest) DeriveRequirements() Requirements {
	jsonObject := request.OutputFormat != nil && request.OutputFormat.Kind == OutputJSONObject
	structuredOutputs := request.OutputFormat != nil && request.OutputFormat.Kind == OutputJSONSchema
	parallelTools := request.ParallelTools != nil && *request.ParallelTools
	// A provider-executed tool is not a function the caller can be asked to run,
	// so it raises its own requirement and not the tools one: a target that does
	// function calling and no upstream search would otherwise look like a match.
	callerTools, providerTools := false, false
	for _, tool := range request.Tools {
		if tool.ProviderExecuted() {
			providerTools = true
			continue
		}
		callerTools = true
	}
	result := Requirements{Streaming: request.Stream, StreamUsage: request.IncludeUsage, Tools: callerTools || request.ToolChoice != nil || parallelTools, ParallelTools: parallelTools, ProviderExecutedTools: providerTools, JSONObject: jsonObject, StructuredOutputs: structuredOutputs, Reasoning: request.ReasoningEffort != "", Seed: request.Seed != nil, MultipleCandidates: request.Candidates != nil && *request.Candidates > 1, EndUserReference: request.EndUserRef != ""}
	for _, message := range request.Messages {
		if message.Role == RoleDeveloper {
			result.DeveloperRole = true
		}
		for _, part := range message.Content {
			switch part.Kind {
			case ContentInputImage:
				result.Vision = true
				if !part.Inline() {
					result.FetchedImage = true
				}
			case ContentToolCall, ContentToolResult:
				result.Tools = true
			case ContentReasoning:
				result.Reasoning = true
			}
		}
	}
	return result
}

type EmbeddingRequest struct {
	Operation      Operation       `json:"operation"`
	Source         Source          `json:"source"`
	Mode           ExecutionMode   `json:"mode"`
	RequestedModel string          `json:"requested_model"`
	Input          json.RawMessage `json:"input"`
	Encoding       string          `json:"encoding,omitempty"`
	Dimensions     *int64          `json:"dimensions,omitempty"`
	EndUserRef     string          `json:"end_user_ref,omitempty"`
	Requirements   Requirements    `json:"requirements"`
}

func (request EmbeddingRequest) Validate() error {
	if request.Operation != OperationEmbed || request.Source.Validate() != nil || request.Mode != ModePortable || request.RequestedModel == "" || len(request.Input) == 0 || !json.Valid(request.Input) || bytes.Equal(bytes.TrimSpace(request.Input), []byte("null")) {
		return errors.New("semantic embedding request is invalid")
	}
	if request.Encoding != "" && request.Encoding != "float" && request.Encoding != "base64" {
		return errors.New("semantic embedding encoding is invalid")
	}
	if request.Dimensions != nil && *request.Dimensions <= 0 {
		return errors.New("semantic embedding dimensions are invalid")
	}
	if request.Requirements != request.DeriveRequirements() {
		return errors.New("semantic embedding requirements do not match request content")
	}
	return nil
}

func (request EmbeddingRequest) DeriveRequirements() Requirements {
	return Requirements{EndUserReference: request.EndUserRef != ""}
}

// EstimatedInputBytes is the size of what the caller sent, measured on the
// portable representation rather than on any one wire form.
//
// It is deliberately a property of the semantic request: every facade's bytes
// differ — the same conversation is longer in Responses items than in Chat
// messages — and charging a caller a different estimate for the same content
// because of the endpoint they used is not something a wire-level measurement
// can avoid.
//
// Image bytes are counted here and taken back out by whoever converts this to
// tokens: a data URL is the whole picture in base64, and there is no useful
// reading of it as text.
func (request GenerateRequest) EstimatedInputBytes() int64 {
	var total int64
	for _, message := range request.Messages {
		total += int64(len(message.Role) + len(message.Name))
		for _, part := range message.Content {
			// The kind is not counted. It is this model's discriminator, not
			// anything the caller wrote, and charging for it would make the
			// estimate depend on how many parts a wire form happened to split the
			// same text into.
			total += int64(len(part.Text) + len(part.URL) + len(part.Detail) +
				len(part.CallID) + len(part.Name) + len(part.Arguments))
		}
	}
	for _, tool := range request.Tools {
		total += int64(len(tool.Name) + len(tool.Description) + len(tool.Schema))
	}
	return total
}
