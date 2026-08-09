package app

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
)

const titanEmbedModel = "amazon.titan-embed-text-v2:0"

func bedrockInstance(bindings ...domain.ProviderProfileBinding) domain.ProviderInstance {
	return domain.ProviderInstance{
		ID: "prov", Type: domain.ProviderBedrock, AccessSurface: domain.SurfaceBedrockRuntime,
		BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com", Enabled: true, Bindings: bindings,
	}
}

func capabilitiesOf(profile domain.ProviderProfileID) domain.ProviderCapabilities {
	return domain.DefaultProviderCapabilitiesForProfile(domain.ProviderBedrock, profile)
}

func TestResolveKnownModelPicksItsBindingWithoutBeingTold(t *testing.T) {
	instance := bedrockInstance(
		bedrockBinding("b-chat", domain.ProfileBedrockConverseText),
		bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2),
	)
	resolution, err := resolveDeploymentTarget(instance, deploymentInput{}, titanEmbedModel, "us-east-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.binding.ID != "b-embed" {
		t.Fatalf("resolved binding=%q", resolution.binding.ID)
	}
	if !resolution.capabilities.Embeddings || resolution.capabilities.Chat {
		t.Fatalf("capabilities=%#v", resolution.capabilities)
	}
	if resolution.declared {
		t.Fatal("a catalog-backed model was recorded as operator declared")
	}
}

func TestResolveRejectsCapabilitiesBeyondTheCatalog(t *testing.T) {
	instance := bedrockInstance(bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2))
	// The Titan embedding profile carries embeddings only. Asking for chat is
	// not a narrowing of anything the catalog established.
	wanted := domain.ProviderCapabilities{Embeddings: true, Chat: true, MaxContextTokens: 8192}
	_, err := resolveDeploymentTarget(instance, deploymentInput{Capabilities: &wanted}, titanEmbedModel, "us-east-1", nil)
	if err == nil {
		t.Fatal("capabilities beyond the catalog were accepted")
	}
}

func TestResolveIgnoresClientClaimedBindingAsAuthority(t *testing.T) {
	instance := bedrockInstance(
		bedrockBinding("b-chat", domain.ProfileBedrockConverseText),
		bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2),
	)
	// Naming the chat binding must not turn a catalogued embedding model into a
	// chat deployment: the binding filters, it does not grant.
	input := deploymentInput{BindingID: "b-chat"}
	_, err := resolveDeploymentTarget(instance, input, titanEmbedModel, "us-east-1", nil)
	if err == nil {
		t.Fatal("a client-named binding produced capabilities the catalog does not support")
	}
}

func TestResolveUnknownModelDemandsAnExplicitDeclaration(t *testing.T) {
	instance := bedrockInstance(bedrockBinding("b-chat", domain.ProfileBedrockConverseText))
	wanted := domain.ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true}

	if _, err := resolveDeploymentTarget(instance, deploymentInput{Capabilities: &wanted}, "amazon.nova-pro-v1:0", "us-east-1", nil); err == nil {
		t.Fatal("an unknown model was deployed without a declaration")
	} else if !isUnknownCapabilityError(err) {
		t.Fatalf("unexpected error: %v", err)
	}

	resolution, err := resolveDeploymentTarget(instance, deploymentInput{
		Mode: deploymentModeOperatorDeclared, Capabilities: &wanted,
	}, "amazon.nova-pro-v1:0", "us-east-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.declared {
		t.Fatal("declaration was not recorded as such")
	}
	if resolution.capabilities != wanted {
		t.Fatalf("capabilities=%#v", resolution.capabilities)
	}
}

func TestResolveDeclarationStaysInsideTheProfileCeiling(t *testing.T) {
	instance := bedrockInstance(bedrockBinding("b-chat", domain.ProfileBedrockConverseText))
	// Converse carries no tools. An operator declaration is a claim about the
	// model, not permission to widen the profile.
	wanted := domain.ProviderCapabilities{Chat: true, Tools: true}
	if _, err := resolveDeploymentTarget(instance, deploymentInput{
		Mode: deploymentModeOperatorDeclared, Capabilities: &wanted,
	}, "amazon.nova-pro-v1:0", "us-east-1", nil); err == nil {
		t.Fatal("a declaration widened the profile ceiling")
	}
}

func TestResolveDeclarationRequiresACoreOperation(t *testing.T) {
	instance := bedrockInstance(bedrockBinding("b-chat", domain.ProfileBedrockConverseText))
	empty := domain.ProviderCapabilities{}
	if _, err := resolveDeploymentTarget(instance, deploymentInput{
		Mode: deploymentModeOperatorDeclared, Capabilities: &empty,
	}, "amazon.nova-pro-v1:0", "us-east-1", nil); err == nil {
		t.Fatal("a declaration with no core operation was accepted")
	}
	// Enhancements cannot ride alone either.
	orphan := domain.ProviderCapabilities{Streaming: true}
	if _, err := resolveDeploymentTarget(instance, deploymentInput{
		Mode: deploymentModeOperatorDeclared, Capabilities: &orphan,
	}, "amazon.nova-pro-v1:0", "us-east-1", nil); err == nil {
		t.Fatal("streaming without chat was accepted")
	}
}

// Declaring is an act performed once. Editing inside an existing declaration is
// not a new claim, so the console does not have to re-send the word on update.
func TestResolveEditWithinAnExistingDeclarationNeedsNoRedeclaration(t *testing.T) {
	instance := bedrockInstance(bedrockBinding("b-chat", domain.ProfileBedrockConverseText))
	prior := domain.ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true}

	narrowed := domain.ProviderCapabilities{Chat: true}
	resolution, err := resolveDeploymentTarget(instance, deploymentInput{Capabilities: &narrowed}, "amazon.nova-pro-v1:0", "us-east-1", &prior)
	if err != nil {
		t.Fatalf("narrowing an existing declaration was rejected: %v", err)
	}
	if resolution.capabilities != narrowed {
		t.Fatalf("capabilities=%#v", resolution.capabilities)
	}

	// Omitting capabilities keeps what is stored rather than reaching for the
	// profile ceiling.
	unchanged, err := resolveDeploymentTarget(instance, deploymentInput{}, "amazon.nova-pro-v1:0", "us-east-1", &prior)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.capabilities != prior {
		t.Fatalf("capabilities=%#v, want the stored declaration %#v", unchanged.capabilities, prior)
	}

	// Widening past the declaration is a new claim and needs the word again.
	wider := domain.ProviderCapabilities{Chat: true, Streaming: true, StreamUsage: true, DeveloperRole: true}
	if _, err := resolveDeploymentTarget(instance, deploymentInput{Capabilities: &wider}, "amazon.nova-pro-v1:0", "us-east-1", &prior); err == nil {
		t.Fatal("an edit widened a declaration without re-declaring")
	}
}

func TestResolveRejectsStaleModelRevision(t *testing.T) {
	instance := bedrockInstance(bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2))
	entry, ok := modelcatalog.Builtin().Lookup(modelcatalog.Key{
		ProviderType: domain.ProviderBedrock, Profile: domain.ProfileBedrockInvokeTitanEmbedV2, Model: titanEmbedModel,
	})
	if !ok {
		t.Fatal("seed entry missing")
	}
	if _, err := resolveDeploymentTarget(instance, deploymentInput{ModelRevision: entry.Revision()}, titanEmbedModel, "us-east-1", nil); err != nil {
		t.Fatalf("current revision rejected: %v", err)
	}
	_, err := resolveDeploymentTarget(instance, deploymentInput{ModelRevision: "sha256:stale"}, titanEmbedModel, "us-east-1", nil)
	if err == nil {
		t.Fatal("a stale revision was accepted")
	}
	recorder := httptest.NewRecorder()
	(&Runtime{capabilityMetrics: newCapabilityMetrics()}).adminDeploymentInputError(recorder, err)
	if recorder.Code != 409 {
		t.Fatalf("status=%d", recorder.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "model_capability_changed" {
		t.Fatalf("body=%#v", body)
	}
}

// Discovery and creation resolve the same catalog entry from different code
// paths. If they disagreed on the key, every create carrying a revision read
// from the console would conflict, and operators would learn to retry through
// the conflict until it meant nothing.
func TestDiscoveryAndCreateAgreeOnTheModelRevision(t *testing.T) {
	// Both bindings are enabled, and neither discovery nor creation is told
	// which one to use — that is the flow the console now performs.
	instance := bedrockInstance(
		bedrockBinding("b-chat", domain.ProfileBedrockConverseText),
		bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2),
	)
	region := deploymentRegion(instance, deploymentInput{})
	if region == "" {
		t.Fatal("test provider is not regional, so it cannot exercise the mismatch")
	}
	declarations := map[string]domain.ProviderCapabilities{
		// The converse ceiling declares its own context limit; a deployment may
		// narrow a non-zero limit but may not leave it undeclared.
		"amazon.nova-pro-v1:0": {Chat: true, MaxContextTokens: 8192},
	}
	for _, model := range []string{titanEmbedModel, "amazon.nova-pro-v1:0"} {
		var results []bindingCatalog
		for _, binding := range instance.Bindings {
			results = append(results, catalogResult(binding, true, model))
		}
		response := aggregateProviderModels(instance, results, nil)
		listed := findModel(t, response, model)
		input := deploymentInput{ModelRevision: listed.ModelRevision}
		if listed.Status != modelcatalog.StatusKnown {
			input.Mode = deploymentModeOperatorDeclared
			declared := declarations[model]
			input.Capabilities = &declared
		}
		if _, err := resolveDeploymentTarget(instance, input, model, region, nil); err != nil {
			t.Fatalf("model %q: the revision the console read was rejected at create time: %v", model, err)
		}
	}
}

// A profile that accepts exactly one model must not be picked for a different
// one. It used to be, because the pin was checked only after the binding had
// been chosen — so the operator saw a refusal naming a profile they never
// selected, instead of the model simply landing on the profile that serves it.
func TestResolveSkipsProfilesThatPinADifferentModel(t *testing.T) {
	instance := bedrockInstance(
		bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2),
		bedrockBinding("b-chat", domain.ProfileBedrockConverseText),
	)
	declared := domain.ProviderCapabilities{Chat: true, MaxContextTokens: 8192}
	resolution, err := resolveDeploymentTarget(instance, deploymentInput{
		Mode: deploymentModeOperatorDeclared, Capabilities: &declared,
	}, "amazon.nova-pro-v1:0", "us-east-1", nil)
	if err != nil {
		t.Fatalf("a model the converse profile serves was refused: %v", err)
	}
	if resolution.binding.ID != "b-chat" {
		t.Fatalf("resolved binding=%q, want the profile that accepts this model", resolution.binding.ID)
	}

	// With only the pinned profile enabled there is nowhere for it to land, and
	// the refusal must name the pin rather than report a missing binding.
	pinnedOnly := bedrockInstance(bedrockBinding("b-embed", domain.ProfileBedrockInvokeTitanEmbedV2))
	_, err = resolveDeploymentTarget(pinnedOnly, deploymentInput{
		Mode: deploymentModeOperatorDeclared, Capabilities: &declared,
	}, "amazon.nova-pro-v1:0", "us-east-1", nil)
	if err == nil {
		t.Fatal("a pinned profile accepted a model it rejects at request time")
	}
	if !strings.Contains(err.Error(), titanEmbedModel) {
		t.Fatalf("refusal does not say which model the profile requires: %v", err)
	}
}

func isUnknownCapabilityError(err error) bool {
	recorder := httptest.NewRecorder()
	(&Runtime{capabilityMetrics: newCapabilityMetrics()}).adminDeploymentInputError(recorder, err)
	var body map[string]string
	if json.Unmarshal(recorder.Body.Bytes(), &body) != nil {
		return false
	}
	return recorder.Code == 400 && body["code"] == "model_capabilities_unknown"
}
