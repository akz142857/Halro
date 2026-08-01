package openaiapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeResponseRequestAcceptsStatelessPortableSubset(t *testing.T) {
	request, err := DecodeResponseRequest(json.NewDecoder(strings.NewReader(`{
		"model":"route","input":"hello","instructions":"be concise","store":false,
		"max_output_tokens":20,"temperature":0.2,"text":{"format":{"type":"json_object"}}
	}`)))
	if err != nil {
		t.Fatal(err)
	}
	if request.Model != "route" || request.Store == nil || *request.Store || request.MaxOutputTokens == nil || *request.MaxOutputTokens != 20 {
		t.Fatalf("unexpected request: %#v", request)
	}
}

func TestDecodeResponseRequestRejectsStatefulUnknownAndLossyFields(t *testing.T) {
	tests := []string{
		`{"model":"route","input":"hello","store":true}`,
		`{"model":"route","input":"hello","previous_response_id":"resp_1"}`,
		`{"model":"route","input":"hello","conversation":"conv_1"}`,
		`{"model":"route","input":"hello","background":true}`,
		`{"model":"route","input":"hello","reasoning":{"effort":"high"}}`,
		`{"model":"route","input":"hello","tools":[{"type":"web_search"}]}`,
		`{"model":"route","input":"hello","tools":[{"type":"function","name":"f","strict":true}],"stream":true}`,
	}
	for _, body := range tests {
		if _, err := DecodeResponseRequest(json.NewDecoder(strings.NewReader(body))); err == nil {
			t.Fatalf("accepted unsafe request: %s", body)
		}
	}
}

func TestDecodeResponseRequestRejectsMultipleValues(t *testing.T) {
	if _, err := DecodeResponseRequest(json.NewDecoder(strings.NewReader(`{"model":"route","input":"hello"} {}`))); err == nil {
		t.Fatal("accepted multiple JSON values")
	}
}
