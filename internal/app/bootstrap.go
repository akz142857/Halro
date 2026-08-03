package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/auth"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/safetransport"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/store/lock"
	"github.com/akz142857/Heimdall/internal/vault"
)

type BootstrapOptions struct {
	ProviderName           string
	ProviderType           domain.ProviderType
	ProviderBaseURL        string
	ProviderAPIVersion     string
	ProviderModel          string
	PublicModel            string
	ProjectName            string
	DailyBudgetMicrosUSD   int64
	InputMicrosPerMillion  int64
	OutputMicrosPerMillion int64
}

type BootstrapResult struct {
	ProjectID    string `json:"project_id"`
	ProviderID   string `json:"provider_id"`
	DeploymentID string `json:"deployment_id"`
	RouteID      string `json:"route_id"`
	KeyID        string `json:"key_id"`
	GatewayKey   string `json:"gateway_key"`
}

func Bootstrap(ctx context.Context, cfg config.Config, options BootstrapOptions, providerSecret []byte) (BootstrapResult, error) {
	if len(providerSecret) == 0 || len(providerSecret) > 16<<10 {
		return BootstrapResult{}, errors.New("provider secret must contain 1 to 16384 bytes")
	}
	if options.ProviderType == "" {
		options.ProviderType = domain.ProviderOpenAI
	}
	endpoint, err := safetransport.ValidateURL(options.ProviderBaseURL, safetransport.Policy{RequireHTTPS: true})
	if err != nil {
		return BootstrapResult{}, err
	}
	audience, err := safetransport.Audience(options.ProviderBaseURL, string(options.ProviderType))
	if err != nil {
		return BootstrapResult{}, err
	}
	dataLock, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return BootstrapResult{}, err
	}
	defer dataLock.Close()
	store, err := boltstore.Open(cfg.MetadataPath())
	if err != nil {
		return BootstrapResult{}, err
	}
	defer store.Close()
	masterKey, err := unlockMasterKey(ctx, cfg)
	if err != nil {
		return BootstrapResult{}, err
	}
	secretVault, err := vault.New(masterKey)
	clear(masterKey)
	if err != nil {
		return BootstrapResult{}, err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		return BootstrapResult{}, err
	}
	credentialID, err := id.New("cred")
	if err != nil {
		return BootstrapResult{}, err
	}
	providerID, err := id.New("prv")
	if err != nil {
		return BootstrapResult{}, err
	}
	deploymentID, err := id.New("dep")
	if err != nil {
		return BootstrapResult{}, err
	}
	routeID, err := id.New("rte")
	if err != nil {
		return BootstrapResult{}, err
	}
	projectID, err := id.New("prj")
	if err != nil {
		return BootstrapResult{}, err
	}
	gatewayPlaintext, gatewayKey, err := auth.GenerateGatewayKey(projectID, "bootstrap", nil)
	if err != nil {
		return BootstrapResult{}, err
	}
	ciphertext, err := secretVault.EncryptCredential(
		credentialID,
		string(options.ProviderType),
		audience,
		providerSecret,
	)
	if err != nil {
		return BootstrapResult{}, err
	}
	now := time.Now().UTC()
	profile, ok := domain.DefaultProviderProfile(options.ProviderType)
	if !ok {
		return BootstrapResult{}, fmt.Errorf("provider profile is not implemented for %q", options.ProviderType)
	}
	capabilities := domain.DefaultProviderCapabilities(options.ProviderType)
	evidence := domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared)
	records := &boltstore.BootstrapRecords{
		Credential: domain.Credential{
			ID: credentialID, Name: options.ProviderName, Type: options.ProviderType,
			AccessSurface: profile.AccessSurface, Scheme: profile.CredentialScheme,
			Audience: audience, Ciphertext: ciphertext, KeyVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Provider: domain.ProviderInstance{
			ID: providerID, Name: options.ProviderName, Type: options.ProviderType,
			AccessSurface: profile.AccessSurface, ProfileID: profile.ProfileID, CredentialScheme: profile.CredentialScheme,
			BaseURL: options.ProviderBaseURL, APIVersion: options.ProviderAPIVersion,
			CredentialID: credentialID,
			AllowedHosts: []string{strings.ToLower(endpoint.Hostname())}, Enabled: true, CreatedAt: now, UpdatedAt: now,
			Capabilities: capabilities, CapabilityEvidence: evidence.Clone(),
		},
		Deployment: domain.Deployment{
			ID: deploymentID, Name: options.ProviderName + " / " + options.ProviderModel,
			ProviderID: providerID, ProviderModel: options.ProviderModel,
			AccessSurface: profile.AccessSurface, ProfileID: profile.ProfileID,
			Capabilities: capabilities, CapabilityEvidence: evidence.Clone(),
			InputMicrosPerMillion:  options.InputMicrosPerMillion,
			OutputMicrosPerMillion: options.OutputMicrosPerMillion,
			Weight:                 1, Enabled: true, CreatedAt: now, UpdatedAt: now,
		},
		Route: domain.Route{
			ID: routeID, PublicModel: options.PublicModel, DeploymentID: deploymentID,
			Enabled: true, CreatedAt: now, UpdatedAt: now,
			Strategy: "ordered",
		},
		Project: domain.Project{
			ID: projectID, Name: options.ProjectName, Enabled: true,
			AllowedRoutes: []string{options.PublicModel}, DailyBudgetMicrosUSD: options.DailyBudgetMicrosUSD,
			MaxInputTokens: 128_000, MaxOutputTokens: 16_384, MaxRequestBytes: cfg.Server.MaxRequestBytes,
			MaxStreamDuration: cfg.Gateway.StreamMaxDuration.Value(), CreatedAt: now, UpdatedAt: now,
		},
		GatewayKey: gatewayKey,
	}
	if err := store.PutBootstrap(ctx, records); err != nil {
		return BootstrapResult{}, fmt.Errorf("persist bootstrap configuration: %w", err)
	}
	return BootstrapResult{
		ProjectID: projectID, ProviderID: providerID, DeploymentID: deploymentID, RouteID: routeID,
		KeyID: gatewayKey.ID, GatewayKey: gatewayPlaintext,
	}, nil
}
