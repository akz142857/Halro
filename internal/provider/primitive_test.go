package provider

import (
	"context"
	"testing"

	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

type usageOnDeltaAdapter struct{}

func (*usageOnDeltaAdapter) Type() string { return "gemini" }
func (*usageOnDeltaAdapter) Close()       {}
func (*usageOnDeltaAdapter) Chat(context.Context, ChatCall) (openaiapi.ChatCompletionResponse, error) {
	return openaiapi.ChatCompletionResponse{}, nil
}
func (*usageOnDeltaAdapter) Embed(context.Context, EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, nil
}
func (*usageOnDeltaAdapter) ChatStream(_ context.Context, call ChatCall, emit func(semantic.Event) error) (*openaiapi.Usage, error) {
	usage := &semantic.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3, Source: semantic.UsageProviderReported}
	if err := emit(semantic.Event{Kind: semantic.EventDelta, ID: "id", Model: call.ProviderModel, Outputs: []semantic.OutputDelta{{Index: 0, Role: semantic.RoleAssistant, Content: []semantic.ContentDelta{{Kind: semantic.ContentText, Text: "a"}}}}, Usage: usage}); err != nil {
		return nil, err
	}
	if err := emit(semantic.Event{Kind: semantic.EventDelta, ID: "id", Model: call.ProviderModel, Outputs: []semantic.OutputDelta{{Index: 0, Content: []semantic.ContentDelta{{Kind: semantic.ContentText, Text: "b"}}, Termination: "complete"}}}); err != nil {
		return nil, err
	}
	return nil, nil
}

func TestLegacyGenerationPrimitiveNormalizesUsageAndAddsAuditMetadata(t *testing.T) {
	primitive := legacyGenerationPrimitive{adapter: &usageOnDeltaAdapter{}, primitive: PrimitiveGeminiStreamGenerateContent, operation: OperationChatStream}
	request := semantic.GenerateRequest{Operation: semantic.OperationGenerate, Source: semantic.Source{ProfileID: "openai.chat-completions", ProfileRevision: 1}, Mode: semantic.ModePortable, RequestedModel: "public", Messages: []semantic.Message{{Role: semantic.RoleUser, Content: []semantic.Content{{Kind: semantic.ContentText, Text: "hi"}}}}, Stream: true, IncludeUsage: true, Requirements: semantic.Requirements{Streaming: true, StreamUsage: true}}
	var events []semantic.Event
	usage, err := primitive.GenerateStream(context.Background(), GenerateCall{RequestID: "request", ProviderModel: "provider-model", Request: request}, func(event semantic.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.TotalTokens != 3 || len(events) != 3 || events[2].Kind != semantic.EventUsage {
		t.Fatalf("usage=%#v events=%#v", usage, events)
	}
	validator := semantic.NewStreamValidator()
	for _, event := range events {
		if event.MappingRevision != openaiwire.MappingRevision || event.Translation != semantic.TranslationDeclared {
			t.Fatalf("missing audit metadata: %#v", event)
		}
		if event.Kind == semantic.EventDelta && event.Usage != nil {
			t.Fatalf("usage remained attached to output delta: %#v", event)
		}
		if err := validator.Accept(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := validator.Finalize(true); err != nil {
		t.Fatal(err)
	}
}
