package openaiapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeChatRequestStrict(t *testing.T) {
	decoder := json.NewDecoder(strings.NewReader(`{
		"model":"chat",
		"messages":[{"role":"user","content":"hello"}],
		"max_completion_tokens":128
	}`))
	request, err := DecodeChatCompletionRequest(decoder)
	if err != nil {
		t.Fatal(err)
	}
	if request.Model != "chat" || len(request.Messages) != 1 {
		t.Fatalf("unexpected request: %#v", request)
	}
}

func TestDecodeChatRequestRejectsUnknownField(t *testing.T) {
	decoder := json.NewDecoder(strings.NewReader(`{
		"model":"chat",
		"messages":[{"role":"user","content":"hello"}],
		"provider_secret":"no"
	}`))
	if _, err := DecodeChatCompletionRequest(decoder); err == nil {
		t.Fatal("unknown fields must be rejected")
	}
}

func TestDecodeChatRequestAcceptsReasoningAndJSONSchema(t *testing.T) {
	decoder := json.NewDecoder(strings.NewReader(`{
		"model":"chat",
		"messages":[{"role":"developer","content":"be precise"},{"role":"user","content":"hello"}],
		"reasoning_effort":"high",
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}
	}`))
	request, err := DecodeChatCompletionRequest(decoder)
	if err != nil {
		t.Fatal(err)
	}
	if request.ReasoningEffort != "high" || len(request.ResponseFormat) == 0 {
		t.Fatalf("unexpected request: %#v", request)
	}
}

func TestDecodeChatRequestRejectsInvalidSemanticOptions(t *testing.T) {
	for name, body := range map[string]string{
		"reasoning": `{"model":"chat","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"extreme"}`,
		"format":    `{"model":"chat","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"yaml"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeChatCompletionRequest(json.NewDecoder(strings.NewReader(body))); err == nil {
				t.Fatal("invalid semantic option was accepted")
			}
		})
	}
}
