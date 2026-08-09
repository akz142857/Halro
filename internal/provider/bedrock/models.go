package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

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
	OutputModalities        []string `json:"outputModalities"`
	InferenceTypesSupported []string `json:"inferenceTypesSupported"`
	ModelLifecycle          struct {
		Status string `json:"status"`
	} `json:"modelLifecycle"`
}

// ListModels answers which model identifiers this binding can be pointed at.
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
func (a *Adapter) ListModels(ctx context.Context) ([]provider.ModelDescriptor, error) {
	if pinned, ok := pinnedProfileModels[a.profileID]; ok {
		return []provider.ModelDescriptor{{ID: pinned.Model, OwnedBy: pinned.Owner}}, nil
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
		class := provider.ErrorConnect
		if errors.Is(err, context.DeadlineExceeded) {
			class = provider.ErrorTimeout
		}
		return nil, &provider.Error{Class: class, Retryable: true, Message: "Bedrock model catalog request failed", Cause: err}
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
	models := make([]provider.ModelDescriptor, 0, len(catalog.ModelSummaries))
	for _, summary := range catalog.ModelSummaries {
		if !offerableFoundationModel(summary) {
			continue
		}
		models = append(models, provider.ModelDescriptor{
			ID: strings.TrimSpace(summary.ModelID), OwnedBy: strings.TrimSpace(summary.ProviderName),
		})
	}
	return models, nil
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
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(endpoint.Hostname()), "."))
	region = strings.ToLower(region)
	replacements := map[string]string{
		"bedrock-runtime." + region + ".amazonaws.com":         "bedrock." + region + ".amazonaws.com",
		"bedrock-runtime-fips." + region + ".amazonaws.com":    "bedrock-fips." + region + ".amazonaws.com",
		"bedrock-runtime." + region + ".amazonaws.com.cn":      "bedrock." + region + ".amazonaws.com.cn",
		"bedrock-runtime-fips." + region + ".amazonaws.com.cn": "bedrock-fips." + region + ".amazonaws.com.cn",
		"bedrock-runtime." + region + ".api.aws":               "bedrock." + region + ".api.aws",
		"bedrock-runtime-fips." + region + ".api.aws":          "bedrock-fips." + region + ".api.aws",
	}
	controlPlaneHost, ok := replacements[host]
	if !ok {
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
