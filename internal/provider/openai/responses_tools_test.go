package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

const responseToolTurn = `{"id":"resp_tool","created_at":1,"model":"gpt-6.1-sol","status":"completed","output":[{"type":"reasoning","id":"rs_original","summary":[{"type":"summary_text","text":"Use ping."}],"encrypted_content":"ciphertext+/="},{"type":"function_call","id":"fc_original","call_id":"call_ping","name":"ping","arguments":"{\"value\":1}"}],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":30},"output_tokens":15,"output_tokens_details":{"reasoning_tokens":10},"total_tokens":115}}`

func toolChatRequest() openaiapi.ChatCompletionRequest {
	return openaiapi.ChatCompletionRequest{Model: "gpt-sol", ReasoningEffort: "max", Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("ping")}}, Tools: []openaiapi.Tool{{Type: "function", Function: openaiapi.ToolFunction{Name: "ping", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}}}`)}}}}
}
func TestResponsesToolsReasoningAndReplayReachOnlyResponses(t *testing.T) {
	calls := 0
	adapter := newResponsesAdapter(t, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("wrong upstream: %s", r.URL)
		}
		var body struct {
			Model     string                        `json:"model"`
			Store     *bool                         `json:"store"`
			Reasoning openaiapi.ResponseReasoning   `json:"reasoning"`
			Tools     []openaiapi.ResponseTool      `json:"tools"`
			Input     []openaiapi.ResponseInputItem `json:"input"`
			Include   []string                      `json:"include"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "gpt-6.1-sol" || body.Store == nil || *body.Store || body.Reasoning.Effort != "max" || len(body.Include) != 1 {
			t.Fatalf("request control lost: %+v", body)
		}
		if len(body.Tools) != 1 || body.Tools[0].Strict == nil || *body.Tools[0].Strict {
			t.Fatal("Chat tools must explicitly retain non-strict schemas")
		}
		answer := responseToolTurn
		if calls == 2 {
			if len(body.Input) != 4 || body.Input[1].Type != "reasoning" || body.Input[1].ID != "rs_original" || body.Input[1].EncryptedContent != "ciphertext+/=" || body.Input[2].CallID != "call_ping" || body.Input[3].Type != "function_call_output" {
				t.Fatalf("replay state lost: %+v", body.Input)
			}
			answer = `{"id":"resp_answer","created_at":2,"model":"gpt-6.1-sol","status":"completed","output":[{"type":"message","id":"msg_2","role":"assistant","content":[{"type":"output_text","text":"pong"}]}]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(answer))}, nil
	})})
	first := toolChatRequest()
	reply, err := adapter.Chat(context.Background(), provider.ChatCall{ProviderModel: "gpt-6.1-sol", Request: first})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Choices[0].FinishReason == nil || *reply.Choices[0].FinishReason != "tool_calls" {
		t.Fatal("tool finish reason lost")
	}
	if reply.Usage.CachedPromptTokens() != 20 || reply.Usage.CacheWritePromptTokens() != 30 || reply.Usage.ReasoningTokens() != 10 {
		t.Fatalf("usage tiers lost: %+v", reply.Usage)
	}
	first.Messages = append(first.Messages, *reply.Choices[0].Message, openaiapi.Message{Role: "tool", ToolCallID: "call_ping", Content: openaiapi.TextContent("pong")})
	answer, err := adapter.Chat(context.Background(), provider.ChatCall{ProviderModel: "gpt-6.1-sol", Request: first})
	if err != nil || string(answer.Choices[0].Message.Content) != `"pong"` || calls != 2 {
		t.Fatalf("tool continuation failed: %+v %v", answer, err)
	}
}

func toolResponsesSSE() string {
	var response openaiapi.Response
	_ = json.Unmarshal([]byte(responseToolTurn), &response)
	var frames strings.Builder
	write := func(e openaiapi.ResponseStreamEvent) {
		body, _ := json.Marshal(e)
		frames.WriteString("event: " + e.Type + "\ndata: " + string(body) + "\n\n")
	}
	created := response
	created.Output = nil
	created.Status = "in_progress"
	created.Usage = nil
	write(openaiapi.ResponseStreamEvent{Type: "response.created", Response: &created})
	zero, one := 0, 1
	reasoning := response.Output[0]
	reasoning.EncryptedContent = ""
	reasoning.Summary = nil
	write(openaiapi.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &zero, Item: &reasoning})
	tool := response.Output[1]
	tool.Arguments = ""
	write(openaiapi.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &one, Item: &tool})
	write(openaiapi.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: &one, ItemID: tool.ID, Delta: `{"value":`})
	write(openaiapi.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: &one, ItemID: tool.ID, Delta: `1}`})
	write(openaiapi.ResponseStreamEvent{Type: "response.completed", Response: &response})
	return frames.String()
}

func TestResponsesStreamCarriesToolsReasoningAndFinalUsage(t *testing.T) {
	adapter := newResponsesAdapter(t, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/responses" {
			t.Fatal("stream reached Chat endpoint")
		}
		var wire openaiapi.ResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Fatal(err)
		}
		if !wire.Stream || len(wire.Tools) != 1 || wire.Reasoning.Effort != "max" {
			t.Fatalf("stream request lost controls: %+v", wire)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(toolResponsesSSE()))}, nil
	})})
	canonical, err := openaiwire.DecodeGenerate(toolChatRequest())
	if err != nil {
		t.Fatal(err)
	}
	canonical.Stream = true
	canonical.IncludeUsage = true
	canonical.Requirements = canonical.DeriveRequirements()
	manifest, _ := provider.BuiltinProfile("openai.responses.v1")
	bridge, err := provider.NewLegacyAdapterBridge(adapter, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	operation, ok := bridge.Operations().Resolve(provider.OperationChatStream)
	if !ok {
		t.Fatal("stream not resolvable through wrapper")
	}
	validator := semantic.NewStreamValidator()
	var arguments, opaque strings.Builder
	var termination string
	usage, err := operation.(provider.GenerationAdapter).GenerateStream(context.Background(), provider.GenerateCall{ProviderModel: "gpt-6.1-sol", Request: canonical}, func(e semantic.Event) error {
		if err := validator.Accept(e); err != nil {
			return err
		}
		for _, out := range e.Outputs {
			if out.Termination != "" {
				termination = out.Termination
			}
			for _, part := range out.Content {
				arguments.WriteString(part.ArgumentsFragment)
				opaque.WriteString(part.EncryptedContent)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.Finalize(true); err != nil {
		t.Fatal(err)
	}
	if arguments.String() != `{"value":1}` || termination != "tool_call" || opaque.Len() == 0 || usage.CacheWriteInputTokens != 30 {
		t.Fatalf("stream fields lost: %s %s %d %+v", arguments.String(), termination, opaque.Len(), usage)
	}
}

func TestResponsesTruncatedAndFailedStreamsRemainAmbiguous(t *testing.T) {
	full := toolResponsesSSE()
	for _, body := range []string{full[:strings.Index(full, "event: response.completed")], strings.Replace(full, "event: response.completed", "event: response.failed", 1)} {
		adapter := newResponsesAdapter(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})})
		canonical, _ := openaiwire.DecodeGenerate(toolChatRequest())
		_, err := adapter.GenerateSemanticStream(context.Background(), provider.GenerateCall{ProviderModel: "gpt-6.1-sol", Request: canonical}, func(semantic.Event) error { return nil })
		var classified *provider.Error
		if !errors.As(err, &classified) || !classified.Ambiguous {
			t.Fatalf("truncated stream was not ambiguous: %v", err)
		}
	}
}
