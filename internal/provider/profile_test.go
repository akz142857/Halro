package provider

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/semantic"
)

func TestBuiltinProfilesAreValidAndReturnedAsCopies(t *testing.T) {
	defaults := []domain.ProviderType{
		domain.ProviderOpenAI, domain.ProviderAzureOpenAI, domain.ProviderDeepSeek,
		domain.ProviderOpenAICompatible, domain.ProviderGemini, domain.ProviderBedrock,
	}
	for _, providerType := range defaults {
		profile, ok := domain.DefaultProviderProfile(providerType)
		if !ok {
			t.Fatalf("missing defaults for %s", providerType)
		}
		manifest, ok := BuiltinProfile(profile.ProfileID)
		if !ok || manifest.Validate() != nil {
			t.Fatalf("invalid manifest for %s: %#v", providerType, manifest)
		}
		manifest.Operations[0] = "mutated"
		fresh, _ := BuiltinProfile(profile.ProfileID)
		if fresh.Operations[0] == "mutated" {
			t.Fatalf("profile %s shared mutable operations", profile.ProfileID)
		}
	}
}

func TestProfileValidationRejectsAnUnregisteredVersion(t *testing.T) {
	manifest, _ := BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	manifest.ID = domain.ProviderProfileID("openai.chat-embeddings.v2")
	if err := manifest.Validate(); err == nil {
		t.Fatal("unregistered profile version was accepted")
	}
}

func TestBedrockMantleProfilesUseDistinctPrimitivesOnOneIsolatedSurface(t *testing.T) {
	cases := map[domain.ProviderProfileID]struct {
		operation Operation
		primitive Primitive
	}{
		domain.ProfileBedrockMantleOpenAIChat:        {OperationChat, PrimitiveBedrockMantleOpenAIChat},
		domain.ProfileBedrockMantleOpenAIResponses:   {OperationChat, PrimitiveBedrockMantleOpenAIResponses},
		domain.ProfileBedrockMantleAnthropicMessages: {OperationMessages, PrimitiveBedrockMantleAnthropicMessages},
	}
	for profileID, expected := range cases {
		manifest, ok := BuiltinProfile(profileID)
		if !ok || manifest.Validate() != nil {
			t.Fatalf("invalid Mantle profile %s: %#v", profileID, manifest)
		}
		if manifest.ProviderType != domain.ProviderBedrock || manifest.AccessSurface != domain.SurfaceBedrockMantle || manifest.CredentialScheme != domain.CredentialBedrockAPIKey {
			t.Fatalf("profile axes collapsed for %s: %#v", profileID, manifest)
		}
		found := false
		for _, binding := range manifest.PrimitiveBindings {
			if binding.LegacyOperation == expected.operation && binding.Primitive == expected.primitive {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s missing %s -> %s", profileID, expected.operation, expected.primitive)
		}
	}
}

func TestBedrockTitanEmbeddingProfileUsesInvokePrimitiveOnly(t *testing.T) {
	manifest, ok := BuiltinProfile(domain.ProfileBedrockInvokeTitanEmbedV2)
	if !ok || manifest.Validate() != nil {
		t.Fatalf("invalid Titan embedding profile: %#v", manifest)
	}
	if manifest.AccessSurface != domain.SurfaceBedrockRuntime || manifest.CredentialScheme != domain.CredentialAWSSigV4Explicit ||
		len(manifest.Operations) != 1 || manifest.Operations[0] != OperationEmbeddings ||
		len(manifest.PrimitiveBindings) != 1 || manifest.PrimitiveBindings[0].Primitive != PrimitiveBedrockInvokeTitanEmbedV2 {
		t.Fatalf("profile axes or primitive are incorrect: %#v", manifest)
	}
}

func TestInferenceResourcesProfilesKeepAccessSurfacesAndPrimitivesIsolated(t *testing.T) {
	cases := []struct {
		id        domain.ProviderProfileID
		surface   domain.AccessSurface
		operation Operation
		primitive Primitive
	}{
		{domain.ProfileOpenAIMediaResources, domain.SurfaceOpenAI, OperationFiles, PrimitiveOpenAIFiles},
		{domain.ProfileBedrockInvokeTitanImageV2, domain.SurfaceBedrockRuntime, OperationImages, PrimitiveBedrockTitanImageV2},
		{domain.ProfileBedrockAgentRerankCohere35, domain.SurfaceBedrockAgentRuntime, OperationRerank, PrimitiveBedrockAgentRerankCohere35},
		{domain.ProfileBedrockAsyncNovaReel, domain.SurfaceBedrockRuntime, OperationAsyncInvoke, PrimitiveBedrockAsyncNovaReel},
	}
	for _, tc := range cases {
		manifest, ok := BuiltinProfile(tc.id)
		if !ok {
			t.Fatalf("missing profile %s", tc.id)
		}
		if err := manifest.Validate(); err != nil {
			t.Fatalf("profile %s: %v", tc.id, err)
		}
		if manifest.AccessSurface != tc.surface {
			t.Fatalf("profile %s surface=%s", tc.id, manifest.AccessSurface)
		}
		found := false
		for _, binding := range manifest.PrimitiveBindings {
			if binding.LegacyOperation == tc.operation && binding.Primitive == tc.primitive {
				found = true
			}
		}
		if !found {
			t.Fatalf("profile %s missing isolated primitive", tc.id)
		}
	}
}

func TestLegacyAdapterBridgeExposesProfileOperationsAndEvidenceCopies(t *testing.T) {
	manifest, _ := BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	evidence := domain.EvidenceForCapabilities(domain.DefaultProviderCapabilities(domain.ProviderOpenAI), domain.EvidenceDeclared)
	bridge, err := NewLegacyAdapterBridge(&typedRegistryAdapter{providerType: "openai"}, manifest, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !bridge.Operations().Supports(OperationChatStream) || !bridge.Operations().Supports(OperationEmbeddings) {
		t.Fatalf("operations=%#v", bridge.Operations().List())
	}
	manifest.Operations[0] = "mutated-input"
	exposed := bridge.Profile()
	exposed.Operations[0] = "mutated-output"
	if !bridge.Operations().Supports(OperationChat) {
		t.Fatal("bridge manifest was mutated through an aliased operations slice")
	}
	copy := bridge.CapabilityEvidence()
	copy["chat"] = domain.EvidenceVerified
	if bridge.CapabilityEvidence()["chat"] != domain.EvidenceDeclared {
		t.Fatal("bridge returned mutable capability evidence")
	}
}

func TestRegistryRejectsTargetWhoseProfileDoesNotMatchAdapter(t *testing.T) {
	manifest, _ := BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	evidence := domain.EvidenceForCapabilities(domain.DefaultProviderCapabilities(domain.ProviderOpenAI), domain.EvidenceDeclared)
	bridge, err := NewLegacyAdapterBridge(&typedRegistryAdapter{providerType: "openai"}, manifest, evidence)
	if err != nil {
		t.Fatal(err)
	}
	err = NewRegistry().Register(Target{
		ID: "route", DeploymentID: "dep_route", PublicModel: "public", ProviderModel: "upstream", Adapter: bridge,
		ProfileID: domain.ProfileGeminiText,
	})
	if err == nil {
		t.Fatal("mismatched target profile was accepted")
	}
}

func TestStaticHeaderAuthorizerClearsConflictingHeadersAndZeroizesOnClose(t *testing.T) {
	authorizer, err := NewStaticHeaderAuthorizer(
		domain.CredentialBearerStatic, "Authorization", "Bearer ", []byte("secret"), "x-api-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, "https://example.com", nil)
	request.Header.Set("x-api-key", "stale")
	if err := authorizer.Authorize(request, nil); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("x-api-key") != "" {
		t.Fatalf("headers=%#v", request.Header)
	}
	authorizer.Close()
	if err := authorizer.Authorize(request, nil); err == nil {
		t.Fatal("closed authorizer accepted a request")
	}
}

func TestStaticHeaderAuthorizerCloseIsConcurrentSafe(t *testing.T) {
	authorizer, err := NewStaticHeaderAuthorizer(
		domain.CredentialBearerStatic, "Authorization", "Bearer ", []byte("secret"),
	)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				request, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
				_ = authorizer.Authorize(request, nil)
			}
		}()
	}
	authorizer.Close()
	workers.Wait()
}

type typedRegistryAdapter struct {
	registryAdapter
	providerType string
}

func (a *typedRegistryAdapter) Type() string                        { return a.providerType }
func (a *typedRegistryAdapter) Probe(context.Context, string) error { return nil }

// The rejected design inferred "this profile stores files locally" from a Go
// interface not being implemented. It could never have worked, and the reason is
// worth keeping executable: every adapter reaches the gateway wrapped in
// LegacyAdapterBridge, and the bridge implements the file interface
// unconditionally. The inference would have been dead code in production while
// unit tests registering bare fakes watched it behave.
//
// This asserts the fact that killed it, so nobody rebuilds the same inference
// on the assumption that a bare adapter is what the gateway sees.
func TestBridgeSatisfiesTheResourceInterfaceWhateverItWraps(t *testing.T) {
	manifest, ok := BuiltinProfile(domain.ProfileAnthropicMessages)
	if !ok {
		t.Fatal("the Anthropic profile is not registered")
	}
	// An adapter with no file methods at all — the case the rejected inference
	// believed it could detect.
	bridge, err := NewLegacyAdapterBridge(&bridgeProbeAdapter{}, manifest,
		domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true}, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	if _, isResourceAdapter := any(bridge).(ResourceInferenceResourcesAdapter); !isResourceAdapter {
		t.Fatal("the bridge stopped satisfying the resource interface; the rejected inference would now appear to work")
	}
	// The same fact from the other direction: an optional capability the bridge
	// does not forward is invisible to the gateway, because the gateway only
	// ever holds the bridge. Adding one without teaching the bridge about it
	// produces a feature that works in a unit test holding a bare adapter and
	// never fires in production.
	if _, collectsResults := any(bridge).(BatchResultsAdapter); !collectsResults {
		t.Fatal("the bridge does not forward batch result collection, so no adapter can offer it")
	}
}

// The split between files and batches is not a taxonomy preference. Anthropic's
// Message Batch carries its requests inline, so its adapter has no file methods
// at all and Halro holds the caller's JSONL locally. While one interface
// demanded both, the bridge's assertion failed on the missing file methods and
// every batch call came back "batches are unavailable" — from an adapter that
// implements all three.
func TestBridgeForwardsBatchesToAnAdapterWithNoFileStore(t *testing.T) {
	manifest, _ := BuiltinProfile(domain.ProfileAnthropicMessages)
	bridge, err := NewLegacyAdapterBridge(&batchOnlyAdapter{}, manifest,
		domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true, Batches: true}, domain.EvidenceDeclared))
	if err != nil {
		t.Fatal(err)
	}
	batch, err := bridge.CreateBatch(context.Background(), BatchCreateCall{})
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if batch.ID != "batch_forwarded" {
		t.Fatalf("batch=%#v", batch)
	}
	if _, err := bridge.GetBatch(context.Background(), "req", "batch_forwarded"); err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if _, err := bridge.CancelBatch(context.Background(), "req", "batch_forwarded"); err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}
	// The other direction still holds: no file store means no file calls.
	if _, err := bridge.DownloadFile(context.Background(), "req", "file"); err == nil {
		t.Fatal("a batch-only adapter served a file download")
	}
}

type batchOnlyAdapter struct{ bridgeProbeAdapter }

func (*batchOnlyAdapter) CreateBatch(context.Context, BatchCreateCall) (BatchObject, error) {
	return BatchObject{ID: "batch_forwarded"}, nil
}
func (*batchOnlyAdapter) GetBatch(context.Context, string, string) (BatchObject, error) {
	return BatchObject{ID: "batch_forwarded"}, nil
}
func (*batchOnlyAdapter) CancelBatch(context.Context, string, string) (BatchObject, error) {
	return BatchObject{ID: "batch_forwarded"}, nil
}

type bridgeProbeAdapter struct{}

func (*bridgeProbeAdapter) Type() string { return string(domain.ProviderAnthropic) }
func (*bridgeProbeAdapter) Close()       {}
func (*bridgeProbeAdapter) Chat(context.Context, ChatCall) (openaiapi.ChatCompletionResponse, error) {
	return openaiapi.ChatCompletionResponse{}, nil
}
func (*bridgeProbeAdapter) ChatStream(context.Context, ChatCall, func(semantic.Event) error) (*openaiapi.Usage, error) {
	return nil, nil
}
func (*bridgeProbeAdapter) Embed(context.Context, EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, nil
}
