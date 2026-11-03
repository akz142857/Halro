package compatibility

import (
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestVolcengineReasoningSwitchAndExactModelEffortContract(t *testing.T) {
	for _, effort := range []string{"", "none", "low", "medium", "high"} {
		body, err := RenderVolcengineChatRequest(openaiapi.ChatCompletionRequest{Model: "doubao-seed-2-1-lite-260915", ReasoningEffort: effort, Messages: []openaiapi.Message{{Role: "assistant", EncryptedContent: "opaque"}}})
		if err != nil {
			t.Fatal(err)
		}
		want := "enabled"
		if effort == "" || effort == "none" {
			want = "disabled"
		}
		if body.Thinking.Type != want || body.Messages[0].EncryptedContent != "opaque" {
			t.Fatalf("effort=%q body=%+v", effort, body)
		}
	}
	for _, test := range []struct{ model, effort string }{
		{"doubao-seed-2-0-mini-260428", "minimal"}, {"doubao-seed-2-0-mini-260428", "xhigh"},
		{"deepseek-v4-1-flash-261017", "medium"}, {"glm-5-2-260617", "low"},
		{"glm-5-3-flash-260828", "none"}, {"future-model", "high"},
	} {
		if _, err := RenderVolcengineChatRequest(openaiapi.ChatCompletionRequest{Model: test.model, ReasoningEffort: test.effort}); err == nil {
			t.Fatalf("silently accepted %s/%s", test.model, test.effort)
		}
		if fields := UnsupportedGenerateFieldsForTarget(domain.ProfileVolcengineChat, test.model, semantic.GenerateRequest{ReasoningEffort: test.effort}); !slices.Contains(fields, "reasoning_effort") {
			t.Fatalf("routing admitted %s/%s", test.model, test.effort)
		}
	}
	forced, err := RenderVolcengineChatRequest(openaiapi.ChatCompletionRequest{Model: "glm-5-3-flash-260828"})
	if err != nil || forced.Thinking.Type != "enabled" {
		t.Fatal("forced thinking was implicitly disabled")
	}
	kimi, err := RenderVolcengineChatRequest(openaiapi.ChatCompletionRequest{Model: "kimi-k3-1-pro-261024", ReasoningEffort: "high"})
	if err != nil || kimi.Thinking != nil || !VolcengineReasonsUnasked(kimi.Model) {
		t.Fatal("forced Kimi thinking must omit the unsupported switch")
	}
	if VolcengineReasoningSupported(kimi.Model, "none") || VolcengineReasoningSupported(kimi.Model, "medium") {
		t.Fatal("forced or rounded Kimi depth was accepted")
	}
	for _, effort := range []string{"medium", "xhigh"} {
		if _, err := RenderVolcengineChatRequest(openaiapi.ChatCompletionRequest{Model: "deepseek-v4-pro-260425", ReasoningEffort: effort}); err != nil {
			t.Fatalf("native DeepSeek effort %s refused: %v", effort, err)
		}
	}
}

func TestEncryptedReasoningCannotCrossProviderProfiles(t *testing.T) {
	request := semantic.GenerateRequest{Messages: []semantic.Message{{Role: semantic.RoleAssistant, Content: []semantic.Content{{Kind: semantic.ContentReasoning, EncryptedContent: "opaque"}}}}}
	for _, profile := range domain.AllProviderProfiles() {
		fields := UnsupportedGenerateFields(profile.ID, request)
		if slices.Contains(fields, "messages[].encrypted_content") == (profile.ID == domain.ProfileVolcengineChat || profile.ID == domain.ProfileOpenAIResponses) {
			t.Errorf("wrong encrypted replay boundary for %s: %v", profile.ID, fields)
		}
	}
}

func TestVolcengineEndpointIDUsesReviewedModelOnlyForParameterSemantics(t *testing.T) {
	request := openaiapi.ChatCompletionRequest{Model: "ep-custom", ReasoningEffort: "high"}
	if _, err := RenderVolcengineChatRequest(request); err == nil {
		t.Fatal("opaque endpoint identifier invented a depth contract")
	}
	body, err := RenderVolcengineChatRequestForModel(request, "doubao-seed-2-0-lite-260428")
	if err != nil || body.Model != "ep-custom" || body.Thinking.Type != "enabled" || body.ReasoningEffort != "high" {
		t.Fatalf("reviewed endpoint mapping changed its wire target: %+v %v", body, err)
	}
}

func TestVolcengineDeepSeekAnswerBudgetCannotBecomeAReasoningBudget(t *testing.T) {
	limit := int64(64)
	request := openaiapi.ChatCompletionRequest{Model: "deepseek-v4-1-flash-261017", ReasoningEffort: "high", MaxTokens: &limit}
	if _, err := RenderVolcengineChatRequest(request); err == nil {
		t.Fatal("answer limit became a total limit")
	}
	if !slices.Contains(UnsupportedGenerateFieldsForTarget(domain.ProfileVolcengineChat, request.Model, semantic.GenerateRequest{ReasoningEffort: "high", VisibleOutputTokenLimit: &limit}), "max_tokens") {
		t.Fatal("routing admitted a lossy budget")
	}
	request.MaxTokens, request.MaxCompletionTokens = nil, &limit
	if _, err := RenderVolcengineChatRequest(request); err != nil {
		t.Fatal(err)
	}
}
