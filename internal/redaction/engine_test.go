package redaction

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/akz142857/Heimdall/internal/openaiapi"
)

func TestInboundSecretDetectionParsesEscapedJSON(t *testing.T) {
	engine := NewDefault()
	request := openaiapi.ChatCompletionRequest{
		Model: "chat",
		Messages: []openaiapi.Message{{
			Role:    "user",
			Content: json.RawMessage(`"sk-abcdefghijklmnop\u0051RST"`),
		}},
	}
	if !errors.Is(engine.ValidateInboundChat(request), ErrSecretDetected) {
		t.Fatal("escaped secret was not detected")
	}
}

func TestInboundEmbeddingDetectsNestedSecret(t *testing.T) {
	engine := NewDefault()
	request := openaiapi.EmbeddingRequest{
		Model: "embedding",
		Input: json.RawMessage(`["normal",{"nested":"Bearer abcdefghijklmnopqrstuvwxyz"}]`),
	}
	if !errors.Is(engine.ValidateInboundEmbedding(request), ErrSecretDetected) {
		t.Fatal("nested secret was not detected")
	}
}

func TestOutboundSecretIsReplacedWithoutMutatingNonSecrets(t *testing.T) {
	engine := NewDefault()
	response := openaiapi.ChatCompletionResponse{
		Choices: []openaiapi.Choice{{Message: &openaiapi.Message{
			Role:    "assistant",
			Content: json.RawMessage(`{"safe":"hello","secret":"AKIAABCDEFGHIJKLMNOP"}`),
			ToolCalls: []openaiapi.ToolCall{{
				Function: openaiapi.ToolCallFunction{Arguments: `{"token":"sk-abcdefghijklmnopqrstuvwxyz"}`},
			}},
		}}},
	}
	response = engine.SanitizeOutboundChat(response)
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	got := string(encoded)
	if strings.Contains(got, "AKIAABCDEFGHIJKLMNOP") ||
		strings.Contains(got, "sk-abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("secret leaked: %s", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("unexpected sanitization: %s", got)
	}
}

func TestOutboundPrivateKeyFailsClosedAndCompatibilityWrapperReturnsNoMaterial(t *testing.T) {
	engine := NewDefault()
	privateMaterial := "-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----"
	response := openaiapi.ChatCompletionResponse{
		Choices: []openaiapi.Choice{{Message: &openaiapi.Message{
			Role: "assistant", Content: openaiapi.TextContent(privateMaterial),
		}}},
	}
	if _, err := engine.ProcessOutboundChat("", response); !errors.Is(err, ErrPolicyRejected) {
		t.Fatalf("private key output was not rejected: %v", err)
	}
	safe := engine.SanitizeOutboundChat(response)
	encoded, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-material") ||
		!strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("compatibility wrapper returned private material: %s", encoded)
	}
}
