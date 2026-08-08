package redaction

import (
	"errors"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
)

func FuzzBoundedStreamMatchesNonStream(f *testing.F) {
	for _, seed := range []struct {
		value string
		size  uint8
	}{
		{"ordinary response", 1},
		{"call 13800138000 now", 2},
		{"prefix sk-abcdefghijklmnopqrstuvwxyz suffix", 3},
		{"Unicode 高度机密内容", 4},
		{"4111111111111112", 5},
		{"-----BEGIN PRIVATE KEY-----\nmaterial", 7},
	} {
		f.Add(seed.value, seed.size)
	}
	f.Fuzz(func(t *testing.T, raw string, rawChunkSize uint8) {
		if len(raw) > 8<<10 {
			t.Skip()
		}
		value := string([]rune(raw))
		engine, err := New([]domain.RedactionPolicy{{
			ID: "fuzz", Name: "Fuzz", Enabled: true, Mode: "bounded_stream",
			Rules: []domain.RedactionRule{{
				ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
				Scopes: []string{"outbound"}, Action: "mask", Enabled: true,
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		full, fullErr := engine.ProcessOutboundChat("fuzz", textResponse(value))

		stream, err := engine.NewStream("fuzz")
		if err != nil {
			t.Fatal(err)
		}
		runes := []rune(value)
		chunkSize := int(rawChunkSize%32) + 1
		var streamed strings.Builder
		var streamErr error
		for start := 0; start < len(runes); start += chunkSize {
			end := min(start+chunkSize, len(runes))
			chunks, err := stream.Process(textDelta(string(runes[start:end]), nil))
			if err != nil {
				streamErr = err
				break
			}
			appendStreamText(&streamed, chunks)
		}
		if streamErr == nil {
			chunks, err := stream.Flush()
			streamErr = err
			appendStreamText(&streamed, chunks)
		}
		if errors.Is(fullErr, ErrPolicyRejected) != errors.Is(streamErr, ErrPolicyRejected) {
			t.Fatalf("error mismatch full=%v stream=%v input=%q", fullErr, streamErr, value)
		}
		if fullErr == nil {
			expected, ok := openaiapi.DecodeTextContent(full.Choices[0].Message.Content)
			if !ok {
				t.Fatal("full response is not text")
			}
			if streamed.String() != expected {
				t.Fatalf("stream mismatch got=%q want=%q input=%q", streamed.String(), expected, value)
			}
		}
	})
}

func FuzzRuleCompilerNeverPanics(f *testing.F) {
	for _, pattern := range []string{`[0-9]{3,8}`, `(?:foo|bar)+`, `.*`, `(?i)secret`} {
		f.Add(pattern)
	}
	f.Fuzz(func(t *testing.T, pattern string) {
		if len(pattern) == 0 || len(pattern) > 2048 {
			t.Skip()
		}
		_, _ = CompilePolicy(domain.RedactionPolicy{
			ID: "fuzz", Name: "Fuzz", Enabled: true, Mode: "strict",
			Rules: []domain.RedactionRule{{
				ID: "regex", Name: "Regex", Kind: "regex", Pattern: pattern,
				Scopes: []string{"outbound"}, Action: "detect_only", Enabled: true,
			}},
		})
	})
}

func BenchmarkStandardRedaction(b *testing.B) {
	engine, err := New([]domain.RedactionPolicy{{
		ID: "bench", Name: "Bench", Enabled: true, Mode: "strict",
		Rules: []domain.RedactionRule{
			{
				ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
				Scopes: []string{"outbound"}, Action: "mask", Enabled: true,
			},
			{
				ID: "terms", Name: "Terms", Kind: "dictionary",
				Dictionary: []string{"internal", "confidential"},
				Scopes:     []string{"outbound"}, Action: "replace", Replacement: "[TERM]", Enabled: true,
			},
		},
	}})
	if err != nil {
		b.Fatal(err)
	}
	value := strings.Repeat("ordinary text ", 64) + "call 13800138000 internal"
	b.ReportAllocs()
	b.SetBytes(int64(len(value)))
	b.ResetTimer()
	for range b.N {
		if _, err := engine.ProcessOutboundChat("bench", textResponse(value)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRollingRedaction(b *testing.B) {
	engine, err := New([]domain.RedactionPolicy{{
		ID: "bench", Name: "Bench", Enabled: true, Mode: "bounded_stream",
		Rules: []domain.RedactionRule{{
			ID: "phone", Name: "Phone", Kind: "builtin", Builtin: "china_phone",
			Scopes: []string{"outbound"}, Action: "mask", Enabled: true,
		}},
	}})
	if err != nil {
		b.Fatal(err)
	}
	chunks := []string{
		strings.Repeat("ordinary text ", 16), "call 138", "0013", "8000 now",
	}
	totalBytes := 0
	for _, chunk := range chunks {
		totalBytes += len(chunk)
	}
	b.ReportAllocs()
	b.SetBytes(int64(totalBytes))
	b.ResetTimer()
	for range b.N {
		stream, err := engine.NewStream("bench")
		if err != nil {
			b.Fatal(err)
		}
		for _, chunk := range chunks {
			if _, err := stream.Process(textDelta(chunk, nil)); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := stream.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}

func textResponse(value string) openaiapi.ChatCompletionResponse {
	return openaiapi.ChatCompletionResponse{
		ID: "chunk", Object: "chat.completion.chunk",
		Choices: []openaiapi.Choice{{Index: 0, Message: &openaiapi.Message{
			Role: "assistant", Content: openaiapi.TextContent(value),
		}}},
	}
}
