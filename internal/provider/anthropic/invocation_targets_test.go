package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// The capability objects and field names below are the ones GET /v1/models
// actually documents: every capability member is a `{"supported": bool}` object,
// the context window is `max_input_tokens`, and the output ceiling is
// `max_tokens` — named after the request parameter it bounds. Fixtures invented
// from memory here would only prove the decoder matches the memory.
func TestListInvocationTargetsPaginatesAndKeepsOnlyAllowlistedMetadata(t *testing.T) {
	requests := 0
	adapter := newTestAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/v1/models" || request.URL.Query().Get("limit") != "1000" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("x-api-key") != "provider-secret" {
			t.Errorf("missing provider authorization: %#v", request.Header)
		}
		writer.Header().Set("content-type", "application/json")
		switch request.URL.Query().Get("after_id") {
		case "":
			_, _ = writer.Write([]byte(`{
				"data":[{"id":"claude-a","display_name":"Claude A","type":"model",
					"capabilities":{"image_input":{"supported":true},"pdf_input":{"supported":true},
						"thinking":{"supported":true,"types":{"adaptive":{"supported":true}}},
						"future_superpower":{"supported":true}},
					"max_input_tokens":200000,"max_tokens":8192,"future_limit":999}],
				"has_more":true,"first_id":"claude-a","last_id":"claude-a"
			}`))
		case "claude-a":
			_, _ = writer.Write([]byte(`{
				"data":[{"id":"claude-b","display_name":"Claude B","type":"model","future_field":{"supported":true}}],
				"has_more":false
			}`))
		default:
			t.Errorf("unexpected pagination cursor %q", request.URL.Query().Get("after_id"))
			writer.WriteHeader(http.StatusBadRequest)
		}
	})

	targets, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(targets) != 2 {
		t.Fatalf("requests=%d targets=%#v", requests, targets)
	}
	first := targets[0]
	if first.TargetID != "claude-a" || first.DisplayName != "Claude A" || first.Metadata.MaxContextTokens != 200000 || first.Metadata.MaxOutputTokens != 8192 {
		t.Fatalf("first target=%#v", first)
	}
	if len(first.Metadata.SupportedOperations) != 3 || first.Metadata.SupportedOperations[0] != "image_input" ||
		first.Metadata.SupportedOperations[1] != "pdf_input" || first.Metadata.SupportedOperations[2] != "thinking" {
		t.Fatalf("allowlisted operations=%v", first.Metadata.SupportedOperations)
	}
	second := targets[1]
	if second.TargetID != "claude-b" || len(second.Metadata.SupportedOperations) != 0 || second.Metadata.MaxContextTokens != 0 || second.Metadata.MaxOutputTokens != 0 {
		t.Fatalf("missing fields did not remain unknown: %#v", second)
	}
}

func TestDescribeInvocationTargetReturnsExactListedDescriptor(t *testing.T) {
	adapter := newTestAdapter(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" || request.URL.Query().Get("after_id") != "" {
			t.Errorf("unexpected description request: %s", request.URL)
		}
		_, _ = writer.Write([]byte(`{
			"data":[{"id":"claude-described","display_name":"Claude Described","type":"model",
				"capabilities":{"batch":{"supported":true},"image_input":{"supported":true}},
				"max_input_tokens":150000,"max_tokens":4096}],
			"has_more":false
		}`))
	})
	target, err := adapter.DescribeInvocationTarget(context.Background(), domain.InvocationTargetDescriptor{TargetID: "claude-described", TargetKind: domain.TargetModelID})
	if err != nil {
		t.Fatal(err)
	}
	if target.TargetID != "claude-described" || target.TargetKind != domain.TargetModelID || target.Metadata.MaxContextTokens != 150000 || target.Metadata.MaxOutputTokens != 4096 || len(target.Metadata.SupportedOperations) != 2 {
		t.Fatalf("target=%#v", target)
	}
}

func TestAnthropicCapabilityMetadataIgnoresUnknownAndUnsupportedFields(t *testing.T) {
	metadata := map[string]json.RawMessage{
		"batch":              json.RawMessage(`{"supported":true}`),
		"image_input":        json.RawMessage(`{"supported":false}`),
		"effort":             json.RawMessage(`{"supported":true,"high":{"supported":true}}`),
		"future_superpower":  json.RawMessage(`{"supported":true}`),
		"structured_outputs": json.RawMessage(`{"supported":true}`),
	}
	operations := allowlistedAnthropicCapabilities(metadata)
	if len(operations) != 3 || operations[0] != "batch" || operations[1] != "effort" || operations[2] != "structured_outputs" {
		t.Fatalf("operations=%v", operations)
	}
	target := domain.InvocationTargetDescriptor{TargetID: "claude-future", Metadata: domain.NormalizedModelMetadata{SupportedOperations: operations}}
	scope := domain.InvocationTargetScopeKey{ProviderID: "provider", TargetKind: domain.TargetModelID, TargetID: target.TargetID, BindingID: "binding", ProfileID: domain.ProfileAnthropicMessages}
	claims := (&Adapter{}).MapCapabilityClaims(target, scope, time.Now().UTC())
	// chat and streaming come from the endpoint rather than from a flag, and
	// effort maps onto no Halro capability, so it contributes no claim.
	if len(claims) != 4 || claims[0].CapabilityID != "chat" || claims[1].CapabilityID != "streaming" ||
		claims[2].CapabilityID != "batches" || claims[3].CapabilityID != "structured_outputs" {
		t.Fatalf("claims=%#v", claims)
	}
}

// A capability the upstream reports as unsupported must not survive as a claim,
// which is the failure the `{"supported": false}` shape invites: the member is
// present, and a decoder that only checks presence reads it as a yes.
func TestAnthropicCapabilityClaimsDropUnsupportedThinking(t *testing.T) {
	operations := allowlistedAnthropicCapabilities(map[string]json.RawMessage{
		"thinking":    json.RawMessage(`{"supported":false,"types":{"enabled":{"supported":true}}}`),
		"image_input": json.RawMessage(`{"supported":true}`),
	})
	if len(operations) != 1 || operations[0] != "image_input" {
		t.Fatalf("operations=%v", operations)
	}
	target := domain.InvocationTargetDescriptor{TargetID: "claude-no-thinking", Metadata: domain.NormalizedModelMetadata{SupportedOperations: operations}}
	claims := (&Adapter{}).MapCapabilityClaims(target, domain.InvocationTargetScopeKey{TargetID: target.TargetID}, time.Now().UTC())
	for _, claim := range claims {
		if claim.CapabilityID == "reasoning" {
			t.Fatalf("unsupported thinking became a claim: %#v", claims)
		}
	}
}
