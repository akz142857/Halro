package app

import (
	"context"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
)

// Halro's model catalog was seeded so an operator would not have to type an
// identifier, and on a profile that lists nothing it was unreachable from the
// create form: the aggregation builds its items only from what a provider
// enumerated, so entries could enrich a listed target and never be one.
//
// MiniMax's Anthropic face is the case. It deliberately does not enumerate —
// MiniMax's model list is OpenAI-shaped and carries an identifier and an owner,
// so building targets from it would credit every id on the account, speech and
// video models included, with chat and streaming on declared evidence.
func TestModelCatalogOffersFillAProfileThatCannotEnumerate(t *testing.T) {
	offers := modelCatalogOffers(domain.ProviderMiniMax, domain.ProfileMiniMaxAnthropicMessages, modelcatalog.Builtin())
	if len(offers) == 0 {
		t.Fatal("a profile with catalog entries offered nothing; the create form is back to a blank field")
	}
	byID := map[string]domain.InvocationTargetDescriptor{}
	for _, offer := range offers {
		byID[offer.TargetID] = offer
	}
	m3, ok := byID["MiniMax-M3"]
	if !ok {
		t.Fatalf("MiniMax-M3 is in the catalog and was not offered; offers were %v", byID)
	}
	if m3.Metadata.MaxContextTokens != 1_000_000 || m3.Metadata.MaxOutputTokens != 524_288 {
		t.Errorf("M3 offered with context=%d output=%d, want the catalog's 1000000 and 524288",
			m3.Metadata.MaxContextTokens, m3.Metadata.MaxOutputTokens)
	}
}

// The label is the point. A provider listing a model is evidence the account
// reaches it; a catalog entry is evidence only that Halro pre-checked the
// identifier. Presenting the second as the first would tell an operator their
// account has a model it may not be entitled to.
func TestModelCatalogOffersTravelAsOffersNotFindings(t *testing.T) {
	for _, offer := range modelCatalogOffers(domain.ProviderMiniMax, domain.ProfileMiniMaxChat, modelcatalog.Builtin()) {
		if offer.MetadataSource != domain.MetadataSourceModelCatalog {
			t.Errorf("%s carries source %q, want model_catalog", offer.TargetID, offer.MetadataSource)
		}
		if offer.Availability != domain.AvailabilityUnverified {
			t.Errorf("%s claims availability %q; the catalog cannot know what this account reaches", offer.TargetID, offer.Availability)
		}
		if offer.Lifecycle != domain.TargetLifecycleUnknown {
			t.Errorf("%s claims lifecycle %q; the catalog does not track retirement", offer.TargetID, offer.Lifecycle)
		}
		if !offer.FetchedAt.IsZero() {
			t.Errorf("%s carries a fetch time; nothing was fetched", offer.TargetID)
		}
	}
}

// A profile with no catalog entries offers nothing rather than something
// borrowed from a neighbour.
func TestModelCatalogOffersAreScopedToTheProfile(t *testing.T) {
	if offers := modelCatalogOffers(domain.ProviderMiniMax, domain.ProfileAnthropicMessages, modelcatalog.Builtin()); len(offers) != 0 {
		t.Fatalf("offers leaked across the (type, profile) key: %d returned", len(offers))
	}
	azure := modelCatalogOffers(domain.ProviderAzureOpenAI, domain.ProfileAzureChatEmbeddings, modelcatalog.Builtin())
	for _, offer := range azure {
		if offer.TargetKind != domain.TargetAzureDeployment && offer.TargetKind != domain.TargetModelID {
			t.Errorf("unexpected target kind %q for an Azure offer", offer.TargetKind)
		}
	}
}

// nonEnumeratingMappingAdapter is the shape the real MiniMax Anthropic binding
// has: a profile that cannot enumerate, on an adapter that also implements
// ProviderMetadataMapper. The direct Anthropic adapter is exactly this, and it
// claims chat and streaming from the fact that it enumerated the target — a
// premise an offer does not satisfy.
type nonEnumeratingMappingAdapter struct{ provider.Adapter }

func (nonEnumeratingMappingAdapter) InvocationTargetDiscovery() domain.InvocationTargetDiscoveryCapabilities {
	return domain.InvocationTargetDiscoveryCapabilities{
		TargetKinds: []domain.DeploymentTargetKind{domain.TargetModelID}, CanVerify: true,
	}
}

func (nonEnumeratingMappingAdapter) MapCapabilityClaims(target domain.InvocationTargetDescriptor, scope domain.InvocationTargetScopeKey, observedAt time.Time) []domain.CapabilityClaim {
	claim := func(capabilityID string) domain.CapabilityClaim {
		return domain.CapabilityClaim{
			CapabilityID: capabilityID, Status: domain.ClaimSupported, Evidence: domain.EvidenceDeclared,
			Source: domain.ClaimSourceProviderMetadata, Scope: scope, ObservedAt: observedAt,
			Revision: provider.CapabilityClaimRevision(string(domain.ClaimSourceProviderMetadata), target.TargetID, capabilityID),
		}
	}
	return []domain.CapabilityClaim{claim("chat"), claim("streaming")}
}

func (nonEnumeratingMappingAdapter) Close() {}

// The offers have to survive the aggregation that presents them, and the three
// tests above only exercise the pure function that builds them. They did not:
// the fetch attached the adapter's metadata mapper before the cannot-enumerate
// branch returned, so every offer was run through a mapper with nothing to map.
// An offer carries no fetch time, so each claim it produced had a zero
// observation instant and no expiry — both of which CapabilityClaim.Validate
// refuses — and every offer arrived at the create form as a capability conflict
// with no deployable variant, which is what the blank field was replaced with.
func TestModelCatalogOffersResolveThroughTheAggregation(t *testing.T) {
	registry := provider.NewRegistry()
	binding := testBinding("anthropic", domain.ProviderMiniMax, domain.ProfileMiniMaxAnthropicMessages)
	instance := domain.ProviderInstance{
		ID: "provider", Type: domain.ProviderMiniMax, Revision: 1, Enabled: true,
		Bindings: []domain.ProviderProfileBinding{binding},
	}
	if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, nonEnumeratingMappingAdapter{}); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{providers: registry, config: config.Default(), capabilityMetrics: newCapabilityMetrics(), now: time.Now}
	results := runtime.fetchInvocationTargetCatalogs(context.Background(), instance, instance.Bindings, catalogAlwaysDial, modelcatalog.Builtin())
	listed := aggregateInvocationTargets(instance, results, testResolutionInstant)
	target := findTarget(t, listed, "MiniMax-M3")
	if target.MetadataSource != domain.MetadataSourceModelCatalog || target.Availability != domain.AvailabilityUnverified {
		t.Errorf("the offer stopped reading as an offer: source=%q availability=%q", target.MetadataSource, target.Availability)
	}
	if len(target.ConflictingBindings) != 0 || target.ResolutionState == domain.ResolutionConflicting {
		t.Fatalf("an offer resolved as a capability conflict: state=%q conflicting=%v", target.ResolutionState, target.ConflictingBindings)
	}
	if len(target.Variants) != 1 {
		t.Fatalf("an offer produced %d variants; nothing could be deployed from the list", len(target.Variants))
	}
}

// The deployment save resolves the target a second time, through
// admin_deployments.go, and builds its own mappers map from the adapter — so the
// mapper is not stopped at the fetch, it is stopped here, at the one place both
// consumers pass through. Guarded separately because the listing passing is not
// evidence the save does: for a while the offers listed fine and every attempt
// to deploy one came back a capability conflict with no variant.
func TestAnOfferResolvesForTheDeploymentSaveToo(t *testing.T) {
	binding := testBinding("anthropic", domain.ProviderMiniMax, domain.ProfileMiniMaxAnthropicMessages)
	instance := domain.ProviderInstance{
		ID: "provider", Type: domain.ProviderMiniMax, Revision: 1,
		Bindings: []domain.ProviderProfileBinding{binding},
	}
	var target domain.InvocationTargetDescriptor
	for _, offer := range modelCatalogOffers(domain.ProviderMiniMax, domain.ProfileMiniMaxAnthropicMessages, modelcatalog.Builtin()) {
		if offer.TargetID == "MiniMax-M3" {
			target = offer
		}
	}
	if target.TargetID == "" {
		t.Fatal("MiniMax-M3 was not offered")
	}
	resolved := resolveInvocationTargetWithCatalog(
		instance, target,
		map[string]domain.InvocationTargetDescriptor{binding.ID: target},
		[]domain.ProviderProfileBinding{binding},
		map[string]provider.ProviderMetadataMapper{binding.ID: nonEnumeratingMappingAdapter{}},
		0, testResolutionInstant, modelcatalog.Builtin(),
	)
	if len(resolved.ConflictingBindings) != 0 || len(resolved.Variants) != 1 {
		t.Fatalf("an offer could not be deployed: state=%q variants=%d conflicting=%v",
			resolved.ResolutionState, len(resolved.Variants), resolved.ConflictingBindings)
	}
}

// The offers follow the effective catalog, not the one compiled into the binary.
// Halro's catalog refreshes from a signed remote snapshot, which exists so a
// model published after this release can be reached without a new binary — and
// the offers path read modelcatalog.Builtin() directly, so the one list an
// operator can actually pick from was the only thing frozen to build time.
func TestOffersFollowARefreshedCatalogRatherThanTheBuild(t *testing.T) {
	if _, found := modelcatalog.Builtin().Lookup(modelcatalog.Key{
		ProviderType: domain.ProviderMiniMax, Profile: domain.ProfileMiniMaxAnthropicMessages,
		TargetKind: domain.TargetModelID, Model: "MiniMax-M9-unreleased",
	}); found {
		t.Fatal("the fixture model is in the bundled catalog, so this proves nothing")
	}
	catalog, err := modelcatalog.MergeSnapshots(modelcatalog.Snapshot{Entries: []modelcatalog.SnapshotEntry{{
		ProviderType: domain.ProviderMiniMax, ProfileID: domain.ProfileMiniMaxAnthropicMessages,
		TargetKind: domain.TargetModelID, Model: "MiniMax-M9-unreleased",
		Capabilities: domain.ProviderCapabilities{Chat: true, Streaming: true},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	binding := testBinding("anthropic", domain.ProviderMiniMax, domain.ProfileMiniMaxAnthropicMessages)
	instance := domain.ProviderInstance{
		ID: "provider", Type: domain.ProviderMiniMax, Revision: 1, Enabled: true,
		Bindings: []domain.ProviderProfileBinding{binding},
	}
	if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, nonEnumeratingMappingAdapter{}); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{providers: registry, config: config.Default(), capabilityMetrics: newCapabilityMetrics(), now: time.Now}
	results := runtime.fetchInvocationTargetCatalogs(context.Background(), instance, instance.Bindings, catalogAlwaysDial, catalog)
	listed := aggregateInvocationTargetsWithCatalog(instance, results, testResolutionInstant, catalog)
	findTarget(t, listed, "MiniMax-M9-unreleased")
}
