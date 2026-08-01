package app

import (
	"errors"
	"net/http"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/id"
	"github.com/akz142857/Heimdall/internal/redaction"
	"github.com/go-chi/chi/v5"
)

type redactionRuleInput struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Builtin     string   `json:"builtin"`
	Pattern     string   `json:"pattern"`
	Dictionary  []string `json:"dictionary"`
	Scopes      []string `json:"scopes"`
	Action      string   `json:"action"`
	Replacement string   `json:"replacement"`
	Enabled     bool     `json:"enabled"`
	Priority    int      `json:"priority"`
}

type redactionPolicyInput struct {
	Name    string               `json:"name"`
	Enabled bool                 `json:"enabled"`
	Mode    string               `json:"mode"`
	Rules   []redactionRuleInput `json:"rules"`
}

type redactionTestInput struct {
	Input string `json:"input"`
	Scope string `json:"scope"`
}

func (r *Runtime) listAdminRedactionPolicies(writer http.ResponseWriter, request *http.Request) {
	items, err := r.store.ListRedactionPolicies(request.Context())
	if err != nil {
		adminStoreError(writer)
		return
	}
	active := make([]domain.RedactionPolicy, 0, len(items))
	for _, item := range items {
		if item.DeletedAt == nil {
			active = append(active, item)
		}
	}
	writeResourcePage(writer, request, active, func(item domain.RedactionPolicy) string { return item.ID })
}

func (r *Runtime) getAdminRedactionPolicy(writer http.ResponseWriter, request *http.Request) {
	policy, err := r.store.GetRedactionPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || policy.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writeJSON(writer, http.StatusOK, policy)
}

func (r *Runtime) createAdminRedactionPolicy(writer http.ResponseWriter, request *http.Request) {
	var input redactionPolicyInput
	if err := decodeAdminJSONLimit(request, &input, 256<<10); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	policyID, err := id.New("rdp")
	if err != nil {
		adminStoreError(writer)
		return
	}
	now := time.Now().UTC()
	policy, err := input.policy(policyID, now, now)
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	policy, err = r.store.PutRedactionPolicy(request.Context(), policy, 0)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if !r.reloadRedaction(writer, request) {
		return
	}
	if err := r.auditAdminMutation(request, "redaction_policy.create", "redaction_policy", policy.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writeJSON(writer, http.StatusCreated, policy)
}

func (r *Runtime) updateAdminRedactionPolicy(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	var input redactionPolicyInput
	if err := decodeAdminJSONLimit(request, &input, 256<<10); err != nil {
		adminBadRequest(writer, "invalid request")
		return
	}
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	current, err := r.store.GetRedactionPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || current.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if current.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	policy, err := input.policy(current.ID, current.CreatedAt, time.Now().UTC())
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	if err := r.validateRedactionCanDeactivate(request, policy.ID, policy.Enabled); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	policy, err = r.store.PutRedactionPolicy(request.Context(), policy, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if !r.reloadRedaction(writer, request) {
		return
	}
	if err := r.auditAdminMutation(request, "redaction_policy.update", "redaction_policy", policy.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writeJSON(writer, http.StatusOK, policy)
}

func (r *Runtime) deleteAdminRedactionPolicy(writer http.ResponseWriter, request *http.Request) {
	expected, ok := requireRevision(writer, request)
	if !ok {
		return
	}
	r.adminProjectMu.Lock()
	defer r.adminProjectMu.Unlock()
	policy, err := r.store.GetRedactionPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || policy.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	if policy.Revision != expected {
		adminPreconditionFailed(writer)
		return
	}
	if err := r.validateRedactionCanDeactivate(request, policy.ID, false); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	policy.Enabled = false
	policy.UpdatedAt = now
	policy.DeletedAt = &now
	policy, err = r.store.PutRedactionPolicy(request.Context(), policy, expected)
	if err != nil {
		adminMutationError(writer, err)
		return
	}
	if !r.reloadRedaction(writer, request) {
		return
	}
	if err := r.auditAdminMutation(request, "redaction_policy.delete", "redaction_policy", policy.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writer.Header().Set("ETag", revisionETag(policy.Revision))
	writer.WriteHeader(http.StatusNoContent)
}

func (r *Runtime) testAdminRedactionPolicy(writer http.ResponseWriter, request *http.Request) {
	policy, err := r.store.GetRedactionPolicy(request.Context(), chi.URLParam(request, "id"))
	if err != nil || policy.DeletedAt != nil {
		adminNotFound(writer)
		return
	}
	var input redactionTestInput
	if err := decodeAdminJSON(request, &input); err != nil || len(input.Input) > 16<<10 {
		adminBadRequest(writer, "invalid request")
		return
	}
	matches, err := r.redactor.Test(policy.ID, input.Input, input.Scope)
	input.Input = ""
	if err != nil {
		adminBadRequest(writer, err.Error())
		return
	}
	if err := r.auditAdminMutation(request, "redaction_policy.test", "redaction_policy", policy.ID); err != nil {
		adminAuditError(writer)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"matches": matches, "match_count": len(matches),
	})
}

func (input redactionPolicyInput) policy(
	policyID string,
	createdAt, updatedAt time.Time,
) (domain.RedactionPolicy, error) {
	policy := domain.RedactionPolicy{
		ID: policyID, Name: input.Name, Enabled: input.Enabled, Mode: input.Mode,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
		Rules: make([]domain.RedactionRule, 0, len(input.Rules)),
	}
	for _, value := range input.Rules {
		ruleID := value.ID
		if ruleID == "" {
			var err error
			ruleID, err = id.New("rrl")
			if err != nil {
				return domain.RedactionPolicy{}, err
			}
		}
		policy.Rules = append(policy.Rules, domain.RedactionRule{
			ID: ruleID, Name: value.Name, Kind: value.Kind, Builtin: value.Builtin,
			Pattern: value.Pattern, Dictionary: value.Dictionary, Scopes: value.Scopes,
			Action: value.Action, Replacement: value.Replacement,
			Enabled: value.Enabled, Priority: value.Priority,
		})
	}
	return redaction.CompilePolicy(policy)
}

func (r *Runtime) validateRedactionCanDeactivate(
	request *http.Request,
	id string,
	enabled bool,
) error {
	if enabled {
		return nil
	}
	projects, err := r.store.ListProjects(request.Context())
	if err != nil {
		return errors.New("projects are unavailable")
	}
	for _, project := range projects {
		if project.DeletedAt == nil && project.Enabled && project.RedactionPolicyID == id {
			return errors.New("remove this policy from active projects first")
		}
	}
	return nil
}

func (r *Runtime) reloadRedaction(writer http.ResponseWriter, request *http.Request) bool {
	policies, err := r.store.ListRedactionPolicies(request.Context())
	if err != nil || r.redactor.ReplacePolicies(policies) != nil {
		adminStoreError(writer)
		return false
	}
	return true
}
