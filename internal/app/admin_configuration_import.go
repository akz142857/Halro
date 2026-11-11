package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/modelcatalog"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

type configurationImportInput struct {
	Bundle   configurationBundle                    `json:"bundle"`
	Bindings map[string]configurationProviderTarget `json:"bindings"`
}

type configurationProviderTarget struct {
	CredentialID               string  `json:"credential_id"`
	BaseURL                    *string `json:"base_url,omitempty"`
	EgressProxyID              string  `json:"egress_proxy_id,omitempty"`
	BedrockProjectID           *string `json:"bedrock_project_id,omitempty"`
	AcknowledgedPolicyRevision string  `json:"acknowledged_policy_revision,omitempty"`
}

type configurationApplyInput struct {
	configurationImportInput
	stepUpMaterial
	PlanID                     string `json:"plan_id"`
	AcceptOperatorDeclarations bool   `json:"accept_operator_declarations,omitempty"`
}

type configurationImportPreview struct {
	Counts              map[string]int                `json:"counts"`
	Providers           []configurationProviderReview `json:"providers"`
	DeploymentsToEnable []configurationObject         `json:"deployments_to_enable"`
	RoutesToEnable      []configurationObject         `json:"routes_to_enable"`
	Issues              []configurationIssue          `json:"issues"`
	Notices             []configurationNotice         `json:"notices,omitempty"`
	Declarations        []configurationDeclaration    `json:"operator_declarations,omitempty"`
	RegionChanges       []configurationRegionChange   `json:"region_changes,omitempty"`
	PlanID              string                        `json:"plan_id,omitempty"`
}

type configurationObject struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Target string `json:"target,omitempty"`
}

type configurationDeclaration struct {
	configurationObject
	Model        string   `json:"model"`
	Capabilities []string `json:"capabilities"`
}

type configurationRegionChange struct {
	configurationObject
	SourceRegion string `json:"source_region"`
	TargetRegion string `json:"target_region"`
}

type configurationProviderReview struct {
	configurationObject
	Type                domain.ProviderType     `json:"type"`
	AccessSurface       domain.AccessSurface    `json:"access_surface"`
	CredentialScheme    domain.CredentialScheme `json:"credential_scheme"`
	BaseURL             string                  `json:"base_url"`
	RequiresEgressProxy bool                    `json:"requires_egress_proxy"`
	BedrockProjectID    string                  `json:"bedrock_project_id,omitempty"`
	PolicyRevision      string                  `json:"policy_revision,omitempty"`
	PolicyURL           string                  `json:"policy_url,omitempty"`
}

type configurationIssue struct {
	Resource string `json:"resource"`
	ID       string `json:"id,omitempty"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

type configurationIssuedKey struct {
	configurationObject
	ProjectID string `json:"project_id"`
	Key       string `json:"key,omitempty"`
}

var configurationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

var (
	errConfigurationEndpoint   = errors.New("enter a target endpoint without user info, a query string or a fragment")
	errConfigurationCredential = errors.New("select a credential from the target instance")
	errConfigurationProxy      = errors.New("select an egress proxy from the target instance")
	errConfigurationPolicy     = errors.New("acknowledge the target provider usage policy")
)

func configurationBindingIssueCode(err error) string {
	switch {
	case errors.Is(err, errConfigurationEndpoint):
		return "target_endpoint_required"
	case errors.Is(err, errConfigurationCredential):
		return "target_credential_required"
	case errors.Is(err, errConfigurationProxy):
		return "target_proxy_required"
	case errors.Is(err, errConfigurationPolicy):
		return "target_policy_required"
	default:
		return "provider_binding"
	}
}

func decodeConfigurationImport(writer http.ResponseWriter, request *http.Request, input any) bool {
	// Allow space for the wrapper and target selections outside the bundle.
	const limit = configurationBundleLimit + (256 << 10)
	request.Body = http.MaxBytesReader(writer, request.Body, limit)
	if err := decodeAdminJSONLimit(request, input, limit); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(writer, http.StatusRequestEntityTooLarge, map[string]string{
				"code": "configuration_bundle_too_large", "error": "configuration exceeds the bundle limit",
			})
		} else {
			adminBadRequestCode(writer, "invalid_request", "invalid configuration bundle")
		}
		return false
	}
	return true
}

func (r *Runtime) previewAdminConfigurationImport(writer http.ResponseWriter, request *http.Request) {
	var input configurationImportInput
	if !decodeConfigurationImport(writer, request, &input) {
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	preview, _, err := r.prepareConfigurationImport(request, input, time.Now().UTC())
	if err != nil {
		adminStoreError(writer)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, preview)
}

func (r *Runtime) applyAdminConfigurationImport(writer http.ResponseWriter, request *http.Request) {
	var input configurationApplyInput
	if !decodeConfigurationImport(writer, request, &input) {
		return
	}
	if input.CurrentPassword == "" {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{
			"code": "recent_reauth_required", "error": "recent re-authentication required",
		})
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	if !r.verifyAdminStepUp(writer, request, admin.session.Username, input.CurrentPassword, input.TOTPCode) {
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()

	preview, config, err := r.prepareConfigurationImport(request, input.configurationImportInput, time.Now().UTC())
	if err != nil {
		adminStoreError(writer)
		return
	}
	if preview.PlanID == "" || input.PlanID != preview.PlanID {
		writer.Header().Set("Cache-Control", "no-store")
		writeJSON(writer, http.StatusConflict, map[string]any{
			"code": "configuration_preview_required", "error": "review the current import preview before importing",
			"preview": preview,
		})
		return
	}
	if len(preview.Declarations) > 0 && !input.AcceptOperatorDeclarations {
		adminBadRequestCode(writer, "configuration_declarations_required", "review and acknowledge the operator declarations")
		return
	}
	issuedKeys := make([]configurationIssuedKey, 0, len(config.GatewayKeys))
	for i, template := range config.GatewayKeys {
		plaintext, key, err := auth.GenerateGatewayKeyWithID(template.ID, template.ProjectID, template.Name, template.ExpiresAt)
		if err != nil {
			adminStoreError(writer)
			return
		}
		key.Scopes, key.Enabled = template.Scopes, template.Enabled
		config.GatewayKeys[i] = key
		issuedKeys = append(issuedKeys, configurationIssuedKey{configurationObject: configurationObject{ID: key.ID, Name: key.Name}, ProjectID: key.ProjectID, Key: plaintext})
	}
	intents, err := r.configurationImportAudit(request, config, preview.PlanID, input.Bundle.recordCount())
	if err != nil {
		adminStoreError(writer)
		return
	}
	intent := intents[0]
	writer.Header().Set("Cache-Control", "no-store")
	if err := r.store.ImportPortableConfiguration(request.Context(), config, intents...); err != nil {
		var unconfirmed *boltstore.ConfigurationImportConfirmationError
		switch {
		case errors.As(err, &unconfirmed):
			// A confirmation error cannot roll back the local commit. Do not
			// hand out keys from an unconfirmed replication prefix.
			writer.Header().Set("Halro-Operation-Id", intent.EventID)
			for i := range issuedKeys {
				issuedKeys[i].Key = ""
			}
			writeJSON(writer, http.StatusAccepted, map[string]any{
				"status": "confirmation_unavailable", "operation_id": intent.EventID,
				"counts": preview.Counts, "keys_to_reissue": issuedKeys,
			})
		case errors.Is(err, boltstore.ErrConfigurationTargetNotEmpty):
			writeJSON(writer, http.StatusConflict, map[string]string{
				"code": "configuration_target_not_empty", "error": "the target already has configuration",
			})
		default:
			r.logger.Error("configuration import failed", "error", err)
			adminStoreError(writer)
		}
		return
	}
	r.activateTopologyAfterCommit()
	r.activateRedactionPolicies()
	r.activateTokenGuardPolicies()
	r.activateAuthSnapshot()
	r.completeAdminMutation(writer, request, *intent)
	writeJSON(writer, http.StatusCreated, map[string]any{
		"status": "imported", "operation_id": intent.EventID,
		"counts": preview.Counts, "gateway_keys": issuedKeys,
		"deployments_to_enable": preview.DeploymentsToEnable, "routes_to_enable": preview.RoutesToEnable,
		"activation_stale": r.activation.status().Stale,
	})
}

func (r *Runtime) configurationImportAudit(request *http.Request, config boltstore.PortableConfiguration, planID string, count int) ([]*domain.AdminAuditIntent, error) {
	summary, err := r.newAdminAuditIntentWithMetadata(request, "configuration.import", "configuration", "v1",
		map[string]string{"plan_id": planID, "record_count": fmt.Sprint(count)})
	if err != nil {
		return nil, err
	}
	intents := []*domain.AdminAuditIntent{summary}
	record := func(action, resource, id string, metadata map[string]string) error {
		metadata["configuration_import_id"] = summary.EventID
		intent, err := r.newAdminAuditIntentWithMetadata(request, action, resource, id, metadata)
		if err == nil {
			intents = append(intents, intent)
		}
		return err
	}
	for _, instance := range config.Providers {
		metadata := make(map[string]string)
		if requirement, ok := domain.UsagePolicyRequirementForProfile(instance.ProfileID, instance.BaseURL); ok {
			metadata = usagePolicyAuditMetadata(requirement, instance.ProfileID, true)
		}
		if instance.EgressProxyID != "" {
			metadata["egress_mode_before"] = "none"
			metadata["egress_mode_after"] = "proxy"
			metadata["proxy_id_after"] = instance.EgressProxyID
		}
		if len(metadata) > 0 {
			if err := record("provider.create", "provider", instance.ID, metadata); err != nil {
				return nil, err
			}
		}
	}
	for _, deployment := range config.Deployments {
		metadata := make(map[string]string)
		for key, value := range capabilitySnapshotMetadata(deployment) {
			if names, ok := value.([]string); ok {
				metadata[key] = strings.Join(names, ",")
			} else {
				metadata[key] = fmt.Sprint(value)
			}
		}
		if model := deployment.ModelCapabilitySnapshot.CanonicalModelRef; model != "" {
			metadata["canonical_model_ref"] = model
		}
		if err := record(auditCapabilitySnapshotCreated, "deployment", deployment.ID, metadata); err != nil {
			return nil, err
		}
		if deployment.ModelCapabilitySnapshot.Source == string(modelcatalog.SourceOperatorDeclared) {
			if err := record(auditOperatorCapabilitiesDeclared, "deployment", deployment.ID, metadata); err != nil {
				return nil, err
			}
		}
	}
	// The summary is delivered synchronously. The existing audit drain delivers
	// the per-resource records without delaying one-time key delivery.
	return intents, nil
}

func (r *Runtime) prepareConfigurationImport(request *http.Request, input configurationImportInput, now time.Time) (configurationImportPreview, boltstore.PortableConfiguration, error) {
	b := input.Bundle
	preview := configurationImportPreview{
		Counts: map[string]int{
			"providers": len(b.Providers), "deployments": len(b.Deployments), "routes": len(b.Routes),
			"redaction_policies": len(b.RedactionPolicies), "token_guard_policies": len(b.TokenGuardPolicies),
			"projects": len(b.Projects), "gateway_keys": len(b.GatewayKeys),
		},
		Providers: []configurationProviderReview{}, Issues: []configurationIssue{},
		DeploymentsToEnable: []configurationObject{}, RoutesToEnable: []configurationObject{},
		Notices: append([]configurationNotice(nil), b.Omissions...),
	}
	config := boltstore.PortableConfiguration{Prices: make(map[string]domain.DeploymentPriceVersion)}
	if b.Version != configurationBundleVersion {
		preview.issue("bundle", "", "unsupported_version", errors.New("unsupported configuration bundle version"))
		return preview, config, nil
	}
	if b.recordCount() == 0 || b.recordCount() > configurationRecordLimit {
		preview.issue("bundle", "", "record_count", fmt.Errorf("configuration must contain 1 to %d records", configurationRecordLimit))
		return preview, config, nil
	}
	if err := r.store.CheckConfigurationImportTarget(); err != nil {
		if !errors.Is(err, boltstore.ErrConfigurationTargetNotEmpty) {
			return preview, config, err
		}
		preview.issue("target", "", "target_not_empty", err)
	}

	providers := make(map[string]domain.ProviderInstance, len(b.Providers))
	providerIDs := make(map[string]struct{}, len(b.Providers))
	for _, item := range b.Providers {
		if !preview.checkID(providerIDs, "providers", item.ID) {
			continue
		}
		target := input.Bindings[item.ID]
		review, candidate, err := r.prepareConfigurationProvider(request, item, target, now)
		preview.Providers = append(preview.Providers, review)
		if err != nil {
			preview.issue("providers", item.ID, configurationBindingIssueCode(err), err)
			continue
		}
		providers[item.ID] = candidate
		config.Providers = append(config.Providers, candidate)
	}
	for providerID := range input.Bindings {
		if _, ok := providerIDs[providerID]; !ok {
			preview.issue("providers", providerID, "unknown_provider", errors.New("binding names a provider outside the bundle"))
		}
	}

	deployments := make(map[string]domain.Deployment, len(b.Deployments))
	deploymentIDs := make(map[string]struct{}, len(b.Deployments))
	deploymentNames := make(map[string]string, len(b.Deployments))
	for _, item := range b.Deployments {
		if !preview.checkID(deploymentIDs, "deployments", item.ID) {
			continue
		}
		deploymentNames[item.ID] = item.Name
		if item.Enabled {
			preview.DeploymentsToEnable = append(preview.DeploymentsToEnable, configurationObject{ID: item.ID, Name: item.Name})
		}
		instance, ok := providers[item.ProviderID]
		if !ok {
			// A missing binding already has an actionable provider error.
			if _, exists := providerIDs[item.ProviderID]; !exists {
				preview.issue("deployments", item.ID, "missing_provider", errors.New("deployment provider is outside the bundle"))
			}
			continue
		}
		deployment, err := r.prepareConfigurationDeployment(request, instance, item, now)
		if err != nil {
			preview.issue("deployments", item.ID, "invalid_record", err)
			continue
		}
		if item.Price != nil {
			admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
			price := item.Price.version(item.ID, admin.session.Username, now)
			if err := price.Validate(); err != nil {
				preview.issue("deployments", item.ID, "invalid_price", err)
				continue
			}
			config.Prices[item.ID] = price
		}
		if deployment.ModelCapabilitySnapshot.Source == string(modelcatalog.SourceOperatorDeclared) {
			preview.Declarations = append(preview.Declarations, configurationDeclaration{
				configurationObject: configurationObject{ID: deployment.ID, Name: deployment.Name},
				Model:               deployment.ProviderModel, Capabilities: domain.EnabledCapabilityNames(deployment.Capabilities),
			})
		}
		if instance.AccessSurface == domain.SurfaceBedrockMantle && deployment.Region != item.Region {
			preview.RegionChanges = append(preview.RegionChanges, configurationRegionChange{
				configurationObject: configurationObject{ID: deployment.ID, Name: deployment.Name},
				SourceRegion:        item.Region, TargetRegion: deployment.Region,
			})
		}
		config.Deployments = append(config.Deployments, deployment)
		deployments[item.ID] = deployment
	}

	aliases := make(map[string]struct{}, len(b.Routes))
	routeIDs := make(map[string]struct{}, len(b.Routes))
	for _, item := range b.Routes {
		if !preview.checkID(routeIDs, "routes", item.ID) {
			continue
		}
		if item.Enabled {
			preview.RoutesToEnable = append(preview.RoutesToEnable, configurationObject{ID: item.ID, Name: item.PublicModel, Target: deploymentNames[item.DeploymentID]})
		}
		route := item.route(item.ID, now, now)
		if err := route.Validate(); err != nil {
			preview.issue("routes", item.ID, "invalid_record", err)
			continue
		}
		aliases[route.PublicModel] = struct{}{}
		if _, ok := deployments[route.DeploymentID]; !ok {
			if _, exists := deploymentIDs[route.DeploymentID]; !exists {
				preview.issue("routes", item.ID, "missing_deployment", errors.New("route deployment is outside the bundle"))
			}
			continue
		}
		route.Enabled = false
		config.Routes = append(config.Routes, route)
	}

	preview.prepareProjects(b, aliases, now, &config)
	if len(preview.Issues) == 0 {
		encoded, err := json.Marshal(struct {
			Input         configurationImportInput
			Declarations  []configurationDeclaration
			RegionChanges []configurationRegionChange
		}{input, preview.Declarations, preview.RegionChanges})
		if err != nil {
			return preview, config, err
		}
		digest := sha256.Sum256(encoded)
		preview.PlanID = hex.EncodeToString(digest[:])
	}
	return preview, config, nil
}

func (r *Runtime) prepareConfigurationProvider(request *http.Request, item configurationProvider, target configurationProviderTarget, now time.Time) (configurationProviderReview, domain.ProviderInstance, error) {
	baseURL := item.BaseURL
	if target.BaseURL != nil {
		baseURL = strings.TrimSpace(*target.BaseURL)
	}
	projectID := item.BedrockProjectID
	if target.BedrockProjectID != nil {
		projectID = strings.TrimSpace(*target.BedrockProjectID)
	}
	review := configurationProviderReview{
		configurationObject: configurationObject{ID: item.ID, Name: item.Name},
		Type:                item.Type, BaseURL: portableEndpoint(baseURL), BedrockProjectID: projectID,
		RequiresEgressProxy: item.RequiresEgressProxy,
	}
	if len(item.Bindings) == 0 || item.Bindings[0].ProfileID == "" {
		return review, domain.ProviderInstance{}, errors.New("provider requires an explicit profile binding")
	}
	primary, ok := domain.ResolveProviderProfile(item.Type, item.Bindings[0].ProfileID)
	if !ok {
		return review, domain.ProviderInstance{}, errors.New("provider profile is unavailable on this Halro version")
	}
	review.AccessSurface, review.CredentialScheme = primary.AccessSurface, primary.CredentialScheme
	if requirement, required := domain.UsagePolicyRequirementForProfile(primary.ProfileID, review.BaseURL); required {
		review.PolicyRevision, review.PolicyURL = requirement.PolicyRevision, requirement.DocumentationURL
	}
	if review.BaseURL == "" {
		return review, domain.ProviderInstance{}, errConfigurationEndpoint
	}
	if strings.TrimSpace(target.CredentialID) == "" {
		return review, domain.ProviderInstance{}, errConfigurationCredential
	}
	if item.RequiresEgressProxy && strings.TrimSpace(target.EgressProxyID) == "" {
		return review, domain.ProviderInstance{}, errConfigurationProxy
	}
	instance, err := r.providerConnectionFromInput(request, item.ID, providerInput{
		Name: item.Name, Type: item.Type, BaseURL: baseURL, APIVersion: item.APIVersion,
		ProfileID: primary.ProfileID, CredentialID: strings.TrimSpace(target.CredentialID),
		BedrockProjectID: projectID, AllowedAnthropicBetas: item.AllowedAnthropicBetas,
		MaxConcurrency: item.MaxConcurrency, Enabled: item.Enabled,
	}, "", strings.TrimSpace(target.EgressProxyID), now, now)
	if err != nil {
		return review, domain.ProviderInstance{}, err
	}
	if review.PolicyRevision != "" {
		ack, ok := domain.UsagePolicyAcknowledgementForProfileAtEndpoint(primary.ProfileID, baseURL, target.AcknowledgedPolicyRevision)
		if !ok {
			return review, domain.ProviderInstance{}, errConfigurationPolicy
		}
		instance.UsagePolicyAcknowledgement = ack
	}
	for _, itemBinding := range item.Bindings {
		bound, ok := domain.ResolveProviderProfile(item.Type, itemBinding.ProfileID)
		if !ok || itemBinding.ProfileID == "" || domain.IsWithheldProfile(itemBinding.ProfileID) ||
			!offeredProfile(r.config.ProviderSubscriptions, itemBinding.ProfileID) {
			return review, domain.ProviderInstance{}, errors.New("provider binding is unavailable on the target instance")
		}
		instance.Bindings = append(instance.Bindings, domain.ProviderProfileBinding{
			ID: domain.DefaultProviderProfileBindingID(instance.ID, bound.ProfileID), ProviderID: instance.ID,
			ProfileID: bound.ProfileID, AccessSurface: bound.AccessSurface, CredentialScheme: bound.CredentialScheme,
			Capabilities: itemBinding.Capabilities, Enabled: itemBinding.Enabled,
			CapabilityEvidence: domain.EvidenceForCapabilities(itemBinding.Capabilities, domain.EvidenceDeclared),
		})
	}
	instance.Capabilities, instance.CapabilityEvidence = domain.BindingsCapabilitiesSummary(instance.Bindings)
	instance.Revision = 1 // The new deployment's capability snapshot refers to this revision.
	return review, instance, instance.Validate()
}

func (r *Runtime) prepareConfigurationDeployment(request *http.Request, instance domain.ProviderInstance, item configurationDeployment, now time.Time) (domain.Deployment, error) {
	bindingID := domain.DefaultProviderProfileBindingID(instance.ID, item.ProfileID)
	// An intentionally disabled binding still has a definition worth importing.
	// Enable it only in the copy used to validate the deployment's declaration.
	bindings := append([]domain.ProviderProfileBinding(nil), instance.Bindings...)
	for i := range bindings {
		if bindings[i].ID == bindingID {
			bindings[i].Enabled = true
		}
	}
	instance.Bindings = bindings
	region := item.Region
	if instance.AccessSurface == domain.SurfaceBedrockMantle {
		// Mantle's endpoint is the authority for deployment region. A target
		// endpoint override must not carry the source endpoint's region.
		region = ""
	}
	for _, name := range item.OperatorDisabled {
		if enabled, known := domain.CapabilityValue(item.Capabilities, name); !known || enabled {
			return domain.Deployment{}, fmt.Errorf("invalid disabled capability %q", name)
		}
	}
	deployment, err := r.deploymentFromProvider(request, instance, item.ID, deploymentInput{
		Name: item.Name, ProviderID: item.ProviderID, ProviderModel: item.ProviderModel, CapabilityModel: item.CapabilityModel,
		TargetKind: item.TargetKind, ProfileID: item.ProfileID, BindingID: bindingID, Region: region,
		Capabilities: &item.Capabilities, MaxConcurrency: item.MaxConcurrency, Mode: deploymentModeOperatorDeclared,
	}, nil, now, now)
	if err != nil {
		return domain.Deployment{}, err
	}
	deployment.OperatorDisabled = domain.OperatorDisabledCapabilities(deployment.ModelCapabilitySnapshot, deployment.Capabilities, item.OperatorDisabled)
	return deployment, nil
}

func (p configurationPrice) version(deploymentID, actor string, now time.Time) domain.DeploymentPriceVersion {
	return domain.DeploymentPriceVersion{
		ID: "price_import_" + deploymentID, DeploymentID: deploymentID, Version: 1, Revision: 1,
		BillingMode: p.BillingMode, Currency: "USD", FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: p.InputMicrosPerMillion, CachedInputMicrosPerMillion: p.CachedInputMicrosPerMillion,
		OutputMicrosPerMillion: p.OutputMicrosPerMillion, FixedRequestMicrosUSD: p.FixedRequestMicrosUSD, Schedule: p.Schedule,
		CreatedBy: actor, CreatedAt: now, EffectiveFrom: now,
		Source: domain.PriceSource{
			Type: domain.PriceSourceManual, Assurance: domain.PriceAssuranceAsserted, Reference: "Configuration import",
			AssertedWithoutArchive: true, ReceivedAt: now,
		},
	}
}

func (p *configurationImportPreview) prepareProjects(b configurationBundle, aliases map[string]struct{}, now time.Time, config *boltstore.PortableConfiguration) {
	redactionIDs, tokenGuardIDs := map[string]struct{}{}, map[string]struct{}{}
	redactionEnabled, tokenGuardEnabled := map[string]bool{}, map[string]bool{}
	for _, item := range b.RedactionPolicies {
		if !p.checkID(redactionIDs, "redaction_policies", item.ID) {
			continue
		}
		policy, err := item.policy(item.ID, now, now)
		if err != nil {
			p.issue("redaction_policies", item.ID, "invalid_record", err)
			continue
		}
		redactionEnabled[item.ID] = policy.Enabled
		config.RedactionPolicies = append(config.RedactionPolicies, policy)
	}
	for _, item := range b.TokenGuardPolicies {
		if !p.checkID(tokenGuardIDs, "token_guard_policies", item.ID) {
			continue
		}
		policy := item.policy(item.ID, now, now)
		if err := policy.Validate(); err != nil {
			p.issue("token_guard_policies", item.ID, "invalid_record", err)
			continue
		}
		tokenGuardEnabled[item.ID] = policy.Enabled
		config.TokenGuardPolicies = append(config.TokenGuardPolicies, policy)
	}
	projectIDs, keyIDs := map[string]struct{}{}, map[string]struct{}{}
	for _, item := range b.Projects {
		if !p.checkID(projectIDs, "projects", item.ID) {
			continue
		}
		project, err := item.project(item.ID, now, now, nil)
		if err != nil {
			p.issue("projects", item.ID, "invalid_record", err)
			continue
		}
		for _, alias := range project.AllowedModels {
			if _, ok := aliases[alias]; !ok {
				// Keep the explicit allowlist: pruning the last alias would
				// grant access to every model. Missing aliases are unavailable.
				p.Notices = append(p.Notices, configurationNotice{
					Resource: "projects", ID: item.ID, Code: "unavailable_alias", Alias: alias,
				})
			}
		}
		if enabled, ok := redactionEnabled[project.RedactionPolicyID]; project.RedactionPolicyID != "" && (!ok || (project.Enabled && !enabled)) {
			p.issue("projects", item.ID, "missing_policy", errors.New("redaction policy is unavailable"))
		}
		if enabled, ok := tokenGuardEnabled[project.TokenGuardPolicyID]; project.TokenGuardPolicyID != "" && (!ok || (project.Enabled && !enabled)) {
			p.issue("projects", item.ID, "missing_policy", errors.New("Token Guard policy is unavailable"))
		}
		config.Projects = append(config.Projects, project)
	}
	for _, item := range b.GatewayKeys {
		if !p.checkID(keyIDs, "gateway_keys", item.ID) {
			continue
		}
		if _, ok := projectIDs[item.ProjectID]; !ok {
			p.issue("gateway_keys", item.ID, "missing_project", errors.New("key project is outside the bundle"))
			continue
		}
		key := domain.GatewayKey{
			ID: item.ID, ProjectID: item.ProjectID, Name: item.Name, Enabled: item.Enabled,
			Scopes: item.Scopes, ExpiresAt: item.ExpiresAt, HashVersion: auth.HashVersion,
		}
		if len(key.Scopes) == 0 {
			p.issue("gateway_keys", item.ID, "invalid_record", errors.New("key scopes cannot be empty"))
			continue
		}
		if err := key.Validate(); err != nil {
			p.issue("gateway_keys", item.ID, "invalid_record", err)
			continue
		}
		config.GatewayKeys = append(config.GatewayKeys, key)
	}
}

func (p *configurationImportPreview) checkID(seen map[string]struct{}, resource, id string) bool {
	if !configurationIDPattern.MatchString(id) {
		p.issue(resource, "", "invalid_id", errors.New("configuration record ID is missing or invalid"))
		return false
	}
	if _, exists := seen[id]; exists {
		p.issue(resource, id, "duplicate_id", errors.New("configuration record ID is repeated"))
		return false
	}
	seen[id] = struct{}{}
	return true
}

func (p *configurationImportPreview) issue(resource, id, code string, err error) {
	p.Issues = append(p.Issues, configurationIssue{Resource: resource, ID: id, Code: code, Message: err.Error()})
}
