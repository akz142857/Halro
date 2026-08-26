package openai

import (
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

// A provider-executed tool has to survive the whole way down and the whole way
// back, because either half alone is useless: a search that reaches the upstream
// and comes back as an answer with no sources is indistinguishable from the
// model making it up.
func TestWebSearchSurvivesRequestAndResultMapping(t *testing.T) {
	request := openaiapi.ResponseRequest{
		Model: "public", Input: json.RawMessage(`"who won?"`),
		Tools: []openaiapi.ResponseTool{{Type: openaiapi.ProviderExecutedToolWebSearch}},
	}
	canonical, err := DecodeResponseGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	if !canonical.Requirements.ProviderExecutedTools || canonical.Requirements.Tools {
		t.Fatalf("requirements=%#v", canonical.Requirements)
	}
	rendered, err := RenderProviderResponseRequest(canonical, "provider-model")
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Tools) != 1 || rendered.Tools[0].Type != openaiapi.ProviderExecutedToolWebSearch || rendered.Tools[0].Name != "" {
		t.Fatalf("rendered tools=%#v", rendered.Tools)
	}

	upstream := openaiapi.Response{
		ID: "resp_1", Model: "provider-model", Status: "completed",
		Output: []openaiapi.ResponseOutputItem{
			{ID: "ws_1", Type: openaiapi.OutputItemWebSearchCall, Status: "completed",
				Action: &openaiapi.ResponseToolAction{Type: openaiapi.ToolActionSearch, Query: "who won"}},
			{ID: "msg_1", Type: "message", Status: "completed", Role: "assistant", Content: []openaiapi.ResponseOutputContent{{
				Type: "output_text", Text: "Halro won.",
				Annotations: []openaiapi.ResponseAnnotation{{
					Type: openaiapi.AnnotationURLCitation, URL: "https://example.test/a",
					Title: "A", StartIndex: 0, EndIndex: 5,
				}},
			}}},
		},
	}
	result, err := DecodeProviderResponse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	content := result.Choices[0].Message.Content
	if len(content) != 2 || content[0].Kind != semantic.ContentProviderToolCall ||
		content[0].Name != semantic.ProviderToolWebSearch || content[0].Status != "completed" || content[0].Text != "who won" {
		t.Fatalf("decoded content=%#v", content)
	}
	if len(content[1].Citations) != 1 || content[1].Citations[0].URL != "https://example.test/a" {
		t.Fatalf("citations were lost: %#v", content[1])
	}

	back, err := RenderResponseResult(result, request)
	if err != nil {
		t.Fatal(err)
	}
	var searchItems, messageItems int
	for _, item := range back.Output {
		switch item.Type {
		case openaiapi.OutputItemWebSearchCall:
			searchItems++
			if item.Action == nil || item.Action.Query != "who won" {
				t.Fatalf("the query the model wrote was lost: %#v", item)
			}
		case "message":
			messageItems++
			if len(item.Content[0].Annotations) != 1 || item.Content[0].Annotations[0].URL != "https://example.test/a" {
				t.Fatalf("citations were not rendered back: %#v", item.Content[0])
			}
		}
	}
	if searchItems != 1 || messageItems != 1 {
		t.Fatalf("output=%#v", back.Output)
	}
}

// An answer whose sources cannot be carried is not the same answer. The Chat
// wire has no member for either half, so it refuses rather than returning the
// text alone.
func TestChatWireRefusesWhatOnlyResponsesCanCarry(t *testing.T) {
	for name, message := range map[string]semantic.Message{
		"provider tool call": {Role: semantic.RoleAssistant, Content: []semantic.Content{
			{Kind: semantic.ContentProviderToolCall, CallID: "ws_1", Name: semantic.ProviderToolWebSearch, Status: "completed"},
		}},
		"cited text": {Role: semantic.RoleAssistant, Content: []semantic.Content{
			{Kind: semantic.ContentText, Text: "Halro won.", Citations: []semantic.Citation{{URL: "https://example.test/a"}}},
		}},
	} {
		result := semantic.GenerateResult{
			ID: "resp_1", Created: 1, Model: "provider-model", Translation: semantic.TranslationNone,
			MappingRevision: MappingRevision,
			Choices:         []semantic.GenerateChoice{{Index: 0, Message: message, Termination: "complete"}},
		}
		if _, err := RenderGenerateResult(result); err == nil {
			t.Fatalf("%s was rendered into a Chat response that cannot hold it", name)
		}
	}
}
