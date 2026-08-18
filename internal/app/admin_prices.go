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

	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
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

// priceWindowInput is one rung of a time-of-day schedule as the console sends
// it: local clock strings the operator typed, and USD-per-million decimals in
// the same shape as the base rates beside them.
type priceWindowInput struct {
	Start                    string `json:"start"`
	End                      string `json:"end"`
	InputUSDPerMillion       string `json:"input_usd_per_million"`
	CachedInputUSDPerMillion string `json:"cached_input_usd_per_million"`
	OutputUSDPerMillion      string `json:"output_usd_per_million"`
	FixedRequestUSD          string `json:"fixed_request_usd"`
}

type priceScheduleInput struct {
	Timezone string             `json:"timezone"`
	Windows  []priceWindowInput `json:"windows"`
}

type createPriceInput struct {
	BillingMode              domain.BillingMode  `json:"billing_mode"`
	Currency                 string              `json:"currency"`
	InputUSDPerMillion       string              `json:"input_usd_per_million"`
	CachedInputUSDPerMillion string              `json:"cached_input_usd_per_million"`
	OutputUSDPerMillion      string              `json:"output_usd_per_million"`
	FixedRequestUSD          string              `json:"fixed_request_usd"`
	Schedule                 *priceScheduleInput `json:"schedule,omitempty"`
	EffectiveFrom            time.Time           `json:"effective_from"`
	EffectiveImmediately     bool                `json:"effective_immediately,omitempty"`
	Source                   priceSourceInput    `json:"source"`
	CurrentPassword          string              `json:"current_password,omitempty"`
	TOTPCode                 string              `json:"totp_code,omitempty"`
}

// priceScheduleFromInput parses the operator's window table. The clock strings
// are parsed here rather than in the domain so the stored rule stays minutes
// from midnight — one representation, comparable and hashable, with no room for
// "09:00" and "9:00" to be the same window with two digests.
func priceScheduleFromInput(input *priceScheduleInput) (*domain.PriceSchedule, error) {
	if input == nil {
		return nil, nil
	}
	schedule := domain.PriceSchedule{Timezone: strings.TrimSpace(input.Timezone)}
	for index, window := range input.Windows {
		start, err := parseClockMinute(window.Start)
		if err != nil {
			return nil, fmt.Errorf("schedule window %d start: %w", index, err)
		}
		end, err := parseClockMinute(window.End)
		if err != nil {
			return nil, fmt.Errorf("schedule window %d end: %w", index, err)
		}
		rates := [4]int64{}
		for position, value := range []string{window.InputUSDPerMillion, window.CachedInputUSDPerMillion, window.OutputUSDPerMillion, window.FixedRequestUSD} {
			micros, err := domain.ParseUSDMicros(value)
			if err != nil {
				return nil, fmt.Errorf("schedule window %d: %w", index, err)
			}
			rates[position] = micros
		}
		schedule.Windows = append(schedule.Windows, domain.PriceWindow{
			StartMinute: start, EndMinute: end,
			InputMicrosPerMillion: rates[0], CachedInputMicrosPerMillion: rates[1],
			OutputMicrosPerMillion: rates[2], FixedRequestMicrosUSD: rates[3],
		})
	}
	return &schedule, nil
}

// parseClockMinute accepts "HH:MM" and the single end-of-day spelling "24:00",
// which is how a window that runs to midnight is written without pretending the
// next day's 00:00 belongs to it.
func parseClockMinute(value string) (int, error) {
	value = strings.TrimSpace(value)
	hour, minute, found := strings.Cut(value, ":")
	if !found || len(hour) != 2 || len(minute) != 2 {
		return 0, errors.New("time of day must be written HH:MM")
	}
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		if value == "24:00" {
			return 24 * 60, nil
		}
		return 0, errors.New("time of day must be a valid HH:MM clock time")
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
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
	var canonicalPayload any = canonicalRequest
	if input.EffectiveImmediately {
		// The server-selected timestamp must not enter the request digest. A retry
		// with the same idempotency key represents the same immediate intent even
		// though it reaches the server at a later wall-clock time.
		canonicalRequest.EffectiveFrom = time.Time{}
		canonicalPayload = struct {
			Price                domain.DeploymentPriceVersion `json:"price"`
			EffectiveImmediately bool                          `json:"effective_immediately"`
		}{canonicalRequest, true}
	}
	intent, err := newPricingAuditIntent("deployment_price.create", price.ID, admin.session.Username, request.Header.Get("X-Request-ID"), canonicalPayload, &price.Source)
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
	} else if !r.verifyAdminStepUp(writer, request, admin.session.Username, password, totpCode) {
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	pricingUnlock := r.store.LockDeploymentPricingExclusive(price.DeploymentID)
	defer pricingUnlock()
	price, effectiveIntent, replayed, err := r.store.CreateDeploymentPriceVersionIdempotent(request.Context(), price, intent, keyDigest)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
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
	intent, err := newPricingAuditIntent("deployment_price.cancel", priceID, admin.session.Username, request.Header.Get("X-Request-ID"), requestDigest, nil)
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
	intent.RecordSource(currentPrice.Source)
	intent.ChangeSummary = fmt.Sprintf("before={v:%d,status:scheduled,effective:%s} after={v:%d,status:cancelled}", currentPrice.Version, effective.Format(time.RFC3339Nano), currentPrice.Version)
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	pricingUnlock := r.store.LockDeploymentPricingExclusive(deploymentID)
	defer pricingUnlock()
	price, err := r.store.CancelDeploymentPriceVersionWithAuditIntent(request.Context(), deploymentID, priceID, admin.session.Username, now, expected, intent)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
	if err := r.deliverPricingAuditIntent(request.Context(), intent); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(price.Revision))
	writeJSON(writer, http.StatusOK, price)
}

// priceTierPreview is one rung of a previewed price priced against the same
// token counts, so the operator compares the rungs rather than a single number
// whose hour they have to infer.
type priceTierPreview struct {
	Source   domain.PriceTierSource    `json:"source"`
	Timezone string                    `json:"timezone,omitempty"`
	Start    string                    `json:"start,omitempty"`
	End      string                    `json:"end,omitempty"`
	Selected bool                      `json:"selected"`
	Cost     domain.PriceCostBreakdown `json:"cost"`
}

func (r *Runtime) previewAdminDeploymentPrice(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		createPriceInput
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
		// At is the instant to price at. A price that bills by time of day has
		// no single cost, so the answer is meaningless without one; an omitted
		// At is read as "now" rather than as "any time".
		At time.Time `json:"at,omitempty"`
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
	at := now
	if !input.At.IsZero() {
		at = input.At.UTC()
	}
	cost, err := domain.CalculateUSDTokensV1(input.InputTokens, input.CachedInputTokens, input.OutputTokens, price, at)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	tiers, err := previewPriceTiers(price, at, input.InputTokens, input.CachedInputTokens, input.OutputTokens)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"canonical_price": price, "at": at, "cost": cost, "tiers": tiers})
}

// previewPriceTiers prices every rung the version can select, marking the one
// that answers `at`. A version with no schedule has exactly one rung, which is
// what the console showed before schedules existed.
func previewPriceTiers(price domain.DeploymentPriceVersion, at time.Time, inputTokens, cachedInputTokens, outputTokens int64) ([]priceTierPreview, error) {
	selected := price.TierAt(at)
	selectedStart := -1
	if selected.Provenance != nil && selected.Provenance.StartMinute != nil {
		selectedStart = *selected.Provenance.StartMinute
	}
	timezone := ""
	if price.Schedule != nil {
		timezone = price.Schedule.Timezone
	}
	previews := make([]priceTierPreview, 0, 1)
	base := domain.PriceTier{
		InputMicrosPerMillion: price.InputMicrosPerMillion, CachedInputMicrosPerMillion: price.CachedInputMicrosPerMillion,
		OutputMicrosPerMillion: price.OutputMicrosPerMillion, FixedRequestMicrosUSD: price.FixedRequestMicrosUSD,
	}
	baseCost, err := priceTierCost(price.BillingMode, base, inputTokens, cachedInputTokens, outputTokens)
	if err != nil {
		return nil, err
	}
	previews = append(previews, priceTierPreview{
		Source: domain.PriceTierBase, Timezone: timezone, Cost: baseCost,
		Selected: selectedStart < 0 && (selected.Provenance == nil || selected.Provenance.Source == domain.PriceTierBase),
	})
	if price.Schedule == nil {
		return previews, nil
	}
	for _, window := range price.Schedule.Windows {
		tier := domain.PriceTier{
			InputMicrosPerMillion: window.InputMicrosPerMillion, CachedInputMicrosPerMillion: window.CachedInputMicrosPerMillion,
			OutputMicrosPerMillion: window.OutputMicrosPerMillion, FixedRequestMicrosUSD: window.FixedRequestMicrosUSD,
		}
		windowCost, err := priceTierCost(price.BillingMode, tier, inputTokens, cachedInputTokens, outputTokens)
		if err != nil {
			return nil, err
		}
		previews = append(previews, priceTierPreview{
			Source: domain.PriceTierWindow, Timezone: price.Schedule.Timezone,
			Start: formatClockMinute(window.StartMinute), End: formatClockMinute(window.EndMinute),
			Selected: window.StartMinute == selectedStart, Cost: windowCost,
		})
	}
	return previews, nil
}

func priceTierCost(mode domain.BillingMode, tier domain.PriceTier, inputTokens, cachedInputTokens, outputTokens int64) (domain.PriceCostBreakdown, error) {
	if mode == domain.BillingModeFree {
		return domain.PriceCostBreakdown{}, nil
	}
	return tier.Calculate(inputTokens, cachedInputTokens, outputTokens)
}

func formatClockMinute(minute int) string {
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
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
	if !r.verifyAdminStepUp(writer, request, admin.session.Username, input.CurrentPassword, input.TOTPCode) {
		return
	}
	clear([]byte(input.CurrentPassword))
	input.CurrentPassword, input.TOTPCode = "", ""
	deploymentID := chi.URLParam(request, "id")
	intent, err := newPricingAuditIntent(
		"deployment_price.restore_confirm", deploymentID, admin.session.Username,
		request.Header.Get("X-Request-ID"), map[string]string{"deployment_id": deploymentID}, nil,
	)
	if err != nil {
		adminStoreError(writer)
		return
	}
	intent.DeploymentID = deploymentID
	intent.ChangeSummary = "before={pricing_quarantine:true} after={pricing_quarantine:false}"
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	pricingUnlock := r.store.LockDeploymentPricingExclusive(deploymentID)
	defer pricingUnlock()
	if err := r.store.ConfirmRestoredPricingWithAuditIntent(request.Context(), deploymentID, intent); err != nil {
		writePriceError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
	if err := r.deliverPricingAuditIntent(request.Context(), intent); err != nil {
		adminAuditError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"deployment_id": deploymentID, "pricing_quarantine": false})
}

type createPriceProposalInput struct {
	BillingMode              domain.BillingMode        `json:"billing_mode"`
	Currency                 string                    `json:"currency"`
	InputUSDPerMillion       string                    `json:"input_usd_per_million"`
	CachedInputUSDPerMillion string                    `json:"cached_input_usd_per_million"`
	OutputUSDPerMillion      string                    `json:"output_usd_per_million"`
	FixedRequestUSD          string                    `json:"fixed_request_usd"`
	Schedule                 *priceScheduleInput       `json:"schedule,omitempty"`
	Tier                     string                    `json:"tier,omitempty"`
	Warnings                 []string                  `json:"warnings,omitempty"`
	Match                    domain.PriceProposalMatch `json:"match"`
	ExpiresAt                time.Time                 `json:"expires_at"`
	Source                   priceSourceInput          `json:"source"`
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
	// The cache-read rate has no default. An omitted field parses as an error
	// rather than zero, because zero would silently make every cached prompt
	// token free for the life of this price version.
	cachedInputMicros, err := domain.ParseUSDMicros(input.CachedInputUSDPerMillion)
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
	schedule, err := priceScheduleFromInput(input.Schedule)
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
		InputMicrosPerMillion: inputMicros, CachedInputMicrosPerMillion: cachedInputMicros,
		OutputMicrosPerMillion: outputMicros, FixedRequestMicrosUSD: fixedMicros, Schedule: schedule,
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
	intent, err := newPricingAuditIntent("deployment_price.proposal_create", proposal.ID, admin.session.Username, request.Header.Get("X-Request-ID"), canonicalRequest, &proposal.Source)
	if err != nil {
		adminStoreError(writer)
		return
	}
	intent.DeploymentID = proposal.DeploymentID
	intent.ChangeSummary = fmt.Sprintf("proposal=%s match=%s tier=%s billing=%s input=%d cached_input=%d output=%d fixed=%d", proposal.ID, proposal.Match, proposal.Tier, proposal.BillingMode, proposal.InputMicrosPerMillion, proposal.CachedInputMicrosPerMillion, proposal.OutputMicrosPerMillion, proposal.FixedRequestMicrosUSD) + proposal.Schedule.AuditSummary()
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
		EffectiveFrom        time.Time `json:"effective_from"`
		EffectiveImmediately bool      `json:"effective_immediately,omitempty"`
		Confirm              bool      `json:"confirm"`
		CurrentPassword      string    `json:"current_password"`
		TOTPCode             string    `json:"totp_code,omitempty"`
	}
	if err := decodeAdminJSON(request, &input); err != nil || !input.Confirm {
		writePriceError(writer, errors.New("explicit proposal adoption confirmation is required"))
		return
	}
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	if !r.verifyAdminStepUp(writer, request, admin.session.Username, input.CurrentPassword, input.TOTPCode) {
		return
	}
	clear([]byte(input.CurrentPassword))
	deploymentID, proposalID := chi.URLParam(request, "id"), chi.URLParam(request, "proposalID")
	now := time.Now().UTC()
	// Adoption takes effect on the same two terms direct creation does. The
	// server timestamp is chosen here rather than by the client so "now" cannot
	// be backdated past an already-effective version.
	effectiveFrom, err := resolvePriceEffectiveFrom(input.EffectiveFrom, input.EffectiveImmediately, now)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	priceID, err := id.New("price")
	if err != nil {
		adminStoreError(writer)
		return
	}
	// The adopted price inherits the proposal's source verbatim, so the audit
	// intent is built from that source and not from the adoption request, which
	// carries no provenance of its own. The store re-reads it inside the
	// adoption transaction, which is what finally lands in the record.
	proposed, err := r.store.GetDeploymentPriceProposal(request.Context(), deploymentID, proposalID)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	// The server-selected timestamp stays out of the request digest for the same
	// reason it does on the create path: "immediately" is the intent, and the
	// wall clock it lands on is the server's answer to it.
	intent, err := newPricingAuditIntent("deployment_price.proposal_adopt", proposalID, admin.session.Username, request.Header.Get("X-Request-ID"),
		map[string]any{"proposal_id": proposalID, "effective_from": input.EffectiveFrom, "effective_immediately": input.EffectiveImmediately, "revision": expected}, &proposed.Source)
	if err != nil {
		adminStoreError(writer)
		return
	}
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	pricingUnlock := r.store.LockDeploymentPricingExclusive(deploymentID)
	defer pricingUnlock()
	proposal, price, err := r.store.AdoptDeploymentPriceProposal(request.Context(), deploymentID, proposalID, priceID, admin.session.Username, effectiveFrom, now, expected, intent)
	if err != nil {
		writePriceError(writer, err)
		return
	}
	r.activateTopologyAfterCommit()
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
	intent, err := newPricingAuditIntent("deployment_price.proposal_reject", proposalID, admin.session.Username, request.Header.Get("X-Request-ID"), map[string]any{"proposal_id": proposalID, "revision": expected}, nil)
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

var (
	errPriceEffectiveFromConflict = errors.New("effective_from and effective_immediately are mutually exclusive")
	errPriceEffectiveFromRequired = errors.New("effective_from or effective_immediately is required")
)

// resolvePriceEffectiveFrom is the single rule for when a newly established
// price starts applying, shared by direct creation and proposal adoption.
//
// "Immediately" is deliberately the server's clock and not a client-supplied
// timestamp that happens to be near now: a client-chosen instant can be
// backdated behind an already-effective version, which would rewrite what past
// requests were billed at.
func resolvePriceEffectiveFrom(effectiveFrom time.Time, immediately bool, now time.Time) (time.Time, error) {
	if immediately {
		if !effectiveFrom.IsZero() {
			return time.Time{}, errPriceEffectiveFromConflict
		}
		return now.UTC(), nil
	}
	if effectiveFrom.IsZero() {
		return time.Time{}, errPriceEffectiveFromRequired
	}
	return effectiveFrom.UTC(), nil
}

func priceVersionFromInput(priceID, deploymentID, actor string, now time.Time, input createPriceInput) (domain.DeploymentPriceVersion, error) {
	inputMicros, err := domain.ParseUSDMicros(input.InputUSDPerMillion)
	if err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	// An omitted cache-read rate is a rejected request, not a free cached span.
	cachedInputMicros, err := domain.ParseUSDMicros(input.CachedInputUSDPerMillion)
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
	effectiveFrom, err := resolvePriceEffectiveFrom(input.EffectiveFrom, input.EffectiveImmediately, now)
	if err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	schedule, err := priceScheduleFromInput(input.Schedule)
	if err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	price := domain.DeploymentPriceVersion{
		ID: priceID, DeploymentID: deploymentID, BillingMode: input.BillingMode,
		Currency: strings.ToUpper(strings.TrimSpace(input.Currency)), FormulaVersion: domain.PriceFormulaUSDTokensV1,
		InputMicrosPerMillion: inputMicros, CachedInputMicrosPerMillion: cachedInputMicros,
		OutputMicrosPerMillion: outputMicros, FixedRequestMicrosUSD: fixedMicros, Schedule: schedule,
		EffectiveFrom: effectiveFrom, Source: source, CreatedBy: actor, CreatedAt: now,
	}
	validation := price
	validation.Version, validation.Revision = 1, 1
	if err := validation.Validate(); err != nil {
		return domain.DeploymentPriceVersion{}, err
	}
	return price, nil
}

// newPricingAuditIntent builds the audit intent for a pricing mutation.
//
// `request` is the canonical request payload, and it exists for one purpose: to
// be hashed into RequestSHA256. Its shape is free to vary with what makes a
// retry the same request — the immediate-effect creation path, for instance,
// keeps the server-chosen timestamp out of it. `source` is the separate,
// mandatory argument for the provenance of a price this action establishes,
// precisely so that evidence never depends on the digest payload's shape. It is
// nil for the actions that establish no price of their own.
func newPricingAuditIntent(action, targetID, actor, correlationID string, request any, source *domain.PriceSource) (domain.PricingAuditIntent, error) {
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
	if source != nil {
		intent.RecordSource(*source)
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

// verifyReauthenticationMaterial checks step-up material: the caller's current
// password plus a fresh TOTP code, resupplied in the request body and verified
// per request rather than exchanged for a short-lived elevated session.
//
// It is deliberately not the entry point. On its own it is unbounded and
// silent, which is an offline-speed password oracle behind a stolen session.
// Call verifyAdminStepUp, which adds the rate limit and the audit record.
func (r *Runtime) verifyReauthenticationMaterial(writer http.ResponseWriter, request *http.Request, username, passwordText, code string) bool {
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
	// The []byte is the shape VerifyPassword takes, not a scrubbing boundary:
	// the password reached here as a JSON-decoded string and cannot be
	// overwritten. See requireDestructiveStepUp for why nothing here pretends
	// otherwise.
	user, err := r.store.GetAdminUser(request.Context(), username)
	if err != nil || !adminauth.VerifyPassword(user, []byte(passwordText)) {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "recent re-authentication required", "code": "recent_reauth_required"})
		return false
	}
	return true
}

func writePriceError(writer http.ResponseWriter, err error) {
	status, code := http.StatusBadRequest, "invalid_price"
	switch {
	// These two carry a stable code because the Admin console has to say
	// something specific about them in the operator's own language; the English
	// message is a fallback, not the contract.
	case errors.Is(err, errPriceEffectiveFromConflict):
		code = "price_effective_from_conflict"
	case errors.Is(err, errPriceEffectiveFromRequired):
		code = "price_effective_from_required"
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
	digest.Write([]byte("halro:pricing-idempotency:v1\x00"))
	digest.Write([]byte(actor))
	digest.Write([]byte{0})
	digest.Write([]byte(deploymentID))
	digest.Write([]byte{0})
	digest.Write([]byte(key))
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}
