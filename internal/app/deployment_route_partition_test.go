package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/provider"
)

// The model and route this whole file is about. Bedrock Mantle serves
// openai.gpt-5.6-sol from /openai/v1 and refuses it on /v1, and the two routes
// are two profiles — so a deployment on the default-route profile is a request
// the upstream will always refuse. See internal/modelcatalog/builtin.go.
const mantleOpenAIRouteModel = "openai.gpt-5.6-sol"

// The surface travels with the binding in a stored connection, and the target
// kind is derived from it — a fixture that leaves it blank resolves as Bedrock
// Runtime and misses the Mantle catalogue entirely.
func mantleBinding(id string, profile domain.ProviderProfileID) domain.ProviderProfileBinding {
	binding := bedrockBinding(id, profile)
	binding.AccessSurface = domain.SurfaceBedrockMantle
	binding.CredentialScheme = domain.CredentialBedrockAPIKey
	return binding
}

func mantleInstance(bindings ...domain.ProviderProfileBinding) domain.ProviderInstance {
	return domain.ProviderInstance{
		ID: "prov_mantle", Type: domain.ProviderBedrock, AccessSurface: domain.SurfaceBedrockMantle,
		BaseURL: "https://bedrock-mantle.us-east-2.api.aws", Enabled: true, Bindings: bindings,
	}
}

func TestDeploymentOnTheWrongMantleRouteIsRefusedWithTheRouteThatServesIt(t *testing.T) {
	instance := mantleInstance(mantleBinding("b-chat", domain.ProfileBedrockMantleChat))
	_, err := resolveDeploymentTarget(instance, deploymentInput{BindingID: "b-chat", Enabled: true}, mantleOpenAIRouteModel, "us-east-2", nil)
	if !errors.Is(err, errModelNotServedByProfile) {
		t.Fatalf("err=%v, want errModelNotServedByProfile", err)
	}
	// The refusal is only useful if it names the edit.
	if !strings.Contains(err.Error(), string(domain.ProfileBedrockMantleOpenAIChat)) {
		t.Fatalf("refusal does not name the serving profile: %v", err)
	}
}

// Declaring capabilities is the escape hatch for a model nobody has established
// anything about. It is not an escape hatch from a route the upstream refuses,
// because no declaration changes what the upstream does.
func TestOperatorDeclarationCannotOverrideAWrongMantleRoute(t *testing.T) {
	instance := mantleInstance(mantleBinding("b-chat", domain.ProfileBedrockMantleChat))
	declared := domain.ProviderCapabilities{Chat: true, Streaming: true, MaxContextTokens: 272_000}
	input := deploymentInput{BindingID: "b-chat", Mode: deploymentModeOperatorDeclared, Capabilities: &declared, Enabled: true}
	if _, err := resolveDeploymentTarget(instance, input, mantleOpenAIRouteModel, "us-east-2", nil); !errors.Is(err, errModelNotServedByProfile) {
		t.Fatalf("err=%v, want errModelNotServedByProfile", err)
	}
}

func TestDeploymentOnTheRightMantleRouteResolves(t *testing.T) {
	instance := mantleInstance(mantleBinding("b-openai-chat", domain.ProfileBedrockMantleOpenAIChat))
	resolution, err := resolveDeploymentTarget(instance, deploymentInput{BindingID: "b-openai-chat", Enabled: true}, mantleOpenAIRouteModel, "us-east-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.binding.ID != "b-openai-chat" {
		t.Fatalf("resolved binding=%q", resolution.binding.ID)
	}
	if resolution.declared {
		t.Fatal("a catalog-backed model was recorded as operator declared")
	}
}

// The refusal excludes a binding rather than ending the resolution, so a
// connection holding both routes still resolves to the one that serves the
// model instead of reporting a failure the operator did not cause.
func TestAutomaticSelectionSkipsTheRouteThatRefusesTheModel(t *testing.T) {
	instance := mantleInstance(
		mantleBinding("b-chat", domain.ProfileBedrockMantleChat),
		mantleBinding("b-openai-chat", domain.ProfileBedrockMantleOpenAIChat),
	)
	resolution, err := resolveDeploymentTarget(instance, deploymentInput{Enabled: true}, mantleOpenAIRouteModel, "us-east-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.binding.ID != "b-openai-chat" {
		t.Fatalf("resolved binding=%q, want the /openai/v1 route", resolution.binding.ID)
	}
}

// A Mantle model the catalogue places nowhere is still the operator's to
// declare: absence is only evidence of a refusal when the catalogue covers the
// model somewhere else. zai.glm-4.6 is exactly this case — the account lists it
// and builtin.go deliberately omits it for want of a context window.
func TestAModelNoProfileCoversStaysDeclarableOnMantle(t *testing.T) {
	instance := mantleInstance(mantleBinding("b-chat", domain.ProfileBedrockMantleChat))
	declared := domain.ProviderCapabilities{Chat: true, Streaming: true, MaxContextTokens: 128_000}
	input := deploymentInput{BindingID: "b-chat", Mode: deploymentModeOperatorDeclared, Capabilities: &declared, Enabled: true}
	resolution, err := resolveDeploymentTarget(instance, input, "zai.glm-4.6", "us-east-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.declared {
		t.Fatal("an uncovered model resolved as something other than an operator declaration")
	}
}

// The same absence on a profile that is not route-partitioned means what it has
// always meant. kimi-k2.7-code is catalogued on Kimi's Chat face alone, because
// nothing establishes what its Messages face does with it — not because Kimi
// refuses it there.
func TestUncataloguedModelOnANonPartitionedProfileIsStillDeclarable(t *testing.T) {
	instance := domain.ProviderInstance{
		ID: "prov_kimi", Type: domain.ProviderKimi, AccessSurface: domain.SurfaceKimi,
		BaseURL: "https://api.moonshot.ai", Enabled: true,
		Bindings: []domain.ProviderProfileBinding{
			kimiTestBinding("b-kimi-anthropic", domain.ProfileKimiAnthropicMessages),
		},
	}
	declared := domain.ProviderCapabilities{Chat: true, Streaming: true, MaxContextTokens: 262_144}
	input := deploymentInput{BindingID: "b-kimi-anthropic", Mode: deploymentModeOperatorDeclared, Capabilities: &declared, Enabled: true}
	resolution, err := resolveDeploymentTarget(instance, input, "kimi-k2.7-code", "", nil)
	if err != nil {
		t.Fatalf("a declarable model on a non-partitioned profile was refused: %v", err)
	}
	if !resolution.declared {
		t.Fatal("declared model did not resolve as an operator declaration")
	}
}

func kimiTestBinding(id string, profile domain.ProviderProfileID) domain.ProviderProfileBinding {
	binding := testBinding(id, domain.ProviderKimi, profile)
	binding.AccessSurface = domain.SurfaceKimi
	binding.CredentialScheme = domain.CredentialBearerStatic
	return binding
}

// Switching a wrong-route deployment off has to stay possible: the gate is on
// traffic, not on the record. An operator who cannot fix the route in this edit
// still has to be able to take the thing out of service.
func TestAWrongRouteDeploymentCanStillBeTakenOutOfService(t *testing.T) {
	instance := mantleInstance(mantleBinding("b-chat", domain.ProfileBedrockMantleChat))
	declared := domain.ProviderCapabilities{Chat: true, Streaming: true, MaxContextTokens: 272_000}
	input := deploymentInput{BindingID: "b-chat", Mode: deploymentModeOperatorDeclared, Capabilities: &declared, Enabled: false}
	if _, err := resolveDeploymentTarget(instance, input, mantleOpenAIRouteModel, "us-east-2", nil); err != nil {
		t.Fatalf("a disabled deployment on the refusing route could not be saved: %v", err)
	}
}

// The connection test is the other half: a deployment created before this check
// existed still has to stop reporting healthy. Mantle's model list enumerates
// the account rather than the route, so only the catalogue can answer this, and
// it answers before anything is dialled.
func TestConnectionTestRefusesTheWrongRouteBeforeDialling(t *testing.T) {
	instance := mantleInstance(mantleBinding("b-chat", domain.ProfileBedrockMantleChat))
	deployment := domain.Deployment{
		ID: "dep_1", ProviderID: instance.ID, ProviderModel: mantleOpenAIRouteModel,
		AccessSurface: domain.SurfaceBedrockMantle, ProfileID: domain.ProfileBedrockMantleChat,
		BindingID: "b-chat", Region: "us-east-2",
	}
	err := deploymentRouteRefusal(modelcatalog.Builtin(), instance, deployment)
	if err == nil {
		t.Fatal("the connection test would have dialled a route that refuses this model")
	}
	var classified *provider.Error
	if !errors.As(err, &classified) || classified.Class != provider.ErrorBadRequest {
		t.Fatalf("probe refusal is not classified as a bad request: %v", err)
	}

	deployment.ProfileID = domain.ProfileBedrockMantleOpenAIChat
	if err := deploymentRouteRefusal(modelcatalog.Builtin(), instance, deployment); err != nil {
		t.Fatalf("the serving route was refused: %v", err)
	}
}
