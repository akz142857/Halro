package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	wire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/redaction"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestResponsesStreamUsesContiguousChatToolIndices(t *testing.T) {
	adapter := newResponsesAdapter(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(toolResponsesSSE()))}, nil
	})})
	canonical, err := wire.DecodeGenerate(toolChatRequest())
	if err != nil {
		t.Fatal(err)
	}
	canonical.Stream = true
	canonical.Requirements = canonical.DeriveRequirements()
	var chunks []openaiapi.ChatCompletionResponse
	_, err = adapter.GenerateSemanticStream(context.Background(), provider.GenerateCall{ProviderModel: "gpt-6.1-sol", Request: canonical}, func(e semantic.Event) error {
		chunk, err := wire.RenderEvent(e)
		if err != nil {
			return err
		}
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var arguments strings.Builder
	identities := 0
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			if choice.Delta == nil {
				continue
			}
			for _, call := range choice.Delta.ToolCalls {
				arguments.WriteString(call.Function.Arguments)
				if call.ID == "call_ping" && call.Function.Name == "ping" {
					identities++
				}
				if call.Index == nil || *call.Index != 0 {
					t.Errorf("sole Chat tool_call has index %v, want 0", call.Index)
				}
			}
		}
	}
	if identities != 1 || arguments.String() != `{"value":1}` {
		t.Fatalf("tool lost or duplicated: identities=%d arguments=%s", identities, arguments.String())
	}
}

func TestResponsesStreamParallelToolIndicesSkipTextItems(t *testing.T) {
	state := responsesStreamState{items: map[int]*responsesStreamItem{}}
	var parts []semantic.ContentDelta
	accept := func(event openaiapi.ResponseStreamEvent) {
		t.Helper()
		_, err := state.accept(event, false, func(event semantic.Event) error {
			for _, output := range event.Outputs {
				parts = append(parts, output.Content...)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	accept(openaiapi.ResponseStreamEvent{Type: "response.created", Response: &openaiapi.Response{ID: "resp_parallel", Model: "test"}})
	zero := 0
	accept(openaiapi.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &zero, Item: &openaiapi.ResponseOutputItem{Type: "message", ID: "msg"}})
	for index, callID := range []string{"call_a", "call_b"} {
		upstreamIndex := index + 1
		accept(openaiapi.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &upstreamIndex, Item: &openaiapi.ResponseOutputItem{Type: "function_call", ID: callID, CallID: callID, Name: "ping"}})
		accept(openaiapi.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: &upstreamIndex, Delta: `{}`})
	}
	if len(parts) != 4 {
		t.Fatalf("parts=%+v", parts)
	}
	for index, part := range parts {
		if part.ToolIndex == nil || *part.ToolIndex != index/2 {
			t.Fatalf("part %d: index=%v", index, part.ToolIndex)
		}
	}
}

func TestResponsesStreamSendsChatRoleOnceAfterRedaction(t *testing.T) {
	adapter := newResponsesAdapter(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(toolResponsesSSE()))}, nil
	})})
	canonical, err := wire.DecodeGenerate(toolChatRequest())
	if err != nil {
		t.Fatal(err)
	}
	canonical.Stream = true
	canonical.Requirements = canonical.DeriveRequirements()
	stream, err := redaction.NewDefault().NewStream("")
	if err != nil {
		t.Fatal(err)
	}
	var deltas []*openaiapi.Message
	appendChunks := func(chunks []openaiapi.ChatCompletionResponse) {
		for _, c := range chunks {
			for _, choice := range c.Choices {
				if choice.Delta != nil {
					deltas = append(deltas, choice.Delta)
				}
			}
		}
	}
	_, err = adapter.GenerateSemanticStream(context.Background(), provider.GenerateCall{ProviderModel: "gpt-6.1-sol", Request: canonical}, func(e semantic.Event) error {
		chunk, err := wire.RenderEvent(e)
		if err != nil {
			return err
		}
		chunks, err := stream.Process(chunk)
		if err != nil {
			return err
		}
		appendChunks(chunks)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tail, err := stream.Flush()
	if err != nil {
		t.Fatal(err)
	}
	appendChunks(tail)
	roles := 0
	for _, delta := range deltas {
		raw, _ := json.Marshal(delta)
		t.Logf("gateway delta: %s", raw)
		if delta.Role != "" {
			roles++
		}
	}
	if roles != 1 {
		t.Fatalf("gateway sends %d assistant role values to Chat SDK", roles)
	}
}

func TestResponsesStreamWithoutSummaryReplaysAfterRedaction(t *testing.T) {
	var response openaiapi.Response
	if err := json.Unmarshal([]byte(responseToolTurn), &response); err != nil {
		t.Fatal(err)
	}
	response.Output[0].Summary = nil
	// A non-ASCII value also verifies UTF-8 survives deferred re-fragmentation.
	response.Output[1].Arguments = `{"value":"` + strings.Repeat("数", 2731) + `"}`
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
	write(openaiapi.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &zero, Item: &reasoning})
	tool := response.Output[1]
	tool.Arguments = ""
	write(openaiapi.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &one, Item: &tool})
	args := []rune(response.Output[1].Arguments)
	for len(args) > 0 {
		n := min(342, len(args))
		write(openaiapi.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: &one, ItemID: tool.ID, Delta: string(args[:n])})
		args = args[n:]
	}
	write(openaiapi.ResponseStreamEvent{Type: "response.completed", Response: &response})
	adapter := newResponsesAdapter(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames.String()))}, nil
	})})
	request := openaiapi.ResponseRequest{Model: "alias", Input: json.RawMessage(`"ping"`), Stream: true, Tools: []openaiapi.ResponseTool{{Type: "function", Name: "ping", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)}}}
	canonical, err := wire.DecodeResponseGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	renderer := wire.NewResponseStreamRenderer(request)
	stream, err := redaction.NewDefault().NewStream("")
	if err != nil {
		t.Fatal(err)
	}
	acceptChunks := func(chunks []openaiapi.ChatCompletionResponse) error {
		for _, chunk := range chunks {
			e, err := wire.DecodeEvent(chunk)
			if err != nil {
				return err
			}
			if _, err = renderer.Accept(e); err != nil {
				return err
			}
		}
		return nil
	}
	_, err = adapter.GenerateSemanticStream(context.Background(), provider.GenerateCall{ProviderModel: "gpt-6.1-sol", Request: canonical}, func(e semantic.Event) error {
		chunk, err := wire.RenderEvent(e)
		if err != nil {
			return err
		}
		chunks, err := stream.Process(chunk)
		if err != nil {
			return err
		}
		return acceptChunks(chunks)
	})
	if err != nil {
		t.Fatal(err)
	}
	tail, err := stream.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if err = acceptChunks(tail); err != nil {
		t.Fatal(err)
	}
	events, err := renderer.Complete()
	if err != nil {
		t.Fatal(err)
	}
	final := events[len(events)-1].Response
	for i, item := range final.Output {
		t.Logf("northbound output[%d]: type=%s call_id=%s argument_bytes=%d encrypted=%t", i, item.Type, item.CallID, len(item.Arguments), item.EncryptedContent != "")
	}
	raw, err := json.Marshal(final.Output)
	if err != nil {
		t.Fatal(err)
	}
	var replay []json.RawMessage
	if err = json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	replay = append(replay, json.RawMessage(`{"type":"function_call_output","call_id":"call_ping","output":"ok"}`))
	raw, err = json.Marshal(replay)
	if err != nil {
		t.Fatal(err)
	}
	followupRequest := request
	followupRequest.Stream = false
	followupRequest.Input = raw
	followup, err := wire.DecodeResponseGenerate(followupRequest)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := wire.RenderProviderResponseRequest(followup, "gpt-6.1-sol")
	if err != nil {
		t.Fatalf("gateway rejects replay of its own completed stream output through default redactor: %v", err)
	}
	var items []openaiapi.ResponseInputItem
	if err := json.Unmarshal(replayed.Input, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Type != "reasoning" || items[0].EncryptedContent != "ciphertext+/=" || items[1].CallID != "call_ping" || items[1].Arguments != response.Output[1].Arguments || items[2].Type != "function_call_output" {
		t.Fatalf("replayed output changed: %+v", items)
	}
}
