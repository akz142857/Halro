package openaiapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzDecodeRequestsNeverPanic feeds arbitrary bytes to the decoders that stand
// at the gateway's front door. They are the first thing an authenticated
// caller's body touches, and a decoded request feeds the budget and TPM
// estimates directly — so the properties worth pinning are that decoding either
// succeeds or errors, and that anything it accepts produces an estimate the
// accounting layer can use: non-negative, and never a value that only appears
// because the decode half-succeeded.
func FuzzDecodeRequestsNeverPanic(f *testing.F) {
	for _, seed := range []string{
		`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"chat","messages":[{"role":"user","content":["array","content"]}]}`,
		`{"model":"chat","messages":[],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`,
		`{"model":"embed","input":"text"}`,
		`{"model":"embed","input":["a","b"]}`,
		`{"model":"resp","input":"hello"}`,
		`{"model":"chat","messages":[{"role":"user","content":"x"}],"stream":true}`,
		`{"model":"chat","messages":[{"role":"user","content":"x"}]} {"trailing":true}`,
		`{"model":"chat","unknown_field":1}`,
		`{"model":"chat","messages":[{"role":"user","content":"x"}],"max_tokens":99999999999999999999}`,
		`[]`,
		``,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 64<<10 {
			t.Skip()
		}
		if chat, err := DecodeChatCompletionRequest(json.NewDecoder(strings.NewReader(raw))); err == nil {
			if estimate := chat.EstimatedInputBytes(); estimate < 0 {
				t.Fatalf("negative input estimate %d from %q", estimate, raw)
			}
		}
		if embedding, err := DecodeEmbeddingRequest(json.NewDecoder(strings.NewReader(raw))); err == nil {
			if estimate := embedding.EstimatedInputTokens(); estimate < 1 {
				t.Fatalf("embedding estimate %d is below the one-token floor: %q", estimate, raw)
			}
		}
		if response, err := DecodeResponseRequest(json.NewDecoder(strings.NewReader(raw))); err == nil {
			if estimate := response.EstimatedInputBytes(); estimate < 0 {
				t.Fatalf("negative response input estimate %d from %q", estimate, raw)
			}
		}
	})
}

// FuzzDecodePhase2RequestsNeverPanic covers the strict decoders behind the
// non-chat endpoints. They share the reject-unknown-fields, reject-trailing
// contract and had no test file of their own.
func FuzzDecodePhase2RequestsNeverPanic(f *testing.F) {
	for _, seed := range []string{
		`{"model":"mod","input":"text"}`,
		`{"model":"img","prompt":"a cat","n":1}`,
		`{"model":"tts","input":"hello","voice":"alloy"}`,
		`{"model":"rerank","query":"q","documents":["a"]}`,
		`{"model":"async","input":{}}`,
		`{"input_file_id":"file_1","endpoint":"/v1/chat/completions","completion_window":"24h"}`,
		`{"model":123}`,
		`{`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 64<<10 {
			t.Skip()
		}
		_, _ = DecodeModerationRequest(json.NewDecoder(strings.NewReader(raw)))
		_, _ = DecodeImageGenerationRequest(json.NewDecoder(strings.NewReader(raw)))
		_, _ = DecodeSpeechRequest(json.NewDecoder(strings.NewReader(raw)))
		_, _ = DecodeRerankRequest(json.NewDecoder(strings.NewReader(raw)))
		_, _ = DecodeAsyncInvokeRequest(json.NewDecoder(strings.NewReader(raw)))
		_, _ = DecodeBatchCreateRequest(json.NewDecoder(strings.NewReader(raw)))
	})
}
