package bedrock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// modelLifecycleActive is the only lifecycle status a model may be offered
// under. A legacy or deprecated model still appears in the control-plane list;
// offering it would put an identifier in front of an operator that the runtime
// is about to stop accepting.
const modelLifecycleActive = "ACTIVE"

// invocableInferenceTypes are the inference types a deployment can actually be
// pointed at by model identifier. A model available only as PROVISIONED needs a
// provisioned-throughput ARN, which is a different deployment target kind and a
// resource the operator has to have bought — so it is not offered here. Typing
// the ARN by hand still works.
var invocableInferenceTypes = []string{"ON_DEMAND", "INFERENCE_PROFILE"}

type foundationModelSummary struct {
	ModelID                 string   `json:"modelId"`
	ProviderName            string   `json:"providerName"`
	InputModalities         []string `json:"inputModalities"`
	OutputModalities        []string `json:"outputModalities"`
	InferenceTypesSupported []string `json:"inferenceTypesSupported"`
	ModelLifecycle          struct {
		Status string `json:"status"`
	} `json:"modelLifecycle"`
}

// ListInvocationTargets answers which invocation identities this binding can
// actually be pointed at.
//
// A profile that accepts exactly one model has no question to ask upstream, and
// asking would be worse than not asking: listing a hundred foundation models
// under a profile that rejects all but one offers the operator ninety-nine
// choices that cannot be created. Those profiles answer from the pin, which is
// also what finally makes the catalog-covered Bedrock models reachable in the
// console — they are the only models the builtin catalog establishes.
//
// Converse is the profile with a real question, and its answer lives on the
// Bedrock control plane, not the runtime endpoint this adapter is bound to.
// That is a second host. It has to pass the provider's own allowed-hosts policy
// like any other outbound call, and when it does not, discovery fails and the
// console falls back to manual model entry — never a silent call to a host the
// operator did not approve.
func (a *Adapter) InvocationTargetDiscovery() domain.InvocationTargetDiscoveryCapabilities {
	kinds := []domain.DeploymentTargetKind{domain.TargetBedrockFoundationModel}
	if a.profileID == domain.ProfileBedrockConverseText {
		kinds = append(kinds, domain.TargetBedrockInferenceProfile, domain.TargetBedrockProvisionedThroughput)
	}
	return domain.InvocationTargetDiscoveryCapabilities{TargetKinds: kinds, CanEnumerate: true, CanVerify: true}
}

func (a *Adapter) ListInvocationTargets(ctx context.Context, query domain.TargetQuery) ([]domain.InvocationTargetDescriptor, error) {
	if pinned, ok := pinnedProfileModels[a.profileID]; ok {
		region := strings.TrimSpace(query.Region)
		if region == "" {
			region = bedrockTargetRegion(a.controlPlaneEndpoint)
		}
		if region == "" {
			region = bedrockTargetRegion(a.endpoint)
		}
		return []domain.InvocationTargetDescriptor{{
			TargetID: pinned.Model, TargetKind: domain.TargetBedrockFoundationModel, DisplayName: pinned.Model,
			OwnedBy: pinned.Owner, CanonicalModelRef: pinned.Model, Region: region,
			Lifecycle: domain.TargetLifecycleActive, MetadataSource: domain.MetadataSourceNone,
			Availability: domain.AvailabilityAvailable, FetchedAt: time.Now().UTC(),
		}}, nil
	}
	if a.controlPlane == nil {
		return nil, &provider.Error{
			Class:   provider.ErrorBadRequest,
			Message: "Bedrock model discovery requires a regional control-plane endpoint for this connection",
		}
	}
	endpoint := *a.controlPlaneEndpoint
	endpoint.Path = "/foundation-models"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, &provider.Error{Class: provider.ErrorBadRequest, Message: "create Bedrock model catalog request", Cause: err}
	}
	request.Header.Set("Accept", "application/json")
	if err := a.controlPlane.Authorize(request, nil); err != nil {
		return nil, &provider.Error{Class: provider.ErrorAuthentication, Message: "sign Bedrock model catalog request", Cause: err}
	}
	response, err := a.client.Do(request)
	if err != nil {
		class := provider.TransportClass(err)
		return nil, &provider.Error{Class: class, Retryable: class != provider.ErrorCanceled, Message: "Bedrock model catalog request failed", Cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyHTTP(response)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(payload) > maxResponseBytes {
		return nil, &provider.Error{Class: provider.ErrorMalformed, Message: "read Bedrock model catalog response", Cause: err}
	}
	var catalog struct {
		ModelSummaries []foundationModelSummary `json:"modelSummaries"`
	}
	if err := json.Unmarshal(payload, &catalog); err != nil {
		return nil, &provider.Error{Class: provider.ErrorMalformed, Message: "decode Bedrock model catalog response", Cause: err}
	}
	now := time.Now().UTC()
	models := make([]domain.InvocationTargetDescriptor, 0, len(catalog.ModelSummaries))
	for _, summary := range catalog.ModelSummaries {
		if !offerableFoundationModel(summary) {
			continue
		}
		id := strings.TrimSpace(summary.ModelID)
		models = append(models, domain.InvocationTargetDescriptor{
			TargetID: id, TargetKind: domain.TargetBedrockFoundationModel, DisplayName: id,
			OwnedBy: strings.TrimSpace(summary.ProviderName), CanonicalModelRef: id,
			Region: bedrockTargetRegion(a.controlPlaneEndpoint), Lifecycle: domain.TargetLifecycleActive,
			MetadataSource: domain.MetadataSourceProvider, Availability: domain.AvailabilityAvailable, FetchedAt: now,
			Metadata: domain.NormalizedModelMetadata{
				InputModalities: slices.Clone(summary.InputModalities), OutputModalities: slices.Clone(summary.OutputModalities),
				InferenceTypes: slices.Clone(summary.InferenceTypesSupported),
			},
		})
	}
	return models, nil
}

func (a *Adapter) MapCapabilityClaims(target domain.InvocationTargetDescriptor, scope domain.InvocationTargetScopeKey, observedAt time.Time) []domain.CapabilityClaim {
	var capabilityIDs []string
	if containsFold(target.Metadata.InputModalities, "TEXT") && containsFold(target.Metadata.OutputModalities, "TEXT") {
		capabilityIDs = append(capabilityIDs, "chat")
	}
	if containsFold(target.Metadata.InputModalities, "IMAGE") && containsFold(target.Metadata.OutputModalities, "TEXT") {
		capabilityIDs = append(capabilityIDs, "vision")
	}
	claims := make([]domain.CapabilityClaim, 0, len(capabilityIDs))
	for _, capabilityID := range capabilityIDs {
		claims = append(claims, domain.CapabilityClaim{
			CapabilityID: capabilityID, Status: domain.ClaimSupported, Evidence: domain.EvidenceDeclared,
			Source: domain.ClaimSourceProviderMetadata, Scope: scope, ObservedAt: observedAt,
			Revision: provider.CapabilityClaimRevision(string(domain.ClaimSourceProviderMetadata), target.TargetID, capabilityID),
		})
	}
	return claims
}

func containsFold(values []string, wanted string) bool {
	return slices.ContainsFunc(values, func(value string) bool { return strings.EqualFold(value, wanted) })
}

func bedrockTargetRegion(endpoint *url.URL) string {
	if endpoint == nil {
		return ""
	}
	parts := strings.Split(endpoint.Hostname(), ".")
	for index, part := range parts {
		switch strings.ToLower(part) {
		case "bedrock", "bedrock-fips", "bedrock-runtime", "bedrock-runtime-fips", "bedrock-agent-runtime":
			if index+1 < len(parts) {
				return parts[index+1]
			}
		}
	}
	return ""
}

// offerableFoundationModel decides whether a control-plane summary describes a
// model this profile can be pointed at. Everything excluded here is excluded
// because creating a deployment for it would fail later, not because the model
// is uninteresting.
func offerableFoundationModel(summary foundationModelSummary) bool {
	if strings.TrimSpace(summary.ModelID) == "" {
		return false
	}
	if !strings.EqualFold(summary.ModelLifecycle.Status, modelLifecycleActive) {
		return false
	}
	// The Converse text profile translates text output and nothing else, so a
	// model that emits only images or video has no path through it.
	if !slices.ContainsFunc(summary.OutputModalities, func(modality string) bool {
		return strings.EqualFold(modality, "TEXT")
	}) {
		return false
	}
	return slices.ContainsFunc(summary.InferenceTypesSupported, func(inference string) bool {
		return slices.ContainsFunc(invocableInferenceTypes, func(allowed string) bool {
			return strings.EqualFold(inference, allowed)
		})
	})
}

// controlPlaneEndpointFor derives the regional Bedrock control-plane endpoint
// from the runtime endpoint the connection is already bound to.
//
// It is a derivation rather than a second configured URL on purpose: an
// operator who has approved `bedrock-runtime.<region>.amazonaws.com` has said
// which AWS partition and region this connection reaches, and discovery must
// not be able to leave it. Anything that is not one of the approved public
// runtime forms — a PrivateLink endpoint, an agent-runtime host — yields no
// control-plane endpoint at all, because there is no derivation that stays
// inside what the operator approved.
func controlPlaneEndpointFor(endpoint *url.URL, region string) (*url.URL, bool) {
	if endpoint == nil {
		return nil, false
	}
	controlPlaneHost, hostRegion, ok := ControlPlaneHostFor(endpoint.Hostname())
	// The region has to match the credential's, not merely be present: the
	// derivation stays inside the one region and partition the operator approved.
	if !ok || hostRegion != strings.ToLower(strings.TrimSpace(region)) {
		return nil, false
	}
	derived := *endpoint
	derived.Host = controlPlaneHost
	derived.Path = ""
	derived.RawPath = ""
	derived.RawQuery = ""
	derived.Fragment = ""
	return &derived, true
}

// ControlPlaneHostFor derives the control-plane host that model discovery reads,
// and the region it belongs to, from an approved runtime host.
//
// It is exported because the derived host also has to be on the transport
// policy's allowlist. The allowlist is built from the provider record, which
// holds the runtime host alone, so discovery signed a correct request against a
// host its own client would not dial: not a bypass, but fail-closed far enough
// to make the feature silently do nothing.
//
// Anything that is not one of the approved public runtime forms — a PrivateLink
// endpoint, an agent-runtime host — derives nothing, which is what keeps this a
// derivation rather than a second configurable endpoint.
func ControlPlaneHostFor(runtimeHost string) (host, region string, ok bool) {
	candidate := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(runtimeHost), "."))
	for prefix, replacement := range map[string]string{
		"bedrock-runtime.":      "bedrock.",
		"bedrock-runtime-fips.": "bedrock-fips.",
	} {
		rest, found := strings.CutPrefix(candidate, prefix)
		if !found {
			continue
		}
		region, suffix, split := strings.Cut(rest, ".")
		if !split || region == "" {
			return "", "", false
		}
		if !slices.Contains([]string{"amazonaws.com", "amazonaws.com.cn", "api.aws"}, suffix) {
			return "", "", false
		}
		return replacement + rest, region, true
	}
	return "", "", false
}
