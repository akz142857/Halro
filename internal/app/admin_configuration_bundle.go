package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

const (
	configurationBundleVersion = 1
	configurationBundleLimit   = 2 << 20
	configurationRecordLimit   = 1000
)

// These are file records, not storage or Admin response records. In particular
// a provider has no credential reference and a key has no hash or plaintext.
type configurationBundle struct {
	Version            int                       `json:"version"`
	Providers          []configurationProvider   `json:"providers"`
	Deployments        []configurationDeployment `json:"deployments"`
	Routes             []configurationRoute      `json:"routes"`
	RedactionPolicies  []configurationRedaction  `json:"redaction_policies"`
	TokenGuardPolicies []configurationTokenGuard `json:"token_guard_policies"`
	Projects           []configurationProject    `json:"projects"`
	GatewayKeys        []configurationKey        `json:"gateway_keys"`
}

type configurationProvider struct {
	ID                    string                 `json:"id"`
	Name                  string                 `json:"name"`
	Type                  domain.ProviderType    `json:"type"`
	BaseURL               string                 `json:"base_url,omitempty"`
	APIVersion            string                 `json:"api_version,omitempty"`
	BedrockProjectID      string                 `json:"bedrock_project_id,omitempty"`
	RequiresEgressProxy   bool                   `json:"requires_egress_proxy,omitempty"`
	AllowedAnthropicBetas []string               `json:"allowed_anthropic_betas,omitempty"`
	Bindings              []configurationBinding `json:"bindings"`
	MaxConcurrency        int64                  `json:"max_concurrency"`
	Enabled               bool                   `json:"enabled"`
}

type configurationBinding struct {
	ProfileID    domain.ProviderProfileID    `json:"profile_id"`
	Capabilities domain.ProviderCapabilities `json:"capabilities"`
	Enabled      bool                        `json:"enabled"`
}

type configurationDeployment struct {
	ID              string                      `json:"id"`
	Name            string                      `json:"name"`
	ProviderID      string                      `json:"provider_id"`
	ProviderModel   string                      `json:"provider_model"`
	CapabilityModel string                      `json:"capability_model,omitempty"`
	TargetKind      domain.DeploymentTargetKind `json:"target_kind,omitempty"`
	ProfileID       domain.ProviderProfileID    `json:"profile_id"`
	Region          string                      `json:"region,omitempty"`
	Capabilities    domain.ProviderCapabilities `json:"capabilities"`
	MaxConcurrency  int64                       `json:"max_concurrency"`
	Enabled         bool                        `json:"enabled"`
	Price           *configurationPrice         `json:"price,omitempty"`
}

type configurationPrice struct {
	BillingMode                 domain.BillingMode    `json:"billing_mode"`
	InputMicrosPerMillion       int64                 `json:"input_micros_per_million"`
	CachedInputMicrosPerMillion int64                 `json:"cached_input_micros_per_million"`
	OutputMicrosPerMillion      int64                 `json:"output_micros_per_million"`
	FixedRequestMicrosUSD       int64                 `json:"fixed_request_micros_usd"`
	Schedule                    *domain.PriceSchedule `json:"schedule,omitempty"`
}

type configurationRoute struct {
	ID string `json:"id"`
	routeInput
}

type configurationRedaction struct {
	ID string `json:"id"`
	redactionPolicyInput
}

type configurationTokenGuard struct {
	ID string `json:"id"`
	tokenGuardInput
}

type configurationProject struct {
	ID string `json:"id"`
	projectInput
}

type configurationKey struct {
	ID        string                `json:"id"`
	ProjectID string                `json:"project_id"`
	Name      string                `json:"name"`
	Scopes    []domain.GatewayScope `json:"scopes"`
	Enabled   bool                  `json:"enabled"`
	ExpiresAt *time.Time            `json:"expires_at,omitempty"`
}

func (r *Runtime) exportAdminConfiguration(writer http.ResponseWriter, request *http.Request) {
	if !requireAdministratorRole(writer, request.Context().Value(adminContextKey{}).(adminRequestContext)) {
		return
	}
	config, err := r.store.ReadPortableConfiguration(request.Context(), time.Now().UTC())
	if err != nil {
		adminStoreError(writer)
		return
	}
	bundle := exportConfiguration(config)
	body, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		adminStoreError(writer)
		return
	}
	if len(body) > configurationBundleLimit || bundle.recordCount() > configurationRecordLimit {
		writeJSON(writer, http.StatusRequestEntityTooLarge, map[string]string{
			"code": "configuration_bundle_too_large", "error": "configuration exceeds the bundle limit",
		})
		return
	}
	if err := r.auditAdminMutation(request, "configuration.export", "configuration", "v1"); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Disposition", `attachment; filename="halro-configuration.json"`)
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(body)
}

func exportConfiguration(config boltstore.PortableConfiguration) configurationBundle {
	bundle := configurationBundle{
		Version: configurationBundleVersion, Providers: []configurationProvider{}, Deployments: []configurationDeployment{},
		Routes: []configurationRoute{}, RedactionPolicies: []configurationRedaction{}, TokenGuardPolicies: []configurationTokenGuard{},
		Projects: []configurationProject{}, GatewayKeys: []configurationKey{},
	}
	for _, item := range config.Providers {
		if item.DeletedAt != nil {
			continue
		}
		provider := configurationProvider{
			ID: item.ID, Name: item.Name, Type: item.Type, BaseURL: portableEndpoint(item.BaseURL),
			APIVersion: item.APIVersion, BedrockProjectID: item.BedrockProjectID,
			RequiresEgressProxy: item.EgressProxyID != "", AllowedAnthropicBetas: item.AllowedAnthropicBetas,
			MaxConcurrency: item.MaxConcurrency, Enabled: item.Enabled,
		}
		for _, binding := range item.EffectiveProfileBindings() {
			provider.Bindings = append(provider.Bindings, configurationBinding{
				ProfileID: binding.ProfileID, Capabilities: binding.Capabilities, Enabled: binding.Enabled,
			})
		}
		bundle.Providers = append(bundle.Providers, provider)
	}
	for _, item := range config.Deployments {
		if item.DeletedAt != nil {
			continue
		}
		deployment := configurationDeployment{
			ID: item.ID, Name: item.Name, ProviderID: item.ProviderID, ProviderModel: item.ProviderModel,
			TargetKind: item.TargetKind, ProfileID: item.ProfileID, Region: item.Region,
			Capabilities: item.Capabilities, MaxConcurrency: item.MaxConcurrency, Enabled: item.Enabled,
		}
		if item.ModelCapabilitySnapshot.CanonicalModelRef != item.ProviderModel &&
			(item.TargetKind == domain.TargetAzureDeployment || item.TargetKind == domain.TargetCustomEndpointModel) {
			deployment.CapabilityModel = item.ModelCapabilitySnapshot.CanonicalModelRef
		}
		if price, ok := config.Prices[item.ID]; ok {
			deployment.Price = &configurationPrice{
				BillingMode: price.BillingMode, InputMicrosPerMillion: price.InputMicrosPerMillion,
				CachedInputMicrosPerMillion: price.CachedInputMicrosPerMillion, OutputMicrosPerMillion: price.OutputMicrosPerMillion,
				FixedRequestMicrosUSD: price.FixedRequestMicrosUSD, Schedule: price.Schedule,
			}
		}
		bundle.Deployments = append(bundle.Deployments, deployment)
	}
	for _, item := range config.Routes {
		if item.DeletedAt == nil {
			bundle.Routes = append(bundle.Routes, configurationRoute{ID: item.ID, routeInput: routeInput{
				PublicModel: item.PublicModel, DeploymentID: item.DeploymentID, Priority: item.Priority,
				Strategy: item.Strategy, Enabled: item.Enabled,
			}})
		}
	}
	for _, item := range config.RedactionPolicies {
		if item.DeletedAt != nil {
			continue
		}
		settings := redactionPolicyInput{Name: item.Name, Enabled: item.Enabled, Mode: item.Mode, Rules: []redactionRuleInput{}}
		for _, rule := range item.Rules {
			settings.Rules = append(settings.Rules, redactionRuleInput{
				ID: rule.ID, Name: rule.Name, Kind: rule.Kind, Builtin: rule.Builtin, Pattern: rule.Pattern,
				Dictionary: rule.Dictionary, Scopes: rule.Scopes, Action: rule.Action, Replacement: rule.Replacement,
				Enabled: rule.Enabled, Priority: rule.Priority,
			})
		}
		bundle.RedactionPolicies = append(bundle.RedactionPolicies, configurationRedaction{ID: item.ID, redactionPolicyInput: settings})
	}
	for _, item := range config.TokenGuardPolicies {
		if item.DeletedAt == nil {
			bundle.TokenGuardPolicies = append(bundle.TokenGuardPolicies, configurationTokenGuard{ID: item.ID, tokenGuardInput: tokenGuardSettings(item)})
		}
	}
	for _, item := range config.Projects {
		if item.DeletedAt != nil {
			continue
		}
		cidrs := make([]string, 0, len(item.AllowedCIDRs))
		for _, prefix := range item.AllowedCIDRs {
			cidrs = append(cidrs, prefix.String())
		}
		bundle.Projects = append(bundle.Projects, configurationProject{ID: item.ID, projectInput: projectInput{
			Name: item.Name, Enabled: item.Enabled, AllowedModels: append([]string{}, item.AllowedModels...),
			RPM: item.RPM, TPM: item.TPM, MaxConcurrency: item.MaxConcurrency, DailyBudgetMicrosUSD: item.DailyBudgetMicrosUSD,
			MaxInputTokens: item.MaxInputTokens, MaxOutputTokens: item.MaxOutputTokens, MaxRequestBytes: item.MaxRequestBytes,
			MaxStreamDurationSeconds: int64(item.MaxStreamDuration / time.Second), DeferredResponses: item.DeferredResponses,
			MaxDeferredQueue: item.MaxDeferredQueue, AllowedCIDRs: cidrs, RedactionPolicyID: item.RedactionPolicyID,
			TokenGuardPolicyID: item.TokenGuardPolicyID, RunGovernance: &item.RunGovernance,
		}})
	}
	for _, item := range config.GatewayKeys {
		if item.DeletedAt == nil {
			bundle.GatewayKeys = append(bundle.GatewayKeys, configurationKey{
				ID: item.ID, ProjectID: item.ProjectID, Name: item.Name, Enabled: item.Enabled,
				Scopes: domain.EffectiveGatewayScopes(item.Scopes), ExpiresAt: item.ExpiresAt,
			})
		}
	}
	return bundle
}

// Reject URL components commonly used to carry credentials. A bundle without
// an endpoint can still be imported by supplying a target endpoint.
func portableEndpoint(raw string) string {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.RawFragment != "" {
		return ""
	}
	return raw
}

func (b configurationBundle) recordCount() int {
	return len(b.Providers) + len(b.Deployments) + len(b.Routes) + len(b.RedactionPolicies) +
		len(b.TokenGuardPolicies) + len(b.Projects) + len(b.GatewayKeys)
}
