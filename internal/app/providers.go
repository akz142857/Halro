package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/provider"
	bedrockprovider "github.com/akz142857/Heimdall/internal/provider/bedrock"
	geminiprovider "github.com/akz142857/Heimdall/internal/provider/gemini"
	openaiprovider "github.com/akz142857/Heimdall/internal/provider/openai"
	"github.com/akz142857/Heimdall/internal/safetransport"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/vault"
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
	registry := provider.NewRegistry()
	adapters := make(map[string]provider.Adapter)
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
		manifest, ok := provider.BuiltinProfile(instance.ProfileID)
		if !ok || manifest.ProviderType != instance.Type || manifest.AccessSurface != instance.AccessSurface ||
			manifest.CredentialScheme != instance.CredentialScheme {
			return fail(fmt.Errorf("provider %q profile is unavailable or incompatible", instance.ID))
		}
		if credential.AccessSurface != instance.AccessSurface || credential.Scheme != instance.CredentialScheme {
			return fail(fmt.Errorf("provider %q credential profile mismatch", instance.ID))
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
		plaintext, err := secretVault.DecryptCredential(
			credential.ID,
			string(credential.Type),
			credential.Audience,
			credential.Ciphertext,
		)
		if err != nil {
			return fail(fmt.Errorf("provider %q decrypt credential: %w", instance.ID, err))
		}
		client, err := safetransport.NewClient(safetransport.Options{
			Policy:                policy,
			ConnectTimeout:        cfg.Gateway.AttemptConnectTimeout.Value(),
			ResponseHeaderTimeout: cfg.Gateway.AttemptResponseHeaderTimeout.Value(),
		})
		if err != nil {
			clear(plaintext)
			return fail(fmt.Errorf("provider %q transport: %w", instance.ID, err))
		}
		capabilities := normalizedProviderCapabilities(instance)
		adapterCapabilities := provider.Capabilities{
			Chat: capabilities.Chat, Streaming: capabilities.Streaming,
			Embeddings: capabilities.Embeddings, Tools: capabilities.Tools,
			Vision: capabilities.Vision, JSONMode: capabilities.JSONMode,
			DeveloperRole: capabilities.DeveloperRole, Reasoning: capabilities.Reasoning,
			StreamUsage:      capabilities.StreamUsage,
			MaxContextTokens: capabilities.MaxContextTokens, MaxOutputTokens: capabilities.MaxOutputTokens,
		}
		var adapter provider.Adapter
		var authorizer provider.Authorizer
		switch instance.Type {
		case domain.ProviderOpenAI, domain.ProviderOpenAICompatible, domain.ProviderDeepSeek:
			authorizer, err = provider.NewStaticHeaderAuthorizer(instance.CredentialScheme, "Authorization", "Bearer ", plaintext, "api-key")
			if err == nil {
				adapter, err = openaiprovider.NewWithOptions(openaiprovider.Options{
					Endpoint: endpoint, Authorizer: authorizer, Client: client,
					ProviderType: string(instance.Type), Capabilities: adapterCapabilities,
				})
			}
		case domain.ProviderAzureOpenAI:
			authorizer, err = provider.NewStaticHeaderAuthorizer(instance.CredentialScheme, "api-key", "", plaintext, "Authorization")
			if err == nil {
				adapter, err = openaiprovider.NewWithOptions(openaiprovider.Options{
					Endpoint: endpoint, Authorizer: authorizer, Client: client,
					ProviderType: string(instance.Type), APIVersion: instance.APIVersion,
					Azure: true, Capabilities: adapterCapabilities,
				})
			}
		case domain.ProviderGemini:
			authorizer, err = provider.NewStaticHeaderAuthorizer(instance.CredentialScheme, "x-goog-api-key", "", plaintext, "Authorization")
			if err == nil {
				adapter, err = geminiprovider.New(geminiprovider.Options{
					Endpoint: endpoint, Authorizer: authorizer, Client: client,
				})
			}
		case domain.ProviderBedrock:
			authorizer, err = bedrockprovider.NewAuthorizer(endpoint, plaintext, nil)
			if err == nil {
				adapter, err = bedrockprovider.New(bedrockprovider.Options{
					Endpoint: endpoint, Authorizer: authorizer, Client: client,
				})
			}
		default:
			err = errors.New("provider type is not implemented")
		}
		clear(plaintext)
		if err != nil {
			if authorizer != nil {
				authorizer.Close()
			}
			client.CloseIdleConnections()
			return fail(fmt.Errorf("provider %q adapter: %w", instance.ID, err))
		}
		profiled, err := provider.NewLegacyAdapterBridge(adapter, manifest, instance.CapabilityEvidence)
		if err != nil {
			adapter.Close()
			return fail(fmt.Errorf("provider %q legacy bridge: %w", instance.ID, err))
		}
		adapter = profiled
		adapters[instance.ID] = adapter
		providerLimits[instance.ID] = instance.MaxConcurrency
		if err := registry.RegisterAdapter(instance.ID, adapter); err != nil {
			return fail(fmt.Errorf("register provider %q adapter: %w", instance.ID, err))
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
		deploymentID := route.DeploymentID
		deploymentLimit := int64(0)
		var capabilities provider.Capabilities
		if deploymentID != "" {
			deployment, exists := deploymentByID[deploymentID]
			if !exists || !deployment.Enabled || deployment.DeletedAt != nil {
				return fail(fmt.Errorf("route %q references an unavailable deployment", route.ID))
			}
			providerID = deployment.ProviderID
			providerModel = deployment.ProviderModel
			inputPrice = deployment.InputMicrosPerMillion
			outputPrice = deployment.OutputMicrosPerMillion
			deploymentLimit = deployment.MaxConcurrency
			capabilities = deploymentCapabilities(deployment, adapters[providerID])
		}
		adapter := adapters[providerID]
		if adapter == nil {
			return fail(fmt.Errorf("route %q references an unavailable provider", route.ID))
		}
		if deploymentID == "" {
			capabilities = adapterCapabilitiesFor(adapter)
		}
		if err := registry.Register(provider.Target{
			ID:                     route.ID,
			DeploymentID:           deploymentID,
			ProviderID:             providerID,
			PublicModel:            route.PublicModel,
			ProviderModel:          providerModel,
			AccessSurface:          deploymentByID[deploymentID].AccessSurface,
			ProfileID:              deploymentByID[deploymentID].ProfileID,
			Adapter:                adapter,
			InputMicrosPerMillion:  inputPrice,
			OutputMicrosPerMillion: outputPrice,
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

func deploymentCapabilities(deployment domain.Deployment, adapter provider.Adapter) provider.Capabilities {
	available := adapterCapabilitiesFor(adapter)
	declared := deployment.Capabilities
	if !declared.Chat && !declared.Embeddings {
		return available
	}
	return provider.Capabilities{
		Chat:             available.Chat && declared.Chat,
		Streaming:        available.Streaming && declared.Streaming,
		Embeddings:       available.Embeddings && declared.Embeddings,
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
	if !capabilities.Chat && !capabilities.Embeddings {
		return domain.DefaultProviderCapabilities(instance.Type)
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
