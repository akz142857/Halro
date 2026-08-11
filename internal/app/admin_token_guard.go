package app

import (
	"errors"
	"net/http"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/tokenguard"
	"github.com/go-chi/chi/v5"
)

type tokenGuardInput struct {
	Name                            string  `json:"name"`
	Enabled                         bool    `json:"enabled"`
	Action                          string  `json:"action"`
	RequestTokens                   int64   `json:"request_tokens"`
	TokensPerMinute                 int64   `json:"tokens_per_minute"`
	CostMicrosPerMinute             int64   `json:"cost_micros_per_minute"`
	ErrorRate                       float64 `json:"error_rate"`
	MinimumSamples                  int64   `json:"minimum_samples"`
	Concurrency                     int64   `json:"concurrency"`
	UniqueIPsPerMinute              int64   `json:"unique_ips_per_minute"`
	ViolationsBeforeBlock           int64   `json:"violations_before_block"`
	BlockTTLSeconds                 int64   `json:"block_ttl_seconds"`
	CooldownSeconds                 int64   `json:"cooldown_seconds"`
	EWMAEnabled                     bool    `json:"ewma_enabled"`
	EWMAAlpha                       float64 `json:"ewma_alpha"`
	EWMAMultiplier                  float64 `json:"ewma_multiplier"`
	EWMAMinimumSamples              int64   `json:"ewma_minimum_samples"`
	EWMAWarmupSeconds               int64   `json:"ewma_warmup_seconds"`
	EWMAEvaluationWindowSeconds     int64   `json:"ewma_evaluation_window_seconds"`
	EWMACooldownSeconds             int64   `json:"ewma_cooldown_seconds"`
	EWMAAbsoluteRPM                 int64   `json:"ewma_absolute_rpm"`
	EWMAAbsoluteTPM                 int64   `json:"ewma_absolute_tpm"`
	EWMAAbsoluteTokensPerRequest    float64 `json:"ewma_absolute_tokens_per_request"`
	EWMAAbsoluteCostMicrosPerMinute int64   `json:"ewma_absolute_cost_micros_per_minute"`
}

type tokenGuardPreviewInput struct {
	EstimatedTokens        int64                    `json:"estimated_tokens"`
	EstimatedCostMicrosUSD int64                    `json:"estimated_cost_micros_usd"`
	Concurrency            int64                    `json:"concurrency"`
	HasNewSource           bool                     `json:"has_new_source"`
	Window                 tokenguard.PreviewWindow `json:"window"`
}

type tokenGuardView struct {
	ID                              string  `json:"id"`
	Name                            string  `json:"name"`
	Enabled                         bool    `json:"enabled"`
	Action                          string  `json:"action"`
	RequestTokens                   int64   `json:"request_tokens"`
	TokensPerMinute                 int64   `json:"tokens_per_minute"`
	CostMicrosPerMinute             int64   `json:"cost_micros_per_minute"`
	ErrorRate                       float64 `json:"error_rate"`
	MinimumSamples                  int64   `json:"minimum_samples"`
	Concurrency                     int64   `json:"concurrency"`
	UniqueIPsPerMinute              int64   `json:"unique_ips_per_minute"`
	ViolationsBeforeBlock           int64   `json:"violations_before_block"`
	BlockTTLSeconds                 int64   `json:"block_ttl_seconds"`
	CooldownSeconds                 int64   `json:"cooldown_seconds"`
	EWMAEnabled                     bool    `json:"ewma_enabled"`
	EWMAAlpha                       float64 `json:"ewma_alpha"`
	EWMAMultiplier                  float64 `json:"ewma_multiplier"`
	EWMAMinimumSamples              int64   `json:"ewma_minimum_samples"`
	EWMAWarmupSeconds               int64   `json:"ewma_warmup_seconds"`
	EWMAEvaluationWindowSeconds     int64   `json:"ewma_evaluation_window_seconds"`
	EWMACooldownSeconds             int64   `json:"ewma_cooldown_seconds"`
	EWMAAbsoluteRPM                 int64   `json:"ewma_absolute_rpm"`
	EWMAAbsoluteTPM                 int64   `json:"ewma_absolute_tpm"`
	EWMAAbsoluteTokensPerRequest    float64 `json:"ewma_absolute_tokens_per_request"`
	EWMAAbsoluteCostMicrosPerMinute int64   `json:"ewma_absolute_cost_micros_per_minute"`
	Revision                        uint64  `json:"revision"`
	BoundProjects                   int     `json:"bound_projects,omitempty"`
}

func (r *Runtime) getAdminTokenGuardPolicy(writer http.ResponseWriter, request *http.Request) {
	policy, err := r.store.GetTokenGuardPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || policy.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writeJSON(writer, http.StatusOK, tokenGuardPolicyView(policy))
}

func (r *Runtime) createAdminTokenGuardPolicy(writer http.ResponseWriter, request *http.Request) {
	var input tokenGuardInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	policyID, err := id.New("tgp")
	if err != nil {
		adminStoreError(writer)
		return
	}
	now := time.Now().UTC()
	policy := input.policy(policyID, now, now)
	if err := policy.Validate(); err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	intent, intentErr := r.newAdminAuditIntent(request, "token_guard_policy.create", "token_guard_policy", policy.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	policy, err = r.store.PutTokenGuardPolicy(request.Context(), policy, 0, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.activateTokenGuardPolicies()
	r.completeAdminMutation(writer, request, *intent)
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writeJSON(writer, http.StatusCreated, tokenGuardPolicyView(policy))
}

func (r *Runtime) updateAdminTokenGuardPolicy(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input tokenGuardInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	current, err := r.store.GetTokenGuardPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || current.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if current.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	policy := input.policy(current.ID, current.CreatedAt, time.Now().UTC())
	if err := policy.Validate(); err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	if err := r.validateTokenGuardCanDeactivate(request, policy.ID, policy.Enabled); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	intent, intentErr := r.newAdminAuditIntent(request, "token_guard_policy.update", "token_guard_policy", policy.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	policy, err = r.store.PutTokenGuardPolicy(request.Context(), policy, expected, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.activateTokenGuardPolicies()
	r.completeAdminMutation(writer, request, *intent)
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writeJSON(writer, http.StatusOK, tokenGuardPolicyView(policy))
}

func (r *Runtime) deleteAdminTokenGuardPolicy(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	// Deleting is not undoable and a stolen session should not be enough to do
	// it. The revision precondition is checked first: it costs a header parse,
	// while the step-up costs an Argon2id verification, and a request that
	// cannot succeed anyway should not buy one.
	if !r.requireDestructiveStepUp(writer, request) {
		return
	}
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	policy, err := r.store.GetTokenGuardPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || policy.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if policy.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	if err := r.validateTokenGuardCanDelete(request, policy.ID); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	policy.Enabled = false
	policy.UpdatedAt = now
	policy.DeletedAt = &now
	intent, intentErr := r.newAdminAuditIntent(request, "token_guard_policy.delete", "token_guard_policy", policy.ID)
	if intentErr != nil {
		adminStoreError(writer)
		return
	}
	policy, err = r.store.PutTokenGuardPolicy(request.Context(), policy, expected, intent)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	r.activateTokenGuardPolicies()
	r.completeAdminMutation(writer, request, *intent)
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writer.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) testAdminTokenGuardPolicy(writer http.ResponseWriter, request *http.Request) {
	policy, err := r.store.GetTokenGuardPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || policy.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	var input tokenGuardPreviewInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	if input.EstimatedTokens < 0 || input.EstimatedCostMicrosUSD < 0 || input.Concurrency < 0 {
		adminBadRequest(writer, "preview values cannot be negative")
		return
	}
	var source [32]byte
	if input.HasNewSource {
		source[0] = 1
	}
	result, err := tokenguard.Preview(policy, tokenguard.Input{
		EstimatedTokens: input.EstimatedTokens, EstimatedCostMicrosUSD: input.EstimatedCostMicrosUSD,
		Concurrency: input.Concurrency, HasSource: input.HasNewSource, SourceHash: source,
	}, input.Window)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	if err := r.auditAdminMutation(request, "token_guard_policy.test", "token_guard_policy", policy.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (input tokenGuardInput) policy(id string, createdAt, updatedAt time.Time) domain.TokenGuardPolicy {
	return domain.TokenGuardPolicy{
		ID: id, Name: input.Name, Enabled: input.Enabled, Action: input.Action,
		RequestTokens: input.RequestTokens, TokensPerMinute: input.TokensPerMinute,
		CostMicrosPerMinute: input.CostMicrosPerMinute, ErrorRate: input.ErrorRate,
		MinimumSamples: input.MinimumSamples, Concurrency: input.Concurrency,
		UniqueIPsPerMinute:    input.UniqueIPsPerMinute,
		ViolationsBeforeBlock: input.ViolationsBeforeBlock,
		BlockTTL:              time.Duration(input.BlockTTLSeconds) * time.Second,
		Cooldown:              time.Duration(input.CooldownSeconds) * time.Second,
		EWMAEnabled:           input.EWMAEnabled, EWMAAlpha: input.EWMAAlpha,
		EWMAMultiplier: input.EWMAMultiplier, EWMAMinimumSamples: input.EWMAMinimumSamples,
		EWMAWarmup:           time.Duration(input.EWMAWarmupSeconds) * time.Second,
		EWMAEvaluationWindow: time.Duration(input.EWMAEvaluationWindowSeconds) * time.Second,
		EWMACooldown:         time.Duration(input.EWMACooldownSeconds) * time.Second,
		EWMAAbsoluteRPM:      input.EWMAAbsoluteRPM, EWMAAbsoluteTPM: input.EWMAAbsoluteTPM,
		EWMAAbsoluteTokensPerRequest:    input.EWMAAbsoluteTokensPerRequest,
		EWMAAbsoluteCostMicrosPerMinute: input.EWMAAbsoluteCostMicrosPerMinute,
		CreatedAt:                       createdAt, UpdatedAt: updatedAt,
	}
}

func tokenGuardPolicyView(policy domain.TokenGuardPolicy) tokenGuardView {
	return tokenGuardView{
		ID: policy.ID, Name: policy.Name, Enabled: policy.Enabled, Action: policy.Action,
		RequestTokens: policy.RequestTokens, TokensPerMinute: policy.TokensPerMinute,
		CostMicrosPerMinute: policy.CostMicrosPerMinute, ErrorRate: policy.ErrorRate,
		MinimumSamples: policy.MinimumSamples, Concurrency: policy.Concurrency,
		UniqueIPsPerMinute:    policy.UniqueIPsPerMinute,
		ViolationsBeforeBlock: policy.ViolationsBeforeBlock,
		BlockTTLSeconds:       int64(policy.BlockTTL / time.Second),
		CooldownSeconds:       int64(policy.Cooldown / time.Second),
		EWMAEnabled:           policy.EWMAEnabled, EWMAAlpha: policy.EWMAAlpha,
		EWMAMultiplier: policy.EWMAMultiplier, EWMAMinimumSamples: policy.EWMAMinimumSamples,
		EWMAWarmupSeconds:           int64(policy.EWMAWarmup / time.Second),
		EWMAEvaluationWindowSeconds: int64(policy.EWMAEvaluationWindow / time.Second),
		EWMACooldownSeconds:         int64(policy.EWMACooldown / time.Second),
		EWMAAbsoluteRPM:             policy.EWMAAbsoluteRPM, EWMAAbsoluteTPM: policy.EWMAAbsoluteTPM,
		EWMAAbsoluteTokensPerRequest:    policy.EWMAAbsoluteTokensPerRequest,
		EWMAAbsoluteCostMicrosPerMinute: policy.EWMAAbsoluteCostMicrosPerMinute,
		Revision:                        policy.Revision,
	}
}

func (r *Runtime) validateTokenGuardCanDeactivate(request *http.Request, id string, enabled bool) error {
	if enabled {
		return nil
	}
	return r.validateNoTokenGuardReference(request, id, func(project domain.Project) bool {
		return project.Enabled
	})
}

// validateTokenGuardCanDelete consults disabled projects as well; see
// validateRedactionCanDelete for why a reference held by a switched-off project
// is still a reference.
func (r *Runtime) validateTokenGuardCanDelete(request *http.Request, id string) error {
	return r.validateNoTokenGuardReference(request, id, func(domain.Project) bool { return true })
}

func (r *Runtime) validateNoTokenGuardReference(
	request *http.Request,
	id string,
	consider func(domain.Project) bool,
) error {
	projects, err := r.store.ListProjects(request.Context())
	if err != nil {
		return errors.New("projects are unavailable")
	}
	for _, project := range projects {
		if project.DeletedAt == nil && consider(project) && project.TokenGuardPolicyID == id {
			return errors.New("remove this policy from the projects that reference it first")
		}
	}
	return nil
}

// activateTokenGuardPolicies carries a durable Token Guard policy change into
// the live admission control, on the runtime's own context and without failing
// the request that committed it. A Token Guard policy decides what traffic is
// admitted, so serving from a snapshot known to be behind the store is the
// fail-open direction — the runtime is marked stale instead.
func (r *Runtime) activateTokenGuardPolicies() {
	ctx, cancel := r.activationContext()
	defer cancel()
	policies, err := r.store.ListTokenGuardPolicies(ctx)
	if err == nil {
		err = r.tokenGuard.ReplacePolicies(policies)
	}
	if err != nil {
		r.logger.Error("token guard policy activation failed after a durable mutation", "error", err)
		r.activation.markStale("token guard policies: "+err.Error(), time.Now().UTC())
		return
	}
	r.activation.markCurrent()
}
