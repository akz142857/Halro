package redaction

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestInboundSecretDetectionParsesEscapedJSON(t *testing.T) {
	engine := NewDefault()
	request := semantic.GenerateRequest{
		RequestedModel: "chat",
		Messages: []semantic.Message{{
			Role:    semantic.RoleUser,
			Content: []semantic.Content{{Kind: semantic.ContentText, Text: "sk-abcdefghijklmnop\u0051RST"}},
		}},
	}
	if _, err := engine.ProcessInboundGenerate("", request); !errors.Is(err, ErrSecretDetected) {
		t.Fatalf("escaped secret was not detected: %v", err)
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

// A Project whose policy is missing from the live snapshot must not be served
// as though it had no rules. The failure this pins down was observed end to
// end: with the snapshot behind the store, ProcessText returned the prompt
// unchanged — "here is SECRET-12345 in a prompt" went to the provider with
// none of the Project's rules having run, and the call reported success.
func TestAMissingPolicyRefusesInsteadOfPassingTheTextThrough(t *testing.T) {
	engine := NewDefault()
	const prompt = "here is SECRET-12345 in a prompt"

	out, err := engine.ProcessText("redp_not_in_this_snapshot", "inbound", prompt)
	if !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("named-but-absent policy: err=%v out=%q", err, out)
	}

	// An absent policy is not a match, so it must not be reported as one: the
	// two mean different things to the caller and to the audit record.
	if errors.Is(err, ErrPolicyRejected) {
		t.Fatal("an absent policy was reported as a rule rejection")
	}

	// The empty ID is a decision, not a miss: a Project with no policy keeps
	// working, and the mandatory built-in rules still run.
	if out, err := engine.ProcessText("", "inbound", prompt); err != nil || out != prompt {
		t.Fatalf("no-policy project: out=%q err=%v", out, err)
	}
}
