package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
)

func testBinding(id string, providerType domain.ProviderType, profile domain.ProviderProfileID) domain.ProviderProfileBinding {
	return domain.ProviderProfileBinding{
		ID: id, ProviderID: "provider", ProfileID: profile, Enabled: true,
		Capabilities: domain.DefaultProviderCapabilitiesForProfile(providerType, profile),
	}
}

func bedrockBinding(id string, profile domain.ProviderProfileID) domain.ProviderProfileBinding {
	return testBinding(id, domain.ProviderBedrock, profile)
}

// The instant every fixture below is fetched at, and the instant the resolution
// is evaluated at. It used to be a bare date literal inside targetResult while
// the resolver read time.Now(), so a catalog stamped 2026-08-10T12:00Z was only
// inside the 5-minute claim TTL for five minutes on that one day: the
// provider-metadata tests passed when they were written and failed by 12:06.
// Fixture time and evaluation time have to be the same clock.
var testResolutionInstant = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

func targetResult(binding domain.ProviderProfileBinding, mapper provider.ProviderMetadataMapper, targets ...domain.InvocationTargetDescriptor) bindingTargetCatalog {
	fetched := testResolutionInstant
	for index := range targets {
		if targets[index].FetchedAt.IsZero() {
			targets[index].FetchedAt = fetched
		}
	}
	return bindingTargetCatalog{
		binding: binding, items: targets, mapper: mapper,
		discovery: domain.InvocationTargetDiscoveryCapabilities{TargetKinds: []domain.DeploymentTargetKind{domain.TargetModelID}, CanEnumerate: true, CanVerify: true},
		fetchedAt: fetched, expiresAt: fetched.Add(invocationTargetCatalogTTL), cached: true,
	}
}

func findTarget(t *testing.T, response invocationTargetCatalogResponse, id string) adminInvocationTarget {
	t.Helper()
	for _, item := range response.Items {
		if item.TargetID == id {
			return item
		}
	}
	t.Fatalf("target %q missing from %#v", id, response.Items)
	return adminInvocationTarget{}
}

func TestAggregateInvocationTargetsProducesBindingScopedVariants(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderBedrock, Revision: 7}
	embed := testBinding("embed", domain.ProviderBedrock, domain.ProfileBedrockInvokeTitanEmbedV2)
	chat := testBinding("chat", domain.ProviderBedrock, domain.ProfileBedrockConverseText)
	target := domain.InvocationTargetDescriptor{
		TargetID: "amazon.titan-embed-text-v2:0", TargetKind: domain.TargetBedrockFoundationModel,
		DisplayName: "Titan Embed", CanonicalModelRef: "amazon.titan-embed-text-v2:0",
		Availability: domain.AvailabilityAvailable, Lifecycle: domain.TargetLifecycleActive,
	}
	response := aggregateInvocationTargets(instance, []bindingTargetCatalog{
		targetResult(chat, nil, target), targetResult(embed, nil, target),
	}, testResolutionInstant)
	item := findTarget(t, response, target.TargetID)
	if item.ResolutionState != domain.ResolutionResolved || len(item.Variants) != 1 {
		t.Fatalf("resolution=%q variants=%#v", item.ResolutionState, item.Variants)
	}
	variant := item.Variants[0]
	if variant.BindingID != embed.ID || !variant.Capabilities.Embeddings || variant.Capabilities.Chat {
		t.Fatalf("variant leaked across bindings: %#v", variant)
	}
	for _, claim := range variant.CapabilityClaims {
		if claim.Scope.BindingID != embed.ID || claim.Scope.TargetID != target.TargetID {
			t.Fatalf("claim escaped exact scope: %#v", claim.Scope)
		}
	}
}

func TestNewTargetIsAvailableWithoutInventingCapabilities(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderOpenAI, Revision: 1}
	binding := testBinding("chat", domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings)
	target := domain.InvocationTargetDescriptor{
		TargetID: "gpt-future", TargetKind: domain.TargetModelID, DisplayName: "gpt-future",
		CanonicalModelRef: "gpt-future", Availability: domain.AvailabilityAvailable, Lifecycle: domain.TargetLifecycleUnknown,
	}
	item := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{targetResult(binding, nil, target)}, testResolutionInstant), target.TargetID)
	if item.Availability != domain.AvailabilityAvailable || item.ResolutionState != domain.ResolutionUnknown || len(item.Variants) != 0 {
		t.Fatalf("discovery was mistaken for capability evidence: %#v", item)
	}
}

type staticMetadataMapper struct{ claims map[string]domain.ClaimStatus }

func (m staticMetadataMapper) MapCapabilityClaims(target domain.InvocationTargetDescriptor, scope domain.InvocationTargetScopeKey, at time.Time) []domain.CapabilityClaim {
	var result []domain.CapabilityClaim
	for name, status := range m.claims {
		result = append(result, domain.CapabilityClaim{
			CapabilityID: name, Status: status, Evidence: domain.EvidenceDeclared,
			Source: domain.ClaimSourceProviderMetadata, Scope: scope, ObservedAt: at,
			Revision: provider.CapabilityClaimRevision(target.TargetID, name, string(status)),
		})
	}
	return result
}

type operationMetadataMapper struct{}

func (operationMetadataMapper) MapCapabilityClaims(target domain.InvocationTargetDescriptor, scope domain.InvocationTargetScopeKey, at time.Time) []domain.CapabilityClaim {
	var claims []domain.CapabilityClaim
	for _, operation := range target.Metadata.SupportedOperations {
		capability := map[string]string{"chat-op": "chat", "embed-op": "embeddings"}[operation]
		if capability == "" {
			continue
		}
		claims = append(claims, domain.CapabilityClaim{CapabilityID: capability, Status: domain.ClaimSupported, Evidence: domain.EvidenceDeclared, Source: domain.ClaimSourceProviderMetadata, Scope: scope, ObservedAt: at, Revision: provider.CapabilityClaimRevision(target.TargetID, operation)})
	}
	return claims
}

func TestAggregateInvocationTargetsKeepsMetadataBindingScoped(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderOpenAI, Revision: 1}
	chat := testBinding("chat", domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings)
	embed := testBinding("embed", domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings)
	base := domain.InvocationTargetDescriptor{TargetID: "future", TargetKind: domain.TargetModelID, Availability: domain.AvailabilityAvailable, MetadataSource: domain.MetadataSourceProvider}
	chatTarget := base
	chatTarget.Metadata.SupportedOperations = []string{"chat-op"}
	embedTarget := base
	embedTarget.Metadata.SupportedOperations = []string{"embed-op"}
	item := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{
		targetResult(chat, operationMetadataMapper{}, chatTarget), targetResult(embed, operationMetadataMapper{}, embedTarget),
	}, testResolutionInstant), base.TargetID)
	if len(item.Variants) != 2 {
		t.Fatalf("variants=%#v", item.Variants)
	}
	for _, variant := range item.Variants {
		if variant.BindingID == chat.ID && (!variant.Capabilities.Chat || variant.Capabilities.Embeddings) {
			t.Fatalf("chat binding consumed another binding's metadata: %#v", variant.Capabilities)
		}
		if variant.BindingID == embed.ID && (!variant.Capabilities.Embeddings || variant.Capabilities.Chat) {
			t.Fatalf("embedding binding consumed another binding's metadata: %#v", variant.Capabilities)
		}
	}
}

func TestBindingConflictDoesNotDeleteAnotherValidVariant(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderOpenAI, Revision: 1}
	first := testBinding("conflicting", domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings)
	second := testBinding("valid", domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings)
	target := domain.InvocationTargetDescriptor{TargetID: "gpt-4o", TargetKind: domain.TargetModelID, CanonicalModelRef: "gpt-4o", Availability: domain.AvailabilityAvailable}
	item := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{
		targetResult(first, staticMetadataMapper{claims: map[string]domain.ClaimStatus{"chat": domain.ClaimUnsupported}}, target),
		targetResult(second, nil, target),
	}, testResolutionInstant), target.TargetID)
	if item.ResolutionState != domain.ResolutionResolved || len(item.Variants) != 1 || item.Variants[0].BindingID != second.ID {
		t.Fatalf("binding-scoped conflict poisoned valid variant: %#v", item)
	}
	if !slices.Equal(item.ConflictingBindings, []string{first.ID}) {
		t.Fatalf("conflicting binding was not exposed: %#v", item.ConflictingBindings)
	}
}

func TestVariantRevisionCoversNormalizedTokenLimits(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderGemini, Revision: 1}
	binding := testBinding("gemini", domain.ProviderGemini, domain.ProfileGeminiText)
	target := domain.InvocationTargetDescriptor{TargetID: "future", TargetKind: domain.TargetModelID, Availability: domain.AvailabilityAvailable, MetadataSource: domain.MetadataSourceProvider, Metadata: domain.NormalizedModelMetadata{SupportedOperations: []string{"chat-op"}, MaxContextTokens: 1024}}
	first := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{targetResult(binding, operationMetadataMapper{}, target)}, testResolutionInstant), target.TargetID)
	target.Metadata.MaxContextTokens = 2048
	second := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{targetResult(binding, operationMetadataMapper{}, target)}, testResolutionInstant), target.TargetID)
	if len(first.Variants) != 1 || len(second.Variants) != 1 || first.Variants[0].Revision == second.Variants[0].Revision {
		t.Fatalf("token limit change did not rotate variant revision: first=%#v second=%#v", first.Variants, second.Variants)
	}
}

func TestVariantRevisionBindsCredentialGeneration(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderGemini, Revision: 1}
	binding := testBinding("gemini", domain.ProviderGemini, domain.ProfileGeminiText)
	target := domain.InvocationTargetDescriptor{TargetID: "future", TargetKind: domain.TargetModelID, Availability: domain.AvailabilityAvailable, MetadataSource: domain.MetadataSourceProvider, Metadata: domain.NormalizedModelMetadata{SupportedOperations: []string{"chat-op"}}}
	firstResult := targetResult(binding, operationMetadataMapper{}, target)
	firstResult.credentialRevision = 7
	secondResult := targetResult(binding, operationMetadataMapper{}, target)
	secondResult.credentialRevision = 8
	first := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{firstResult}, testResolutionInstant), target.TargetID)
	second := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{secondResult}, testResolutionInstant), target.TargetID)
	// Indexing first: a resolution that produced no variant at all used to panic
	// here, which aborts the whole package and takes every later test's result
	// with it. State the precondition so the failure stays local.
	if len(first.Variants) != 1 || len(second.Variants) != 1 {
		t.Fatalf("credential rotation did not resolve one variant each: first=%#v second=%#v", first.Variants, second.Variants)
	}
	if first.Variants[0].Revision == second.Variants[0].Revision {
		t.Fatal("credential rotation did not rotate the deployment variant revision")
	}
}

func TestCompatibleTargetNameDoesNotImplyCanonicalCapabilityIdentity(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderOpenAICompatible, Revision: 1}
	binding := testBinding("compatible", domain.ProviderOpenAICompatible, domain.ProfileOpenAICompatible)
	target := domain.InvocationTargetDescriptor{TargetID: "gpt-5", TargetKind: domain.TargetCustomEndpointModel, Availability: domain.AvailabilityAvailable}
	item := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{targetResult(binding, nil, target)}, testResolutionInstant), target.TargetID)
	if item.ResolutionState != domain.ResolutionUnknown || len(item.Variants) != 0 {
		t.Fatalf("compatible alias inherited builtin capability identity: %#v", item)
	}
}

func TestCompatibleExplicitCanonicalMappingProducesVariant(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderOpenAICompatible, Revision: 1}
	binding := testBinding("compatible", domain.ProviderOpenAICompatible, domain.ProfileOpenAICompatible)
	target := domain.InvocationTargetDescriptor{TargetID: "tenant-alias", TargetKind: domain.TargetCustomEndpointModel, CanonicalModelRef: "gpt-4o", Availability: domain.AvailabilityAvailable}
	item := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{targetResult(binding, nil, target)}, testResolutionInstant), target.TargetID)
	if item.ResolutionState != domain.ResolutionResolved || len(item.Variants) != 1 || !item.Variants[0].Capabilities.Chat {
		t.Fatalf("explicit compatible mapping did not establish a reviewed variant: %#v", item)
	}
}

func TestSignedCatalogResolvesNewExactModelWithoutBinaryRelease(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderOpenAI, Revision: 1}
	binding := testBinding("openai", domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings)
	model := "gpt-5.future-exact"
	catalog, err := modelcatalog.MergeSnapshots(modelcatalog.Snapshot{Entries: []modelcatalog.SnapshotEntry{{
		ProviderType: domain.ProviderOpenAI, ProfileID: domain.ProfileOpenAIChatEmbeddings, TargetKind: domain.TargetModelID, Model: model,
		Capabilities: domain.ProviderCapabilities{Chat: true, Streaming: true, MaxContextTokens: 8192},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	target := domain.InvocationTargetDescriptor{TargetID: model, TargetKind: domain.TargetModelID, CanonicalModelRef: model, Availability: domain.AvailabilityAvailable}
	item := findTarget(t, aggregateInvocationTargetsWithCatalog(instance, []bindingTargetCatalog{targetResult(binding, nil, target)}, testResolutionInstant, catalog), model)
	if item.ResolutionState != domain.ResolutionResolved || len(item.Variants) != 1 || item.Variants[0].CapabilityClaims[0].Source != domain.ClaimSourceSignedCatalog {
		t.Fatalf("signed catalog did not resolve exact model: %#v", item)
	}
}

func TestVariantRevisionCoversAvailabilityAndLifecycle(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderOpenAI, Revision: 1}
	binding := testBinding("openai", domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings)
	target := domain.InvocationTargetDescriptor{TargetID: "gpt-4o", TargetKind: domain.TargetModelID, CanonicalModelRef: "gpt-4o", Availability: domain.AvailabilityAvailable, Lifecycle: domain.TargetLifecycleActive}
	first := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{targetResult(binding, nil, target)}, testResolutionInstant), target.TargetID)
	target.Availability, target.Lifecycle = domain.AvailabilityUnavailable, domain.TargetLifecycleDeprecated
	second := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{targetResult(binding, nil, target)}, testResolutionInstant), target.TargetID)
	if len(first.Variants) != 1 || len(second.Variants) != 1 {
		t.Fatalf("availability/lifecycle change did not resolve one variant each: first=%#v second=%#v", first.Variants, second.Variants)
	}
	if first.Variants[0].Revision == second.Variants[0].Revision {
		t.Fatal("availability/lifecycle change did not rotate the variant revision")
	}
}

func TestEmptyCatalogSerializesTargetKindsAsArray(t *testing.T) {
	response := aggregateInvocationTargets(domain.ProviderInstance{}, []bindingTargetCatalog{{failed: true}}, testResolutionInstant)
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"target_kinds":[]`) {
		t.Fatalf("target_kinds is not a stable array: %s", payload)
	}
}

func TestProviderMetadataCreatesOnlyAllowlistedClaimsAndConflictsFailClosed(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderGemini, Revision: 2}
	binding := testBinding("gemini", domain.ProviderGemini, domain.ProfileGeminiText)
	known := "gemini-2.5-flash"
	target := domain.InvocationTargetDescriptor{
		TargetID: known, TargetKind: domain.TargetModelID, DisplayName: known, CanonicalModelRef: known,
		Availability: domain.AvailabilityAvailable, Lifecycle: domain.TargetLifecycleActive,
		MetadataSource: domain.MetadataSourceProvider,
	}
	mapper := staticMetadataMapper{claims: map[string]domain.ClaimStatus{"chat": domain.ClaimUnsupported}}
	item := findTarget(t, aggregateInvocationTargets(instance, []bindingTargetCatalog{targetResult(binding, mapper, target)}, testResolutionInstant), known)
	if item.ResolutionState != domain.ResolutionConflicting || len(item.Variants) != 0 {
		t.Fatalf("conflicting provider metadata opened a variant: %#v", item)
	}
}

func TestSignedCatalogAndProviderMetadataConflictFailsClosed(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderOpenAI, Revision: 2}
	binding := testBinding("openai", domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings)
	model := "gpt-signed-conflict"
	catalog, err := modelcatalog.MergeSnapshots(modelcatalog.Snapshot{Entries: []modelcatalog.SnapshotEntry{{
		ProviderType: domain.ProviderOpenAI, ProfileID: binding.ProfileID, TargetKind: domain.TargetModelID, Model: model,
		Capabilities: domain.ProviderCapabilities{Chat: true},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	target := domain.InvocationTargetDescriptor{TargetID: model, TargetKind: domain.TargetModelID, Availability: domain.AvailabilityAvailable, MetadataSource: domain.MetadataSourceProvider}
	mapper := staticMetadataMapper{claims: map[string]domain.ClaimStatus{"chat": domain.ClaimUnsupported}}
	item := findTarget(t, aggregateInvocationTargetsWithCatalog(instance, []bindingTargetCatalog{targetResult(binding, mapper, target)}, testResolutionInstant, catalog), model)
	if item.ResolutionState != domain.ResolutionConflicting || len(item.Variants) != 0 {
		t.Fatalf("signed/provider conflict opened a variant: %#v", item)
	}
}

func TestCanonicalTemplatesAreMappingsNotAvailableTargets(t *testing.T) {
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderAzureOpenAI, Revision: 1}
	binding := testBinding("azure", domain.ProviderAzureOpenAI, domain.ProfileAzureChatEmbeddings)
	templates := canonicalModelTemplates(instance, []domain.ProviderProfileBinding{binding}, 0, testResolutionInstant)
	if len(templates) == 0 {
		t.Fatal("Azure has no reviewed canonical model mappings")
	}
	for _, item := range templates {
		if item.Availability != domain.AvailabilityUnverified || item.CanonicalModelRef == "" {
			t.Fatalf("canonical model was presented as an account target: %#v", item)
		}
	}
}

type listingTargetAdapter struct {
	provider.Adapter
	targets  []domain.InvocationTargetDescriptor
	delay    time.Duration
	err      error
	inFlight *atomic.Int64
	peak     *atomic.Int64
}

func (a *listingTargetAdapter) InvocationTargetDiscovery() domain.InvocationTargetDiscoveryCapabilities {
	return domain.InvocationTargetDiscoveryCapabilities{TargetKinds: []domain.DeploymentTargetKind{domain.TargetModelID}, CanEnumerate: true, CanVerify: true}
}

func (a *listingTargetAdapter) ListInvocationTargets(ctx context.Context, _ domain.TargetQuery) ([]domain.InvocationTargetDescriptor, error) {
	if a.inFlight != nil {
		current := a.inFlight.Add(1)
		defer a.inFlight.Add(-1)
		for {
			peak := a.peak.Load()
			if current <= peak || a.peak.CompareAndSwap(peak, current) {
				break
			}
		}
	}
	if a.delay > 0 {
		select {
		case <-time.After(a.delay):
		case <-ctx.Done():
			return nil, &provider.Error{Class: provider.ErrorTimeout, Message: "listing timed out", Cause: ctx.Err()}
		}
	}
	return a.targets, a.err
}

func (a *listingTargetAdapter) Close() {}

type sequencedListingAdapter struct {
	provider.Adapter
	calls        atomic.Int64
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (a *sequencedListingAdapter) InvocationTargetDiscovery() domain.InvocationTargetDiscoveryCapabilities {
	return domain.InvocationTargetDiscoveryCapabilities{TargetKinds: []domain.DeploymentTargetKind{domain.TargetModelID}, CanEnumerate: true}
}

func (a *sequencedListingAdapter) ListInvocationTargets(context.Context, domain.TargetQuery) ([]domain.InvocationTargetDescriptor, error) {
	if a.calls.Add(1) == 1 {
		close(a.firstStarted)
		<-a.releaseFirst
		return []domain.InvocationTargetDescriptor{{TargetID: "older", TargetKind: domain.TargetModelID}}, nil
	}
	return []domain.InvocationTargetDescriptor{{TargetID: "newer", TargetKind: domain.TargetModelID}}, nil
}

func (a *sequencedListingAdapter) Close() {}

func TestInvocationTargetCacheRejectsOlderLateRefresh(t *testing.T) {
	adapter := &sequencedListingAdapter{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
	registry := provider.NewRegistry()
	binding := testBinding("binding", domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings)
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderOpenAI, Enabled: true, Bindings: []domain.ProviderProfileBinding{binding}}
	if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, adapter); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{providers: registry, config: config.Default(), capabilityMetrics: newCapabilityMetrics()}
	firstDone := make(chan bindingTargetCatalog, 1)
	go func() {
		firstDone <- runtime.fetchInvocationTargetCatalog(context.Background(), instance, binding, true)
	}()
	<-adapter.firstStarted
	newer := runtime.fetchInvocationTargetCatalog(context.Background(), instance, binding, true)
	close(adapter.releaseFirst)
	older := <-firstDone
	if len(newer.items) != 1 || newer.items[0].TargetID != "newer" || len(older.items) != 1 || older.items[0].TargetID != "newer" {
		t.Fatalf("late refresh replaced or returned stale data: newer=%#v older=%#v", newer.items, older.items)
	}
}

func TestSaveResolutionRefreshRejectsRemovedTarget(t *testing.T) {
	adapter := &listingTargetAdapter{targets: []domain.InvocationTargetDescriptor{{TargetID: "gpt-4o", TargetKind: domain.TargetModelID, CanonicalModelRef: "gpt-4o", Availability: domain.AvailabilityAvailable}}}
	registry := provider.NewRegistry()
	binding := testBinding("binding", domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings)
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderOpenAI, Revision: 1, Enabled: true, Bindings: []domain.ProviderProfileBinding{binding}}
	if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, adapter); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{providers: registry, config: config.Default(), capabilityMetrics: newCapabilityMetrics(), now: time.Now}
	listed := aggregateInvocationTargets(instance, runtime.fetchInvocationTargetCatalogs(context.Background(), instance, []domain.ProviderProfileBinding{binding}, true), runtime.clockNow().UTC())
	target := findTarget(t, listed, "gpt-4o")
	if len(target.Variants) != 1 {
		t.Fatalf("listing did not resolve one variant: %#v", target)
	}
	variant := target.Variants[0]
	adapter.targets = nil
	_, err := runtime.resolveDeploymentVariant(context.Background(), instance, deploymentInput{BindingID: binding.ID, TargetKind: domain.TargetModelID, ResolutionRevision: variant.Revision}, "gpt-4o", "")
	var changed *resolutionChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("removed target was not rejected as a changed resolution: %v", err)
	}
}

func TestInvocationTargetFetchIsConcurrencyBounded(t *testing.T) {
	const bindingCount = 12
	var inFlight, peak atomic.Int64
	registry := provider.NewRegistry()
	instance := domain.ProviderInstance{ID: "provider", Type: domain.ProviderOpenAI, Enabled: true}
	for index := range bindingCount {
		id := fmt.Sprintf("b-%02d", index)
		adapter := &listingTargetAdapter{
			targets: []domain.InvocationTargetDescriptor{{TargetID: fmt.Sprintf("model-%02d", index), TargetKind: domain.TargetModelID}},
			delay:   20 * time.Millisecond, inFlight: &inFlight, peak: &peak,
		}
		if index == 0 {
			adapter.delay = time.Hour
		}
		if err := registry.RegisterBindingAdapter(instance.ID, id, adapter); err != nil {
			t.Fatal(err)
		}
		instance.Bindings = append(instance.Bindings, testBinding(id, domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings))
	}
	cfg := config.Default()
	cfg.Gateway.AttemptResponseHeaderTimeout = config.Duration(250 * time.Millisecond)
	runtime := &Runtime{providers: registry, config: cfg, capabilityMetrics: newCapabilityMetrics()}
	results := runtime.fetchInvocationTargetCatalogs(context.Background(), instance, instance.Bindings, true)
	if peak.Load() > int64(invocationTargetFetchConcurrency) || peak.Load() < 2 {
		t.Fatalf("peak=%d cap=%d", peak.Load(), invocationTargetFetchConcurrency)
	}
	failed := 0
	for _, result := range results {
		if result.failed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("failed=%d, want only the timed-out binding", failed)
	}
}

func TestAdminUsesInvocationTargetRouteAndRemovesLegacyModelsRoute(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte("correct horse battery staple")); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, csrf := loginAdminForTest(t, runtime)
	secret := `{"access_key_id":"AKIDEXAMPLE12345678","secret_access_key":"test-secret-access-key-value","region":"us-east-1"}`
	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Bedrock", "type": "bedrock", "base_url": "https://bedrock-runtime.us-east-1.amazonaws.com", "secret": secret,
	})
	var credential credentialView
	if credentialResponse.Code != http.StatusCreated || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil {
		t.Fatalf("credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "Titan", "type": "bedrock", "base_url": "https://bedrock-runtime.us-east-1.amazonaws.com",
		"credential_id": credential.ID, "profile_id": domain.ProfileBedrockInvokeTitanEmbedV2, "enabled": true,
	})
	var instance domain.ProviderInstance
	if providerResponse.Code != http.StatusCreated || json.Unmarshal(providerResponse.Body.Bytes(), &instance) != nil {
		t.Fatalf("provider status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
	listing := performAdminMutation(t, runtime, cookie, csrf, http.MethodGet, "/admin/api/v1/providers/"+instance.ID+"/invocation-targets", "", nil)
	if listing.Code != http.StatusOK {
		t.Fatalf("targets status=%d body=%s", listing.Code, listing.Body.String())
	}
	var catalog invocationTargetCatalogResponse
	if err := json.Unmarshal(listing.Body.Bytes(), &catalog); err != nil || len(catalog.Items) != 1 {
		t.Fatalf("catalog=%#v err=%v body=%s", catalog, err, listing.Body.String())
	}
	if catalog.Items[0].ResolutionState != domain.ResolutionResolved || len(catalog.Items[0].Variants) != 1 || !catalog.Items[0].Variants[0].Capabilities.Embeddings {
		t.Fatalf("target resolution=%#v", catalog.Items[0])
	}
	variant := catalog.Items[0].Variants[0]
	deploymentInput := map[string]any{
		"name": "Titan embeddings", "provider_id": instance.ID, "provider_model": catalog.Items[0].TargetID,
		"target_kind": catalog.Items[0].TargetKind, "binding_id": variant.BindingID,
		"capabilities": variant.Capabilities, "resolution_revision": "sha256:stale", "enabled": false,
	}
	stale := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/deployments", "", deploymentInput)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale resolution status=%d body=%s", stale.Code, stale.Body.String())
	}
	var conflict struct {
		Code       string                `json:"code"`
		Mismatches []string              `json:"mismatches"`
		Resolution adminInvocationTarget `json:"resolution"`
	}
	if err := json.Unmarshal(stale.Body.Bytes(), &conflict); err != nil || conflict.Code != "resolution_changed" || len(conflict.Mismatches) == 0 || len(conflict.Resolution.Variants) != 1 {
		t.Fatalf("conflict=%#v err=%v body=%s", conflict, err, stale.Body.String())
	}
	deploymentInput["resolution_revision"] = variant.Revision
	created := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/deployments", "", deploymentInput)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var deployment domain.Deployment
	if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.ModelCapabilitySnapshot.ResolutionRevision != variant.Revision || len(deployment.ModelCapabilitySnapshot.ClaimRevisions) == 0 {
		t.Fatalf("snapshot did not bind the reviewed variant: %#v", deployment.ModelCapabilitySnapshot)
	}
	runtime.clearInvocationTargetCatalog(instance.ID)
	stored, err := runtime.store.GetDeployment(context.Background(), deployment.ID)
	if err != nil || stored.ModelCapabilitySnapshot.ResolutionRevision != variant.Revision {
		t.Fatalf("catalog eviction changed the immutable snapshot: %#v err=%v", stored.ModelCapabilitySnapshot, err)
	}
	legacy := performAdminMutation(t, runtime, cookie, csrf, http.MethodGet, "/admin/api/v1/providers/"+instance.ID+"/models", "", nil)
	if legacy.Code != http.StatusNotFound {
		t.Fatalf("legacy route status=%d body=%s", legacy.Code, legacy.Body.String())
	}
	if catalog.CatalogRevision != modelcatalog.Builtin().Revision() {
		t.Fatalf("catalog revision=%q", catalog.CatalogRevision)
	}
	catalogStatus := performAdminMutation(t, runtime, cookie, csrf, http.MethodGet, "/admin/api/v1/model-catalog", "", nil)
	if catalogStatus.Code != http.StatusOK || !strings.Contains(catalogStatus.Body.String(), `"state":"disabled"`) {
		t.Fatalf("model catalog status=%d body=%s", catalogStatus.Code, catalogStatus.Body.String())
	}
	refresh := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/model-catalog/refresh", "", map[string]any{})
	if refresh.Code != http.StatusBadGateway || !strings.Contains(refresh.Body.String(), `"state":"disabled"`) {
		t.Fatalf("disabled model catalog refresh=%d body=%s", refresh.Code, refresh.Body.String())
	}
}

// Listing invocation targets is a read of a cached catalog, but refresh=true is
// not: it spends the operator's provider credential upstream, against their
// quota and their bill, as often as it is asked for. read_only exists to keep a
// session that cannot change anything from causing effects on the outside
// world, so the refresh has to be gated on the administrator role even though
// it arrives as a GET.
func TestReadOnlySessionCannotForceAnInvocationTargetRefresh(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	registry := provider.NewRegistry()
	if err := registry.RegisterAdapter(bootstrap.ProviderID, &adminProbeAdapter{
		targets: []domain.InvocationTargetDescriptor{{TargetID: "gpt-test", TargetKind: domain.TargetModelID}},
	}); err != nil {
		t.Fatal(err)
	}
	runtime.providers.Replace(registry)
	admin := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")
	create := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/admin-users", admin, map[string]string{
		"username": "viewer", "password": "another correct horse battery staple", "role": "read_only",
		"current_password": "correct horse battery staple",
	})
	createResponse := httptest.NewRecorder()
	runtime.adminRouter().ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create viewer status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	viewer := loginTestAdmin(t, runtime, "viewer", "another correct horse battery staple")
	targets := "/admin/api/v1/providers/" + bootstrap.ProviderID + "/invocation-targets"
	resolution := targets + "/unlisted-model/resolution?target_kind=model_id"

	for _, path := range []string{targets, resolution} {
		forced := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(forced, adminMutationRequest(t, http.MethodPost, path, viewer, nil))
		if forced.Code != http.StatusForbidden {
			t.Fatalf("read_only POST %s status=%d body=%s", path, forced.Code, forced.Body.String())
		}
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(forced.Body.Bytes(), &body); err != nil || body.Code != "read_only_role" {
			t.Fatalf("read_only POST %s: 403 was not the role gate (code=%q body=%s)", path, body.Code, forced.Body.String())
		}
	}
	// The cached read stays available: read_only is a restriction on causing
	// effects, not on looking.
	cached := authenticatedAdminGet(t, runtime, viewer.cookie, targets)
	if cached.Code != http.StatusOK {
		t.Fatalf("read_only listing status=%d body=%s", cached.Code, cached.Body.String())
	}
	for _, path := range []string{targets, resolution} {
		allowed := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(allowed, adminMutationRequest(t, http.MethodPost, path, admin, nil))
		if allowed.Code != http.StatusOK {
			t.Fatalf("administrator POST %s status=%d body=%s", path, allowed.Code, allowed.Body.String())
		}
	}

	// A cross-site page can drive a POST at this router, so the credential is
	// protected by the CSRF token and the Origin the browser does send on a
	// POST — not by the method being unusual.
	for _, path := range []string{targets, resolution} {
		crossOrigin := adminMutationRequest(t, http.MethodPost, path, admin, nil)
		crossOrigin.Header.Set("Origin", "https://attacker.example")
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, crossOrigin)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "CSRF validation failed") {
			t.Fatalf("cross-origin POST %s status=%d body=%s", path, response.Code, response.Body.String())
		}

		noToken := adminMutationRequest(t, http.MethodPost, path, admin, nil)
		noToken.Header.Del("X-CSRF-Token")
		tokenless := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(tokenless, noToken)
		if tokenless.Code != http.StatusForbidden || !strings.Contains(tokenless.Body.String(), "CSRF validation failed") {
			t.Fatalf("tokenless POST %s status=%d body=%s", path, tokenless.Code, tokenless.Body.String())
		}
	}
}

// A browser sends no Origin on a same-origin GET, and this router sets
// Referrer-Policy: no-referrer, so a console read arrives with neither header.
// An Origin check placed on these reads therefore rejects the console itself
// while a cross-site POST would sail past it — the check has to live on the
// method that carries a CSRF token. This test is the console's shape: cookie
// only, no Origin, no Referer.
func TestConsoleReadsSucceedWithNeitherOriginNorReferer(t *testing.T) {
	runtime, bootstrap, _ := openBootstrappedRuntime(t)
	registry := provider.NewRegistry()
	if err := registry.RegisterAdapter(bootstrap.ProviderID, &adminProbeAdapter{
		targets: []domain.InvocationTargetDescriptor{{TargetID: "gpt-test", TargetKind: domain.TargetModelID}},
	}); err != nil {
		t.Fatal(err)
	}
	runtime.providers.Replace(registry)
	admin := loginTestAdmin(t, runtime, "admin", "correct horse battery staple")

	for _, path := range []string{
		"/admin/api/v1/providers/" + bootstrap.ProviderID + "/invocation-targets",
		"/admin/api/v1/system/status",
		"/admin/api/v1/providers",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(admin.cookie)
		if request.Header.Get("Origin") != "" || request.Header.Get("Referer") != "" {
			t.Fatalf("%s: the request under test must carry neither header", path)
		}
		response := httptest.NewRecorder()
		runtime.adminRouter().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("console-shaped GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}
