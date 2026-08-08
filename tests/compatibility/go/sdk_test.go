package compatibility

import (
	"context"
	"os"
	"strings"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

func TestOfficialGoSDK(t *testing.T) {
	baseURL := os.Getenv("HALRO_COMPAT_BASE_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:18088/v1"
	}
	client := openai.NewClient(
		option.WithAPIKey("gw_sdk_compatibility"),
		option.WithBaseURL(baseURL),
		option.WithMaxRetries(0),
	)
	anthropicClient := anthropic.NewClient(anthropicoption.WithAPIKey("gw_sdk_compatibility"), anthropicoption.WithBaseURL(strings.TrimSuffix(baseURL, "/v1")), anthropicoption.WithMaxRetries(0))
	message, err := anthropicClient.Messages.New(context.Background(), anthropic.MessageNewParams{Model: anthropic.Model("compat-chat"), MaxTokens: 32, Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("ping"))}})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.Content) == 0 || message.Content[0].Text != "compat-ok" || message.Usage.OutputTokens != 2 {
		t.Fatalf("unexpected Anthropic message: %#v", message)
	}
	params := openai.ChatCompletionNewParams{
		Model: "compat-chat",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("ping"),
		},
	}
	completion, err := client.Chat.Completions.New(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if completion.Choices[0].Message.Content != "compat-ok" || completion.Usage.TotalTokens != 5 {
		t.Fatalf("unexpected completion: %#v", completion)
	}
	responseParams := responses.ResponseNewParams{
		Model: shared.ResponsesModel("compat-chat"),
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("ping")},
		Store: openai.Bool(false),
	}
	response, err := client.Responses.New(context.Background(), responseParams)
	if err != nil {
		t.Fatal(err)
	}
	if response.OutputText() != "compat-ok" || response.Usage.TotalTokens != 5 {
		t.Fatalf("unexpected response: %#v", response)
	}
	responseStream := client.Responses.NewStreaming(context.Background(), responseParams)
	defer responseStream.Close()
	var responseText string
	var completed bool
	for responseStream.Next() {
		event := responseStream.Current()
		if event.Type == "response.output_text.delta" {
			responseText += event.Delta
		}
		if event.Type == "response.completed" {
			completed = true
		}
	}
	if err := responseStream.Err(); err != nil {
		t.Fatal(err)
	}
	if responseText != "compat-ok" || !completed {
		t.Fatalf("unexpected Responses stream text=%q completed=%v", responseText, completed)
	}

	stream := client.Chat.Completions.NewStreaming(context.Background(), params)
	defer stream.Close()
	var content, arguments string
	var totalTokens int64
	for stream.Next() {
		chunk := stream.Current()
		if chunk.Usage.TotalTokens != 0 {
			totalTokens = chunk.Usage.TotalTokens
		}
		for _, choice := range chunk.Choices {
			content += choice.Delta.Content
			for _, call := range choice.Delta.ToolCalls {
				arguments += call.Function.Arguments
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if content != "compat-ok" || arguments != `{"value":"ok"}` || totalTokens != 7 {
		t.Fatalf("unexpected stream content=%q arguments=%q total=%d", content, arguments, totalTokens)
	}

	_, err = client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model: "error-rate", Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("ping")},
	})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected typed HTTP error, got %v", err)
	}
}
