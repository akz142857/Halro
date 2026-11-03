package compatibility

import (
	"encoding/json"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"testing"
)

func TestMiniMaxM31PreservesForcedReasoningAndDepth(t *testing.T) {
	for _, effort := range []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		request := minimaxBaseRequest()
		request.Model = "MiniMax-M3.1-Flash"
		request.ReasoningEffort = effort
		body, err := RenderMiniMaxChatRequest(request)
		if effort == "none" || effort == "minimal" {
			if err == nil {
				t.Errorf("unsupported effort %s accepted", effort)
			}
			continue
		}
		if err != nil {
			t.Fatalf("effort %s: %v", effort, err)
		}
		if body.Thinking == nil || body.Thinking.Type != MiniMaxThinkingOn || !body.ReasoningSplit || body.ReasoningEffort != effort {
			t.Fatalf("effort %s: %#v", effort, body)
		}
	}
	request := minimaxBaseRequest()
	request.Model = "MiniMax-M3.1-Flash"
	limit := int64(64)
	request.MaxTokens = &limit
	if _, err := RenderMiniMaxChatRequest(request); err == nil {
		t.Fatal("visible-only output budget accepted on forced-thinking model")
	}
}

func TestKimiK31EffortsDoNotChangeK3OffSwitch(t *testing.T) {
	for _, model := range []string{"kimi-k3.1-pro", "kimi-k3.1-pro-highspeed"} {
		for _, effort := range []string{"", "none", "low", "high", "max", "medium"} {
			request := kimiBaseRequest()
			request.Model = model
			request.ReasoningEffort = effort
			body, err := RenderKimiChatRequest(request)
			if effort == "none" || effort == "medium" {
				if err == nil {
					t.Fatalf("%s/%s accepted", model, effort)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s/%s: %v", model, effort, err)
			}
			if body.ReasoningEffort != effort {
				t.Fatalf("lost depth: %#v", body)
			}
		}
	}
	request := kimiBaseRequest()
	request.Model = "kimi-k3"
	request.ReasoningEffort = "none"
	body, err := RenderKimiChatRequest(request)
	if err != nil || body.Thinking == nil || body.Thinking.Type != "disabled" {
		t.Fatalf("legacy K3 off switch changed: %#v %v", body, err)
	}
}

func TestGLMMaxEffortAndToolHistory(t *testing.T) {
	for _, model := range []string{"glm-5.2", "glm-5.3", "glm-5.3-flash"} {
		request := bigModelBaseRequest(model)
		request.ReasoningEffort = "max"
		body, err := RenderBigModelChatRequest(domain.ProfileBigModelGlobalChat, request, "")
		if err != nil || body.ReasoningEffort != "max" || body.Thinking == nil || body.Thinking.Type != "enabled" {
			t.Fatalf("%s max: %#v %v", model, body, err)
		}
	}
	for _, effort := range []string{"", "high"} {
		request := bigModelBaseRequest("glm-5.3")
		request.ReasoningEffort = effort
		request.Stream = true
		request.Tools = []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}
		request.Messages = append(request.Messages, openaiapi.Message{Role: "assistant", ReasoningContent: "prior reasoning", ToolCalls: []openaiapi.ToolCall{{ID: "call1", Type: "function", Function: openaiapi.ToolCallFunction{Name: "lookup", Arguments: `{}`}}}}, openaiapi.Message{Role: "tool", ToolCallID: "call1", Content: openaiapi.TextContent("ok")})
		body, err := RenderBigModelChatRequest(domain.ProfileBigModelGlobalChat, request, "")
		if err != nil {
			t.Fatal(err)
		}
		if !body.ToolStream || body.Thinking == nil || body.Thinking.ClearThinking == nil || *body.Thinking.ClearThinking || body.Messages[1].ReasoningContent != "prior reasoning" {
			t.Fatalf("tool history lost: %#v", body)
		}
	}
}
