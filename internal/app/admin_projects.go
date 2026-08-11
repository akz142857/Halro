package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/auth"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/id"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/go-chi/chi/v5"
)

type projectInput struct {
	Name                     string   `json:"name"`
	Enabled                  bool     `json:"enabled"`
	AllowedRoutes            []string `json:"allowed_routes"`
	RPM                      int64    `json:"rpm"`
	TPM                      int64    `json:"tpm"`
	MaxConcurrency           int64    `json:"max_concurrency"`
	DailyBudgetMicrosUSD     int64    `json:"daily_budget_micros_usd"`
	MaxInputTokens           int64    `json:"max_input_tokens"`
	MaxOutputTokens          int64    `json:"max_output_tokens"`
	MaxRequestBytes          int64    `json:"max_request_bytes"`
	MaxStreamDurationSeconds int64    `json:"max_stream_duration_seconds"`
	AllowedCIDRs             []string `json:"allowed_cidrs"`
	RedactionPolicyID        string   `json:"redaction_policy_id"`
	TokenGuardPolicyID       string   `json:"token_guard_policy_id"`
}

type gatewayKeyInput struct {
	Name      string     `json:"name"`
	Enabled   *bool      `json:"enabled,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Minting a key is gated the same way a destructive delete is. A Gateway
	// Key is returned in plaintext once and outlives the Admin session that
	// asked for it, so a stolen session that cannot delete anything could still
	// walk away with durable, billable access — a better outcome for the thief
	// than any of the deletions step-up already covered. Carried on this struct
	// rather than read by the shared middleware because the body is decoded
	// here and can only be read once.
	CurrentPassword string `json:"current_password,omitempty"`
	TOTPCode        string `json:"totp_code,omitempty"`
}

func (r *Runtime) createAdminProject(writer http.ResponseWriter, request *http.Request) {
	var input projectInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	projectID, err := id.New("prj")
	if err != nil {
		adminStoreError(writer)
		return
	}
	now := time.Now().UTC()
	project, err := input.project(projectID, now, now)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	// validateProjectReferences reads routes, so the topology coordinator is held
	// across the check and the commit — otherwise the check could see a route that
	// a concurrent delete removes before this write lands. Topology first, matching
	// the order the route handlers take them in.
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	if err := r.validateProjectReferences(request, project); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	project, err = r.store.PutProject(request.Context(), project, 0)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if !r.refreshAdminAuth(writer, request) {
		return
	}
	if err := r.auditAdminMutation(request, "project.create", "project", project.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(project.Revision))
	writeJSON(writer, http.StatusCreated, project)
}

func (r *Runtime) updateAdminProject(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input projectInput
	if err := decodeAdminJSON(request, &input); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	// validateProjectReferences reads routes, so the topology coordinator is held
	// across the check and the commit — otherwise the check could see a route that
	// a concurrent delete removes before this write lands. Topology first, matching
	// the order the route handlers take them in.
	r.adminTopologyMu.Lock()
	defer r.adminTopologyMu.Unlock()
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	current, err := r.store.GetProject(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if current.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	replacement, err := input.project(current.ID, current.CreatedAt, time.Now().UTC())
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	replacement.DeletedAt = current.DeletedAt
	if err := r.validateProjectReferences(request, replacement); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	replacement, err = r.store.PutProject(request.Context(), replacement, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if !r.refreshAdminAuth(writer, request) {
		return
	}
	if err := r.auditAdminMutation(request, "project.update", "project", replacement.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(replacement.Revision))
	writeJSON(writer, http.StatusOK, replacement)
}

func (r *Runtime) deleteAdminProject(writer http.ResponseWriter, request *http.Request) {
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
	project, err := r.store.GetProject(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if project.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	now := time.Now().UTC()
	project.Enabled = false
	project.UpdatedAt = now
	project.DeletedAt = &now
	project, err = r.store.PutProject(request.Context(), project, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if !r.refreshAdminAuth(writer, request) {
		return
	}
	if err := r.auditAdminMutation(request, "project.delete", "project", project.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(project.Revision))
	writer.WriteHeader(http.StatusNoContent)
}

// Lifting a Token Guard block is a security control being switched off, which
// is the other half of the step-up criterion: not only what destroys state, but
// what removes a protection that is currently in force.
func (r *Runtime) unblockAdminProject(writer http.ResponseWriter, request *http.Request) {
	if !r.requireDestructiveStepUp(writer, request) {
		return
	}
	projectID := chi.URLParam(request, "id")
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	project, err := r.store.GetProject(request.Context(), projectID)
	if err != nil || project.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	count := r.tokenGuard.UnblockProject(projectID)
	if err := r.auditAdminMutation(request, "project.unblock", "project", projectID); err != nil {
		adminAuditError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "unblocked", "subjects": count})
}

func (r *Runtime) createAdminProjectKey(writer http.ResponseWriter, request *http.Request) {
	idempotencyKey := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 256 {
		adminBadRequestCode(writer, "idempotency_key_required", "Idempotency-Key is required")
		return
	}
	var input gatewayKeyInput
	if err := decodeAdminJSON(request, &input); err != nil || strings.TrimSpace(input.Name) == "" {
		adminBadRequest(writer, "invalid request")
		return
	}
	password := input.CurrentPassword
	input.CurrentPassword = ""
	projectID := chi.URLParam(request, "id")
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	if !r.verifyAdminStepUp(writer, request, admin.session.Username, password, input.TOTPCode) {
		return
	}
	// The plaintext exists only in this response, so a retry can never replay it. Deriving
	// the record ID from the idempotency key turns a retried create into a storage
	// collision instead of a second live credential the operator never learns about.
	keyID := deterministicMutationID("key", gatewayKeyIdempotencyDigest(admin.session.Username, projectID, idempotencyKey))
	plaintext, key, err := auth.GenerateGatewayKeyWithID(keyID, projectID, input.Name, input.ExpiresAt)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	project, err := r.store.GetProject(request.Context(), projectID)
	if err != nil || project.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	key, err = r.store.PutGatewayKey(request.Context(), key, 0)
	if errors.Is(err, boltstore.ErrAlreadyExists) {
		// The first attempt already landed. Say so explicitly rather than minting a
		// duplicate: the operator has to revoke and reissue to obtain a plaintext.
		writeJSON(writer, http.StatusConflict, map[string]string{
			"code":  "gateway_key_idempotency_replay",
			"error": "this request already created gateway key " + keyID,
			"id":    keyID,
		})
		return
	}
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if !r.refreshAdminAuth(writer, request) {
		return
	}
	if err := r.auditAdminMutation(request, "gateway_key.create", "gateway_key", key.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("ETag", revisionETag(key.Revision))
	writeJSON(writer, http.StatusCreated, map[string]any{
		"key": plaintext,
		"metadata": gatewayKeyView{
			ID: key.ID, ProjectID: key.ProjectID, Name: key.Name, Enabled: key.Enabled,
			ExpiresAt: key.ExpiresAt, CreatedAt: key.CreatedAt, Revision: key.Revision,
		},
	})
}

func (r *Runtime) getAdminProjectKey(writer http.ResponseWriter, request *http.Request) {
	key, ok := r.adminProjectKey(writer, request)
	if !ok {
		return
	}
	writer.Header().Set("ETag", revisionETag(key.Revision))
	writeJSON(writer, http.StatusOK, gatewayKeyView{
		ID: key.ID, ProjectID: key.ProjectID, Name: key.Name, Enabled: key.Enabled,
		ExpiresAt: key.ExpiresAt, CreatedAt: key.CreatedAt, Revision: key.Revision,
	})
}

func (r *Runtime) updateAdminProjectKey(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input gatewayKeyInput
	if err := decodeAdminJSON(request, &input); err != nil || strings.TrimSpace(input.Name) == "" {
		adminBadRequest(writer, "invalid request")
		return
	}
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	key, ok := r.adminProjectKey(writer, request)
	if !ok {
		return
	}
	if key.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	key.Name = input.Name
	key.ExpiresAt = input.ExpiresAt
	if input.Enabled != nil {
		key.Enabled = *input.Enabled
	}
	key, err := r.store.PutGatewayKey(request.Context(), key, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if !r.refreshAdminAuth(writer, request) {
		return
	}
	if err := r.auditAdminMutation(request, "gateway_key.update", "gateway_key", key.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(key.Revision))
	writeJSON(writer, http.StatusOK, gatewayKeyView{
		ID: key.ID, ProjectID: key.ProjectID, Name: key.Name, Enabled: key.Enabled,
		ExpiresAt: key.ExpiresAt, CreatedAt: key.CreatedAt, Revision: key.Revision,
	})
}

func (r *Runtime) deleteAdminProjectKey(writer http.ResponseWriter, request *http.Request) {
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
	key, ok := r.adminProjectKey(writer, request)
	if !ok {
		return
	}
	if key.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	now := time.Now().UTC()
	key.Enabled = false
	key.DeletedAt = &now
	key, err := r.store.PutGatewayKey(request.Context(), key, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if !r.refreshAdminAuth(writer, request) {
		return
	}
	if err := r.auditAdminMutation(request, "gateway_key.delete", "gateway_key", key.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(key.Revision))
	writer.WriteHeader(http.StatusNoContent)
}

func (input projectInput) project(id string, createdAt, updatedAt time.Time) (domain.Project, error) {
	cidrs := make([]netip.Prefix, 0, len(input.AllowedCIDRs))
	for _, raw := range input.AllowedCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			// The console offers "a single IPv4, IPv6 or CIDR range", so a bare address
			// has to mean the host itself rather than being rejected as malformed.
			address, addressErr := netip.ParseAddr(raw)
			if addressErr != nil {
				return domain.Project{}, errors.New("allowed_cidrs contains an invalid CIDR: " + raw)
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		cidrs = append(cidrs, prefix.Masked())
	}
	if input.MaxStreamDurationSeconds < 0 {
		return domain.Project{}, errors.New("max_stream_duration_seconds cannot be negative")
	}
	project := domain.Project{
		ID: id, Name: input.Name, Enabled: input.Enabled,
		// Never nil: a nil slice marshals to JSON null, which clients cannot iterate.
		AllowedRoutes: append([]string{}, input.AllowedRoutes...), RPM: input.RPM, TPM: input.TPM,
		MaxConcurrency: input.MaxConcurrency, DailyBudgetMicrosUSD: input.DailyBudgetMicrosUSD,
		MaxInputTokens: input.MaxInputTokens, MaxOutputTokens: input.MaxOutputTokens,
		MaxRequestBytes:   input.MaxRequestBytes,
		MaxStreamDuration: time.Duration(input.MaxStreamDurationSeconds) * time.Second,
		AllowedCIDRs:      cidrs, RedactionPolicyID: input.RedactionPolicyID,
		TokenGuardPolicyID: input.TokenGuardPolicyID, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	return project, project.Validate()
}

func (r *Runtime) adminProjectKey(writer http.ResponseWriter, request *http.Request) (domain.GatewayKey, bool) {
	key, err := r.store.GetGatewayKey(request.Context(), chi.URLParam(request, "keyID"))
	if err != nil || key.ProjectID != chi.URLParam(request, "id") || key.DeletedAt != nil {
		adminNotFound(writer)
		return domain.GatewayKey{}, false
	}
	return key, true
}

func (r *Runtime) validateProjectReferences(request *http.Request, project domain.Project) error {
	if project.TokenGuardPolicyID != "" {
		policy, err := r.store.GetTokenGuardPolicy(request.Context(), project.TokenGuardPolicyID)
		if err != nil || policy.DeletedAt != nil || !policy.Enabled {
			return errors.New("token guard policy is unavailable")
		}
	}
	if project.RedactionPolicyID != "" {
		policy, err := r.store.GetRedactionPolicy(request.Context(), project.RedactionPolicyID)
		if err != nil || policy.DeletedAt != nil || !policy.Enabled {
			return errors.New("redaction policy is unavailable")
		}
	}
	if len(project.AllowedRoutes) > 0 {
		routes, err := r.store.ListRoutes(request.Context())
		if err != nil {
			return errors.New("routes are unavailable")
		}
		known := make(map[string]struct{}, len(routes))
		for _, route := range routes {
			if route.DeletedAt == nil {
				known[route.PublicModel] = struct{}{}
			}
		}
		// A disabled route stays bindable — the console surfaces it as unavailable. An
		// alias with no route at all only fails at request time, silently, so reject here.
		for _, alias := range project.AllowedRoutes {
			if _, ok := known[alias]; !ok {
				return errors.New("allowed_routes references unknown model alias " + alias)
			}
		}
	}
	return nil
}

func gatewayKeyIdempotencyDigest(actor, projectID, key string) string {
	digest := sha256.New()
	digest.Write([]byte("halro:gateway-key-idempotency:v1\x00"))
	digest.Write([]byte(actor))
	digest.Write([]byte{0})
	digest.Write([]byte(projectID))
	digest.Write([]byte{0})
	digest.Write([]byte(key))
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func (r *Runtime) refreshAdminAuth(writer http.ResponseWriter, request *http.Request) bool {
	ctx, cancel := r.activationContext()
	defer cancel()
	if err := r.auth.Refresh(ctx, r.store); err != nil {
		// The write is already durable, so this is the disagreement case rather
		// than a rejected request: the operator has revoked something that is
		// still being honoured. Loud, because the reply alone tells them the
		// mutation failed, which is not what happened.
		r.logger.Error("authentication snapshot activation failed after a durable mutation",
			"error", err)
		adminStoreError(writer)
		return false
	}
	return true
}

func (r *Runtime) auditAdminMutation(request *http.Request, action, targetType, targetID string) error {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	return r.appendAdminAudit("admin_user", admin.session.Username, action, targetType, targetID, "success", "")
}
