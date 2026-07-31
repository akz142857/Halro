package compatibility

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestOfficialGoSDK(t *testing.T) {
	baseURL := os.Getenv("HEIMDALL_COMPAT_BASE_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:18088/v1"
	}
	client := openai.NewClient(
		option.WithAPIKey("gw_sdk_compatibility"),
		option.WithBaseURL(baseURL),
		option.WithMaxRetries(0),
	)
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
