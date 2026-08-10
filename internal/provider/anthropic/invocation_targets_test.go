package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

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
				"data":[{"id":"claude-a","display_name":"Claude A",
					"capabilities":{"messages":true,"tool_use":{"supported":true},"future_superpower":true},
					"max_input_tokens":200000,"max_output_tokens":8192,"future_limit":999}],
				"has_more":true,"last_id":"claude-a"
			}`))
		case "claude-a":
			_, _ = writer.Write([]byte(`{
				"data":[{"id":"claude-b","display_name":"Claude B","future_field":{"enabled":true}}],
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
	if len(first.Metadata.SupportedOperations) != 2 || first.Metadata.SupportedOperations[0] != "messages" || first.Metadata.SupportedOperations[1] != "tool_use" {
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
			"data":[{"id":"claude-described","display_name":"Claude Described",
				"capabilities":{"messages":{"enabled":true},"image_input":true},
				"max_input_tokens":150000,"max_output_tokens":4096}],
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

func TestAnthropicCapabilityMetadataIgnoresUnknownAndFalseFields(t *testing.T) {
	metadata := map[string]json.RawMessage{
		"messages":           json.RawMessage(`true`),
		"streaming":          json.RawMessage(`{"supported":true}`),
		"tool_use":           json.RawMessage(`false`),
		"future_superpower":  json.RawMessage(`true`),
		"structured_outputs": json.RawMessage(`{"enabled":true}`),
	}
	operations := allowlistedAnthropicCapabilities(metadata)
	if len(operations) != 3 || operations[0] != "messages" || operations[1] != "streaming" || operations[2] != "structured_outputs" {
		t.Fatalf("operations=%v", operations)
	}
	target := domain.InvocationTargetDescriptor{TargetID: "claude-future", Metadata: domain.NormalizedModelMetadata{SupportedOperations: operations}}
	scope := domain.InvocationTargetScopeKey{ProviderID: "provider", TargetKind: domain.TargetModelID, TargetID: target.TargetID, BindingID: "binding", ProfileID: domain.ProfileAnthropicMessages}
	claims := (&Adapter{}).MapCapabilityClaims(target, scope, time.Now().UTC())
	if len(claims) != 3 || claims[0].CapabilityID != "chat" || claims[1].CapabilityID != "streaming" || claims[2].CapabilityID != "json_mode" {
		t.Fatalf("claims=%#v", claims)
	}
}
