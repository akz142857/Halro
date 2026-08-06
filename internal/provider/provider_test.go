package provider

import (
	"context"
	"fmt"
	"testing"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/semantic"
)

type registryAdapter struct{}

func (*registryAdapter) Type() string { return "test" }
func (*registryAdapter) Close()       {}
func (*registryAdapter) Chat(context.Context, ChatCall) (openaiapi.ChatCompletionResponse, error) {
	return openaiapi.ChatCompletionResponse{}, nil
}

func BenchmarkRegistryResolveCandidates(b *testing.B) {
	registry := NewRegistry()
	adapter := &registryAdapter{}
	for index := 0; index < 8; index++ {
		if err := registry.Register(Target{
			ID: fmt.Sprintf("route_%d", index), DeploymentID: fmt.Sprintf("dep_%d", index),
			PublicModel: "chat", ProviderModel: fmt.Sprintf("model_%d", index),
			Adapter: adapter, Priority: index, Strategy: "round_robin",
			Capabilities: Capabilities{Chat: true, Streaming: true},
		}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		candidates := registry.ResolveCandidatesFor("chat", OperationChatStream)
		if len(candidates) != 8 {
			b.Fatalf("candidates=%d", len(candidates))
		}
	}
}
func (*registryAdapter) ChatStream(
	context.Context,
	ChatCall,
	func(semantic.Event) error,
) (*openaiapi.Usage, error) {
	return nil, nil
}
func (*registryAdapter) Embed(context.Context, EmbeddingCall) (openaiapi.EmbeddingResponse, error) {
	return openaiapi.EmbeddingResponse{}, nil
}

type mutableProfileAdapter struct {
	registryAdapter
	manifest ProfileManifest
	enabled  bool
}

func (*mutableProfileAdapter) Type() string                     { return string(domain.ProviderOpenAI) }
func (adapter *mutableProfileAdapter) Profile() ProfileManifest { return adapter.manifest }
func (adapter *mutableProfileAdapter) Operations() OperationRegistry {
	if !adapter.enabled {
		return operationSet{adapter: adapter}
	}
	return operationSet{
		operations: []Operation{OperationChat},
		bindings:   adapter.manifest.PrimitiveBindings,
		adapter:    adapter,
	}
}
func (*mutableProfileAdapter) CapabilityEvidence() domain.CapabilityEvidenceSet {
	return domain.EvidenceForCapabilities(domain.ProviderCapabilities{Chat: true}, domain.EvidenceDeclared)
}

func TestRegistryOrdersTargetsAndRoundRobinsStartingTarget(t *testing.T) {
	registry := NewRegistry()
	adapter := &registryAdapter{}
	for _, target := range []Target{
		{ID: "second", PublicModel: "chat", ProviderModel: "b", Adapter: adapter, Priority: 20, Strategy: "round_robin"},
		{ID: "first", PublicModel: "chat", ProviderModel: "a", Adapter: adapter, Priority: 10, Strategy: "round_robin"},
	} {
		if err := registry.Register(target); err != nil {
			t.Fatal(err)
		}
	}
	first := registry.ResolveCandidates("chat")
	second := registry.ResolveCandidates("chat")
	third := registry.ResolveCandidates("chat")
	if first[0].ID != "first" || second[0].ID != "second" || third[0].ID != "first" {
		t.Fatalf("unexpected rotation: %s %s %s", first[0].ID, second[0].ID, third[0].ID)
	}
}

func TestRegistryRejectsMixedStrategies(t *testing.T) {
	registry := NewRegistry()
	adapter := &registryAdapter{}
	if err := registry.Register(Target{
		ID: "first", PublicModel: "chat", ProviderModel: "a", Adapter: adapter, Strategy: "ordered",
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(Target{
		ID: "second", PublicModel: "chat", ProviderModel: "b", Adapter: adapter, Strategy: "round_robin",
	}); err == nil {
		t.Fatal("mixed route strategies were accepted")
	}
}

func TestRegistryFiltersCandidatesByDeclaredCapability(t *testing.T) {
	registry := NewRegistry()
	adapter := &registryAdapter{}
	for _, target := range []Target{
		{
			ID: "chat", PublicModel: "shared", ProviderModel: "chat",
			Adapter: adapter, Capabilities: Capabilities{Chat: true, Streaming: true},
		},
		{
			ID: "embedding", PublicModel: "shared", ProviderModel: "embedding",
			Adapter: adapter, Capabilities: Capabilities{Embeddings: true},
		},
	} {
		if err := registry.Register(target); err != nil {
			t.Fatal(err)
		}
	}
	chat := registry.ResolveCandidatesFor("shared", OperationChat)
	embeddings := registry.ResolveCandidatesFor("shared", OperationEmbeddings)
	if len(chat) != 1 || chat[0].ID != "chat" {
		t.Fatalf("chat candidates=%#v", chat)
	}
	if len(embeddings) != 1 || embeddings[0].ID != "embedding" {
		t.Fatalf("embedding candidates=%#v", embeddings)
	}
}

func TestRegistryRoutesEveryInferenceResourcesCapability(t *testing.T) {
	operations := []Operation{OperationModerations, OperationImages, OperationTranscriptions, OperationSpeech, OperationFiles, OperationBatches, OperationRerank, OperationAsyncInvoke}
	bindings := make([]PrimitiveBinding, 0, len(operations))
	for _, operation := range operations {
		bindings = append(bindings, PrimitiveBinding{LegacyOperation: operation, SemanticOperation: semanticOperationFor(operation), Primitive: Primitive("test." + string(operation))})
	}
	adapter := &registryAdapter{}
	registry := NewRegistry()
	target := Target{ID: "inferenceResources", PublicModel: "inferenceResources", ProviderModel: "provider-model", Adapter: adapter,
		Capabilities: Capabilities{Moderations: true, Images: true, Transcriptions: true, Speech: true, Files: true, Batches: true, Rerank: true, AsyncGenerate: true},
		operations:   operationSet{operations: operations, bindings: bindings, adapter: adapter},
	}
	if err := registry.Register(target); err != nil {
		t.Fatal(err)
	}
	for _, operation := range operations {
		if candidates := registry.ResolveCandidatesFor("inferenceResources", operation); len(candidates) != 1 {
			t.Fatalf("operation=%s candidates=%d", operation, len(candidates))
		}
	}
}

func TestRegistryCanRequireMinimumCapabilityEvidence(t *testing.T) {
	registry := NewRegistry()
	adapter := &registryAdapter{}
	for _, target := range []Target{
		{
			ID: "legacy", PublicModel: "shared", ProviderModel: "legacy", Adapter: adapter,
			Capabilities: Capabilities{Chat: true},
			CapabilityEvidence: domain.EvidenceForCapabilities(
				domain.ProviderCapabilities{Chat: true}, domain.EvidenceLegacy,
			),
		},
		{
			ID: "verified", PublicModel: "shared", ProviderModel: "verified", Adapter: adapter,
			Capabilities: Capabilities{Chat: true}, Priority: 10,
			CapabilityEvidence: domain.EvidenceForCapabilities(
				domain.ProviderCapabilities{Chat: true}, domain.EvidenceVerified,
			),
		},
	} {
		if err := registry.Register(target); err != nil {
			t.Fatal(err)
		}
	}
	candidates := registry.ResolveCandidatesForEvidence("shared", OperationChat, domain.EvidenceDeclared)
	if len(candidates) != 1 || candidates[0].ID != "verified" {
		t.Fatalf("evidence-filtered candidates=%#v", candidates)
	}
}

func TestRegistryEvidenceFilteringFailsClosedAndRequiresChatForStreaming(t *testing.T) {
	registry := NewRegistry()
	evidence := domain.EvidenceForCapabilities(
		domain.ProviderCapabilities{Chat: true, Streaming: true}, domain.EvidenceVerified,
	)
	evidence["chat"] = domain.EvidenceLegacy
	if err := registry.Register(Target{
		ID: "mixed", PublicModel: "stream", ProviderModel: "model", Adapter: &registryAdapter{},
		Capabilities: Capabilities{Chat: true, Streaming: true}, CapabilityEvidence: evidence,
	}); err != nil {
		t.Fatal(err)
	}
	if candidates := registry.ResolveCandidatesForEvidence("stream", OperationChatStream, domain.EvidenceVerified); len(candidates) != 0 {
		t.Fatalf("streaming accepted weak chat evidence: %#v", candidates)
	}
	if candidates := registry.ResolveCandidatesForEvidence("stream", OperationChatStream, "typo"); len(candidates) != 0 {
		t.Fatalf("invalid evidence threshold failed open: %#v", candidates)
	}
}

func TestRegistryExcludesActivelyUnhealthyDeploymentAndRecovers(t *testing.T) {
	registry := NewRegistry()
	adapter := &registryAdapter{}
	for _, target := range []Target{
		{ID: "primary", DeploymentID: "dep_primary", PublicModel: "chat", ProviderModel: "a", Adapter: adapter},
		{ID: "fallback", DeploymentID: "dep_fallback", PublicModel: "chat", ProviderModel: "b", Adapter: adapter, Priority: 10},
	} {
		if err := registry.Register(target); err != nil {
			t.Fatal(err)
		}
	}
	registry.SetDeploymentHealthy("dep_primary", false)
	candidates := registry.ResolveCandidates("chat")
	if len(candidates) != 1 || candidates[0].DeploymentID != "dep_fallback" {
		t.Fatalf("unhealthy deployment was not excluded: %#v", candidates)
	}
	registry.SetDeploymentHealthy("dep_primary", true)
	if candidates = registry.ResolveCandidates("chat"); len(candidates) != 2 || candidates[0].DeploymentID != "dep_primary" {
		t.Fatalf("healthy deployment did not recover: %#v", candidates)
	}
}

type closingRegistryAdapter struct {
	registryAdapter
	closed bool
}

func (a *closingRegistryAdapter) Close() { a.closed = true }

func TestRegistryReplaceIsAtomicAndReturnsRetiredAdapters(t *testing.T) {
	current := NewRegistry()
	oldAdapter := &closingRegistryAdapter{}
	if err := current.Register(Target{
		ID: "old", PublicModel: "chat", ProviderModel: "old", Adapter: oldAdapter,
	}); err != nil {
		t.Fatal(err)
	}
	next := NewRegistry()
	newAdapter := &closingRegistryAdapter{}
	if err := next.Register(Target{
		ID: "new", PublicModel: "chat", ProviderModel: "new", Adapter: newAdapter,
	}); err != nil {
		t.Fatal(err)
	}
	retired := current.Replace(next)
	target, ok := current.Resolve("chat")
	if !ok || target.ID != "new" {
		t.Fatalf("replacement was not visible: %#v", target)
	}
	if len(retired) != 1 || retired[0] != oldAdapter || oldAdapter.closed {
		t.Fatalf("unexpected retired adapters: %#v closed=%v", retired, oldAdapter.closed)
	}
	next.Close()
	if newAdapter.closed {
		t.Fatal("replacement registry retained ownership after transfer")
	}
}

func TestRegistryAddressesAdaptersByProviderBinding(t *testing.T) {
	registry := NewRegistry()
	chat := &registryAdapter{}
	media := &registryAdapter{}
	if err := registry.RegisterBindingAdapter("provider", "binding_chat", chat); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterBindingAdapter("provider", "binding_media", media); err != nil {
		t.Fatal(err)
	}
	if adapter, ok := registry.AdapterForBinding("provider", "binding_chat"); !ok || adapter != chat {
		t.Fatalf("chat binding adapter=%#v ok=%v", adapter, ok)
	}
	if _, ok := registry.AdapterForBinding("other", "binding_chat"); ok {
		t.Fatal("binding was addressable through a different provider")
	}
	if _, ok := registry.AdapterForProvider("provider"); ok {
		t.Fatal("ambiguous provider adapter lookup did not fail closed")
	}
	if ids := registry.ProviderIDs(); len(ids) != 1 || ids[0] != "provider" {
		t.Fatalf("provider ids=%v", ids)
	}
}

func TestRegistryRejectsDuplicateBindingIdentityAcrossProviders(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterBindingAdapter("first", "shared", &registryAdapter{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterBindingAdapter("second", "shared", &registryAdapter{}); err == nil {
		t.Fatal("duplicate global binding identity was accepted")
	}
}

func TestRegistryCapabilityEvidenceIsImmutableAcrossBoundaries(t *testing.T) {
	evidence := domain.CapabilityEvidenceSet{"chat": domain.EvidenceVerified}
	registry := NewRegistry()
	if err := registry.Register(Target{ID: "target", PublicModel: "chat", ProviderModel: "model", Adapter: &registryAdapter{}, CapabilityEvidence: evidence}); err != nil {
		t.Fatal(err)
	}
	evidence["chat"] = domain.EvidenceUnsupported
	resolved, _ := registry.Resolve("chat")
	if resolved.CapabilityEvidence["chat"] != domain.EvidenceVerified {
		t.Fatal("registration retained caller evidence map")
	}
	resolved.CapabilityEvidence["chat"] = domain.EvidenceUnsupported
	all := registry.ResolveAll("chat")
	all[0].CapabilityEvidence["chat"] = domain.EvidenceLegacy
	candidates := registry.ResolveCandidates("chat")
	candidates[0].CapabilityEvidence["chat"] = domain.EvidenceDeclared
	fresh, _ := registry.Resolve("chat")
	if fresh.CapabilityEvidence["chat"] != domain.EvidenceVerified {
		t.Fatal("registry exposed mutable evidence map")
	}
}

func TestRegistryCandidateResolutionUsesCapturedOperationSnapshot(t *testing.T) {
	manifest, ok := BuiltinProfile(domain.ProfileOpenAIChatEmbeddings)
	if !ok {
		t.Fatal("OpenAI profile is unavailable")
	}
	manifest.Operations = []Operation{OperationChat}
	manifest.PrimitiveBindings = []PrimitiveBinding{{
		LegacyOperation: OperationChat, SemanticOperation: semantic.OperationGenerate,
		Primitive: PrimitiveOpenAIChatCompletions,
	}}
	adapter := &mutableProfileAdapter{manifest: manifest, enabled: true}
	registry := NewRegistry()
	if err := registry.Register(Target{
		ID: "target", PublicModel: "chat", ProviderModel: "model", Adapter: adapter,
		Capabilities: Capabilities{Chat: true},
	}); err != nil {
		t.Fatal(err)
	}
	adapter.enabled = false
	candidates := registry.ResolveCandidatesFor("chat", OperationChat)
	if len(candidates) != 1 {
		t.Fatalf("captured operation snapshot changed with adapter: %#v", candidates)
	}
	operation, ok := candidates[0].ResolveOperation(OperationChat)
	if !ok || operation.ProviderPrimitive() != PrimitiveOpenAIChatCompletions {
		t.Fatalf("captured primitive unavailable: %#v", operation)
	}
}

func TestUnprofiledAdapterIsConservativeAndCannotClaimProfile(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(Target{
		ID: "legacy", PublicModel: "chat", ProviderModel: "model", Adapter: &registryAdapter{},
		Capabilities: Capabilities{
			Chat: true, Streaming: true, Embeddings: true, Tools: true, Vision: true,
			JSONMode: true, DeveloperRole: true, Reasoning: true, StreamUsage: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	target, ok := registry.Resolve("chat")
	if !ok || !target.LegacyUnprofiled {
		t.Fatalf("legacy target was not marked conservatively: %#v", target)
	}
	if target.Capabilities.Tools || target.Capabilities.Vision || target.Capabilities.JSONMode ||
		target.Capabilities.DeveloperRole || target.Capabilities.Reasoning || target.Capabilities.StreamUsage {
		t.Fatalf("legacy target retained unproved optional capabilities: %#v", target.Capabilities)
	}
	if target.CapabilityEvidence["chat"] != domain.EvidenceLegacy ||
		target.CapabilityEvidence["tools"] != domain.EvidenceUnsupported {
		t.Fatalf("legacy evidence is not conservative: %#v", target.CapabilityEvidence)
	}
	generation, err := target.Generation(OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	if loss := translationForPrimitive(generation.ProviderPrimitive()); loss != semantic.TranslationDeclared {
		t.Fatalf("legacy primitive claimed lossless translation: %s", loss)
	}
	if err := NewRegistry().Register(Target{
		ID: "false-profile", PublicModel: "chat", ProviderModel: "model", Adapter: &registryAdapter{},
		ProfileID: domain.ProfileOpenAIChatEmbeddings,
	}); err == nil {
		t.Fatal("unprofiled adapter claimed a registered profile")
	}
}
