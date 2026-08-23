package redaction

import (
	"encoding/json"
	"errors"
	"fmt"
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

func TestRollingStreamKeepsByteOrderWhenTerminalChunkCarriesContent(t *testing.T) {
	stream, err := NewDefault().NewStream("")
	if err != nil {
		t.Fatal(err)
	}
	// The mandatory rules alone hold back a couple of kilobytes, so both halves
	// have to be larger than that for the flush to overlap the terminal chunk.
	whole := countedText(1000)
	first, second := whole[:len(whole)/2], whole[len(whole)/2:]
	finish := "stop"
	var output strings.Builder
	for _, chunk := range []openaiapi.ChatCompletionResponse{
		textDelta(first, nil), textDelta(second, &finish),
	} {
		chunks, processErr := stream.Process(chunk)
		if processErr != nil {
			t.Fatal(processErr)
		}
		for index, safe := range chunks {
			if safe.Choices[0].FinishReason != nil && index != len(chunks)-1 {
				t.Fatalf("terminal marker was not the last chunk: %#v", chunks)
			}
		}
		appendStreamText(&output, chunks)
	}
	if got := output.String(); got != whole {
		t.Fatalf("stream text was reordered: diverges from the provider text at byte %d",
			divergence(got, whole))
	}
}

// countedText builds a string whose every ten bytes name their own position, so
// a transposition of two equally sized runs cannot hide.
func countedText(segments int) string {
	var value strings.Builder
	for index := range segments {
		fmt.Fprintf(&value, "%09d ", index)
	}
	return value.String()
}

func divergence(got, want string) int {
	for index := range min(len(got), len(want)) {
		if got[index] != want[index] {
			return index
		}
	}
	return min(len(got), len(want))
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

// A tool call's arguments reach the client as a JSON document it decodes, so an
// escaped character inside a secret defeats every pattern in the raw fragment
// and is reconstituted downstream. The unary path decodes before matching and
// catches it; the streaming path has to reach the same verdict.
func TestRollingStreamRejectsEscapedSecretThatUnaryPathRedacts(t *testing.T) {
	const arguments = `{"note":"sk-ant-\u0061pi03aaaaaaaaaaaaaaaa"}`
	var reconstituted struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(arguments), &reconstituted); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reconstituted.Note, "sk-ant-api03") {
		t.Fatalf("fixture does not reconstitute a secret: %q", reconstituted.Note)
	}
	unary := NewDefault().SanitizeOutboundChat(toolMessage(arguments))
	if got := unary.Choices[0].Message.ToolCalls[0].Function.Arguments; strings.Contains(got, "sk-ant-") {
		t.Fatalf("unary baseline no longer redacts the escaped secret: %q", got)
	}
	// Split mid-escape so the incremental decoder has to carry the partial
	// sequence across fragments.
	zero := 0
	stream, err := NewDefault().NewStream("")
	if err != nil {
		t.Fatal(err)
	}
	for _, piece := range []string{`{"note":"sk-ant-\u00`, `61pi03aaaaaaaaaaaaaaaa"}`} {
		if _, err = stream.Process(toolDelta(&zero, piece)); err != nil {
			break
		}
	}
	if err == nil {
		_, err = stream.Flush()
	}
	if !errors.Is(err, ErrPolicyRejected) {
		t.Fatalf("escaped secret passed the streaming mandatory baseline: %v", err)
	}
}

func TestRollingStreamPassesToolArgumentsWithOrdinaryEscapes(t *testing.T) {
	zero := 0
	stream, err := NewDefault().NewStream("")
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := stream.Process(toolDelta(&zero, `{"note":"line\none\ttwo \"quoted\" 中文 😀"}`))
	if err != nil {
		t.Fatal(err)
	}
	flushed, err := stream.Flush()
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for _, chunk := range append(chunks, flushed...) {
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				output.WriteString(call.Function.Arguments)
			}
		}
	}
	if got := output.String(); got != `{"note":"line\none\ttwo \"quoted\" 中文 😀"}` {
		t.Fatalf("escaped tool arguments were altered: %q", got)
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

func toolMessage(arguments string) openaiapi.ChatCompletionResponse {
	return openaiapi.ChatCompletionResponse{
		ID: "completion", Object: "chat.completion", Model: "provider",
		Choices: []openaiapi.Choice{{
			Index: 0, Message: &openaiapi.Message{Role: "assistant", ToolCalls: []openaiapi.ToolCall{{
				ID: "call_1", Type: "function",
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
