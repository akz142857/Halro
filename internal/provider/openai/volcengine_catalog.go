package openai

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// Measured GET /api/v3/models on 2026-11-02: 138 entries, including
// 83 Shutdown and 17 Retiring entries, plus embeddings/image/video models.
// This is a platform inventory, not proof of account activation or Chat support.
// Preserve the upstream lifecycle; absence of status does not establish Active.
// The captured public-model subset lives in testdata/volcengine-models.json.
func applyVolcengineCatalogLifecycle(payload []byte, targets []domain.InvocationTargetDescriptor) error {
	var catalog struct {
		Data []struct {
			ID         string   `json:"id"`
			Status     string   `json:"status"`
			TaskTypes  []string `json:"task_type"`
			Modalities struct {
				Input  []string `json:"input_modalities"`
				Output []string `json:"output_modalities"`
			} `json:"modalities"`
			Limits struct {
				Context   int64 `json:"context_window"`
				Output    int64 `json:"max_output_token_length"`
				Reasoning int64 `json:"max_reasoning_token_length"`
			} `json:"token_limits"`
			Features struct {
				Tools struct {
					FunctionCalling *bool `json:"function_calling"`
				} `json:"tools"`
				Structured struct {
					Object *bool `json:"json_object"`
					Schema *bool `json:"json_schema"`
				} `json:"structured_outputs"`
			} `json:"features"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &catalog); err != nil {
		return err
	}
	indexes := make(map[string]int, len(catalog.Data))
	for index, item := range catalog.Data {
		indexes[strings.TrimSpace(item.ID)] = index
	}
	for index := range targets {
		itemIndex, found := indexes[targets[index].TargetID]
		if !found {
			continue
		}
		targets[index].Availability = domain.AvailabilityUnverified
		item := catalog.Data[itemIndex]
		switch item.Status {
		case "Shutdown":
			targets[index].Lifecycle = domain.TargetLifecycleDeprecated
			targets[index].Availability = domain.AvailabilityUnavailable
		case "Retiring":
			targets[index].Lifecycle = domain.TargetLifecycleDeprecated
		}
		metadata := domain.NormalizedModelMetadata{
			InputModalities: slices.Clone(item.Modalities.Input), OutputModalities: slices.Clone(item.Modalities.Output),
			MaxContextTokens: max(0, item.Limits.Context), MaxOutputTokens: max(0, item.Limits.Output),
		}
		// A text output and a text-generation task establish chat, not the
		// endpoint name or the presence of an arbitrary identifier in the list.
		if slices.Contains(item.TaskTypes, "TextGeneration") && slices.Contains(item.Modalities.Output, "text") {
			metadata.SupportedOperations = append(metadata.SupportedOperations, "chat")
			if slices.Contains(item.Modalities.Input, "image") {
				metadata.SupportedOperations = append(metadata.SupportedOperations, "vision")
			}
			if item.Limits.Reasoning > 0 {
				metadata.SupportedOperations = append(metadata.SupportedOperations, "reasoning")
			}
			for _, feature := range []struct {
				name  string
				value *bool
			}{
				{"tools", item.Features.Tools.FunctionCalling}, {"json_object", item.Features.Structured.Object}, {"structured_outputs", item.Features.Structured.Schema},
			} {
				if feature.value != nil {
					name := feature.name
					if !*feature.value {
						name = "unsupported:" + name
					}
					metadata.SupportedOperations = append(metadata.SupportedOperations, name)
				}
			}
		}
		if len(metadata.SupportedOperations) > 0 || len(metadata.InputModalities) > 0 || len(metadata.OutputModalities) > 0 || metadata.MaxContextTokens > 0 || metadata.MaxOutputTokens > 0 {
			targets[index].MetadataSource = domain.MetadataSourceProvider
			targets[index].Metadata = metadata
		}
	}
	return nil
}

// Only fields populated by the upstream produce claims. Missing features stay
// unknown; explicit false remains negative evidence. Streaming and URL-fetch
// support have no flag in the measured body, so neither is fabricated here.
func (a *Adapter) MapCapabilityClaims(target domain.InvocationTargetDescriptor, scope domain.InvocationTargetScopeKey, at time.Time) []domain.CapabilityClaim {
	if !a.volcengine || target.MetadataSource != domain.MetadataSourceProvider || target.Availability == domain.AvailabilityUnavailable {
		return nil
	}
	var claims []domain.CapabilityClaim
	for _, operation := range target.Metadata.SupportedOperations {
		name := strings.TrimPrefix(operation, "unsupported:")
		if !slices.Contains([]string{"chat", "vision", "reasoning", "tools", "json_object", "structured_outputs"}, name) {
			continue
		}
		status := domain.ClaimSupported
		if strings.HasPrefix(operation, "unsupported:") {
			status = domain.ClaimUnsupported
		}
		expires := at.Add(5 * time.Minute)
		claims = append(claims, domain.CapabilityClaim{CapabilityID: name, Status: status, Evidence: domain.EvidenceDeclared, Source: domain.ClaimSourceProviderMetadata, Scope: scope, ObservedAt: at, ExpiresAt: &expires, Revision: provider.CapabilityClaimRevision("volcengine-model-metadata", target.TargetID, operation)})
	}
	return claims
}
