package compatibility

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestMiniMaxM31SamplingConstraintsAtAdmissionAndRendering(t *testing.T) {
	fixedTemperature, fixedTopP, changed := 1.0, 0.95, 0.2
	for _, test := range []struct {
		name              string
		temperature, topP *float64
		field             string
	}{
		{name: "defaults"},
		{name: "explicit fixed values", temperature: &fixedTemperature, topP: &fixedTopP},
		{name: "ignored temperature", temperature: &changed, field: "temperature"},
		{name: "ignored top_p", topP: &changed, field: "top_p"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := minimaxBaseRequest()
			request.Model = "MiniMax-M3.1-Flash"
			request.Temperature, request.TopP = test.temperature, test.topP
			fields := UnsupportedGenerateFieldsForTarget(domain.ProfileMiniMaxChat, request.Model, semantic.GenerateRequest{Temperature: test.temperature, TopP: test.topP})
			body, err := RenderMiniMaxChatRequest(request)
			if test.field != "" {
				if !slices.Contains(fields, test.field) || err == nil {
					t.Fatalf("constraint lost: fields=%v err=%v", fields, err)
				}
			} else if len(fields) != 0 || err != nil || body.Temperature != test.temperature || body.TopP != test.topP {
				t.Fatalf("fixed sampling refused or changed: fields=%v body=%+v err=%v", fields, body, err)
			}
		})
	}
}

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
