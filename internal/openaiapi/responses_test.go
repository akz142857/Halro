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
		`{"model":"route","input":"hello","background":true,"stream":true}`,
		`{"model":"route","input":"hello","reasoning":{"effort":"high"}}`,
		// A provider-executed tool is named, not described: the caller is not
		// declaring a function, so function fields on it are a caller who thinks
		// they are constraining something they are not.
		`{"model":"route","input":"hello","tools":[{"type":"web_search","name":"search"}]}`,
		`{"model":"route","input":"hello","tools":[{"type":"web_search","parameters":{"type":"object"}}]}`,
		`{"model":"route","input":"hello","tools":[{"type":"code_interpreter"}]}`,
		`{"model":"route","input":"hello","tools":[{"type":"file_search"}]}`,
		`{"model":"route","input":"hello","tools":[{"type":"function","name":"f","strict":true}],"stream":true}`,
	}
	for _, body := range tests {
		if _, err := DecodeResponseRequest(json.NewDecoder(strings.NewReader(body))); err == nil {
			t.Fatalf("accepted unsafe request: %s", body)
		}
	}
}

// background is the one field of the stateful set that this endpoint now
// accepts, and it does not drag the rest in with it: the answer is collected
// later, but nothing about the request is remembered as conversation.
func TestDecodeResponseRequestAcceptsBackgroundWithoutStore(t *testing.T) {
	request, err := DecodeResponseRequest(json.NewDecoder(strings.NewReader(
		`{"model":"route","input":"hello","background":true}`)))
	if err != nil {
		t.Fatal(err)
	}
	if !request.Background {
		t.Fatalf("background was dropped: %#v", request)
	}
	if _, err := DecodeResponseRequest(json.NewDecoder(strings.NewReader(
		`{"model":"route","input":"hello","background":true,"store":true}`))); err == nil {
		t.Fatal("store=true rode in on background")
	}
}

// web_search is accepted and nothing else hosted is. Whether a given deployment
// may actually run it is a routing decision made later, against the
// provider_executed_tools capability its operator turned on.
func TestDecodeResponseRequestAcceptsWebSearchAlone(t *testing.T) {
	request, err := DecodeResponseRequest(json.NewDecoder(strings.NewReader(
		`{"model":"route","input":"hello","tools":[{"type":"web_search"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Tools) != 1 || request.Tools[0].Type != ProviderExecutedToolWebSearch {
		t.Fatalf("unexpected tools: %#v", request.Tools)
	}
}

func TestDecodeResponseRequestRejectsMultipleValues(t *testing.T) {
	if _, err := DecodeResponseRequest(json.NewDecoder(strings.NewReader(`{"model":"route","input":"hello"} {}`))); err == nil {
		t.Fatal("accepted multiple JSON values")
	}
}
