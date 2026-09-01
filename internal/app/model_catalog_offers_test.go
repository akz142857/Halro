package app

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
	anthropicprovider "github.com/akz142857/Halro/internal/provider/anthropic"
)

// Halro's model catalog was seeded so an operator would not have to type an
// identifier, and on a profile that lists nothing it was unreachable from the
// create form: the aggregation builds its items only from what a provider
// enumerated, so entries could enrich a listed target and never be one.
//
// Azure is the case that remains: its target is a deployment name the account
// chose, and no route lists them. MiniMax's Anthropic face was the case this was
// written for and no longer is — its catalogue was read on 2026-09-01, turned
// out to be OpenAI-shaped and readable, and that profile now enumerates. The
// MiniMax fixtures stay because the catalog still carries those entries and they
// exercise the projection; what they no longer assert is that MiniMax cannot ask.
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

// nonEnumeratingMappingAdapter is a profile that cannot enumerate, on an adapter
// that also implements ProviderMetadataMapper — the combination that produced
// the defect. It is a fake rather than a real adapter on purpose: MiniMax's
// Anthropic face was the live example until its catalogue was read and it began
// enumerating, and the defect belongs to the combination, not to whichever
// profile happens to be in it this month.
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

// realAnthropicMapper builds the adapter whose mapper is the one that needs the
// gate. A fake would only restate the gate's own rule; this is the code that
// ships, and its chat claim comes from the endpoint rather than from any field
// on the target — so it answers for every identifier put in front of it.
func realAnthropicMapper(t *testing.T, providerType domain.ProviderType, profile domain.ProviderProfileID, messagesPath string) provider.ProviderMetadataMapper {
	t.Helper()
	endpoint, _ := url.Parse("https://provider.example.com")
	scheme := domain.CredentialAnthropicAPIKey
	if providerType == domain.ProviderBedrock {
		scheme = domain.CredentialBedrockAPIKey
	}
	authorizer, err := provider.NewStaticHeaderAuthorizer(scheme, "x-api-key", "", []byte("key"), "Authorization")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := anthropicprovider.New(anthropicprovider.Options{
		Endpoint: endpoint, Authorizer: authorizer, Client: &http.Client{},
		ProviderType: string(providerType), CredentialScheme: scheme,
		MessagesPath: messagesPath, ProfileID: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

func resolveOneTarget(t *testing.T, providerType domain.ProviderType, profile domain.ProviderProfileID, messagesPath string, target domain.InvocationTargetDescriptor) adminInvocationTarget {
	t.Helper()
	binding := testBinding("binding", providerType, profile)
	instance := domain.ProviderInstance{
		ID: "provider", Type: providerType, Revision: 1,
		Bindings: []domain.ProviderProfileBinding{binding},
	}
	return resolveInvocationTargetWithCatalog(instance, target,
		map[string]domain.InvocationTargetDescriptor{binding.ID: target},
		[]domain.ProviderProfileBinding{binding},
		map[string]provider.ProviderMetadataMapper{binding.ID: realAnthropicMapper(t, providerType, profile, messagesPath)},
		0, testResolutionInstant, modelcatalog.Builtin())
}

func handEnteredTarget(model string) domain.InvocationTargetDescriptor {
	// The descriptor resolveDeploymentVariant builds when an operator types an
	// identifier instead of choosing one: a real fetch time, and a metadata
	// source saying plainly that no upstream described it.
	return domain.InvocationTargetDescriptor{
		TargetID: model, TargetKind: domain.TargetModelID, DisplayName: model,
		Lifecycle: domain.TargetLifecycleUnknown, MetadataSource: domain.MetadataSourceNone,
		Availability: domain.AvailabilityUnverified, FetchedAt: testResolutionInstant.Add(-time.Minute),
	}
}

// A provider_metadata claim asserts the upstream said something. The Anthropic
// mapper claims chat and streaming from the endpoint rather than from a field,
// which is right for a model that endpoint listed and is a fabrication for one
// nobody listed — and the mapper cannot tell the two apart, because every field
// it reads looks identical. Only the caller knows, so the caller checks.
//
// Measured before the gate: an invented identifier, typed by hand and served by
// no one, resolved as a working chat deployment. Both profiles that reach this
// mapper did it — Bedrock Mantle's Anthropic face, where only one catalog entry
// covers anything, and the direct Anthropic profile, which enumerates and is
// still handed unlisted identifiers by the deployment form.
func TestAMapperSeesOnlyTargetsAnUpstreamEnumerated(t *testing.T) {
	for _, tc := range []struct {
		name         string
		providerType domain.ProviderType
		profile      domain.ProviderProfileID
		messagesPath string
	}{
		{"bedrock mantle anthropic", domain.ProviderBedrock, domain.ProfileBedrockMantleAnthropicMessages, "anthropic/v1/messages"},
		{"direct anthropic", domain.ProviderAnthropic, domain.ProfileAnthropicMessages, ""},
		{"minimax anthropic", domain.ProviderMiniMax, domain.ProfileMiniMaxAnthropicMessages, "anthropic/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invented := resolveOneTarget(t, tc.providerType, tc.profile, tc.messagesPath, handEnteredTarget("model-nobody-serves-9"))
			if len(invented.Variants) != 0 {
				t.Errorf("an identifier no upstream listed produced %d deployable variants with capabilities %+v",
					len(invented.Variants), invented.Variants[0].Capabilities)
			}
			if invented.ResolutionState != domain.ResolutionUnknown {
				t.Errorf("resolution is %q; an unlisted, uncatalogued identifier is exactly what unknown is for — "+
					"it routes the operator to declare or detect", invented.ResolutionState)
			}
		})
	}
}

// And the mapper still answers for the target it is meant to answer for. Without
// this, gating on MetadataSourceProvider could gate everything and the only
// symptom would be capabilities quietly going missing.
func TestAnEnumeratedTargetStillEarnsItsProviderClaims(t *testing.T) {
	enumerated := handEnteredTarget("claude-sonnet-4-5")
	enumerated.MetadataSource = domain.MetadataSourceProvider
	enumerated.Availability = domain.AvailabilityAvailable
	resolved := resolveOneTarget(t, domain.ProviderAnthropic, domain.ProfileAnthropicMessages, "", enumerated)
	if len(resolved.Variants) != 1 {
		t.Fatalf("a model Anthropic itself listed produced %d variants", len(resolved.Variants))
	}
	if !resolved.Variants[0].Capabilities.Chat || !resolved.Variants[0].Capabilities.Streaming {
		t.Fatalf("an enumerated Anthropic model lost chat or streaming: %+v", resolved.Variants[0].Capabilities)
	}
}

// A model the catalog covers keeps working when it is typed rather than chosen.
// This is what the change is allowed to cost and what it is not: capabilities
// stop being invented, and the ones Halro actually reviewed still apply.
func TestACatalogCoveredModelStillResolvesWhenTyped(t *testing.T) {
	resolved := resolveOneTarget(t, domain.ProviderBedrock, domain.ProfileBedrockMantleAnthropicMessages,
		"anthropic/v1/messages", handEnteredTarget("anthropic.claude-haiku-4-5"))
	if resolved.ResolutionState != domain.ResolutionResolved || len(resolved.Variants) != 1 {
		t.Fatalf("a catalog-covered Mantle model stopped resolving: state=%q variants=%d",
			resolved.ResolutionState, len(resolved.Variants))
	}
	if !resolved.Variants[0].Capabilities.Chat {
		t.Fatalf("the catalog's own capabilities were dropped: %+v", resolved.Variants[0].Capabilities)
	}
}
