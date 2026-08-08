package redaction

import (
	"errors"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
)

func TestRollingStreamMasksMatchSplitAcrossEveryChunkAndFlushesBeforeFinish(t *testing.T) {
	engine := streamTestEngine(t, domain.RedactionRule{
		ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
		Scopes: []string{"outbound"}, Action: "mask", Enabled: true,
	})
	stream, err := engine.NewStream("policy")
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for _, runeValue := range []rune("联系13800138000完成") {
		chunks, err := stream.Process(textDelta(string(runeValue), nil))
		if err != nil {
			t.Fatal(err)
		}
		appendStreamText(&output, chunks)
	}
	finish := "stop"
	chunks, err := stream.Process(textDelta("", &finish))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[0].Choices[0].FinishReason != nil ||
		chunks[1].Choices[0].FinishReason == nil {
		t.Fatalf("protected suffix was not flushed before finish: %#v", chunks)
	}
	appendStreamText(&output, chunks)
	if got := output.String(); got != "联系••••8000完成" {
		t.Fatalf("unexpected rolling output %q", got)
	}
}

func TestRollingStreamReplacesMandatorySecretAcrossChunks(t *testing.T) {
	engine := NewDefault()
	stream, err := engine.NewStream("")
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for _, piece := range []string{"prefix ", "sk-abcdefghijkl", "mnopqrstuv", " suffix"} {
		chunks, err := stream.Process(textDelta(piece, nil))
		if err != nil {
			t.Fatal(err)
		}
		appendStreamText(&output, chunks)
	}
	chunks, err := stream.Flush()
	if err != nil {
		t.Fatal(err)
	}
	appendStreamText(&output, chunks)
	if got := output.String(); got != "prefix [REDACTED] suffix" {
		t.Fatalf("mandatory secret leaked or output changed: %q", got)
	}
}

func TestRollingStreamRejectsUnicodeDictionaryAcrossChunks(t *testing.T) {
	engine := streamTestEngine(t, domain.RedactionRule{
		ID: "deny", Name: "Deny", Kind: "dictionary", Dictionary: []string{"高度机密"},
		Scopes: []string{"outbound"}, Action: "reject", Enabled: true,
	})
	stream, err := engine.NewStream("policy")
	if err != nil {
		t.Fatal(err)
	}
	for _, piece := range []string{"这是高", "度", "机", "密内容"} {
		_, err = stream.Process(textDelta(piece, nil))
		if err != nil {
			break
		}
	}
	if err == nil {
		_, err = stream.Flush()
	}
	if !errors.Is(err, ErrPolicyRejected) {
		t.Fatalf("split Unicode dictionary value was not rejected: %v", err)
	}
}

func TestRollingStreamRejectsTransformInParallelToolArgumentFragments(t *testing.T) {
	engine := streamTestEngine(t, domain.RedactionRule{
		ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
		Scopes: []string{"outbound"}, Action: "replace", Replacement: "[PHONE]", Enabled: true,
	})
	stream, err := engine.NewStream("policy")
	if err != nil {
		t.Fatal(err)
	}
	zero, one := 0, 1
	for _, chunk := range []openaiapi.ChatCompletionResponse{
		toolDelta(&zero, `{"phone":"13800`),
		toolDelta(&one, `{"phone":"13900`),
		toolDelta(&zero, `138000"}`),
		toolDelta(&one, `139000"}`),
	} {
		_, err = stream.Process(chunk)
		if err != nil {
			break
		}
	}
	if err == nil {
		_, err = stream.Flush()
	}
	if !errors.Is(err, ErrPolicyRejected) {
		t.Fatalf("streaming tool argument transform did not fail closed: %v", err)
	}
}

func TestRollingStreamHasHardChunkLimit(t *testing.T) {
	stream, err := NewDefault().NewStream("")
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.Process(textDelta(strings.Repeat("x", maxStreamChunkBytes+1), nil))
	if err == nil || !strings.Contains(err.Error(), "chunk exceeds") {
		t.Fatalf("oversized stream chunk was accepted: %v", err)
	}
}

func TestRollingStreamRedactsReasoningAcrossChunks(t *testing.T) {
	engine := NewDefault()
	stream, err := engine.NewStream("")
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for _, piece := range []string{"sk-proj-abc", "defghijklmnop"} {
		chunks, processErr := stream.Process(openaiapi.ChatCompletionResponse{
			ID: "chunk", Object: "chat.completion.chunk", Model: "provider",
			Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{ReasoningContent: piece}}},
		})
		if processErr != nil {
			t.Fatal(processErr)
		}
		appendReasoning(&output, chunks)
	}
	chunks, err := stream.Flush()
	if err != nil {
		t.Fatal(err)
	}
	appendReasoning(&output, chunks)
	if strings.Contains(output.String(), "sk-proj-") || !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("reasoning secret was not safely redacted: %q", output.String())
	}
}

func appendReasoning(output *strings.Builder, chunks []openaiapi.ChatCompletionResponse) {
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			if choice.Delta != nil {
				output.WriteString(choice.Delta.ReasoningContent)
			}
		}
	}
}

func TestDetectOnlyStreamRejectsEnforcingCustomRule(t *testing.T) {
	_, err := CompilePolicy(domain.RedactionPolicy{
		ID: "policy", Name: "Policy", Enabled: true, Mode: "detect_only_stream",
		Rules: []domain.RedactionRule{{
			ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
			Scopes: []string{"outbound"}, Action: "mask", Enabled: true,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "must use detect_only") {
		t.Fatalf("detect-only stream accepted enforcing rule: %v", err)
	}
}

func streamTestEngine(t *testing.T, rule domain.RedactionRule) *Engine {
	t.Helper()
	engine, err := New([]domain.RedactionPolicy{{
		ID: "policy", Name: "Policy", Enabled: true, Mode: "bounded_stream",
		Rules: []domain.RedactionRule{rule},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func textDelta(value string, finish *string) openaiapi.ChatCompletionResponse {
	message := &openaiapi.Message{}
	if value != "" {
		message.Content = openaiapi.TextContent(value)
	}
	return openaiapi.ChatCompletionResponse{
		ID: "chunk", Object: "chat.completion.chunk", Model: "provider",
		Choices: []openaiapi.Choice{{
			Index: 0, Delta: message, FinishReason: finish,
		}},
	}
}

func toolDelta(index *int, arguments string) openaiapi.ChatCompletionResponse {
	return openaiapi.ChatCompletionResponse{
		ID: "chunk", Object: "chat.completion.chunk", Model: "provider",
		Choices: []openaiapi.Choice{{
			Index: 0, Delta: &openaiapi.Message{ToolCalls: []openaiapi.ToolCall{{
				Index: index, Type: "function",
				Function: openaiapi.ToolCallFunction{Name: "lookup", Arguments: arguments},
			}}},
		}},
	}
}

func appendStreamText(output *strings.Builder, chunks []openaiapi.ChatCompletionResponse) {
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			if choice.Delta == nil {
				continue
			}
			if value, ok := openaiapi.DecodeTextContent(choice.Delta.Content); ok {
				output.WriteString(value)
			}
		}
	}
}
