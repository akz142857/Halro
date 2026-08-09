package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"

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

// The gate this whole change exists for: a model the builtin catalog covers has
// to be reachable in the console, not only by typing its identifier. Before
// Bedrock could list models, the catalog held four entries no picker could
// surface, and "known models arrive pre-checked" was vacuously true.
func TestBedrockPinnedProfileOffersItsCataloguedModelThroughTheAdminAPI(t *testing.T) {
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
		"name": "Bedrock Runtime", "type": "bedrock", "base_url": "https://bedrock-runtime.us-east-1.amazonaws.com", "secret": secret,
	})
	var credential credentialView
	if credentialResponse.Code != http.StatusCreated || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil {
		t.Fatalf("credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "Titan embeddings", "type": "bedrock", "base_url": "https://bedrock-runtime.us-east-1.amazonaws.com",
		"credential_id": credential.ID, "profile_id": domain.ProfileBedrockInvokeTitanEmbedV2, "enabled": true,
	})
	var instance domain.ProviderInstance
	if providerResponse.Code != http.StatusCreated || json.Unmarshal(providerResponse.Body.Bytes(), &instance) != nil {
		t.Fatalf("provider status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}

	// A pinned profile answers from the pin, so this reaches no network at all —
	// which is why it can be asserted without a credentialed upstream.
	listing := performAdminMutation(t, runtime, cookie, csrf, http.MethodGet,
		"/admin/api/v1/providers/"+instance.ID+"/models", "", nil)
	if listing.Code != http.StatusOK {
		t.Fatalf("models status=%d body=%s", listing.Code, listing.Body.String())
	}
	var catalog providerModelCatalogResponse
	if err := json.Unmarshal(listing.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Items) != 1 {
		t.Fatalf("listed %d models, want the one the profile accepts: %s", len(catalog.Items), listing.Body.String())
	}
	offered := catalog.Items[0]
	if offered.ID != "amazon.titan-embed-text-v2:0" {
		t.Fatalf("offered %q", offered.ID)
	}
	if offered.Status != modelcatalog.StatusKnown || offered.CapabilitySource != modelcatalog.SourceBuiltin {
		t.Fatalf("status=%q source=%q, want the builtin catalog to establish it", offered.Status, offered.CapabilitySource)
	}
	if !offered.Preselect || !offered.Capabilities.Embeddings || offered.Capabilities.Chat {
		t.Fatalf("offered capabilities=%#v preselect=%v", offered.Capabilities, offered.Preselect)
	}

	// And the revision it carries is the one creating a deployment accepts, so
	// the operator is not sent through a conflict on the first attempt.
	created := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/deployments", "", map[string]any{
		"name": "Titan", "provider_id": instance.ID, "provider_model": offered.ID,
		"model_revision": offered.ModelRevision, "capabilities": offered.Capabilities, "enabled": false,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("deployment status=%d body=%s", created.Code, created.Body.String())
	}
	var deployment domain.Deployment
	if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.ModelCapabilitySnapshot.Source != string(modelcatalog.SourceBuiltin) {
		t.Fatalf("snapshot source=%q, want the catalog to be recorded as the source",
			deployment.ModelCapabilitySnapshot.Source)
	}
}

// §12 asked for the aggregate's failure and latency semantics to be tested, not
// only described. These three cover what the aggregation actually promises.

// listingAdapter is a model lister whose answer and delay the test controls.
type listingAdapter struct {
	provider.Adapter
	models  []provider.ModelDescriptor
	delay   time.Duration
	err     error
	inFlght *atomic.Int64
	peak    *atomic.Int64
}

func (a *listingAdapter) ListModels(ctx context.Context) ([]provider.ModelDescriptor, error) {
	if a.inFlght != nil {
		current := a.inFlght.Add(1)
		defer a.inFlght.Add(-1)
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
	if a.err != nil {
		return nil, a.err
	}
	return a.models, nil
}

func (a *listingAdapter) Close() {}

// A provider with many enabled interfaces must not fan out without a bound, and
// one slow interface must not consume the time the others need. The timeout is
// per binding for that reason, and this asserts both rather than trusting the
// comment that says so.
func TestAggregateCatalogIsConcurrencyBoundedAndTimesOutPerBinding(t *testing.T) {
	const bindings = 12
	var inFlight, peak atomic.Int64
	registry := provider.NewRegistry()
	instance := domain.ProviderInstance{ID: "prov", Type: domain.ProviderOpenAI, Enabled: true}
	for index := range bindings {
		id := fmt.Sprintf("b-%02d", index)
		adapter := &listingAdapter{
			models:  []provider.ModelDescriptor{{ID: fmt.Sprintf("model-%02d", index)}},
			delay:   20 * time.Millisecond,
			inFlght: &inFlight, peak: &peak,
		}
		// One interface hangs past its own timeout. It must fail alone.
		if index == 0 {
			adapter.delay = time.Hour
		}
		if err := registry.RegisterBindingAdapter(instance.ID, id, adapter); err != nil {
			t.Fatal(err)
		}
		instance.Bindings = append(instance.Bindings, domain.ProviderProfileBinding{
			ID: id, ProfileID: domain.ProfileOpenAIChatEmbeddings, Enabled: true,
			Capabilities: domain.DefaultProviderCapabilitiesForProfile(domain.ProviderOpenAI, domain.ProfileOpenAIChatEmbeddings),
		})
	}

	cfg := config.Default()
	cfg.Gateway.AttemptResponseHeaderTimeout = config.Duration(250 * time.Millisecond)
	runtime := &Runtime{providers: registry, config: cfg, capabilityMetrics: newCapabilityMetrics()}

	started := time.Now()
	results := runtime.fetchProviderCatalogs(context.Background(), instance, instance.Bindings, true)
	elapsed := time.Since(started)

	if peak.Load() > int64(providerModelFetchConcurrency) {
		t.Fatalf("peak concurrent fetches=%d, cap=%d", peak.Load(), providerModelFetchConcurrency)
	}
	if peak.Load() < 2 {
		t.Fatalf("the fetches did not overlap at all, so the cap is not what bounded them: peak=%d", peak.Load())
	}
	// The hung interface is bounded by its own timeout rather than running to
	// its full delay, and the others are not waiting behind it.
	if elapsed > 3*time.Second {
		t.Fatalf("one slow interface held the aggregate for %s", elapsed)
	}
	var failed, reached int
	for _, result := range results {
		if result.failed {
			failed++
			continue
		}
		reached++
	}
	if failed != 1 || reached != bindings-1 {
		t.Fatalf("failed=%d reached=%d, want exactly the hung interface to fail", failed, reached)
	}
}

// §7.3 calls manual model entry the kill switch: when nothing can answer, the
// operator must still be told to type the ID rather than be left with a page
// that looks empty.
func TestProviderModelCatalogFailsClosedWhenNoInterfaceCanList(t *testing.T) {
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

	// Anthropic's adapter implements no model discovery, so every enabled
	// binding degrades and none is reachable.
	credentialResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", "", map[string]any{
		"name": "Anthropic", "type": "anthropic", "base_url": "https://api.anthropic.com", "secret": "sk-ant-test-secret-value",
	})
	var credential credentialView
	if credentialResponse.Code != http.StatusCreated || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil {
		t.Fatalf("credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/providers", "", map[string]any{
		"name": "Anthropic", "type": "anthropic", "base_url": "https://api.anthropic.com",
		"credential_id": credential.ID, "enabled": true,
	})
	var instance domain.ProviderInstance
	if providerResponse.Code != http.StatusCreated || json.Unmarshal(providerResponse.Body.Bytes(), &instance) != nil {
		t.Fatalf("provider status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}

	listing := performAdminMutation(t, runtime, cookie, csrf, http.MethodGet,
		"/admin/api/v1/providers/"+instance.ID+"/models", "", nil)
	if listing.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", listing.Code, listing.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(listing.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	message, _ := body["error"].(string)
	if !strings.Contains(message, "enter a model ID manually") {
		t.Fatalf("the refusal does not point at the fallback: %q", message)
	}
	if body["error_class"] == nil || body["error_class"] == "" {
		t.Fatalf("no error class to distinguish an outage from a profile with no discovery: %#v", body)
	}
}
