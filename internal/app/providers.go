package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
	anthropicprovider "github.com/akz142857/Halro/internal/provider/anthropic"
	bedrockprovider "github.com/akz142857/Halro/internal/provider/bedrock"
	bedrockmantleprovider "github.com/akz142857/Halro/internal/provider/bedrockmantle"
	geminiprovider "github.com/akz142857/Halro/internal/provider/gemini"
	openaiprovider "github.com/akz142857/Halro/internal/provider/openai"
	"github.com/akz142857/Halro/internal/safetransport"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

func (r *Runtime) reloadProviderRegistry(ctx context.Context) error {
	next, err := loadProviderRegistry(ctx, r.config, r.store, r.vault)
	if err != nil {
		return err
	}
	retired := r.providers.Replace(next)
	if len(retired) == 0 {
		return nil
	}
	grace := max(
		r.config.Gateway.RouteTotalTimeout.Value(),
		r.config.Gateway.StreamMaxDuration.Value(),
	) + time.Second
	r.backgroundWait.Add(1)
	go func() {
		defer r.backgroundWait.Done()
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.backgroundCtx.Done():
		}
		for _, adapter := range retired {
			adapter.Close()
		}
	}()
	return nil
}

func loadProviderRegistry(
	ctx context.Context,
	cfg config.Config,
	store *boltstore.Store,
	secretVault *vault.Vault,
) (*provider.Registry, error) {
	instances, err := store.ListProviders(ctx)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	routes, err := store.ListRoutes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list routes: %w", err)
	}
	deployments, err := store.ListDeployments(ctx)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	deploymentByID := make(map[string]domain.Deployment, len(deployments))
	for _, deployment := range deployments {
		deploymentByID[deployment.ID] = deployment
	}
	instanceByID := make(map[string]domain.ProviderInstance, len(instances))
	for _, instance := range instances {
		instanceByID[instance.ID] = instance
	}
	registry := provider.NewRegistry()
	pricingSelectedAt := time.Now().UTC()
	adapters := make(map[string]provider.Adapter)
	providerBindingIDs := make(map[string][]string)
	providerLimits := make(map[string]int64)
	fail := func(err error) (*provider.Registry, error) {
		for _, adapter := range adapters {
			adapter.Close()
		}
		return nil, err
	}
	for _, instance := range instances {
		if !instance.Enabled || instance.DeletedAt != nil {
			continue
		}
		credential, err := store.GetCredential(ctx, instance.CredentialID)
		if err != nil {
			return fail(fmt.Errorf("provider %q credential: %w", instance.ID, err))
		}
		if credential.Type != instance.Type {
			return fail(fmt.Errorf("provider %q credential type mismatch", instance.ID))
		}
		policy := safetransport.Policy{
			RequireHTTPS: true,
			AllowPrivate: cfg.Security.AllowPrivateProviderEndpoints,
			AllowedHosts: instance.AllowedHosts,
		}
		endpoint, err := safetransport.ValidateURL(instance.BaseURL, policy)
		if err != nil {
			return fail(fmt.Errorf("provider %q endpoint: %w", instance.ID, err))
		}
		audience, err := safetransport.Audience(instance.BaseURL, string(instance.Type))
		if err != nil {
			return fail(fmt.Errorf("provider %q audience: %w", instance.ID, err))
		}
		if credential.Audience != audience {
			return fail(fmt.Errorf("provider %q credential audience mismatch", instance.ID))
		}
		providerLimits[instance.ID] = instance.MaxConcurrency
		for _, binding := range instance.EffectiveProfileBindings() {
			if !binding.Enabled {
				continue
			}
			manifest, ok := provider.BuiltinProfile(binding.ProfileID)
			if !ok || manifest.ProviderType != instance.Type || manifest.AccessSurface != binding.AccessSurface || manifest.CredentialScheme != binding.CredentialScheme {
				return fail(fmt.Errorf("provider %q binding %q profile is unavailable or incompatible", instance.ID, binding.ID))
			}
			if credential.AccessSurface != binding.AccessSurface || credential.Scheme != binding.CredentialScheme {
				return fail(fmt.Errorf("provider %q binding %q credential profile mismatch", instance.ID, binding.ID))
			}
			plaintext, decryptErr := secretVault.DecryptCredential(credential.ID, string(credential.Type), credential.Audience, credential.Ciphertext)
			if decryptErr != nil {
				return fail(fmt.Errorf("provider %q binding %q decrypt credential: %w", instance.ID, binding.ID, decryptErr))
			}
			adapter, adapterErr := newProviderBindingAdapter(cfg, instance, binding, endpoint, policy, plaintext)
			clear(plaintext)
			if adapterErr != nil {
				return fail(fmt.Errorf("provider %q binding %q adapter: %w", instance.ID, binding.ID, adapterErr))
			}
			profiled, bridgeErr := provider.NewLegacyAdapterBridge(adapter, manifest, binding.CapabilityEvidence)
			if bridgeErr != nil {
				adapter.Close()
				return fail(fmt.Errorf("provider %q binding %q legacy bridge: %w", instance.ID, binding.ID, bridgeErr))
			}
			adapters[binding.ID] = profiled
			providerBindingIDs[instance.ID] = append(providerBindingIDs[instance.ID], binding.ID)
			if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, profiled); err != nil {
				return fail(fmt.Errorf("register provider %q binding %q adapter: %w", instance.ID, binding.ID, err))
			}
		}
	}
	for _, route := range routes {
		if !route.Enabled || route.DeletedAt != nil {
			continue
		}
		providerID := route.ProviderID
		providerModel := route.ProviderModel
		inputPrice := route.InputMicrosPerMillion
		outputPrice := route.OutputMicrosPerMillion
		fixedPrice := int64(0)
		deploymentID := route.DeploymentID
		bindingID := ""
		deploymentLimit := int64(0)
		var capabilities provider.Capabilities
		if deploymentID != "" {
			deployment, exists := deploymentByID[deploymentID]
			if !exists || !deployment.Enabled || deployment.DeletedAt != nil {
				return fail(fmt.Errorf("route %q references an unavailable deployment", route.ID))
			}
			// Drift is resolved here rather than on the request path, so a
			// profile this build narrowed is a state an operator can see instead
			// of production traffic failing one request at a time.
			if instance, ok := instanceByID[deployment.ProviderID]; ok {
				binding, bound := instance.ProfileBinding(deployment.BindingID)
				if !bound {
					binding = domain.ProviderProfileBinding{ProfileID: deployment.ProfileID, Capabilities: instance.Capabilities}
				}
				if !capabilityReviewAdmitsTraffic(evaluateCapabilityReview(deployment, binding, instance.Type)) {
					return fail(fmt.Errorf("route %q references deployment %q whose capability snapshot no longer matches its profile or the catalog; review and retest it", route.ID, deployment.ID))
				}
			}
			providerID = deployment.ProviderID
			bindingID = deployment.BindingID
			if bindingID == "" {
				bindingID = matchingBindingID(instanceByID[providerID], deployment.ProfileID)
			}
			providerModel = deployment.ProviderModel
			price, priceErr := store.SelectDeploymentPriceVersion(ctx, deployment.ID, pricingSelectedAt)
			if priceErr != nil && !errors.Is(priceErr, domain.ErrPriceUnavailable) {
				return fail(fmt.Errorf("deployment %q has no effective versioned price at %s: %w", deployment.ID, pricingSelectedAt.Format(time.RFC3339Nano), priceErr))
			}
			if priceErr == nil {
				inputPrice = price.InputMicrosPerMillion
				outputPrice = price.OutputMicrosPerMillion
				fixedPrice = price.FixedRequestMicrosUSD
			} else {
				inputPrice, outputPrice, fixedPrice = 0, 0, 0
			}
			deploymentLimit = deployment.MaxConcurrency
			capabilities = deploymentCapabilities(deployment, adapters[bindingID])
		}
		if deploymentID == "" {
			bindings := providerBindingIDs[providerID]
			if len(bindings) != 1 {
				return fail(fmt.Errorf("legacy route %q requires a provider with exactly one enabled binding", route.ID))
			}
			bindingID = bindings[0]
		}
		adapter := adapters[bindingID]
		if adapter == nil {
			return fail(fmt.Errorf("route %q references an unavailable provider binding", route.ID))
		}
		if deploymentID == "" {
			capabilities = adapterCapabilitiesFor(adapter)
		}
		if profiled, ok := adapter.(provider.ProfiledAdapter); ok {
			if err := bedrockprovider.ValidateProfileModel(profiled.Profile().ID, providerModel); err != nil {
				return fail(fmt.Errorf("route %q provider model: %w", route.ID, err))
			}
		}
		if err := registry.Register(provider.Target{
			ID:                     route.ID,
			DeploymentID:           deploymentID,
			ProviderID:             providerID,
			BindingID:              bindingID,
			PublicModel:            route.PublicModel,
			ProviderModel:          providerModel,
			AccessSurface:          deploymentByID[deploymentID].AccessSurface,
			ProfileID:              deploymentByID[deploymentID].ProfileID,
			Region:                 deploymentByID[deploymentID].Region,
			Adapter:                adapter,
			InputMicrosPerMillion:  inputPrice,
			OutputMicrosPerMillion: outputPrice,
			FixedRequestMicrosUSD:  fixedPrice,
			Priority:               route.Priority,
			Strategy:               route.Strategy,
			Capabilities:           capabilities,
			CapabilityEvidence:     deploymentByID[deploymentID].CapabilityEvidence.Clone(),
			MaxConcurrency:         providerLimits[providerID],
			DeploymentConcurrency:  deploymentLimit,
		}); err != nil {
			return fail(fmt.Errorf("register route %q: %w", route.ID, err))
		}
	}
	return registry, nil
}

func newProviderBindingAdapter(cfg config.Config, instance domain.ProviderInstance, binding domain.ProviderProfileBinding, endpoint *url.URL, policy safetransport.Policy, plaintext []byte) (provider.Adapter, error) {
	client, err := safetransport.NewClient(safetransport.Options{
		Policy: policy, ConnectTimeout: cfg.Gateway.AttemptConnectTimeout.Value(),
		ResponseHeaderTimeout: cfg.Gateway.AttemptResponseHeaderTimeout.Value(),
	})
	if err != nil {
		return nil, err
	}
	capabilities := providerCapabilities(binding.Capabilities)
	var adapter provider.Adapter
	var authorizer provider.Authorizer
	switch instance.Type {
	case domain.ProviderOpenAI, domain.ProviderOpenAICompatible, domain.ProviderDeepSeek:
		authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "Authorization", "Bearer ", plaintext, "api-key")
		if err == nil {
			adapter, err = openaiprovider.NewWithOptions(openaiprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, ProviderType: string(instance.Type), Capabilities: capabilities})
		}
	case domain.ProviderAnthropic:
		authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "x-api-key", "", plaintext, "Authorization")
		if err == nil {
			adapter, err = anthropicprovider.New(anthropicprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, Capabilities: capabilities})
		}
	case domain.ProviderAzureOpenAI:
		authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "api-key", "", plaintext, "Authorization")
		if err == nil {
			adapter, err = openaiprovider.NewWithOptions(openaiprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, ProviderType: string(instance.Type), APIVersion: instance.APIVersion, Azure: true, Capabilities: capabilities})
		}
	case domain.ProviderGemini:
		authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "x-goog-api-key", "", plaintext, "Authorization")
		if err == nil {
			adapter, err = geminiprovider.New(geminiprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client})
		}
	case domain.ProviderBedrock:
		switch binding.ProfileID {
		case domain.ProfileBedrockConverseText, domain.ProfileBedrockInvokeTitanEmbedV2, domain.ProfileBedrockInvokeTitanImageV2, domain.ProfileBedrockAsyncNovaReel, domain.ProfileBedrockAgentRerankCohere35:
			authorizer, err = bedrockprovider.NewAuthorizer(endpoint, plaintext, nil)
			if err == nil {
				adapter, err = bedrockprovider.New(bedrockprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, ProfileID: binding.ProfileID})
			}
		case domain.ProfileBedrockMantleOpenAIChat:
			err = bedrockmantleprovider.ValidateEndpoint(endpoint)
			if err == nil {
				authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "Authorization", "Bearer ", plaintext, "api-key", "x-api-key")
			}
			if err == nil {
				adapter, err = openaiprovider.NewWithOptions(openaiprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, ProviderType: string(domain.ProviderBedrock), CredentialScheme: binding.CredentialScheme, Capabilities: capabilities})
			}
		case domain.ProfileBedrockMantleOpenAIResponses:
			err = bedrockmantleprovider.ValidateEndpoint(endpoint)
			if err == nil {
				authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "Authorization", "Bearer ", plaintext, "api-key", "x-api-key")
			}
			if err == nil {
				adapter, err = bedrockmantleprovider.NewResponses(bedrockmantleprovider.ResponsesOptions{Endpoint: endpoint, Authorizer: authorizer, Client: client, Capabilities: capabilities})
			}
		case domain.ProfileBedrockMantleAnthropicMessages:
			err = bedrockmantleprovider.ValidateEndpoint(endpoint)
			if err == nil {
				authorizer, err = provider.NewStaticHeaderAuthorizer(binding.CredentialScheme, "x-api-key", "", plaintext, "Authorization", "api-key")
			}
			if err == nil {
				adapter, err = anthropicprovider.New(anthropicprovider.Options{Endpoint: endpoint, Authorizer: authorizer, Client: client, Capabilities: capabilities, ProviderType: string(domain.ProviderBedrock), CredentialScheme: binding.CredentialScheme, MessagesPath: "anthropic/v1/messages"})
			}
		default:
			err = errors.New("Bedrock provider profile is not implemented")
		}
	default:
		err = errors.New("provider type is not implemented")
	}
	if err != nil {
		if authorizer != nil {
			authorizer.Close()
		}
		client.CloseIdleConnections()
		return nil, err
	}
	return adapter, nil
}

func matchingBindingID(instance domain.ProviderInstance, profileID domain.ProviderProfileID) string {
	matched := ""
	for _, binding := range instance.EffectiveProfileBindings() {
		if !binding.Enabled || binding.ProfileID != profileID {
			continue
		}
		if matched != "" {
			return ""
		}
		matched = binding.ID
	}
	return matched
}

func adapterForDeployment(registry *provider.Registry, instance domain.ProviderInstance, deployment domain.Deployment) (provider.Adapter, bool) {
	bindingID := deployment.BindingID
	if bindingID == "" {
		bindingID = matchingBindingID(instance, deployment.ProfileID)
	}
	if adapter, ok := registry.AdapterForBinding(instance.ID, bindingID); ok {
		return adapter, true
	}
	// Compatibility for extension registries that still expose exactly one
	// Provider-keyed adapter. AdapterForProvider fails closed when ambiguous.
	return registry.AdapterForProvider(instance.ID)
}

func providerCapabilities(capabilities domain.ProviderCapabilities) provider.Capabilities {
	return provider.Capabilities{
		Chat: capabilities.Chat, Streaming: capabilities.Streaming, Embeddings: capabilities.Embeddings,
		Moderations: capabilities.Moderations, Images: capabilities.Images, Transcriptions: capabilities.Transcriptions,
		Speech: capabilities.Speech, Files: capabilities.Files, Batches: capabilities.Batches,
		Rerank: capabilities.Rerank, AsyncGenerate: capabilities.AsyncGenerate, Tools: capabilities.Tools,
		Vision: capabilities.Vision, JSONMode: capabilities.JSONMode, DeveloperRole: capabilities.DeveloperRole,
		Reasoning: capabilities.Reasoning, StreamUsage: capabilities.StreamUsage,
		MaxContextTokens: capabilities.MaxContextTokens, MaxOutputTokens: capabilities.MaxOutputTokens,
	}
}

func deploymentCapabilities(deployment domain.Deployment, adapter provider.Adapter) provider.Capabilities {
	available := adapterCapabilitiesFor(adapter)
	declared := deployment.Capabilities
	if !declared.AnyOperation() {
		return available
	}
	return provider.Capabilities{
		Chat:        available.Chat && declared.Chat,
		Streaming:   available.Streaming && declared.Streaming,
		Embeddings:  available.Embeddings && declared.Embeddings,
		Moderations: available.Moderations && declared.Moderations, Images: available.Images && declared.Images, Transcriptions: available.Transcriptions && declared.Transcriptions, Speech: available.Speech && declared.Speech, Files: available.Files && declared.Files, Batches: available.Batches && declared.Batches, Rerank: available.Rerank && declared.Rerank, AsyncGenerate: available.AsyncGenerate && declared.AsyncGenerate,
		Tools:            available.Tools && declared.Tools,
		Vision:           available.Vision && declared.Vision,
		JSONMode:         available.JSONMode && declared.JSONMode,
		DeveloperRole:    available.DeveloperRole && declared.DeveloperRole,
		Reasoning:        available.Reasoning && declared.Reasoning,
		StreamUsage:      available.StreamUsage && declared.StreamUsage,
		MaxContextTokens: minimumCapabilityLimit(available.MaxContextTokens, declared.MaxContextTokens),
		MaxOutputTokens:  minimumCapabilityLimit(available.MaxOutputTokens, declared.MaxOutputTokens),
	}
}

func minimumCapabilityLimit(left, right int64) int64 {
	if left == 0 {
		return right
	}
	if right == 0 || left < right {
		return left
	}
	return right
}

func normalizedProviderCapabilities(instance domain.ProviderInstance) domain.ProviderCapabilities {
	capabilities := instance.Capabilities
	if !capabilities.AnyOperation() {
		return domain.DefaultProviderCapabilitiesForProfile(instance.Type, instance.ProfileID)
	}
	return capabilities
}

func adapterCapabilitiesFor(adapter provider.Adapter) provider.Capabilities {
	if reporter, ok := adapter.(provider.CapabilityReporter); ok {
		return reporter.Capabilities()
	}
	// Custom adapters used by embedders and tests predate capability reporting.
	return provider.Capabilities{Chat: true, Streaming: true, Embeddings: true}
}
