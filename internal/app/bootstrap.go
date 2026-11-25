package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/modelcatalog"
	"github.com/akz142857/Halro/internal/safetransport"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/vault"
)

type BootstrapOptions struct {
	ProviderName          string
	ProviderType          domain.ProviderType
	ProviderBaseURL       string
	ProviderAPIVersion    string
	ProviderModel         string
	PublicModel           string
	ProjectName           string
	DailyBudgetMicrosUSD  int64
	BillingMode           domain.BillingMode
	InputMicrosPerMillion int64
	// CachedInputMicrosPerMillion prices prompt tokens the provider serves from
	// its own cache. Nil means the operator did not state one, and bootstrap then
	// charges cached tokens at the ordinary input rate — the conservative
	// reading, and the one that matches what they asked for when they named a
	// single input price. A pointer rather than a sentinel so that omitting the
	// field cannot mean "cached tokens are free".
	CachedInputMicrosPerMillion *int64
	// The cache-write rates follow the same rule: nil bills writes at the
	// ordinary input rate, which is what they cost before write rates existed,
	// and never at zero.
	CacheWriteMicrosPerMillion     *int64
	CacheWriteLongMicrosPerMillion *int64
	OutputMicrosPerMillion         int64
}

// rates resolves the operator's stated prices into a full rate table, filling
// every rate they did not state with the ordinary input rate.
func (options BootstrapOptions) rates() domain.PriceTier {
	stated := func(value *int64) int64 {
		if value == nil {
			return options.InputMicrosPerMillion
		}
		return *value
	}
	return domain.PriceTier{
		InputMicrosPerMillion:          options.InputMicrosPerMillion,
		CachedInputMicrosPerMillion:    stated(options.CachedInputMicrosPerMillion),
		CacheWriteMicrosPerMillion:     stated(options.CacheWriteMicrosPerMillion),
		CacheWriteLongMicrosPerMillion: stated(options.CacheWriteLongMicrosPerMillion),
		CacheWriteRateEvidence: domain.CacheWriteRateEvidence{
			ShortExplicit: options.CacheWriteMicrosPerMillion != nil, LongExplicit: options.CacheWriteLongMicrosPerMillion != nil,
		},
		OutputMicrosPerMillion: options.OutputMicrosPerMillion,
	}
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
	// Bootstrap used to hard-code the strictest policy, so it refused a private
	// endpoint even where configuration allowed one — a second spelling of the
	// same rule, disagreeing with the Admin API.
	policy := providerEndpointPolicy(cfg)
	endpoint, err := safetransport.ValidateURL(options.ProviderBaseURL, policy)
	if err != nil {
		return BootstrapResult{}, err
	}
	audience, err := safetransport.AudienceWithPolicy(options.ProviderBaseURL, string(options.ProviderType), policy)
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
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return BootstrapResult{}, err
	}
	secretVault, err := vault.New(masterKey)
	if err != nil {
		clear(masterKey)
		return BootstrapResult{}, err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		clear(masterKey)
		return BootstrapResult{}, err
	}
	// Bootstrap writes a Credential, Provider, Deployment, Route, Project and
	// Gateway Key — every one of them authoritative, so it goes through the
	// same entry a running instance does.
	_, journalErr := attachMetadataJournal(store, secretVault, masterKey, "bootstrap")
	clear(masterKey)
	if journalErr != nil {
		return BootstrapResult{}, journalErr
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
	priceID, err := id.New("price")
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
	// The bootstrap key exists so an operator can prove the instance serves
	// their application, and the first call most SDK clients make is
	// models.list(). It is granted discovery explicitly rather than by widening
	// the unset-scope default, so every key an operator mints afterwards is
	// still a deliberate choice about what it may enumerate.
	gatewayKey.Scopes = []domain.GatewayScope{domain.GatewayScopeInference, domain.GatewayScopeDiscovery}
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
	billingMode := options.BillingMode
	rates := options.rates()
	if billingMode == "" && (rates.InputMicrosPerMillion > 0 || rates.OutputMicrosPerMillion > 0 ||
		rates.CachedInputMicrosPerMillion > 0 || rates.CacheWriteMicrosPerMillion > 0 || rates.CacheWriteLongMicrosPerMillion > 0) {
		billingMode = domain.BillingModeMetered
	}
	if billingMode == "" {
		return BootstrapResult{}, errors.New("billing mode is required when bootstrap prices are zero; explicitly choose free or metered")
	}
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
			// Bootstrap is the operator naming a provider and a model and asking
			// for it to work. That is a declaration, and the snapshot records it
			// as one rather than dressing it up as catalog evidence.
			ModelCapabilitySnapshot: domain.DeclaredCapabilitySnapshot(
				options.ProviderModel, bootstrapModelRevision(options.ProviderType, profile.ProfileID, options.ProviderModel),
				capabilities, now,
			),
			Enabled: true, CreatedAt: now, UpdatedAt: now,
		},
		Price: bootstrapPriceVersion(priceID, deploymentID, billingMode, rates, now),
		Route: domain.Route{
			ID: routeID, PublicModel: options.PublicModel, DeploymentID: deploymentID,
			Enabled: true, CreatedAt: now, UpdatedAt: now,
			Strategy: "ordered",
		},
		Project: domain.Project{
			ID: projectID, Name: options.ProjectName, Enabled: true,
			AllowedModels: []string{options.PublicModel}, DailyBudgetMicrosUSD: options.DailyBudgetMicrosUSD,
			MaxInputTokens: 128_000, MaxOutputTokens: 16_384, MaxRequestBytes: cfg.Server.MaxRequestBytes,
			// No stream limit of its own: zero follows gateway.stream_max_duration,
			// where a copy of today's value would outlast the next change to it.
			CreatedAt: now, UpdatedAt: now,
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

func bootstrapPriceVersion(priceID, deploymentID string, billingMode domain.BillingMode, rates domain.PriceTier, now time.Time) domain.DeploymentPriceVersion {
	digest := sha256.Sum256([]byte(fmt.Sprintf("halro:bootstrap-pricing:v2:%s:%d:%d:%d:%d:%d", billingMode,
		rates.InputMicrosPerMillion, rates.CachedInputMicrosPerMillion, rates.CacheWriteMicrosPerMillion,
		rates.CacheWriteLongMicrosPerMillion, rates.OutputMicrosPerMillion)))
	return domain.DeploymentPriceVersion{
		ID: priceID, DeploymentID: deploymentID, Version: 1, Revision: 1,
		BillingMode: billingMode, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: rates.InputMicrosPerMillion, CachedInputMicrosPerMillion: rates.CachedInputMicrosPerMillion,
		CacheWriteMicrosPerMillion: rates.CacheWriteMicrosPerMillion, CacheWriteLongMicrosPerMillion: rates.CacheWriteLongMicrosPerMillion,
		CacheWriteRateEvidence: rates.CacheWriteRateEvidence,
		OutputMicrosPerMillion: rates.OutputMicrosPerMillion,
		EffectiveFrom:          now, CreatedBy: "bootstrap", CreatedAt: now,
		Source: domain.PriceSource{
			Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted,
			ReceivedAt: now, ContentSHA256: "sha256:" + hex.EncodeToString(digest[:]),
			Reference: "halro bootstrap CLI", AssertedWithoutArchive: true,
		},
	}
}

// bootstrapModelRevision is the catalog digest for the model being bootstrapped,
// including when the catalog establishes nothing about it: that digest is what
// later reveals the catalog has started covering the model.
func bootstrapModelRevision(providerType domain.ProviderType, profileID domain.ProviderProfileID, model string) string {
	entry, _ := modelcatalog.Builtin().Lookup(modelcatalog.Key{
		ProviderType: providerType, Profile: profileID, Model: model,
	})
	return entry.Revision()
}
