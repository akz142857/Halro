package compatibility

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func bigModelBaseRequest(model string) openaiapi.ChatCompletionRequest {
	return openaiapi.ChatCompletionRequest{
		Model: model, Messages: []openaiapi.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
}

func TestBigModelChatRendersOnlyTheDocumentedWireMembers(t *testing.T) {
	temperature, topP := 0.5, 0.8
	limit, one, noParallel := int64(64), 1, true
	request := bigModelBaseRequest("glm-5.2")
	request.Temperature, request.TopP = &temperature, &topP
	request.MaxCompletionTokens, request.N, request.ParallelToolCalls = &limit, &one, &noParallel
	request.StreamOptions = &openaiapi.StreamOptions{IncludeUsage: true}
	request.User = "halro-user"
	request.ReasoningEffort = "high"
	request.ToolChoice = json.RawMessage(`"auto"`)

	body, err := RenderBigModelChatRequest(request, "request-123")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"model", "messages", "temperature", "top_p", "max_tokens", "thinking", "reasoning_effort", "tool_choice", "request_id", "user_id"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("documented member %q is missing from %s", name, encoded)
		}
	}
	for _, name := range []string{"stream_options", "max_completion_tokens", "n", "parallel_tool_calls", "user"} {
		if _, ok := fields[name]; ok {
			t.Errorf("OpenAI-only member %q leaked onto the BigModel wire: %s", name, encoded)
		}
	}
}

func TestBigModelChatReasoningIsExactPerModel(t *testing.T) {
	for _, test := range []struct {
		model, effort, thinking string
		wantErr                 bool
	}{
		{"glm-5.3", "low", "enabled", false},
		{"glm-5.3", "high", "enabled", false},
		{"glm-5.3", "none", "", true},
		{"glm-5.3", "medium", "", true},
		{"glm-5.2", "high", "enabled", false},
		{"glm-5.2", "none", "disabled", false},
		{"glm-5.2", "low", "", true},
		{"glm-4.7", "none", "", false},
		{"glm-4.7", "high", "", true},
	} {
		request := bigModelBaseRequest(test.model)
		request.ReasoningEffort = test.effort
		body, err := RenderBigModelChatRequest(request, "")
		if test.wantErr {
			if err == nil {
				t.Errorf("%s/%s was accepted", test.model, test.effort)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s/%s: %v", test.model, test.effort, err)
			continue
		}
		got := ""
		if body.Thinking != nil {
			got = body.Thinking.Type
		}
		if got != test.thinking {
			t.Errorf("%s/%s thinking = %q, want %q", test.model, test.effort, got, test.thinking)
		}
	}
}

func TestBigModelChatRefusesLossyValuesBeforeProviderIO(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*openaiapi.ChatCompletionRequest)
	}{
		{"temperature", func(r *openaiapi.ChatCompletionRequest) { value := 1.1; r.Temperature = &value }},
		{"top_p", func(r *openaiapi.ChatCompletionRequest) { value := 0.0; r.TopP = &value }},
		{"top_p_below_minimum", func(r *openaiapi.ChatCompletionRequest) { value := 0.009; r.TopP = &value }},
		{"max_tokens_zero", func(r *openaiapi.ChatCompletionRequest) { value := int64(0); r.MaxCompletionTokens = &value }},
		{"max_tokens_above_maximum", func(r *openaiapi.ChatCompletionRequest) { value := int64(131_073); r.MaxCompletionTokens = &value }},
		{"n", func(r *openaiapi.ChatCompletionRequest) { value := 2; r.N = &value }},
		{"n_zero", func(r *openaiapi.ChatCompletionRequest) { value := 0; r.N = &value }},
		{"seed", func(r *openaiapi.ChatCompletionRequest) { value := int64(3); r.Seed = &value }},
		{"parallel_tool_calls", func(r *openaiapi.ChatCompletionRequest) { value := false; r.ParallelToolCalls = &value }},
		{"too_many_tools", func(r *openaiapi.ChatCompletionRequest) { r.Tools = make([]openaiapi.Tool, 129) }},
		{"short_user", func(r *openaiapi.ChatCompletionRequest) { r.User = "short" }},
		{"named_tool", func(r *openaiapi.ChatCompletionRequest) {
			r.ToolChoice = json.RawMessage(`{"type":"function","function":{"name":"f"}}`)
		}},
		{"too_many_stops", func(r *openaiapi.ChatCompletionRequest) { r.Stop = json.RawMessage(`["1","2","3","4","5"]`) }},
		{"json_schema", func(r *openaiapi.ChatCompletionRequest) { r.ResponseFormat = json.RawMessage(`{"type":"json_schema"}`) }},
		{"developer", func(r *openaiapi.ChatCompletionRequest) { r.Messages[0].Role = "developer" }},
		{"message_name", func(r *openaiapi.ChatCompletionRequest) { r.Messages[0].Name = "name" }},
		{"image_detail", func(r *openaiapi.ChatCompletionRequest) {
			r.Messages[0].Content = json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":"high"}}]`)
		}},
	} {
		request := bigModelBaseRequest("glm-4.7")
		test.mutate(&request)
		if _, err := RenderBigModelChatRequest(request, ""); err == nil {
			t.Errorf("%s was accepted", test.name)
		}
	}
}

func TestBigModelChatCarriesToolsJSONAndImagesWithoutInventingImageFidelity(t *testing.T) {
	request := bigModelBaseRequest("glm-4.6v")
	request.Messages[0].Content = json.RawMessage(`[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"https://example.test/a.png","detail":"auto"}}]`)
	request.Tools = []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "inspect", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	request.ToolChoice = json.RawMessage(`"auto"`)
	body, err := RenderBigModelChatRequest(request, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 1 || string(body.ToolChoice) != `"auto"` {
		t.Fatalf("tools were not preserved: %#v", body)
	}
	if encoded := string(body.Messages[0].Content); strings.Contains(encoded, "detail") || !strings.Contains(encoded, "https://example.test/a.png") {
		t.Fatalf("image content was rendered incorrectly: %s", encoded)
	}

	jsonRequest := bigModelBaseRequest("glm-5.2")
	jsonRequest.ResponseFormat = json.RawMessage(`{"type":"json_object"}`)
	jsonBody, err := RenderBigModelChatRequest(jsonRequest, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(jsonBody.ResponseFormat) != `{"type":"json_object"}` {
		t.Fatalf("JSON response format was not preserved: %#v", jsonBody)
	}
}

func TestBigModelChatAlwaysRendersStopAsAnArray(t *testing.T) {
	request := bigModelBaseRequest("glm-4.7")
	request.Stop = json.RawMessage(`"END"`)
	body, err := RenderBigModelChatRequest(request, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body.Stop), `["END"]`; got != want {
		t.Fatalf("stop=%s want=%s", got, want)
	}
}

func TestBigModelTargetRulesProtectReasoningAndOutputBounds(t *testing.T) {
	answer, completion := int64(8), int64(16)
	for _, test := range []struct {
		model, effort string
		answer, total *int64
		want          []string
	}{
		{"glm-5.3", "none", nil, nil, []string{"reasoning_effort"}},
		{"glm-5.3", "", &answer, nil, []string{"max_tokens"}},
		{"glm-5.2", "high", &answer, nil, []string{"max_tokens"}},
		{"glm-5.2", "none", &answer, nil, nil},
		{"glm-5.2", "none", &answer, &completion, []string{"max_tokens"}},
	} {
		got := BigModelTargetUnsupportedGenerateFields(test.model, semantic.GenerateRequest{
			ReasoningEffort: test.effort, VisibleOutputTokenLimit: test.answer, CompletionTokenLimit: test.total,
		})
		if stringList(got) != stringList(test.want) {
			t.Errorf("%s/%s fields = %v, want %v", test.model, test.effort, got, test.want)
		}
	}
}

func stringList(values []string) string {
	encoded, _ := json.Marshal(values)
	return string(encoded)
}
