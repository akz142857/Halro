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
