package compatibility

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestVolcenginePreservesImagesJSONAndSeparateOutputLimitMeanings(t *testing.T) {
	limit := int64(128)
	for _, completionLimit := range []bool{false, true} {
		request := openaiapi.ChatCompletionRequest{
			Model:          "ep-custom",
			Messages:       []openaiapi.Message{{Role: "user", Name: "client", Content: json.RawMessage(`[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8=","detail":"auto"}}]`)}},
			ResponseFormat: json.RawMessage(`{"type":"json_schema","json_schema":{"name":"result","strict":true,"schema":{"type":"object","properties":{}}}}`),
		}
		if completionLimit {
			request.MaxCompletionTokens = &limit
		} else {
			request.MaxTokens = &limit
		}
		body, err := RenderVolcengineChatRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		if body.MaxTokens != request.MaxTokens || body.MaxCompletionTokens != request.MaxCompletionTokens || string(body.Messages[0].Content) != string(request.Messages[0].Content) || string(body.ResponseFormat) != string(request.ResponseFormat) {
			t.Fatalf("request semantics changed: %+v", body)
		}
	}
}

func TestVolcengineRefusesLossyRequestsBeforeTransport(t *testing.T) {
	limit, candidates := int64(12), 2
	for _, test := range []struct {
		name     string
		request  openaiapi.ChatCompletionRequest
		semantic semantic.GenerateRequest
		field    string
	}{
		{"candidates", openaiapi.ChatCompletionRequest{N: &candidates}, semantic.GenerateRequest{Candidates: &candidates}, "n"},
		{"attribution", openaiapi.ChatCompletionRequest{User: "client"}, semantic.GenerateRequest{EndUserRef: "client"}, "user"},
		{"reasoning", openaiapi.ChatCompletionRequest{ReasoningEffort: "minimal"}, semantic.GenerateRequest{ReasoningEffort: "minimal"}, "reasoning_effort"},
		{"two limits", openaiapi.ChatCompletionRequest{MaxTokens: &limit, MaxCompletionTokens: &limit}, semantic.GenerateRequest{VisibleOutputTokenLimit: &limit, CompletionTokenLimit: &limit}, "max_completion_tokens"},
		{"developer", openaiapi.ChatCompletionRequest{Messages: []openaiapi.Message{{Role: "developer"}}}, semantic.GenerateRequest{Messages: []semantic.Message{{Role: semantic.RoleDeveloper}}}, "messages[].role=developer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := RenderVolcengineChatRequest(test.request); err == nil {
				t.Fatal("renderer silently accepted unsupported semantics")
			}
			if fields := UnsupportedGenerateFields(domain.ProfileVolcengineChat, test.semantic); !slices.Contains(fields, test.field) {
				t.Fatalf("routing does not refuse %s: %v", test.field, fields)
			}
		})
	}
}

func TestVolcengineCoverageStatusIsExplicit(t *testing.T) {
	covered := false
	for _, manifest := range BuiltinEndpointManifests() {
		for _, coverage := range manifest.ProfileCoverage {
			if coverage.ProfileID != domain.ProfileVolcengineChat {
				continue
			}
			covered = true
			if coverage.Status != StatusExperimental {
				t.Errorf("%s changed the reviewed rollout status to %s", manifest.ID, coverage.Status)
			}
		}
	}
	if !covered {
		t.Fatal("Ark has no published endpoint coverage")
	}
}
