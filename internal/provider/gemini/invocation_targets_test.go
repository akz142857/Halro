package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func TestListInvocationTargetsPaginatesAndPreservesReviewedModelFields(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/v1beta/models" || request.URL.Query().Get("pageSize") != "1000" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("x-goog-api-key") != "secret-key" {
			t.Errorf("missing provider authorization: %#v", request.Header)
		}
		writer.Header().Set("content-type", "application/json")
		switch request.URL.Query().Get("pageToken") {
		case "":
			_, _ = writer.Write([]byte(`{
				"models":[{"name":"models/gemini-a","displayName":"Gemini A",
					"inputTokenLimit":1048576,"outputTokenLimit":65536,
					"supportedGenerationMethods":["generateContent","streamGenerateContent","futureVisionMagic"],
					"futureCapability":true}],
				"nextPageToken":"page-2"
			}`))
		case "page-2":
			_, _ = writer.Write([]byte(`{"models":[{"name":"models/gemini-b","displayName":"Gemini B","futureField":{"enabled":true}}]}`))
		default:
			t.Errorf("unexpected page token %q", request.URL.Query().Get("pageToken"))
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	adapter := testAdapter(t, server.URL)
	defer adapter.Close()

	targets, err := adapter.ListInvocationTargets(context.Background(), domain.TargetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(targets) != 2 {
		t.Fatalf("requests=%d targets=%#v", requests, targets)
	}
	first := targets[0]
	if first.TargetID != "gemini-a" || first.DisplayName != "Gemini A" || first.Metadata.MaxContextTokens != 1048576 || first.Metadata.MaxOutputTokens != 65536 || len(first.Metadata.SupportedOperations) != 3 {
		t.Fatalf("first target=%#v", first)
	}
	claims := adapter.MapCapabilityClaims(first, domain.InvocationTargetScopeKey{
		ProviderID: "provider", TargetKind: domain.TargetModelID, TargetID: first.TargetID,
		BindingID: "binding", ProfileID: domain.ProfileGeminiText,
	}, time.Now().UTC())
	if len(claims) != 2 || claims[0].CapabilityID != "chat" || claims[1].CapabilityID != "streaming" {
		t.Fatalf("unknown method produced capability claims: %#v", claims)
	}
	second := targets[1]
	if second.TargetID != "gemini-b" || len(second.Metadata.SupportedOperations) != 0 || second.Metadata.MaxContextTokens != 0 || second.Metadata.MaxOutputTokens != 0 {
		t.Fatalf("missing fields did not remain unknown: %#v", second)
	}
}

func TestDescribeInvocationTargetUsesExactModelResource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1beta/models/gemini-described" {
			t.Errorf("unexpected description request: %s %s", request.Method, request.URL)
		}
		_, _ = writer.Write([]byte(`{
			"name":"models/gemini-described","displayName":"Gemini Described",
			"inputTokenLimit":32768,"outputTokenLimit":8192,
			"supportedGenerationMethods":["generateContent","embedContent"],
			"futureCapability":{"supported":true}
		}`))
	}))
	defer server.Close()
	adapter := testAdapter(t, server.URL)
	defer adapter.Close()

	target, err := adapter.DescribeInvocationTarget(context.Background(), domain.InvocationTargetDescriptor{TargetID: "gemini-described", TargetKind: domain.TargetModelID})
	if err != nil {
		t.Fatal(err)
	}
	if target.TargetID != "gemini-described" || target.DisplayName != "Gemini Described" || target.Metadata.MaxContextTokens != 32768 || target.Metadata.MaxOutputTokens != 8192 || len(target.Metadata.SupportedOperations) != 2 {
		t.Fatalf("target=%#v", target)
	}
}

func TestMapCapabilityClaimsUsesOnlyReviewedGenerationMethods(t *testing.T) {
	adapter := &Adapter{}
	target := domain.InvocationTargetDescriptor{
		TargetID: "gemini-future",
		Metadata: domain.NormalizedModelMetadata{SupportedOperations: []string{
			"generateContent", "streamGenerateContent", "embedContent", "futureVisionMagic",
		}},
	}
	scope := domain.InvocationTargetScopeKey{
		ProviderID: "provider", TargetKind: domain.TargetModelID, TargetID: target.TargetID,
		BindingID: "binding", ProfileID: domain.ProfileGeminiText,
	}
	claims := adapter.MapCapabilityClaims(target, scope, time.Now().UTC())
	want := map[string]bool{"chat": true, "streaming": true, "embeddings": true}
	if len(claims) != len(want) {
		t.Fatalf("claims=%#v", claims)
	}
	for _, claim := range claims {
		if !want[claim.CapabilityID] || claim.CapabilityID == "vision" || claim.CapabilityID == "tools" {
			t.Fatalf("unreviewed method produced a claim: %#v", claim)
		}
	}
}
