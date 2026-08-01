package semantic

import (
	"strings"
	"testing"
)

func TestValidateRejectsMalformedDirectEvent(t *testing.T) {
	event := Event{
		Kind: EventDelta, ID: "id", Model: "model", MappingRevision: 1, Translation: TranslationNone,
		Outputs: []OutputDelta{{Index: 0, Content: []ContentDelta{{Kind: "invalid"}}}},
	}
	if err := event.Validate(); err == nil {
		t.Fatal("invalid raw content was accepted")
	}
}

func TestValidateAcceptsProviderNeutralEvent(t *testing.T) {
	event := Event{
		Kind: EventDelta, ID: "id", Model: "model", MappingRevision: 1, Translation: TranslationNone,
		Outputs: []OutputDelta{{Index: 0, Content: []ContentDelta{{Kind: ContentText, Text: "hello"}}, Termination: "complete"}},
		Usage:   &Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3, Source: UsageProviderReported},
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStreamValidatorEnforcesIdentityTerminationUsageAndAggregateToolLimit(t *testing.T) {
	validator := NewStreamValidator()
	base := Event{Kind: EventDelta, ID: "id", Model: "model", MappingRevision: 1, Translation: TranslationNone, Outputs: []OutputDelta{{Index: 0, Role: RoleAssistant}}}
	if err := validator.Accept(base); err != nil {
		t.Fatal(err)
	}
	toolIndex := 0
	tool := Event{Kind: EventDelta, ID: "id", Model: "model", MappingRevision: 1, Translation: TranslationNone, Outputs: []OutputDelta{{Index: 0, Content: []ContentDelta{{Kind: ContentToolCall, ToolIndex: &toolIndex, CallID: "call", Name: "tool", ArgumentsFragment: strings.Repeat("a", MaxToolArgumentBytes/2+1)}}}}}
	if err := validator.Accept(tool); err != nil {
		t.Fatal(err)
	}
	if err := validator.Accept(tool); err == nil {
		t.Fatal("aggregate tool argument limit was not enforced")
	}
	validator = NewStreamValidator()
	if err := validator.Accept(base); err != nil {
		t.Fatal(err)
	}
	terminal := Event{Kind: EventDelta, ID: "id", Model: "model", MappingRevision: 1, Translation: TranslationNone, Outputs: []OutputDelta{{Index: 0, Termination: "complete"}}}
	if err := validator.Accept(terminal); err != nil {
		t.Fatal(err)
	}
	usage := Event{Kind: EventUsage, ID: "id", Model: "model", MappingRevision: 1, Translation: TranslationNone, Usage: &Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2, Source: UsageProviderReported}}
	if err := validator.Accept(usage); err != nil {
		t.Fatal(err)
	}
	if err := validator.Finalize(true); err != nil {
		t.Fatal(err)
	}
	if err := validator.Accept(base); err == nil {
		t.Fatal("event after final usage accepted")
	}
}

func TestStreamValidatorBindsToolIdentityToOutputAndToolIndex(t *testing.T) {
	toolIndex := 0
	validator := NewStreamValidator()
	first := Event{Kind: EventDelta, ID: "id", Model: "model", MappingRevision: 1, Translation: TranslationDeclared, Outputs: []OutputDelta{{Index: 2, Content: []ContentDelta{{Kind: ContentToolCall, ToolIndex: &toolIndex, CallID: "call_1", Name: "lookup", ArgumentsFragment: `{"q":`}}}}}
	if err := validator.Accept(first); err != nil {
		t.Fatal(err)
	}
	continuation := Event{Kind: EventDelta, ID: "id", Model: "model", MappingRevision: 1, Translation: TranslationDeclared, Outputs: []OutputDelta{{Index: 2, Content: []ContentDelta{{Kind: ContentToolCall, ToolIndex: &toolIndex, ArgumentsFragment: `"x"}`}}}}}
	if err := validator.Accept(continuation); err != nil {
		t.Fatal(err)
	}
	conflict := continuation
	conflict.Outputs = []OutputDelta{{Index: 2, Content: []ContentDelta{{Kind: ContentToolCall, ToolIndex: &toolIndex, CallID: "call_2"}}}}
	if err := validator.Accept(conflict); err == nil {
		t.Fatal("tool call id changed for a stable output/tool index")
	}
}

func TestStreamValidatorRejectsMissingAuditAndIncompleteToolIdentity(t *testing.T) {
	toolIndex := 0
	missingAudit := Event{Kind: EventDelta, ID: "id", Model: "model", Outputs: []OutputDelta{{Index: 0}}}
	if err := NewStreamValidator().Accept(missingAudit); err == nil {
		t.Fatal("stream event without audit metadata was accepted")
	}
	validator := NewStreamValidator()
	incomplete := Event{Kind: EventDelta, ID: "id", Model: "model", MappingRevision: 1, Translation: TranslationNone, Outputs: []OutputDelta{{Index: 0, Content: []ContentDelta{{Kind: ContentToolCall, ToolIndex: &toolIndex, ArgumentsFragment: `{}`}}, Termination: "complete"}}}
	if err := validator.Accept(incomplete); err != nil {
		t.Fatal(err)
	}
	if err := validator.Finalize(false); err == nil {
		t.Fatal("incomplete streamed tool identity was accepted")
	}
}
