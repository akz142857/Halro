package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/akz142857/Heimdall/internal/anthropicapi"
	"github.com/akz142857/Heimdall/internal/gateway"
	"github.com/akz142857/Heimdall/internal/gatewayapi"
	"github.com/akz142857/Heimdall/internal/openaiapi"
)

const compatibilityKey = "gw_sdk_compatibility"

type service struct {
	activeStreams   atomic.Int64
	canceledStreams atomic.Int64
}

func (s *service) Messages(_ context.Context, key string, request anthropicapi.MessageRequest) (anthropicapi.Message, error) {
	if err := contractError(key, request.Model); err != nil {
		return anthropicapi.Message{}, err
	}
	stop := "end_turn"
	return anthropicapi.Message{ID: "msg_compat", Type: "message", Role: "assistant", Content: anthropicapi.ContentBlocks{{Type: "text", Text: "compat-ok"}}, Model: request.Model, StopReason: &stop, Usage: anthropicapi.Usage{InputTokens: 3, OutputTokens: 2}}, nil
}
func (s *service) MessagesStream(ctx context.Context, key string, request anthropicapi.MessageRequest, emit func(anthropicapi.StreamEvent) error) error {
	if err := contractError(key, request.Model); err != nil {
		return err
	}
	s.activeStreams.Add(1)
	defer s.activeStreams.Add(-1)
	message := anthropicapi.Message{ID: "msg_compat", Type: "message", Role: "assistant", Content: anthropicapi.ContentBlocks{}, Model: request.Model, Usage: anthropicapi.Usage{InputTokens: 3}}
	index := 0
	events := []anthropicapi.StreamEvent{{Type: "message_start", Message: &message}, {Type: "content_block_start", Index: &index, ContentBlock: map[string]any{"type": "text", "text": ""}}, {Type: "content_block_delta", Index: &index, Delta: map[string]any{"type": "text_delta", "text": "compat-ok"}}, {Type: "content_block_stop", Index: &index}, {Type: "message_delta", Delta: map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, Usage: &anthropicapi.Usage{OutputTokens: 2}}, {Type: "message_stop"}}
	for _, event := range events {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}
func (s *service) MessagesNative(ctx context.Context, key, _ string, request anthropicapi.MessageRequest) (anthropicapi.Message, error) {
	return s.Messages(ctx, key, request)
}
func (s *service) MessagesNativeStream(ctx context.Context, key, _ string, request anthropicapi.MessageRequest, emit func(anthropicapi.RawStreamEvent) error) error {
	return s.MessagesStream(ctx, key, request, func(event anthropicapi.StreamEvent) error {
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		return emit(anthropicapi.RawStreamEvent{Type: event.Type, Data: payload})
	})
}

func (s *service) Responses(_ context.Context, key string, request openaiapi.ResponseRequest) (openaiapi.Response, error) {
	if err := contractError(key, request.Model); err != nil {
		return openaiapi.Response{}, err
	}
	completedAt := int64(1_800_000_000)
	return openaiapi.Response{
		ID: "resp_compat", Object: "response", CreatedAt: completedAt, Status: "completed", Background: false,
		CompletedAt: &completedAt, Error: nil, IncompleteDetails: nil, Instructions: nil,
		MaxOutputTokens: request.MaxOutputTokens, Model: request.Model,
		Output:            []openaiapi.ResponseOutputItem{{ID: "msg_compat", Type: "message", Status: "completed", Role: "assistant", Content: []openaiapi.ResponseOutputContent{{Type: "output_text", Text: "compat-ok", Annotations: []any{}, Logprobs: []any{}}}}},
		ParallelToolCalls: true, PreviousResponseID: nil, Reasoning: openaiapi.ResponseReasoningOut{Effort: nil, Summary: nil},
		Store: false, Temperature: request.Temperature, Text: openaiapi.ResponseTextOut{Format: openaiapi.ResponseTextFormat{Type: "text"}},
		ToolChoice: "auto", Tools: []openaiapi.ResponseTool{}, TopP: request.TopP, Truncation: "disabled",
		Usage: &openaiapi.ResponseUsage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}, Metadata: map[string]string{},
	}, nil
}

func (s *service) ResponsesStream(ctx context.Context, key string, request openaiapi.ResponseRequest, emit func(openaiapi.ResponseStreamEvent) error) error {
	if err := contractError(key, request.Model); err != nil {
		return err
	}
	s.activeStreams.Add(1)
	defer s.activeStreams.Add(-1)
	response, _ := s.Responses(ctx, key, request)
	response.Status, response.CompletedAt, response.Output, response.Usage = "in_progress", nil, []openaiapi.ResponseOutputItem{}, nil
	sequence := int64(0)
	emitEvent := func(event openaiapi.ResponseStreamEvent) error {
		event.SequenceNumber = sequence
		sequence++
		return emit(event)
	}
	if err := emitEvent(openaiapi.ResponseStreamEvent{Type: "response.created", Response: &response}); err != nil {
		return err
	}
	if request.Model == "slow-stream" {
		<-ctx.Done()
		s.canceledStreams.Add(1)
		return ctx.Err()
	}
	outputIndex, contentIndex := 0, 0
	item := openaiapi.ResponseOutputItem{ID: "msg_compat", Type: "message", Status: "in_progress", Role: "assistant", Content: []openaiapi.ResponseOutputContent{}}
	part := openaiapi.ResponseOutputContent{Type: "output_text", Text: "", Annotations: []any{}, Logprobs: []any{}}
	if err := emitEvent(openaiapi.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &outputIndex, Item: &item}); err != nil {
		return err
	}
	if err := emitEvent(openaiapi.ResponseStreamEvent{Type: "response.content_part.added", OutputIndex: &outputIndex, ContentIndex: &contentIndex, ItemID: item.ID, Part: &part}); err != nil {
		return err
	}
	if err := emitEvent(openaiapi.ResponseStreamEvent{Type: "response.output_text.delta", OutputIndex: &outputIndex, ContentIndex: &contentIndex, ItemID: item.ID, Delta: "compat-ok"}); err != nil {
		return err
	}
	completed, _ := s.Responses(ctx, key, request)
	return emitEvent(openaiapi.ResponseStreamEvent{Type: "response.completed", Response: &completed})
}

func (s *service) Chat(_ context.Context, key string, request openaiapi.ChatCompletionRequest) (openaiapi.ChatCompletionResponse, error) {
	if err := contractError(key, request.Model); err != nil {
		return openaiapi.ChatCompletionResponse{}, err
	}
	finish := "stop"
	return openaiapi.ChatCompletionResponse{
		ID: "chatcmpl-compat", Object: "chat.completion", Created: 1_800_000_000, Model: request.Model,
		Choices: []openaiapi.Choice{{Index: 0, Message: &openaiapi.Message{
			Role: "assistant", Content: openaiapi.TextContent("compat-ok"),
		}, FinishReason: &finish}},
		Usage: &openaiapi.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}, nil
}

func (s *service) ChatStream(ctx context.Context, key string, request openaiapi.ChatCompletionRequest, emit func(openaiapi.ChatCompletionResponse) error) error {
	if err := contractError(key, request.Model); err != nil {
		return err
	}
	s.activeStreams.Add(1)
	defer s.activeStreams.Add(-1)
	base := openaiapi.ChatCompletionResponse{
		ID: "chatcmpl-stream-compat", Object: "chat.completion.chunk", Created: 1_800_000_000, Model: request.Model,
	}
	emitDelta := func(message openaiapi.Message, finish *string) error {
		chunk := base
		chunk.Choices = []openaiapi.Choice{{Index: 0, Delta: &message, FinishReason: finish}}
		return emit(chunk)
	}
	if err := emitDelta(openaiapi.Message{Role: "assistant", ReasoningContent: "reasoning-safe"}, nil); err != nil {
		return err
	}
	if request.Model == "slow-stream" {
		<-ctx.Done()
		s.canceledStreams.Add(1)
		return ctx.Err()
	}
	if err := emitDelta(openaiapi.Message{Content: openaiapi.TextContent("compat-ok")}, nil); err != nil {
		return err
	}
	toolIndex := 0
	if err := emitDelta(openaiapi.Message{ToolCalls: []openaiapi.ToolCall{{
		Index: &toolIndex, ID: "call_compat", Type: "function",
		Function: openaiapi.ToolCallFunction{Name: "echo", Arguments: "{\"value\":"},
	}}}, nil); err != nil {
		return err
	}
	if err := emitDelta(openaiapi.Message{ToolCalls: []openaiapi.ToolCall{{
		Index: &toolIndex, Function: openaiapi.ToolCallFunction{Arguments: "\"ok\"}"},
	}}}, nil); err != nil {
		return err
	}
	finish := "tool_calls"
	if err := emitDelta(openaiapi.Message{}, &finish); err != nil {
		return err
	}
	usage := base
	usage.Choices = []openaiapi.Choice{}
	usage.Usage = &openaiapi.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}
	return emit(usage)
}

func (s *service) Embeddings(_ context.Context, key string, request openaiapi.EmbeddingRequest) (openaiapi.EmbeddingResponse, error) {
	if err := contractError(key, request.Model); err != nil {
		return openaiapi.EmbeddingResponse{}, err
	}
	embedding := json.RawMessage(`[0.125,-0.25,0.5]`)
	if request.EncodingFormat == "base64" {
		encoded := make([]byte, 3*4)
		for index, value := range []float32{0.125, -0.25, 0.5} {
			binary.LittleEndian.PutUint32(encoded[index*4:], math.Float32bits(value))
		}
		embedding, _ = json.Marshal(base64.StdEncoding.EncodeToString(encoded))
	}
	return openaiapi.EmbeddingResponse{
		Object: "list", Model: request.Model,
		Data:  []openaiapi.EmbeddingData{{Object: "embedding", Index: 0, Embedding: embedding}},
		Usage: &openaiapi.Usage{PromptTokens: 2, TotalTokens: 2},
	}, nil
}

func contractError(key, model string) error {
	if key != compatibilityKey {
		return &gateway.Error{Code: "invalid_api_key", Message: "invalid API key", HTTPStatus: http.StatusUnauthorized}
	}
	switch model {
	case "error-budget":
		return &gateway.Error{Code: "budget_exceeded", Message: "budget exceeded", HTTPStatus: http.StatusForbidden}
	case "error-rate":
		return &gateway.Error{Code: "rate_limit_exceeded", Message: "rate limit exceeded", HTTPStatus: http.StatusTooManyRequests, RetryAfter: time.Second}
	case "error-provider":
		return &gateway.Error{Code: "provider_error", Message: "provider failed", HTTPStatus: http.StatusBadGateway}
	default:
		return nil
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18088", "compatibility server listen address")
	flag.Parse()

	backend := &service{}
	handler, err := gatewayapi.NewWithOptions(backend, gatewayapi.Options{
		MaxRequestBytes: 1 << 20, RouteTimeout: 5 * time.Second,
		StreamTimeout: 30 * time.Second, WriteTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handler.ChatCompletions)
	mux.HandleFunc("POST /v1/responses", handler.Responses)
	mux.HandleFunc("POST /v1/messages", handler.Messages)
	mux.HandleFunc("POST /v1/embeddings", handler.Embeddings)
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /compat/stats", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]int64{
			"active_streams": sSafeLoad(&backend.activeStreams), "canceled_streams": sSafeLoad(&backend.canceledStreams),
		})
	})

	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	shutdown, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdown.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	fmt.Printf("compatibility server listening on %s\n", *listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func sSafeLoad(value *atomic.Int64) int64 { return value.Load() }
