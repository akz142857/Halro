package app

import (
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
)

func bedrockBinding(id string, profile domain.ProviderProfileID) domain.ProviderProfileBinding {
	return domain.ProviderProfileBinding{
		ID:           id,
		ProfileID:    profile,
		Capabilities: domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, profile),
		Enabled:      true,
	}
}

func catalogResult(binding domain.ProviderProfileBinding, cached bool, models ...string) bindingCatalog {
	items := make([]provider.ModelDescriptor, 0, len(models))
	for _, model := range models {
		items = append(items, provider.ModelDescriptor{ID: model})
	}
	fetched := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	return bindingCatalog{
		binding: binding, items: items, cached: cached,
		fetchedAt: fetched, expiresAt: fetched.Add(providerModelCatalogTTL),
	}
}

func findModel(t *testing.T, response providerModelCatalogResponse, id string) adminProviderModel {
	t.Helper()
	for _, item := range response.Items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("model %q missing from %#v", id, response.Items)
	return adminProviderModel{}
}

func TestAggregateMergesBindingsAndResolvesCapabilities(t *testing.T) {
	instance := domain.ProviderInstance{ID: "p1", Type: domain.ProviderBedrock}
	embed := bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2)
	converse := bedrockBinding("b-chat", domain.ProfileBedrockConverseText)

	response := aggregateProviderModels(instance, []bindingCatalog{
		catalogResult(embed, true, "amazon.titan-embed-text-v2:0"),
		catalogResult(converse, true, "amazon.nova-pro-v1:0"),
	}, nil)

	if len(response.Items) != 2 {
		t.Fatalf("items=%#v", response.Items)
	}
	if response.Items[0].ID > response.Items[1].ID {
		t.Fatalf("items are not sorted: %#v", response.Items)
	}

	// Seeded: the profile accepts no other model, so the catalog can speak for it.
	titan := findModel(t, response, "amazon.titan-embed-text-v2:0")
	if titan.Status != modelcatalog.StatusKnown {
		t.Fatalf("titan status=%q", titan.Status)
	}
	if !titan.Capabilities.Embeddings || titan.Capabilities.MaxContextTokens != 8192 {
		t.Fatalf("titan capabilities=%#v", titan.Capabilities)
	}
	if !titan.Preselect || titan.CapabilitySource != modelcatalog.SourceBuiltin {
		t.Fatalf("titan source=%q preselect=%v", titan.CapabilitySource, titan.Preselect)
	}
	if titan.CapabilityEvidence["embeddings"] != domain.EvidenceDeclared {
		t.Fatalf("titan evidence=%#v", titan.CapabilityEvidence)
	}
	if titan.ModelRevision == "" {
		t.Fatal("titan has no model revision")
	}

	// Not seeded. The Converse profile allows chat and streaming; none of that
	// may leak into a model no one has established anything about.
	nova := findModel(t, response, "amazon.nova-pro-v1:0")
	if nova.Status != modelcatalog.StatusUnknown {
		t.Fatalf("nova status=%q", nova.Status)
	}
	if nova.Capabilities != (domain.ProviderCapabilities{}) {
		t.Fatalf("unknown model inherited the profile ceiling: %#v", nova.Capabilities)
	}
	if nova.Preselect {
		t.Fatal("unknown model asked the console to pre-check capabilities")
	}
	if len(nova.ProfileCandidates) != 1 || !nova.ProfileCandidates[0].ProfileCapabilities.Chat {
		t.Fatalf("candidate lost the profile ceiling: %#v", nova.ProfileCandidates)
	}
	if nova.ModelRevision == "" {
		t.Fatal("unknown model has no revision to detect change against")
	}
}

func TestAggregatePrefersTheKnownCandidate(t *testing.T) {
	instance := domain.ProviderInstance{ID: "p1", Type: domain.ProviderBedrock}
	embed := bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2)
	converse := bedrockBinding("b-chat", domain.ProfileBedrockConverseText)

	// The same identifier surfaces through both bindings. Only one of them has
	// catalog backing, and that is the binding a deployment should use.
	response := aggregateProviderModels(instance, []bindingCatalog{
		catalogResult(converse, true, "amazon.titan-embed-text-v2:0"),
		catalogResult(embed, true, "amazon.titan-embed-text-v2:0"),
	}, nil)

	model := findModel(t, response, "amazon.titan-embed-text-v2:0")
	if len(model.ProfileCandidates) != 2 {
		t.Fatalf("candidates=%#v", model.ProfileCandidates)
	}
	selected := model.ProfileCandidates[0]
	if !selected.Selected || selected.BindingID != "b-embed" {
		t.Fatalf("selected candidate=%#v", selected)
	}
	if model.ProfileCandidates[1].Selected {
		t.Fatal("two candidates were selected")
	}
	if model.Status != modelcatalog.StatusKnown || !model.Capabilities.Embeddings {
		t.Fatalf("model resolved from the wrong candidate: %#v", model)
	}
	// The unselected candidate keeps its own resolution rather than borrowing
	// the winner's: through Converse this model is not established at all.
	if model.ProfileCandidates[1].Status != modelcatalog.StatusUnknown ||
		model.ProfileCandidates[1].Capabilities != (domain.ProviderCapabilities{}) {
		t.Fatalf("runner-up candidate=%#v", model.ProfileCandidates[1])
	}
}

func TestAggregateReportsDegradedBindingsWithoutDenyingCapabilities(t *testing.T) {
	instance := domain.ProviderInstance{ID: "p1", Type: domain.ProviderBedrock}
	embed := bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2)
	broken := bedrockBinding("b-chat", domain.ProfileBedrockConverseText)

	degraded := []degradedBinding{{BindingID: broken.ID, ProfileID: broken.ProfileID, ErrorClass: provider.ErrorTimeout}}
	response := aggregateProviderModels(instance, []bindingCatalog{
		catalogResult(embed, true, "amazon.titan-embed-text-v2:0"),
		{binding: broken, failed: true, errorClass: provider.ErrorTimeout},
	}, degraded)

	if len(response.Items) != 1 {
		t.Fatalf("items=%#v", response.Items)
	}
	if len(response.DegradedBindings) != 1 || response.DegradedBindings[0].ErrorClass != provider.ErrorTimeout {
		t.Fatalf("degraded=%#v", response.DegradedBindings)
	}
	// A binding that could not be read contributes no models. The missing
	// models are absent, not marked unsupported.
	for _, item := range response.Items {
		for _, candidate := range item.ProfileCandidates {
			if candidate.BindingID == broken.ID {
				t.Fatalf("failed binding contributed a candidate: %#v", candidate)
			}
		}
	}
}

func TestAggregateReportsTheEarliestExpiryAndCacheState(t *testing.T) {
	instance := domain.ProviderInstance{ID: "p1", Type: domain.ProviderBedrock}
	embed := bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2)
	converse := bedrockBinding("b-chat", domain.ProfileBedrockConverseText)

	fresh := catalogResult(converse, false, "amazon.nova-pro-v1:0")
	fresh.fetchedAt = fresh.fetchedAt.Add(time.Minute)
	fresh.expiresAt = fresh.fetchedAt.Add(providerModelCatalogTTL)
	stale := catalogResult(embed, true, "amazon.titan-embed-text-v2:0")

	response := aggregateProviderModels(instance, []bindingCatalog{fresh, stale}, nil)
	if response.Cached {
		t.Fatal("aggregate claimed cache while one binding was fetched live")
	}
	if !response.ExpiresAt.Equal(stale.expiresAt) {
		t.Fatalf("expiry=%s, want the earliest %s", response.ExpiresAt, stale.expiresAt)
	}
	if !response.FetchedAt.Equal(stale.fetchedAt) {
		t.Fatalf("fetched=%s, want the oldest %s", response.FetchedAt, stale.fetchedAt)
	}
	if response.CatalogRevision != modelcatalog.Builtin().Revision() {
		t.Fatalf("catalog revision=%q", response.CatalogRevision)
	}
}

func TestCatalogProviderBindingsNoLongerRequiresDisambiguation(t *testing.T) {
	instance := domain.ProviderInstance{
		ID: "p1", Type: domain.ProviderBedrock,
		Bindings: []domain.ProviderProfileBinding{
			bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2),
			bedrockBinding("b-chat", domain.ProfileBedrockConverseText),
		},
	}
	bindings, err := catalogProviderBindings(instance, "")
	if err != nil {
		t.Fatalf("multiple enabled bindings still demand a binding_id: %v", err)
	}
	if len(bindings) != 2 {
		t.Fatalf("bindings=%#v", bindings)
	}
	filtered, err := catalogProviderBindings(instance, "b-chat")
	if err != nil || len(filtered) != 1 || filtered[0].ID != "b-chat" {
		t.Fatalf("diagnostic filter returned %#v (%v)", filtered, err)
	}

	instance.Bindings[0].Enabled = false
	instance.Bindings[1].Enabled = false
	if _, err := catalogProviderBindings(instance, ""); err == nil {
		t.Fatal("a provider with no enabled binding produced a catalog")
	}
}
