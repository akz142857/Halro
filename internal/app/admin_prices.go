package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/akz142857/Heimdall/internal/adminauth"
	"github.com/akz142857/Heimdall/internal/audit"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/go-chi/chi/v5"
)

type priceSourceInput struct {
	Type                   domain.PriceSourceType `json:"type"`
	URI                    string                 `json:"uri,omitempty"`
	PublishedAt            *time.Time             `json:"published_at,omitempty"`
	RetrievedAt            *time.Time             `json:"retrieved_at,omitempty"`
	ContentSHA256          string                 `json:"content_sha256,omitempty"`
	Reference              string                 `json:"reference,omitempty"`
	Note                   string                 `json:"note,omitempty"`
	ExternalEvidenceID     string                 `json:"external_evidence_id,omitempty"`
	CustodyOwner           string                 `json:"custody_owner,omitempty"`
	AssertedWithoutArchive bool                   `json:"asserted_without_archive,omitempty"`
	Adapter                string                 `json:"adapter,omitempty"`
	ProviderRequestID      string                 `json:"provider_request_id,omitempty"`
	ManifestID             string                 `json:"manifest_id,omitempty"`
	ManifestSchemaVersion  uint64                 `json:"manifest_schema_version,omitempty"`
	Signer                 string                 `json:"signer,omitempty"`
	SignatureSHA256        string                 `json:"signature_sha256,omitempty"`
}

type createPriceInput struct {
	BillingMode         domain.BillingMode `json:"billing_mode"`
	Currency            string             `json:"currency"`
	InputUSDPerMillion  string             `json:"input_usd_per_million"`
	OutputUSDPerMillion string             `json:"output_usd_per_million"`
	FixedRequestUSD     string             `json:"fixed_request_usd"`
	EffectiveFrom       time.Time          `json:"effective_from"`
	Source              priceSourceInput   `json:"source"`
	CurrentPassword     string             `json:"current_password,omitempty"`
	TOTPCode            string             `json:"totp_code,omitempty"`
}

type priceVersionView struct {
	domain.DeploymentPriceVersion
	Status    domain.PriceLifecycleStatus `json:"status"`
	cursorKey string
}

func (r *Runtime) listAdminDeploymentPrices(writer http.ResponseWriter, request *http.Request) {
	deploymentID := chi.URLParam(request, "id")
	if _, err := r.store.GetDeployment(request.Context(), deploymentID); err != nil {
		adminNotFound(writer)
		return
	}
	prices, err := r.store.ListDeploymentPriceVersions(request.Context(), deploymentID)
	if err != nil {
		adminStoreError(writer)
		return
	}
	now := time.Now().UTC()
	states, err := domain.DerivePriceLifecycle(prices, deploymentID, now)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	views := make([]priceVersionView, 0, len(prices))
	for _, price := range prices {
		views = append(views, priceVersionView{
			DeploymentPriceVersion: price, Status: states[price.ID], cursorKey: priceCursorKey(price),
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].cursorKey < views[j].cursorKey })
	writeResourcePage(writer, request, views, func(item priceVersionView) string { return item.cursorKey })
}

func (r *Runtime) createAdminDeploymentPrice(writer http.ResponseWriter, request *http.Request) {
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 256 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key is required", "code": "idempotency_key_required"})
		return
	}
	var input createPriceInput
	if err := decodeAdminJSON(request, &input); err != nil {
		writePriceError(writer, errors.New("invalid price request"))
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	deploymentID := chi.URLParam(request, "id")
	keyDigest := pricingIdempotencyDigest(admin.session.Username, deploymentID, idempotencyKey)
	priceID := deterministicMutationID("price", keyDigest)
	now := time.Now().UTC()
	password, totpCode := input.CurrentPassword, input.TOTPCode
	price, err := priceVersionFromInput(priceID, deploymentID, admin.session.Username, now, input)
	clear([]byte(input.CurrentPassword))
	input.CurrentPassword, input.TOTPCode = "", ""
	if err != nil {
		writePriceError(writer, err)
		return
	}
	canonicalRequest := price
	canonicalRequest.ID, canonicalRequest.CreatedBy, canonicalRequest.CreatedAt = "", "", time.Time{}
	canonicalRequest.Source.ReceivedAt = time.Time{}
	intent, err := newPricingAuditIntent("deployment_price.create", price.ID, admin.session.Username, request.Header.Get("X-Request-ID"), canonicalRequest)
	if err != nil {
		adminStoreError(writer)
		return
	}
	priorRequest, priorExists, err := r.store.PricingIdempotencyRequestSHA256(request.Context(), keyDigest)
	if err != nil {
		adminStoreError(writer)
		return
	}
	if priorExists && priorRequest == intent.RequestSHA256 {
		if !r.verifyPricingPassword(writer, request, admin.session.Username, password) {
			return
		}
	} else if !r.verifyPricingReauthentication(writer, request, admin.session.Username, password, totpCode) {
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	pricingUnlock := r.store.LockDeploymentPricing(price.DeploymentID)
	defer pricingUnlock()
	price, effectiveIntent, replayed, err := r.store.CreateDeploymentPriceVersionIdempotent(request.Context(), price, intent, keyDigest)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.deliverPricingAuditIntent(request.Context(), effectiveIntent); err != nil {
		adminAuditError(writer)
		return
	}
	if replayed {
		writer.Header().Set("Idempotent-Replayed", "true")
	}
	writer.Header().Set("ETag", revisionETag(price.Revision))
	writeJSON(writer, http.StatusCreated, price)
}

func (r *Runtime) cancelAdminDeploymentPrice(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	deploymentID, priceID := chi.URLParam(request, "id"), chi.URLParam(request, "priceID")
	now := time.Now().UTC()
	requestDigest := struct {
		DeploymentID string    `json:"deployment_id"`
		PriceID      string    `json:"price_id"`
		Revision     uint64    `json:"revision"`
		CancelledAt  time.Time `json:"cancelled_at"`
	}{deploymentID, priceID, expected, now}
	intent, err := newPricingAuditIntent("deployment_price.cancel", priceID, admin.session.Username, request.Header.Get("X-Request-ID"), requestDigest)
	if err != nil {
		adminStoreError(writer)
		return
	}
	currentPrice, err := r.store.GetDeploymentPriceVersion(request.Context(), deploymentID, priceID)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	effective := currentPrice.EffectiveFrom.UTC()
	intent.DeploymentID, intent.PriceVersion, intent.EffectiveFrom = deploymentID, currentPrice.Version, &effective
	intent.SourceType, intent.SourceContentSHA256 = currentPrice.Source.Type, currentPrice.Source.ContentSHA256
	intent.ChangeSummary = fmt.Sprintf("before={v:%d,status:scheduled,effective:%s} after={v:%d,status:cancelled}", currentPrice.Version, effective.Format(time.RFC3339Nano), currentPrice.Version)
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	pricingUnlock := r.store.LockDeploymentPricing(deploymentID)
	defer pricingUnlock()
	price, err := r.store.CancelDeploymentPriceVersionWithAuditIntent(request.Context(), deploymentID, priceID, admin.session.Username, now, expected, intent)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.deliverPricingAuditIntent(request.Context(), intent); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(price.Revision))
	writeJSON(writer, http.StatusOK, price)
}

func (r *Runtime) previewAdminDeploymentPrice(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		createPriceInput
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		writePriceError(writer, errors.New("invalid price request"))
		return
	}
	now := time.Now().UTC()
	price, err := priceVersionFromInput("price_preview", chi.URLParam(request, "id"), "admin:preview", now, input.createPriceInput)
	clear([]byte(input.CurrentPassword))
	if err != nil {
		writePriceError(writer, err)
		return
	}
	price.Version, price.Revision = 1, 1
	cost, err := domain.CalculateUSDTokensV1(input.InputTokens, input.OutputTokens, price)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"canonical_price": price, "cost": cost})
}

func (r *Runtime) confirmRestoredDeploymentPricing(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		CurrentPassword string `json:"current_password"`
		TOTPCode        string `json:"totp_code,omitempty"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		writePriceError(writer, errors.New("invalid restore confirmation request"))
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	if !r.verifyPricingReauthentication(writer, request, admin.session.Username, input.CurrentPassword, input.TOTPCode) {
		return
	}
	clear([]byte(input.CurrentPassword))
	input.CurrentPassword, input.TOTPCode = "", ""
	deploymentID := chi.URLParam(request, "id")
	intent, err := newPricingAuditIntent(
		"deployment_price.restore_confirm", deploymentID, admin.session.Username,
		request.Header.Get("X-Request-ID"), map[string]string{"deployment_id": deploymentID},
	)
	if err != nil {
		adminStoreError(writer)
		return
	}
	intent.DeploymentID = deploymentID
	intent.ChangeSummary = "before={pricing_quarantine:true} after={pricing_quarantine:false}"
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	pricingUnlock := r.store.LockDeploymentPricing(deploymentID)
	defer pricingUnlock()
	if err := r.store.ConfirmRestoredPricingWithAuditIntent(request.Context(), deploymentID, intent); err != nil {
		writePriceError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.deliverPricingAuditIntent(request.Context(), intent); err != nil {
		adminAuditError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"deployment_id": deploymentID, "pricing_quarantine": false})
}

type createPriceProposalInput struct {
	BillingMode         domain.BillingMode        `json:"billing_mode"`
	Currency            string                    `json:"currency"`
	InputUSDPerMillion  string                    `json:"input_usd_per_million"`
	OutputUSDPerMillion string                    `json:"output_usd_per_million"`
	FixedRequestUSD     string                    `json:"fixed_request_usd"`
	Tier                string                    `json:"tier,omitempty"`
	Warnings            []string                  `json:"warnings,omitempty"`
	Match               domain.PriceProposalMatch `json:"match"`
	ExpiresAt           time.Time                 `json:"expires_at"`
	Source              priceSourceInput          `json:"source"`
}

func (r *Runtime) listAdminDeploymentPriceProposals(writer http.ResponseWriter, request *http.Request) {
	proposals, err := r.store.ListDeploymentPriceProposals(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminStoreError(writer)
		return
	}
	writeResourcePage(writer, request, proposals, func(item domain.DeploymentPriceProposal) string {
		return item.CreatedAt.Format(time.RFC3339Nano) + "\x00" + item.ID
	})
}

func (r *Runtime) createAdminDeploymentPriceProposal(writer http.ResponseWriter, request *http.Request) {
	key := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 256 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key is required", "code": "idempotency_key_required"})
		return
	}
	var input createPriceProposalInput
	if err := decodeAdminJSON(request, &input); err != nil {
		writePriceError(writer, errors.New("invalid price proposal request"))
		return
	}
	deployment, err := r.store.GetDeployment(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		writePriceError(writer, err)
		return
	}
	now := time.Now().UTC()
	if !input.ExpiresAt.After(now) || input.ExpiresAt.After(now.Add(30*24*time.Hour)) {
		writePriceError(writer, errors.New("proposal expiry must be within the next 30 days"))
		return
	}
	inputMicros, err := domain.ParseUSDMicros(input.InputUSDPerMillion)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	outputMicros, err := domain.ParseUSDMicros(input.OutputUSDPerMillion)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	fixedMicros, err := domain.ParseUSDMicros(input.FixedRequestUSD)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	source, err := priceProposalSource(input.Source, now)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	keyDigest := pricingIdempotencyDigest(admin.session.Username, deployment.ID, "proposal:"+key)
	proposalID := deterministicMutationID("proposal", keyDigest)
	proposal := domain.DeploymentPriceProposal{
		ID: proposalID, DeploymentID: deployment.ID, ProviderID: deployment.ProviderID, ProviderModel: deployment.ProviderModel,
		Region: deployment.Region, Tier: strings.TrimSpace(input.Tier), BillingMode: input.BillingMode,
		Currency: strings.ToUpper(strings.TrimSpace(input.Currency)), FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: inputMicros, OutputMicrosPerMillion: outputMicros, FixedRequestMicrosUSD: fixedMicros,
		Source: source, FetchedAt: now, Warnings: input.Warnings, Match: input.Match, ExpiresAt: input.ExpiresAt.UTC(),
		Status: domain.PriceProposalPending, CreatedBy: admin.session.Username, CreatedAt: now, Revision: 1,
	}
	proposal.Digest, err = proposal.ComputeDigest()
	if err != nil || proposal.Validate() != nil {
		writePriceError(writer, errors.New("invalid price proposal"))
		return
	}
	canonicalRequest := struct {
		DeploymentID string                   `json:"deployment_id"`
		Input        createPriceProposalInput `json:"input"`
	}{deployment.ID, input}
	intent, err := newPricingAuditIntent("deployment_price.proposal_create", proposal.ID, admin.session.Username, request.Header.Get("X-Request-ID"), canonicalRequest)
	if err != nil {
		adminStoreError(writer)
		return
	}
	intent.DeploymentID, intent.SourceType, intent.SourceContentSHA256 = proposal.DeploymentID, proposal.Source.Type, proposal.Source.ContentSHA256
	intent.ChangeSummary = fmt.Sprintf("proposal=%s match=%s tier=%s billing=%s input=%d output=%d fixed=%d", proposal.ID, proposal.Match, proposal.Tier, proposal.BillingMode, proposal.InputMicrosPerMillion, proposal.OutputMicrosPerMillion, proposal.FixedRequestMicrosUSD)
	proposal, effectiveIntent, replayed, err := r.store.CreateDeploymentPriceProposal(request.Context(), proposal, intent, keyDigest)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	if err := r.deliverPricingAuditIntent(request.Context(), effectiveIntent); err != nil {
		adminAuditError(writer)
		return
	}
	if replayed {
		writer.Header().Set("Idempotent-Replayed", "true")
	}
	writer.Header().Set("ETag", revisionETag(proposal.Revision))
	writeJSON(writer, http.StatusCreated, proposal)
}

func (r *Runtime) adoptAdminDeploymentPriceProposal(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input struct {
		EffectiveFrom   time.Time `json:"effective_from"`
		Confirm         bool      `json:"confirm"`
		CurrentPassword string    `json:"current_password"`
		TOTPCode        string    `json:"totp_code,omitempty"`
	}
	if err := decodeAdminJSON(request, &input); err != nil || !input.Confirm {
		writePriceError(writer, errors.New("explicit proposal adoption confirmation is required"))
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	if !r.verifyPricingReauthentication(writer, request, admin.session.Username, input.CurrentPassword, input.TOTPCode) {
		return
	}
	clear([]byte(input.CurrentPassword))
	proposalID := chi.URLParam(request, "proposalID")
	priceID, err := id.New("price")
	if err != nil {
		adminStoreError(writer)
		return
	}
	intent, err := newPricingAuditIntent("deployment_price.proposal_adopt", proposalID, admin.session.Username, request.Header.Get("X-Request-ID"), map[string]any{"proposal_id": proposalID, "effective_from": input.EffectiveFrom, "revision": expected})
	if err != nil {
		adminStoreError(writer)
		return
	}
	now := time.Now().UTC()
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	pricingUnlock := r.store.LockDeploymentPricing(chi.URLParam(request, "id"))
	defer pricingUnlock()
	proposal, price, err := r.store.AdoptDeploymentPriceProposal(request.Context(), chi.URLParam(request, "id"), proposalID, priceID, admin.session.Username, input.EffectiveFrom.UTC(), now, expected, intent)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	if err := r.reloadProviderRegistry(request.Context()); err != nil {
		adminConfigurationError(writer, err)
		return
	}
	if err := r.deliverPricingAuditIntent(request.Context(), intent); err != nil {
		adminAuditError(writer)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"proposal": proposal, "price_version": price})
}

func (r *Runtime) rejectAdminDeploymentPriceProposal(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	proposalID := chi.URLParam(request, "proposalID")
	intent, err := newPricingAuditIntent("deployment_price.proposal_reject", proposalID, admin.session.Username, request.Header.Get("X-Request-ID"), map[string]any{"proposal_id": proposalID, "revision": expected})
	if err != nil {
		adminStoreError(writer)
		return
	}
	intent.DeploymentID = chi.URLParam(request, "id")
	intent.ChangeSummary = fmt.Sprintf("before={proposal:%s,status:pending,revision:%d} after={status:rejected}", proposalID, expected)
	proposal, err := r.store.RejectDeploymentPriceProposal(request.Context(), chi.URLParam(request, "id"), proposalID, admin.session.Username, time.Now().UTC(), expected, intent)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	if err := r.deliverPricingAuditIntent(request.Context(), intent); err != nil {
		adminAuditError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, proposal)
}

func priceProposalSource(input priceSourceInput, now time.Time) (domain.PriceSource, error) {
	// This public Admin endpoint is an assertion boundary. verified_api and
	// signed_import are reserved for trusted adapters that verify transport or
	// signatures before constructing domain objects.
	if input.Type != domain.PriceSourceOfficialURL {
		return domain.PriceSource{}, errors.New("interactive proposal source must be official_url; verified API and signed import require a trusted adapter")
	}
	assurance := domain.PriceAssuranceAsserted
	source := domain.PriceSource{
		Type: input.Type, Assurance: assurance, URI: strings.TrimSpace(input.URI), ContentSHA256: strings.ToLower(strings.TrimSpace(input.ContentSHA256)),
		Reference: strings.TrimSpace(input.Reference), Note: strings.TrimSpace(input.Note), ExternalEvidenceID: strings.TrimSpace(input.ExternalEvidenceID),
		CustodyOwner: strings.TrimSpace(input.CustodyOwner), AssertedWithoutArchive: input.AssertedWithoutArchive,
		Adapter: strings.TrimSpace(input.Adapter), ProviderRequestID: strings.TrimSpace(input.ProviderRequestID), ManifestID: strings.TrimSpace(input.ManifestID),
		ManifestSchemaVersion: input.ManifestSchemaVersion, Signer: strings.TrimSpace(input.Signer), SignatureSHA256: strings.ToLower(strings.TrimSpace(input.SignatureSHA256)), ReceivedAt: now,
	}
	if input.PublishedAt != nil {
		value := input.PublishedAt.UTC()
		source.PublishedAt = &value
	}
	if input.RetrievedAt != nil {
		value := input.RetrievedAt.UTC()
		source.RetrievedAt = &value
	}
	return source, source.Validate()
}

func deterministicMutationID(prefix, digest string) string {
	value := strings.TrimPrefix(digest, "sha256:")
	if len(value) > 24 {
		value = value[:24]
	}
	return prefix + "_" + value
}

func priceVersionFromInput(priceID, deploymentID, actor string, now time.Time, input createPriceInput) (domain.DeploymentPriceVersion, error) {
	inputMicros, err := domain.ParseUSDMicros(input.InputUSDPerMillion)
	if err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	outputMicros, err := domain.ParseUSDMicros(input.OutputUSDPerMillion)
	if err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	fixedMicros, err := domain.ParseUSDMicros(input.FixedRequestUSD)
	if err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	if input.Source.Type != domain.PriceSourceManual && input.Source.Type != domain.PriceSourceOfficialURL {
		return domain.DeploymentPriceVersion{}, errors.New("interactive price source must be manual or official_url")
	}
	source := domain.PriceSource{
		Type: input.Source.Type, Assurance: domain.PriceAssuranceAsserted,
		URI: strings.TrimSpace(input.Source.URI), ContentSHA256: strings.ToLower(strings.TrimSpace(input.Source.ContentSHA256)),
		Reference: strings.TrimSpace(input.Source.Reference), Note: strings.TrimSpace(input.Source.Note),
		ExternalEvidenceID: strings.TrimSpace(input.Source.ExternalEvidenceID), CustodyOwner: strings.TrimSpace(input.Source.CustodyOwner),
		AssertedWithoutArchive: input.Source.AssertedWithoutArchive, ReceivedAt: now,
	}
	if input.Source.PublishedAt != nil {
		value := input.Source.PublishedAt.UTC()
		source.PublishedAt = &value
	}
	if input.Source.RetrievedAt != nil {
		value := input.Source.RetrievedAt.UTC()
		source.RetrievedAt = &value
	}
	price := domain.DeploymentPriceVersion{
		ID: priceID, DeploymentID: deploymentID, BillingMode: input.BillingMode,
		Currency: strings.ToUpper(strings.TrimSpace(input.Currency)), FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: inputMicros, OutputMicrosPerMillion: outputMicros, FixedRequestMicrosUSD: fixedMicros,
		EffectiveFrom: input.EffectiveFrom.UTC(), Source: source, CreatedBy: actor, CreatedAt: now,
	}
	validation := price
	validation.Version, validation.Revision = 1, 1
	if err := validation.Validate(); err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	return price, nil
}

func newPricingAuditIntent(action, targetID, actor, correlationID string, request any) (domain.PricingAuditIntent, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return domain.PricingAuditIntent{}, err
	}
	digest := sha256.Sum256(payload)
	eventID, err := id.New("aud")
	if err != nil {
		return domain.PricingAuditIntent{}, err
	}
	intent := domain.PricingAuditIntent{
		EventID: eventID, OccurredAt: time.Now().UTC(), ActorID: actor, Action: action,
		TargetType: "deployment_price_version", TargetID: targetID, CorrelationID: correlationID,
		RequestSHA256: "sha256:" + hex.EncodeToString(digest[:]),
	}
	switch value := request.(type) {
	case domain.DeploymentPriceVersion:
		intent.DeploymentID, intent.PriceVersion, intent.SourceType = value.DeploymentID, value.Version, value.Source.Type
		intent.SourceAssurance, intent.SourceReference = value.Source.Assurance, value.Source.Reference
		intent.SourceWithoutArchive = value.Source.AssertedWithoutArchive
		intent.SourceContentSHA256 = value.Source.ContentSHA256
		effective := value.EffectiveFrom.UTC()
		intent.EffectiveFrom = &effective
		intent.ChangeSummary = fmt.Sprintf("before=none after={billing:%s,input:%d,output:%d,fixed:%d}", value.BillingMode, value.InputMicrosPerMillion, value.OutputMicrosPerMillion, value.FixedRequestMicrosUSD)
	}
	if strings.Contains(action, ".proposal_") {
		intent.TargetType = "deployment_price_proposal"
	}
	if action == "deployment_price.restore_confirm" {
		intent.TargetType = "deployment_pricing"
	}
	return intent, intent.Validate()
}

func (r *Runtime) deliverPricingAuditIntent(ctx context.Context, intent domain.PricingAuditIntent) error {
	if _, err := r.audit.Append(ctx, audit.Event{
		EventID: intent.EventID, OccurredAt: intent.OccurredAt, ActorType: "admin_user", ActorID: intent.ActorID,
		Action: intent.Action, TargetType: intent.TargetType, TargetID: intent.TargetID,
		Outcome: "success", CorrelationID: intent.CorrelationID,
		Metadata: map[string]any{"deployment_id": intent.DeploymentID, "price_version": intent.PriceVersion, "effective_from": intent.EffectiveFrom,
			"request_sha256": intent.RequestSHA256, "source_type": intent.SourceType, "source_assurance": intent.SourceAssurance,
			"source_content_sha256": intent.SourceContentSHA256, "source_reference": intent.SourceReference,
			"source_without_archive": intent.SourceWithoutArchive, "change_summary": intent.ChangeSummary},
	}); err != nil {
		return err
	}
	if err := checkpointAudit(r.store, r.audit.Summary()); err != nil {
		return err
	}
	return r.store.MarkPricingAuditIntentDelivered(ctx, intent.EventID)
}

func (r *Runtime) drainPricingAuditIntents(ctx context.Context) error {
	intents, err := r.store.ListPendingPricingAuditIntents(ctx)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if err := r.deliverPricingAuditIntent(ctx, intent); err != nil {
			return err
		}
	}
	return nil
}

// verifyPricingReauthentication is the general step-up primitive (current
// password + a fresh TOTP code resupplied in the request body, verified
// per-request rather than a short-lived elevated session) — the name is
// pricing's because pricing needed it first, but nothing in the body is
// pricing-specific. Other high-risk mutations reuse it through the
// verifyAdminReauthentication alias rather than duplicating it.
func (r *Runtime) verifyPricingReauthentication(writer http.ResponseWriter, request *http.Request, username, passwordText, code string) bool {
	if !r.verifyPricingPassword(writer, request, username, passwordText) {
		return false
	}
	active, err := r.activeAdminMFA(request.Context(), username)
	if err != nil {
		adminStoreError(writer)
		return false
	}
	if len(active) > 0 {
		if _, ok := r.verifyAnyTOTP(request.Context(), active, code, time.Now()); !ok {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "recent re-authentication required", "code": "recent_reauth_required"})
			return false
		}
	}
	return true
}

func (r *Runtime) verifyPricingPassword(writer http.ResponseWriter, request *http.Request, username, passwordText string) bool {
	password := []byte(passwordText)
	defer clear(password)
	user, err := r.store.GetAdminUser(request.Context(), username)
	if err != nil || !adminauth.VerifyPassword(user, password) {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "recent re-authentication required", "code": "recent_reauth_required"})
		return false
	}
	return true
}

func writePriceError(writer http.ResponseWriter, err error) {
	status, code := http.StatusBadRequest, "invalid_price"
	switch {
	case errors.Is(err, boltstore.ErrNotFound):
		status, code = http.StatusNotFound, "price_version_not_found"
	case errors.Is(err, boltstore.ErrRevisionConflict):
		status, code = http.StatusPreconditionFailed, "revision_mismatch"
	case errors.Is(err, domain.ErrPriceTimelineConflict), errors.Is(err, boltstore.ErrAlreadyExists):
		status, code = http.StatusConflict, "price_timeline_conflict"
	case errors.Is(err, domain.ErrPriceVersionUnavailable):
		status, code = http.StatusConflict, "price_version_in_use"
	case errors.Is(err, boltstore.ErrIdempotencyConflict):
		status, code = http.StatusConflict, "idempotency_conflict"
	}
	writeJSON(writer, status, map[string]string{"error": err.Error(), "code": code})
}

func priceCursorKey(price domain.DeploymentPriceVersion) string {
	return price.EffectiveFrom.UTC().Format("20060102T150405.000000000Z") + "\x00" + price.ID
}

func pricingIdempotencyDigest(actor, deploymentID, key string) string {
	digest := sha256.New()
	digest.Write([]byte("heimdall:pricing-idempotency:v1\x00"))
	digest.Write([]byte(actor))
	digest.Write([]byte{0})
	digest.Write([]byte(deploymentID))
	digest.Write([]byte{0})
	digest.Write([]byte(key))
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}
