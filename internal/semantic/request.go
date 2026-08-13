package semantic

import (
	"bytes"
	"encoding/json"
	"errors"
)

type Requirements struct {
	Streaming          bool `json:"streaming,omitempty"`
	StreamUsage        bool `json:"stream_usage,omitempty"`
	Tools              bool `json:"tools,omitempty"`
	ParallelTools      bool `json:"parallel_tools,omitempty"`
	InputImage         bool `json:"input_image,omitempty"`
	StructuredJSON     bool `json:"structured_json,omitempty"`
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

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

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
	structuredJSON := request.OutputFormat != nil && (request.OutputFormat.Kind == OutputJSONObject || request.OutputFormat.Kind == OutputJSONSchema)
	parallelTools := request.ParallelTools != nil && *request.ParallelTools
	result := Requirements{Streaming: request.Stream, StreamUsage: request.IncludeUsage, Tools: len(request.Tools) > 0 || request.ToolChoice != nil || parallelTools, ParallelTools: parallelTools, StructuredJSON: structuredJSON, Reasoning: request.ReasoningEffort != "", Seed: request.Seed != nil, MultipleCandidates: request.Candidates != nil && *request.Candidates > 1, EndUserReference: request.EndUserRef != ""}
	for _, message := range request.Messages {
		if message.Role == RoleDeveloper {
			result.DeveloperRole = true
		}
		for _, part := range message.Content {
			switch part.Kind {
			case ContentInputImage:
				result.InputImage = true
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
